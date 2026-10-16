import json
import os
from pathlib import Path
import subprocess
import sys
import threading
import unittest
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))


class Management:
    def __init__(self):
        self.posts = []

    def accounts(self):
        return {"accounts": [
            {"provider": "claude", "auth_index": "auth-04", "email": "research04@example.test"},
            {"provider": "codex", "auth_index": "codex-1", "email": "codex@example.test"},
            {"provider": "claude", "auth_index": "auth-05", "email": "research05@example.test"},
        ]}

    def post_scan(self, scan):
        self.posts.append(scan)


class Exporter:
    def __init__(self, fail=False):
        self.request = None
        self.fail = fail

    def export(self, request):
        from sync_receipts import SyncError
        self.request = request
        if self.fail:
            raise SyncError("export_unavailable")
        return {"scans": [{
            "auth_index": account["auth_index"], "account_email": account["email"],
            "observed_at": "2026-10-16T15:00:00Z", "source": "gmail", "receipts": [],
        } for account in request["accounts"]]}


class SyncTests(unittest.TestCase):
    def test_scans_only_inventory_claude_accounts_and_posts_empty_success(self):
        from sync_receipts import sync_accounts
        api, exporter = Management(), Exporter()
        summary = sync_accounts(api, exporter, "billing@example.test")
        self.assertEqual(exporter.request, {"mailbox": "billing@example.test", "accounts": [
            {"auth_index": "auth-04", "email": "research04@example.test"},
            {"auth_index": "auth-05", "email": "research05@example.test"},
        ]})
        self.assertEqual([scan["auth_index"] for scan in api.posts], ["auth-04", "auth-05"])
        self.assertTrue(all(scan["receipts"] == [] and "error_code" not in scan for scan in api.posts))
        self.assertEqual(summary, {"accounts": 2, "posted": 2, "failed_scans": 0, "post_failures": 0})

    def test_ssh_failure_posts_failed_freshness_for_every_inventory_account(self):
        from sync_receipts import sync_accounts
        api = Management()
        summary = sync_accounts(api, Exporter(fail=True), "billing@example.test",
                                observed_at="2026-10-16T15:01:00Z")
        self.assertEqual(summary["failed_scans"], 2)
        self.assertTrue(all(scan["error_code"] == "source_unavailable" and scan["receipts"] == []
                            and scan["observed_at"] == "2026-10-16T15:01:00Z" for scan in api.posts))

    def test_email_and_shell_characters_never_enter_the_ssh_command(self):
        from sync_receipts import SSHExporter, SyncError
        calls = []

        def run(argv, **kwargs):
            calls.append((argv, kwargs))
            return subprocess.CompletedProcess(argv, 0, '{"scans":[]}', '')

        exporter = SSHExporter("jf@192.168.1.184", runner=run)
        request = {"mailbox": "billing@example.test", "accounts": []}
        exporter.export(request)
        argv, kwargs = calls[0]
        self.assertIn("StrictHostKeyChecking=yes", argv)
        self.assertIn("BatchMode=yes", argv)
        self.assertNotIn("billing@example.test", " ".join(argv))
        self.assertEqual(json.loads(kwargs["input"]), request)
        with self.assertRaises(SyncError):
            SSHExporter("-oProxyCommand=bad")

    def test_service_ssh_identity_and_trust_are_explicit_argv_values(self):
        from sync_receipts import SSHExporter
        calls = []
        def run(argv, **kwargs):
            calls.append(argv)
            return subprocess.CompletedProcess(argv, 0, '{"scans":[]}', '')
        SSHExporter("jf@192.168.1.184", identity_file="/private/receipt key",
                    known_hosts="/private/known hosts", runner=run).export({"accounts": []})
        argv = calls[0]
        self.assertEqual(argv[argv.index("-i") + 1], "/private/receipt key")
        self.assertIn("IdentitiesOnly=yes", argv)
        self.assertIn('UserKnownHostsFile="/private/known hosts"', argv)

    def test_service_ssh_ignores_unrelated_system_and_user_configuration(self):
        from sync_receipts import SSHExporter
        calls = []
        def run(argv, **kwargs):
            calls.append(argv)
            return subprocess.CompletedProcess(argv, 0, '{"scans":[]}', '')
        SSHExporter("jf@192.168.1.184", identity_file="/private/billing-key",
                    known_hosts="/private/billing-hosts", runner=run).export({"accounts": []})
        argv = calls[0]
        self.assertIn("-F", argv)
        self.assertEqual(argv[argv.index("-F") + 1], "none")
        self.assertIn("StrictHostKeyChecking=yes", argv)
        self.assertIn("IdentitiesOnly=yes", argv)
        self.assertIn('UserKnownHostsFile="/private/billing-hosts"', argv)

    def test_invalid_inventory_returns_a_fixed_error(self):
        from sync_receipts import inventory_accounts, SyncError
        with self.assertRaises(SyncError):
            inventory_accounts({"accounts": ["bad row"]})


@contextmanager
def management_server(redirect=False):
    requests = []
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass
        def do_GET(self):
            requests.append((self.path, self.headers.get("Authorization")))
            if redirect and self.path != "/redirected":
                self.send_response(302)
                self.send_header("Location", "/redirected")
                self.end_headers()
                return
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"accounts":[]}')
    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.01}, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}", requests
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


class HTTPTests(unittest.TestCase):
    def test_environment_key_selection_without_secret_file_or_ssh(self):
        with management_server() as (url, requests):
            for provided, expected, args in [
                ({"CLIPROXY_MGMT_KEY": "fixture-primary"}, "fixture-primary", []),
                ({"CPA_MGMT_KEY": "fixture-fallback"}, "fixture-fallback", []),
                ({"CUSTOM_TEST_KEY": "fixture-explicit"}, "fixture-explicit", ["--key-env", "CUSTOM_TEST_KEY"]),
            ]:
                env = {key: value for key, value in os.environ.items()
                       if key not in {"CLIPROXY_MGMT_KEY", "CPA_MGMT_KEY"}}
                env.update(provided)
                result = subprocess.run([sys.executable, str(ROOT / "sync_receipts.py"),
                                         "--management-url", url, "--mailbox", "billing@example.test", *args],
                                        env=env, text=True, capture_output=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(requests[-1][1], "Bearer " + expected)
                self.assertNotIn(expected, result.stdout + result.stderr)

    def test_redirect_cannot_forward_management_key(self):
        from sync_receipts import ManagementClient, SyncError
        with management_server(redirect=True) as (url, requests):
            with self.assertRaises(SyncError):
                ManagementClient(url, "fixture-private").accounts()
            self.assertEqual(len(requests), 1)


if __name__ == "__main__":
    unittest.main()
