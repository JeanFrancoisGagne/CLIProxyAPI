# Account receipt collection

These tools read invoices through an existing Gmail authorization. They do not
log in to Claude billing, change subscriptions, or make payments.

## Runtime

- `receipt_exporter.py` runs inside the existing SAM gateway container on ldb.
  It needs Python 3.10+, the installed `gog`, `pdftotext`, and the existing Gog
  keyring environment. It reads one JSON request from stdin and prints one JSON
  response. Deploy with `--mailbox` to pin the authorized mailbox.
- `sync_receipts.py` runs on the proxy host. Install both Python files in the
  same directory, since the driver imports shared validation from the exporter.
  It reads `GET /v8/management/credentials/accounts`, selects Claude accounts,
  calls the fixed SSH exporter command, and posts each scan to the receipts API.
  The deployment templates are in `deployment/account-billing/`.

The driver reads its management key from `CLIPROXY_MGMT_KEY`, then
`CPA_MGMT_KEY`. `--key-env NAME` selects one variable without fallback.
`--key-file PATH` is an alternative and requires an owner-only file on Linux.
Never put the management key in command arguments.

SSH defaults to `jf@192.168.1.184`. `--ssh-identity` adds `IdentitiesOnly=yes`;
`--ssh-known-hosts` selects a dedicated trust file. Host verification stays
enabled. `-F none` isolates the job from unrelated system and user SSH configuration;
the target, identity and trust file are supplied explicitly by its service.
The command is fixed; mailbox and account values travel only in stdin
JSON. The exporter retrieves attachments with Gmail's attachment API and never
opens links found in mail.

## Evidence and failure rules

Direct mail requires an Anthropic or Stripe sender with aligned DKIM and DMARC
reported by Gmail. Forwarded mail requires the outer sender to equal the billed
account, aligned outer authentication, Gmail's successful ARC validation, a
contiguous Google-sealed ARC chain, and earlier aligned Anthropic or Stripe
authentication. These paths are recorded as `gmail_direct` and `gmail_arc`.
The PDF must name `Anthropic, PBC`; another Stripe merchant is rejected.

The authenticated message recipient or PDF must match the inventory email.
A different PDF billing email is rejected. Invoice PDFs take precedence over
payment receipts with the same invoice ID. Stored PDF bytes and their SHA-256
digest are exact. Issue dates and service periods keep their printed calendar
dates. A period end never becomes a renewal date without an explicit label.

Every account produces a scan, including successful empty results. Failures
carry an error code and no replacement records so the backend preserves old
data. Error codes are `source_error`, `source_unavailable`,
`receipt_parse_failed`, `incomplete_scan`, `receipt_sender_unverified`, and
`gmail_read_failed`. The driver also accepts backend codes
`mailbox_auth_required` and `rate_limited`.

Each PDF is limited to 2 MB, each scan to 100 invoices and 7.5 MB of JSON, and
the exporter response to 32 MB. Pagination, size, or count exhaustion reports
`incomplete_scan`; it never reports a partial scan as complete. Invoices that
use other layouts report a parse failure for review.

## Verification

```sh
python -m unittest discover -s tools/account_billing/tests -v
```

Fixtures retain the observed invoice/receipt layout from two accounts. All
identities, invoice IDs, and PDF payloads are synthetic. `exported_scan.json`
is an actual exporter response from those fixtures for backend contract tests.
Live read-only checks on the two authorized accounts each found one invoice,
verified `gmail_arc`, preserved the exact attachment, and returned no error.
No service deployment or POST is part of that dry export.
