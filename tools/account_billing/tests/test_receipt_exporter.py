import base64
import hashlib
from pathlib import Path
import sys
import subprocess
import unittest


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))


class Gmail:
    def __init__(self, messages=None, failure=None):
        self.messages = messages or []
        self.failure = failure
        self.queries = []

    def search(self, query, page=None):
        self.queries.append(query)
        if self.failure:
            from receipt_exporter import ReceiptError
            raise ReceiptError(self.failure)
        return {"messages": [{"id": "message-05"}] if self.messages else []}

    def get(self, message_id):
        return {
            "headers": {"to": "research05@example.test"},
            "message": {"payload": {"headers": [
                {"name": "From", "value": "Anthropic <billing@anthropic.com>"},
                {"name": "Authentication-Results", "value":
                 "mx.google.com; dkim=pass header.i=@anthropic.com; "
                 "dmarc=pass header.from=anthropic.com"},
            ]}},
            "attachments": [{"attachmentId": item, "mimeType": "application/pdf", "size": 50}
                            for item in self.messages],
        }

    def attachment(self, message_id, attachment_id):
        return b"%PDF-1.4\n" + attachment_id.encode() + b"\n%%EOF\n"


def fixture_text(pdf):
    fixture = "research05_receipt.txt" if b"receipt" in pdf else "research05_invoice.txt"
    return (ROOT / "tests/fixtures" / fixture).read_text(encoding="utf-8")


class ReceiptParsingTests(unittest.TestCase):
    def test_invoice_keeps_exact_pdf_and_paid_period_without_inventing_renewal(self):
        from receipt_exporter import parse_invoice

        text = (ROOT / "tests/fixtures/research04_invoice.txt").read_text(encoding="utf-8")
        pdf = b"%PDF-1.4\nsynthetic invoice 04\n%%EOF\n"
        receipt = parse_invoice(
            text, pdf, recipient="research04@example.test",
            source_mailbox="billing@example.test", source_message_id="message-04",
        )
        self.assertEqual(receipt, {
            "invoice_id": "SYN-04-0001",
            "issued_at": "2026-10-16",
            "currency": "CAD", "amount_minor": 32193,
            "billing_name": "Example Buyer",
            "billing_email": "research04@example.test",
            "period_start": "2026-10-16", "period_end": "2026-11-16",
            "source_mailbox": "billing@example.test", "source_message_id": "message-04",
            "evidence_sha256": hashlib.sha256(pdf).hexdigest(),
            "pdf_base64": base64.b64encode(pdf).decode("ascii"),
        })

    def test_second_invoice_uses_its_billing_identity(self):
        from receipt_exporter import parse_invoice

        text = (ROOT / "tests/fixtures/research05_invoice.txt").read_text(encoding="utf-8")
        parsed = parse_invoice(text, b"%PDF-1.4\n05", recipient="research05@example.test",
                               source_mailbox="billing@example.test", source_message_id="05")
        self.assertEqual((parsed["invoice_id"], parsed["billing_name"], parsed["billing_email"]),
                         ("SYN-05-0002", "Example Company", "research05@example.test"))

    def test_pdf_layout_indentation_does_not_drop_invoice_fields(self):
        from receipt_exporter import parse_invoice
        text = (ROOT / "tests/fixtures/research05_invoice.txt").read_text(encoding="utf-8")
        text = "\n".join("    " + line for line in text.splitlines())
        receipt = parse_invoice(text, b"%PDF-1.4\n05", recipient="research05@example.test",
                                source_mailbox="billing@example.test", source_message_id="05")
        self.assertEqual(receipt["period_end"], "2026-11-16")

    def test_other_stripe_merchant_cannot_be_imported_as_anthropic(self):
        from receipt_exporter import parse_invoice, ReceiptError
        text = (ROOT / "tests/fixtures/research05_invoice.txt").read_text(encoding="utf-8")
        with self.assertRaises(ReceiptError):
            parse_invoice(text.replace("Anthropic, PBC", "Unrelated Merchant"), b"%PDF-1.4",
                          recipient="research05@example.test", source_mailbox="billing@example.test",
                          source_message_id="05")

    def test_pdf_billed_email_must_match_inventory_account(self):
        from receipt_exporter import parse_invoice, ReceiptError
        text = (ROOT / "tests/fixtures/research05_invoice.txt").read_text(encoding="utf-8")
        with self.assertRaises(ReceiptError):
            parse_invoice(text, b"%PDF-1.4", recipient="someoneelse@example.test",
                          source_mailbox="billing@example.test", source_message_id="05")


class ExportTests(unittest.TestCase):
    request = {"mailbox": "billing@example.test", "accounts": [
        {"auth_index": "auth-05", "email": "research05@example.test"},
    ]}

    def test_invoice_replaces_same_charge_receipt_and_preserves_pdf_bytes(self):
        from receipt_exporter import export_accounts

        result = export_accounts(self.request, gog=Gmail(["receipt", "invoice"]),
                                 extract_text=fixture_text, observed_at="2026-10-16T15:00:00Z")
        scan = result["scans"][0]
        self.assertEqual(scan["account_email"], "research05@example.test")
        self.assertNotIn("error_code", scan)
        self.assertEqual(len(scan["receipts"]), 1)
        self.assertEqual(base64.b64decode(scan["receipts"][0]["pdf_base64"]),
                         b"%PDF-1.4\ninvoice\n%%EOF\n")

    def test_successful_empty_scan_has_freshness_and_no_error(self):
        from receipt_exporter import export_accounts

        result = export_accounts(self.request, gog=Gmail(), extract_text=fixture_text,
                                 observed_at="2026-10-16T15:00:00Z")
        self.assertEqual(result, {"scans": [{
            "auth_index": "auth-05", "account_email": "research05@example.test",
            "source": "gmail", "observed_at": "2026-10-16T15:00:00Z", "receipts": [],
        }]})

    def test_failed_scan_does_not_disguise_failure_as_empty_success(self):
        from receipt_exporter import export_accounts

        scan = export_accounts(self.request, gog=Gmail(failure="gmail_unavailable"),
                               extract_text=fixture_text)["scans"][0]
        self.assertEqual(scan["error_code"], "source_unavailable")
        self.assertEqual(scan["receipts"], [])

    def test_forged_or_unverified_senders_do_not_create_financial_records(self):
        from receipt_exporter import export_accounts

        cases = [
            ("Anthropic <billing@attacker.example>",
             "mx.google.com; dkim=pass header.i=@attacker.example; dmarc=pass header.from=attacker.example"),
            ("Anthropic <billing@anthropic.com>",
             "mx.google.com; dkim=fail header.i=@anthropic.com; dmarc=fail header.from=anthropic.com"),
            ("Forwarder <billing@example.test>",
             "mx.google.com; dkim=pass header.i=@example.test; dmarc=pass header.from=example.test"),
        ]
        for sender, authentication in cases:
            with self.subTest(sender=sender):
                class UnverifiedGmail(Gmail):
                    def get(self, message_id):
                        message = super().get(message_id)
                        message["message"]["payload"]["headers"] = [
                            {"name": "From", "value": sender},
                            {"name": "Authentication-Results", "value": authentication},
                        ]
                        return message
                scan = export_accounts(self.request, gog=UnverifiedGmail(["invoice"]),
                                       extract_text=fixture_text)["scans"][0]
                self.assertEqual(scan.get("error_code"), "receipt_sender_unverified")
                self.assertEqual(scan["receipts"], [])

    def test_google_verified_arc_preserves_vendor_provenance_for_exact_recipient(self):
        from receipt_exporter import export_accounts

        class ForwardedGmail(Gmail):
            def get(self, message_id):
                message = super().get(message_id)
                message["message"]["payload"]["headers"] = arc_headers()
                return message
        scan = export_accounts(self.request, gog=ForwardedGmail(["invoice"]),
                               extract_text=fixture_text)["scans"][0]
        self.assertNotIn("error_code", scan)
        self.assertEqual(scan["receipts"][0]["source_authentication"], "gmail_arc")

    def test_unvalidated_arc_headers_cannot_authorize_a_forwarded_invoice(self):
        from receipt_exporter import export_accounts

        class ForgedArcGmail(Gmail):
            def get(self, message_id):
                message = super().get(message_id)
                headers = arc_headers()
                headers[1]["value"] = headers[1]["value"].replace("arc=pass", "arc=fail")
                message["message"]["payload"]["headers"] = headers
                return message
        scan = export_accounts(self.request, gog=ForgedArcGmail(["invoice"]),
                               extract_text=fixture_text)["scans"][0]
        self.assertEqual(scan["error_code"], "receipt_sender_unverified")
        self.assertEqual(scan["receipts"], [])

    def test_pinned_mailbox_rejects_other_request_before_gmail_access(self):
        result = subprocess.run([sys.executable, str(ROOT / "receipt_exporter.py"),
                                 "--mailbox", "pinned@example.test", "--gog", "does-not-exist"],
                                input='{"mailbox":"other@example.test","accounts":[{"auth_index":"1","email":"research05@example.test"}]}',
                                capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertEqual(result.stdout.strip(), '{"error_code": "request_invalid"}')

    def test_gmail_process_commands_cannot_write_or_send(self):
        from receipt_exporter import GogClient
        calls = []
        def run(argv, **kwargs):
            calls.append(argv)
            return subprocess.CompletedProcess(argv, 0, '{"messages":[]}', '')
        GogClient("billing@example.test", runner=run).search("target query")
        argv = calls[0]
        self.assertIn("--readonly", argv)
        self.assertIn("--gmail-no-send", argv)
        self.assertEqual(argv[argv.index("--enable-commands-exact") + 1],
                         "gmail.messages.search,gmail.get,gmail.attachment")


def arc_headers():
    return [
        {"name": "From", "value": "research05@example.test"},
        {"name": "Authentication-Results", "value":
         "mx.google.com; dkim=pass header.i=@example.test; "
         "dmarc=pass header.from=example.test; arc=pass"},
        {"name": "X-Original-From", "value": "Anthropic <billing@mail.anthropic.com>"},
        {"name": "X-Original-Sender", "value": "billing@mail.anthropic.com"},
        {"name": "ARC-Seal", "value": "i=3; d=google.com; cv=pass; b=synthetic"},
        {"name": "ARC-Seal", "value": "i=2; d=google.com; cv=pass; b=synthetic"},
        {"name": "ARC-Seal", "value": "i=1; d=google.com; cv=none; b=synthetic"},
        {"name": "ARC-Authentication-Results", "value":
         "i=3; mx.google.com; dkim=pass header.i=@example.test; dmarc=pass header.from=example.test"},
        {"name": "ARC-Authentication-Results", "value":
         "i=2; mx.google.com; dkim=pass header.i=@mail.anthropic.com; dmarc=pass header.from=mail.anthropic.com"},
        {"name": "ARC-Authentication-Results", "value":
         "i=1; mx.google.com; dkim=pass header.i=@mail.anthropic.com; dmarc=pass header.from=mail.anthropic.com"},
    ]


if __name__ == "__main__":
    unittest.main()
