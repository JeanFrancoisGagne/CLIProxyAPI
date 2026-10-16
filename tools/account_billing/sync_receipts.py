"""Pull CLIProxy accounts, read receipts through fixed SSH, and post scan results."""

from __future__ import annotations

import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import subprocess
import sys
from urllib.error import HTTPError, URLError
from urllib.parse import urlsplit
from urllib.request import HTTPRedirectHandler, Request, build_opener

from receipt_exporter import checked_email, ReceiptError, SCAN_ERRORS, scan_error


BASE = "/v8/management/credentials/accounts"
REMOTE_COMMAND = (
    "docker exec -i --user node openclaw-openclaw-gateway-1 python3 "
    "/home/node/.openclaw/workspace/tools/account_billing/receipt_exporter.py"
)


class SyncError(Exception):
    """A fixed error code; command output and HTTP bodies remain private."""


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, request, response, code, message, headers, new_url):
        # urllib copies Authorization across redirects; keep the key on its configured host.
        raise HTTPError(request.full_url, code, message, headers, response)


class ManagementClient:
    def __init__(self, base_url: str, key: str):
        parsed = urlsplit(base_url)
        if (parsed.scheme not in {"http", "https"} or not parsed.hostname
                or parsed.username or parsed.password or parsed.query or parsed.fragment):
            raise SyncError("management_url_invalid")
        if not key or any(char.isspace() for char in key):
            raise SyncError("management_key_invalid")
        self.url = base_url.rstrip("/") + BASE
        self.key = key
        self.opener = build_opener(NoRedirect())

    def request(self, suffix: str = "", body: dict | None = None) -> dict:
        data = None if body is None else json.dumps(body, separators=(",", ":")).encode()
        request = Request(self.url + suffix, data=data, headers={
            "Authorization": "Bearer " + self.key, "Content-Type": "application/json",
        })
        try:
            with self.opener.open(request, timeout=45) as response:
                raw = response.read(2_000_001)
        except (HTTPError, URLError, OSError):
            raise SyncError("management_request_failed") from None
        if len(raw) > 2_000_000:
            raise SyncError("management_response_invalid")
        try:
            result = json.loads(raw)
        except (ValueError, TypeError):
            raise SyncError("management_response_invalid") from None
        if not isinstance(result, dict):
            raise SyncError("management_response_invalid")
        return result

    def accounts(self) -> dict:
        return self.request()

    def post_scan(self, scan: dict) -> dict:
        return self.request("/receipts", scan)


class SSHExporter:
    def __init__(self, target: str, *, identity_file: str | None = None,
                 known_hosts: str | None = None, runner=subprocess.run):
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.@:-]*", target):
            raise SyncError("ssh_target_invalid")
        # All connection settings are explicit; host config includes can fail inside the service sandbox.
        self.argv = ["ssh", "-F", "none", "-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes",
                     "-o", "ConnectTimeout=5", "-o", "ServerAliveInterval=15",
                     "-o", "ServerAliveCountMax=2"]
        if identity_file:
            self.argv += ["-i", identity_file, "-o", "IdentitiesOnly=yes"]
        if known_hosts:
            if any(char in known_hosts for char in "\r\n\0"):
                raise SyncError("ssh_trust_path_invalid")
            quoted = known_hosts.replace("\\", "\\\\").replace('"', '\\"')
            self.argv += ["-o", 'UserKnownHostsFile="' + quoted + '"']
        self.argv += [target, REMOTE_COMMAND]
        self.runner = runner

    def export(self, request: dict) -> dict:
        try:
            result = self.runner(self.argv, input=json.dumps(request), text=True,
                                 capture_output=True, timeout=900, check=False)
        except (OSError, subprocess.TimeoutExpired):
            raise SyncError("export_unavailable") from None
        if result.returncode:
            raise SyncError("export_failed")
        if len(result.stdout) > 64_000_000:
            raise SyncError("export_response_invalid")
        try:
            value = json.loads(result.stdout)
        except (ValueError, TypeError):
            raise SyncError("export_response_invalid") from None
        if not isinstance(value, dict):
            raise SyncError("export_response_invalid")
        return value


def inventory_accounts(inventory: dict) -> list[dict]:
    values = inventory.get("accounts")
    if not isinstance(values, list):
        raise SyncError("inventory_invalid")
    accounts = []
    seen = set()
    for account in values:
        if not isinstance(account, dict):
            raise SyncError("inventory_invalid")
        if account.get("provider") != "claude" or not account.get("email"):
            continue
        try:
            email = checked_email(account["email"])
        except ReceiptError:
            raise SyncError("inventory_invalid") from None
        index = account.get("auth_index")
        if not isinstance(index, str) or not index or index in seen:
            raise SyncError("inventory_invalid")
        seen.add(index)
        accounts.append({"auth_index": index, "email": email})
    return accounts


def checked_scans(response: dict, accounts: list[dict]) -> list[dict]:
    scans = response.get("scans")
    if not isinstance(scans, list) or len(scans) != len(accounts):
        raise SyncError("export_response_invalid")
    expected = {account["auth_index"]: account["email"].casefold() for account in accounts}
    seen = set()
    for scan in scans:
        if not isinstance(scan, dict):
            raise SyncError("export_response_invalid")
        index = scan.get("auth_index")
        if (index not in expected or index in seen
                or str(scan.get("account_email", "")).casefold() != expected[index]
                or scan.get("source") != "gmail" or not scan.get("observed_at")
                or not isinstance(scan.get("receipts"), list)):
            raise SyncError("export_response_invalid")
        if scan.get("error_code") and scan["receipts"]:
            raise SyncError("export_response_invalid")
        if scan.get("error_code") and scan["error_code"] not in SCAN_ERRORS:
            raise SyncError("export_response_invalid")
        seen.add(index)
    return scans


def sync_accounts(api, exporter, mailbox: str, *, observed_at: str | None = None) -> dict:
    checked_email(mailbox)
    accounts = inventory_accounts(api.accounts())
    summary = {"accounts": len(accounts), "posted": 0, "failed_scans": 0, "post_failures": 0}
    if not accounts:
        return summary
    observed_at = observed_at or datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
    try:
        scans = checked_scans(exporter.export({"mailbox": mailbox, "accounts": accounts}), accounts)
    except SyncError as error:
        scans = [{"auth_index": account["auth_index"], "account_email": account["email"],
                  "source": "gmail", "observed_at": observed_at, "receipts": [],
                  "error_code": scan_error(str(error))} for account in accounts]
    for scan in scans:
        summary["failed_scans"] += bool(scan.get("error_code"))
        try:
            api.post_scan(scan)
            summary["posted"] += 1
        except SyncError:
            summary["post_failures"] += 1
    return summary


def read_key(path: str) -> str:
    target = Path(path)
    info = target.stat()
    if os.name != "nt" and (info.st_uid != os.getuid() or info.st_mode & 0o077):
        raise SyncError("management_key_permissions")
    return target.read_text(encoding="utf-8").strip()


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--management-url", required=True)
    keys = parser.add_mutually_exclusive_group()
    keys.add_argument("--key-file")
    keys.add_argument("--key-env", help="Environment variable containing the management key")
    parser.add_argument("--mailbox", required=True)
    parser.add_argument("--ssh-target", default="jf@192.168.1.184")
    parser.add_argument("--ssh-identity")
    parser.add_argument("--ssh-known-hosts")
    args = parser.parse_args()
    try:
        key = read_key(args.key_file) if args.key_file else os.environ.get(args.key_env or "CLIPROXY_MGMT_KEY", "")
        if not key and not args.key_file and not args.key_env:
            key = os.environ.get("CPA_MGMT_KEY", "")
        client = ManagementClient(args.management_url, key)
        exporter = SSHExporter(args.ssh_target, identity_file=args.ssh_identity, known_hosts=args.ssh_known_hosts)
        result = sync_accounts(client, exporter, args.mailbox)
    except (SyncError, ReceiptError, OSError, ValueError, TypeError):
        print(json.dumps({"error_code": "sync_failed"}))
        return 1
    print(json.dumps(result, separators=(",", ":")))
    return int(bool(result["failed_scans"] or result["post_failures"]))


if __name__ == "__main__":
    raise SystemExit(main())
