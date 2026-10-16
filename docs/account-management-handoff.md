# T3 handoff: automatic account management

User intent: manage connected Claude account identity, subscription details and monthly invoices through the existing proxy, without signing in to every account. Keep the Codex recovery session on its direct account. The shared endpoint remains `http://100.113.156.52:8317`.

## Read first

- `docs/account-management.md` owns the data-source and freshness rules.
- `docs/account-management-deployment.md` owns deployment evidence and the exact rollback.
- `tools/account_billing/` owns read-only mailbox collection.
- UI work is in the sibling `E:/wt/cpamc-account-management` checkout. Backend work is in `E:/wt/cliproxy-account-management`. Both use branch `codex/account-management` in their respective repositories.

## Decisions that matter

Claude's CLI OAuth profile supplies plan and account fields. Its token was tested against the actual web billing endpoints and received HTTP 403. Browser billing sessions are a different authorization route. Preserve the working Gmail receipt source instead of saving browser cookies.

The Nera mailbox already receives the new accounts' invoice PDFs. These messages preserve a Google-validated ARC chain for Anthropic. Accept only the narrow validated direct or forwarded paths implemented in the exporter. The collector uses the proxy inventory each run; it does not keep a second account list.

Invoice dates are calendar dates. Keep them that way. A midnight UTC conversion displays the preceding day in Toronto. A printed billing period end does not prove an upcoming renewal charge. The UI labels these separately.

Profile and billing failures retain last confirmed data. Expired observations become stale. Raw access tokens, refresh tokens, management keys, payment links and mailbox authorization material do not belong in API overviews, browser output, logs or this handoff.

## Change verification

Before changing collection, run the Python boundary tests and test an actual read-only receipt export. Before changing profile persistence, run the Go management, account-management, refresh/concurrency and executor tests. Build the proxy before deployment. Before changing the UI, use the pinned Bun version, run its checks, then inspect the live account page and download one invoice through the authenticated action.

The deployment record is the source for current completion state. This handoff records decisions, not a replacement for that evidence.
