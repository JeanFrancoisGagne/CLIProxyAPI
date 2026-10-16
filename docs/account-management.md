# Automatic account management

This feature combines two independently verified sources:

- Claude OAuth profile data supplies the account identity, plan, subscription status and subscription start timestamp.
- The already authorized Gmail receipt source supplies invoices, billing identity, amounts and printed service periods.

The management page loads and refreshes these records automatically. The operator does not need to sign in to every Claude account to collect routine billing data.

## Evidence and dates

A token expiration is not a subscription expiration. A quota reset is not a billing renewal. A subscription creation timestamp does not prove the next charge date.

Keep `renewal_at` separate from `period_end`. Populate renewal only from an explicit source statement. An invoice line-item period proves the billed period, not that the subscription will renew.

A missing profile name remains missing. Keep the login email and billed recipient email separate.

## Ownership

The proxy stores safe account snapshots alongside each credential through its existing durable credential store. Login and token refresh populate profile snapshots. A bounded background enrichment pass handles older credentials without adding network work to inference.

A read-only receipt exporter uses the existing Gmail authorization where it already lives. A collector submits normalized per-account scans to the authenticated management API. Google tokens stay with their current owner; the proxy receives verified billing records and invoice PDFs.

The collector identifies a credential by its current auth index and exact login email. A billing address or invoice name cannot reassign a receipt to another credential.

Invoice PDFs are named private artifacts under `.account-invoices` in the configured auth directory. The overview returns safe metadata. The authenticated download route resolves only an invoice belonging to the selected account.

## Refresh behavior

Profile and billing freshness are independent. A failed source check retains the last confirmed data and marks it stale. An empty successful receipt scan does not erase older invoices. Invoice IDs deduplicate repeated collection.

The collector is read-only with respect to Gmail. It does not send, delete, archive, label or mark messages read.

## Validation contract

Tests exercise the management HTTP boundary, credential refresh/persistence, receipt exporter stdin/stdout contract and collector transport. Fixtures must be synthetic or redacted. Tests must prove stale-data preservation, exact account matching, idempotent receipt import, bounded PDF validation, authenticated download and exclusion of tokens from public responses.

Live verification uses actual connected profiles and two separately issued invoices. It also confirms normal inference still works after deployment.

## Receipt trust

Accept direct mail only when Gmail reports aligned vendor authentication. Forwarded mail additionally needs Gmail's passing ARC result, the preserved Google-sealed vendor authentication chain, and an outer sender matching the connected account. A PDF must identify the exact billed account. A matching subject or a vendor logo alone is insufficient.

The collector downloads attachments through Gmail's API. It never follows payment links or arbitrary URLs from a message. A receipt that cannot be verified produces a visible source error and leaves earlier invoices intact.

## Host installation

The host-specific units and restricted exporter wrapper are in `deployment/account-billing/`.

| Host | Installed component | Authority |
| --- | --- | --- |
| NVR, `192.168.1.205` | `~/.local/lib/cliproxy-account-billing/` | Proxy management key from the existing private `secrets.env` |
| NVR | `cliproxy-account-billing.timer` | Runs collection every six hours, with up to five minutes of jitter |
| Existing mailbox host, `192.168.1.184` | SAM workspace `tools/account_billing/receipt_exporter.py` | Existing read-only Gmail authorization |
| Mailbox host | `/usr/local/libexec/cliproxy-billing-export` | Fixed container command, fixed mailbox, JSON on standard input |

The dedicated NVR SSH public key is restricted on the mailbox host to source `192.168.1.205`, no forwarding, no PTY, and that one forced command. NVR pins the independently verified mailbox-host public key in `~/.ssh/cliproxy-billing-known-hosts`. The key has no general remote-shell role. No client machine needs a tunnel or an SSH key for this feature.

The scheduler reads the current proxy account inventory on each run. Newly connected Claude accounts are included without editing a static email list. The exporter reports empty scans separately from source failures. It retains invoices already imported when no new mail is found.

## Operations

Check `systemctl --user status cliproxy-account-billing.timer` and `journalctl --user -u cliproxy-account-billing.service` on NVR. The journal contains counts and fixed error categories, never receipt bodies or keys. Trigger a collection with `systemctl --user start cliproxy-account-billing.service`.

If the source mailbox authorization expires, renew that one existing Gmail authorization on its host. Do not export its token to NVR. If a Claude credential becomes invalid, the account page shows the profile failure separately from billing history.

Rollback stops and disables only `cliproxy-account-billing.timer`, then restores the previous proxy binary and `static/management.html` from the deployment backup and restarts `cliproxy.service`. Keep `.account-invoices` and credential snapshots: the additive metadata does not alter inference credentials. Remove only the dedicated restricted authorized-key line if retiring collection.
