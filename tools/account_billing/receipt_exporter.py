"""Read scoped Gmail invoices without exporting Google credentials."""

from __future__ import annotations

import argparse
import base64
from datetime import date, datetime, timezone
from decimal import Decimal
from email.utils import getaddresses
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile


EMAIL = re.compile(r"[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}")
MONTH = r"(?:Jan(?:uary)?|Feb(?:ruary)?|Mar(?:ch)?|Apr(?:il)?|May|Jun(?:e)?|Jul(?:y)?|Aug(?:ust)?|Sep(?:tember)?|Oct(?:ober)?|Nov(?:ember)?|Dec(?:ember)?)"
DATE = MONTH + r"\s+\d{1,2}(?:,?\s+\d{4})?"
COMMANDS = "gmail.messages.search,gmail.get,gmail.attachment"
SCAN_LIMIT_BYTES = 7_500_000
EXPORT_LIMIT_BYTES = 32_000_000
SCAN_ERRORS = frozenset({"source_error", "source_unavailable", "mailbox_auth_required",
                         "receipt_parse_failed", "incomplete_scan", "rate_limited",
                         "receipt_sender_unverified", "gmail_read_failed"})


class ReceiptError(Exception):
    """A fixed, non-sensitive error code suitable for scan metadata."""


def scan_error(code: str) -> str:
    if code in SCAN_ERRORS:
        return code
    if code in {"gmail_unavailable", "pdf_text_unavailable", "export_unavailable"}:
        return "source_unavailable"
    if code.endswith("_limit") or code in {"gmail_pagination_invalid", "export_response_invalid"}:
        return "incomplete_scan"
    if code.startswith("receipt_"):
        return "receipt_parse_failed"
    return "source_error"


def parse_date(value: str, year: int | None = None) -> date:
    value = value.replace(",", "")
    if not re.search(r"\b\d{4}\b", value):
        if year is None:
            raise ReceiptError("receipt_date_invalid")
        value += f" {year}"
    for fmt in ("%B %d %Y", "%b %d %Y"):
        try:
            return datetime.strptime(value, fmt).date()
        except ValueError:
            pass
    raise ReceiptError("receipt_date_invalid")


def billing_fields(text: str, recipient: str) -> dict:
    lines = text.splitlines()
    for index, line in enumerate(lines):
        if "Bill to" not in line:
            continue
        column = line.index("Bill to")
        block = []
        for row in lines[index + 1:index + 9]:
            if "Description" in row or re.search(r"\b(?:due|paid)\b", row, re.I):
                break
            value = re.split(r"\s{2,}", row.strip())[-1] if column else row.strip()
            if value:
                block.append(value)
        emails = EMAIL.findall("\n".join(block))
        if emails and recipient.casefold() not in {email.casefold() for email in emails}:
            raise ReceiptError("receipt_recipient_mismatch")
        fields = {}
        if block and not EMAIL.search(block[0]):
            fields["billing_name"] = block[0]
        if emails:
            fields["billing_email"] = recipient
        return fields
    raise ReceiptError("receipt_billing_missing")


def receipt_amount(text: str) -> tuple[str, int]:
    matches = re.findall(
        r"^(?:Amount (?:due|paid)|Total)\s+(CA\$|US\$|\$|€|£)\s*([\d,]+\.\d{2})\s*$",
        text, re.M | re.I,
    )
    if not matches:
        raise ReceiptError("receipt_amount_missing")
    symbol, value = matches[-1]
    currency = {"CA$": "CAD", "US$": "USD", "$": "USD", "€": "EUR", "£": "GBP"}[symbol.upper()]
    return currency, int(Decimal(value.replace(",", "")) * 100)


def parse_invoice(text: str, pdf: bytes, *, recipient: str,
                  source_mailbox: str, source_message_id: str) -> dict:
    """Parse one invoice or receipt; period end never implies renewal."""
    layout_text = text
    text = "\n".join(line.strip() for line in text.splitlines())
    if not re.search(r"^Anthropic,\s+PBC\b", text, re.M | re.I):
        raise ReceiptError("receipt_issuer_mismatch")
    invoice = re.search(r"^Invoice number\s+(\S+)\s*$", text, re.M | re.I)
    issued = re.search(r"^Date (?:of issue|paid)\s+(" + DATE + r")\s*$", text, re.M | re.I)
    if not invoice or not issued:
        raise ReceiptError("receipt_identity_missing")
    issued_date = parse_date(issued.group(1))
    currency, amount = receipt_amount(text)
    result = {
        "invoice_id": invoice.group(1),
        "issued_at": issued_date.isoformat(),
        "currency": currency, "amount_minor": amount,
        **billing_fields(layout_text, recipient),
        "source_mailbox": source_mailbox, "source_message_id": source_message_id,
        "evidence_sha256": hashlib.sha256(pdf).hexdigest(),
        "pdf_base64": base64.b64encode(pdf).decode("ascii"),
    }
    period = re.search(r"^(" + DATE + r")\s*[–—-]\s*(" + DATE + r")\s*$", text, re.M | re.I)
    if period:
        end = parse_date(period.group(2), issued_date.year)
        start = parse_date(period.group(1), end.year)
        if start > end:
            start = parse_date(period.group(1), end.year - 1)
        result.update(period_start=start.isoformat(), period_end=end.isoformat())
    renewal = re.search(r"^(?:Next billing date|Next renewal|Renews on)\s*:?\s*(" + DATE + r")\s*$", text, re.M | re.I)
    if renewal:
        result["renewal_at"] = parse_date(renewal.group(1), issued_date.year).isoformat()
    return result


def checked_email(value: object) -> str:
    if not isinstance(value, str) or not EMAIL.fullmatch(value):
        raise ReceiptError("account_email_invalid")
    return value


class GogClient:
    """The only Gmail boundary; every invocation forbids writes and sends."""

    def __init__(self, mailbox: str, executable: str = "/home/node/.openclaw/workspace/gog", runner=subprocess.run):
        self.base = [executable, "--account", checked_email(mailbox), "--readonly",
                     "--gmail-no-send", "--no-input", "--enable-commands-exact", COMMANDS]
        self.runner = runner
        self.env = dict(os.environ, HOME="/home/node", XDG_CONFIG_HOME="/home/node/.openclaw/workspace/.config")

    def command(self, args: list[str]) -> dict:
        try:
            result = self.runner(self.base + args + ["--json"], env=self.env,
                                 capture_output=True, text=True, timeout=45, check=False)
        except (OSError, subprocess.TimeoutExpired):
            raise ReceiptError("gmail_unavailable") from None
        if result.returncode:
            raise ReceiptError("gmail_read_failed")
        try:
            value = json.loads(result.stdout)
        except (ValueError, TypeError):
            raise ReceiptError("gmail_response_invalid") from None
        if not isinstance(value, dict):
            raise ReceiptError("gmail_response_invalid")
        return value

    def search(self, query: str, page: str | None = None) -> dict:
        args = ["gmail", "messages", "search", query, "--max", "25"]
        if page:
            args += ["--page", page]
        return self.command(args)

    def get(self, message_id: str) -> dict:
        return self.command(["gmail", "get", message_id, "--format", "full"])

    def attachment(self, message_id: str, attachment_id: str) -> bytes:
        # The existing keyring remains in place; only this process's PDF is temporary.
        with tempfile.TemporaryDirectory(prefix="account-billing-") as directory:
            destination = Path(directory) / "invoice.pdf"
            self.command(["gmail", "attachment", message_id, attachment_id, "--out", str(destination)])
            if not destination.is_file() or destination.stat().st_size > 2_000_000:
                raise ReceiptError("receipt_pdf_invalid")
            data = destination.read_bytes()
        if not data.startswith(b"%PDF-"):
            raise ReceiptError("receipt_pdf_invalid")
        return data


def pdf_text(data: bytes, executable: str = "pdftotext") -> str:
    try:
        result = subprocess.run([executable, "-layout", "-enc", "UTF-8", "-", "-"],
                                input=data, capture_output=True, timeout=30, check=False)
    except (OSError, subprocess.TimeoutExpired):
        raise ReceiptError("pdf_text_unavailable") from None
    if result.returncode or len(result.stdout) > 1_000_000:
        raise ReceiptError("receipt_pdf_invalid")
    return result.stdout.decode("utf-8", errors="replace")


def message_recipients(message: dict) -> set[str]:
    headers = message.get("message", {}).get("payload", {}).get("headers", [])
    values = [str(item.get("value", "")) for item in headers
              if str(item.get("name", "")).lower() in {"to", "delivered-to", "x-original-to"}]
    normalized = message.get("headers", {})
    if isinstance(normalized, dict):
        values.extend(str(value) for key, value in normalized.items()
                      if key.lower() in {"to", "delivered-to", "x-original-to"})
    return {email.casefold() for value in values for email in EMAIL.findall(value)}


def vendor_domain(domain: str) -> str | None:
    domain = domain.lower().rstrip(".")
    for vendor in ("anthropic.com", "stripe.com"):
        if domain == vendor or domain.endswith("." + vendor):
            return vendor
    return None


def aligned_authentication(result: str, domain: str) -> bool:
    dmarc = re.search(r"(?:^|;)\s*dmarc=pass\b[^;]*\bheader\.from=([\w.-]+)", result, re.I)
    dkim = re.findall(r"(?:^|;)\s*dkim=pass\b[^;]*\bheader\.[id]=(?:[^@\s;]*@)?([\w.-]+)", result, re.I)
    if not dmarc or dmarc.group(1).lower() != domain:
        return False
    vendor = vendor_domain(domain)
    return any(item.lower() == domain or (vendor and vendor_domain(item) == vendor) for item in dkim)


def arc_vendor_verified(headers: list[dict], final_auth: str) -> bool:
    if not re.search(r"(?:^|;)\s*arc=pass\b", final_auth, re.I):
        return False
    seals, results = {}, {}
    original = [item["value"] for item in headers if item["name"].lower() == "x-original-from"]
    addresses = getaddresses(original)
    if len(original) != 1 or len(addresses) != 1 or "@" not in addresses[0][1]:
        return False
    domain = addresses[0][1].rsplit("@", 1)[1].lower()
    if not vendor_domain(domain):
        return False
    for header in headers:
        name, value = header["name"].lower(), header["value"]
        if name not in {"arc-seal", "arc-authentication-results"}:
            continue
        match = re.search(r"(?:^|;)\s*i=(\d+)\b", value)
        if not match:
            return False
        index = int(match.group(1))
        target = seals if name == "arc-seal" else results
        if index in target:
            return False
        target[index] = value
    if not seals or len(seals) > 20 or set(seals) != set(range(1, len(seals) + 1)) or set(results) != set(seals):
        return False
    for index, value in seals.items():
        cv = "none" if index == 1 else "pass"
        if not re.search(r"(?:^|;)\s*d=google\.com(?:;|\s|$)", value, re.I):
            return False
        if not re.search(r"(?:^|;)\s*cv=" + cv + r"(?:;|\s|$)", value, re.I):
            return False
    return any(index < len(seals) and re.match(r"\s*i=\d+;\s*mx\.google\.com\s*;", value, re.I)
               and aligned_authentication(value, domain) for index, value in results.items())


def verify_sender(message: dict, recipient: str) -> str:
    headers = message.get("message", {}).get("payload", {}).get("headers", [])
    senders = [item.get("value", "") for item in headers if item.get("name", "").lower() == "from"]
    auth = [item.get("value", "") for item in headers
            if item.get("name", "").lower() == "authentication-results"]
    addresses = getaddresses(senders)
    if len(senders) != 1 or len(addresses) != 1 or "@" not in addresses[0][1] or not auth:
        raise ReceiptError("receipt_sender_unverified")
    domain = addresses[0][1].rsplit("@", 1)[1].lower()
    vendor = vendor_domain(domain)
    # Gmail's first result must authenticate the sender. Forwarded vendor identity
    # requires its validated ARC chain; a later injected auth header cannot authorize it.
    result = auth[0]
    if not re.match(r"\s*mx\.google\.com\s*;", result, re.I):
        raise ReceiptError("receipt_sender_unverified")
    if not aligned_authentication(result, domain):
        raise ReceiptError("receipt_sender_unverified")
    if vendor:
        return "gmail_direct"
    if addresses[0][1].casefold() == recipient.casefold() and arc_vendor_verified(headers, result):
        return "gmail_arc"
    raise ReceiptError("receipt_sender_unverified")


def collect_receipts(gog, recipient: str, mailbox: str, extract_text) -> list[dict]:
    query = f'(to:{recipient} OR deliveredto:{recipient}) subject:"Your receipt from Anthropic"'
    selected = {}
    page = None
    seen_pages = set()
    for _ in range(20):
        response = gog.search(query, page)
        for item in response.get("messages", []):
            message_id = item["id"]
            message = gog.get(message_id)
            authentication = verify_sender(message, recipient)
            candidates = message.get("attachments", [])
            if len(candidates) > 20:
                raise ReceiptError("receipt_attachment_limit")
            for attachment in candidates:
                if attachment.get("mimeType") != "application/pdf":
                    continue
                if int(attachment.get("size", 0)) > 2_000_000:
                    raise ReceiptError("receipt_pdf_invalid")
                data = gog.attachment(message_id, attachment["attachmentId"])
                text = extract_text(data)
                # Forwarded receipts may name the account only inside the invoice.
                pdf_recipients = {email.casefold() for email in EMAIL.findall(text)}
                if recipient.casefold() not in message_recipients(message) | pdf_recipients:
                    raise ReceiptError("receipt_recipient_mismatch")
                receipt = parse_invoice(text, data, recipient=recipient,
                                        source_mailbox=mailbox, source_message_id=message_id)
                receipt["source_authentication"] = authentication
                rank = (bool(re.search(r"^Invoice\s*$", text, re.M)), receipt["evidence_sha256"])
                previous = selected.get(receipt["invoice_id"])
                if previous is None or rank > previous[0]:
                    selected[receipt["invoice_id"]] = (rank, receipt)
                size = sum(len(json.dumps(value[1], separators=(",", ":"))) for value in selected.values())
                if len(selected) > 100 or size > SCAN_LIMIT_BYTES:
                    raise ReceiptError("receipt_export_limit")
        page = response.get("nextPageToken")
        if not page:
            return sorted((value[1] for value in selected.values()),
                          key=lambda item: (item["issued_at"], item["invoice_id"]))
        if page in seen_pages:
            raise ReceiptError("gmail_pagination_invalid")
        seen_pages.add(page)
    raise ReceiptError("gmail_scan_limit")


def export_accounts(request: dict, *, gog=None, extract_text=pdf_text, observed_at: str | None = None) -> dict:
    mailbox = checked_email(request["mailbox"])
    accounts = request["accounts"]
    if not isinstance(accounts, list) or len(accounts) > 100:
        raise ReceiptError("accounts_invalid")
    checked = []
    for account in accounts:
        recipient = checked_email(account["email"])
        index = account["auth_index"]
        if not isinstance(index, str) or not index or len(index) > 256:
            raise ReceiptError("auth_index_invalid")
        checked.append((index, recipient))
    gog = gog or GogClient(mailbox)
    observed_at = observed_at or datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
    scans = []
    total_size = 0
    for index, recipient in checked:
        scan = {"auth_index": index, "account_email": recipient, "source": "gmail",
                "observed_at": observed_at, "receipts": []}
        try:
            scan["receipts"] = collect_receipts(gog, recipient, mailbox, extract_text)
        except ReceiptError as error:
            scan["error_code"] = scan_error(str(error))
        except (KeyError, TypeError, ValueError):
            scan["error_code"] = "source_error"
        size = len(json.dumps(scan, separators=(",", ":")))
        if total_size + size > EXPORT_LIMIT_BYTES:
            scan["receipts"] = []
            scan["error_code"] = "incomplete_scan"
            size = len(json.dumps(scan, separators=(",", ":")))
        total_size += size
        scans.append(scan)
    return {"scans": scans}


def main() -> int:
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--gog", default="/home/node/.openclaw/workspace/gog")
    parser.add_argument("--pdftotext", default="pdftotext")
    parser.add_argument("--mailbox", help="Pin the only mailbox accepted by this deployment")
    args = parser.parse_args()
    try:
        raw = sys.stdin.read(1_000_001)
        if len(raw) > 1_000_000:
            raise ReceiptError("request_too_large")
        request = json.loads(raw)
        if args.mailbox and checked_email(request["mailbox"]).casefold() != checked_email(args.mailbox).casefold():
            raise ReceiptError("mailbox_mismatch")
        result = export_accounts(request, gog=GogClient(request["mailbox"], args.gog),
                                 extract_text=lambda data: pdf_text(data, args.pdftotext))
    except (ReceiptError, KeyError, TypeError, ValueError):
        print(json.dumps({"error_code": "request_invalid"}))
        return 2
    print(json.dumps(result, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
