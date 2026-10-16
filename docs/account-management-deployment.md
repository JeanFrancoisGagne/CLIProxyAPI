# Account management deployment record

Control path: Windows desktop to NVR over LAN SSH at `192.168.1.205`. The acting Codex harness uses its direct account. Neither route depends on the CLIProxy process being replaced. LAN SSH to NVR and to the mailbox host at `192.168.1.184` succeeded before deployment preparation.

Predeployment backup: `/home/jf/.local/state/cliproxy-account-management/20261016T160234Z` on NVR. This private directory holds the installed binary, dashboard, configuration, manifest hashes and 13 credential JSON snapshots. It contains credentials; do not copy it into a repository or a support report.

The existing configuration already has `disable-auto-update-panel: true`. Keep the custom dashboard on this reviewed deployment path; an upstream dashboard download would remove the new page. No proxy configuration change is needed for this deployment.

## Revert card

Run through the existing LAN SSH route on NVR:

```sh
systemctl --user disable --now cliproxy-account-billing.timer
systemctl --user stop cliproxy-account-billing.service
install -m 700 /home/jf/.local/state/cliproxy-account-management/20261016T160234Z/binary /home/jf/.local/bin/cli-proxy-api.rollback
mv /home/jf/.local/bin/cli-proxy-api.rollback /home/jf/.local/bin/cli-proxy-api
install -m 600 /home/jf/.local/state/cliproxy-account-management/20261016T160234Z/ui /home/jf/.cli-proxy-api/static/management.html.rollback
mv /home/jf/.cli-proxy-api/static/management.html.rollback /home/jf/.cli-proxy-api/static/management.html
systemctl --user restart cliproxy.service
systemctl --user is-active cliproxy.service
```

Do not restore the credential snapshots during a normal rollback: active refresh tokens may have rotated since the backup. The old binary tolerates the added account metadata. Restoring old tokens can invalidate healthy credentials.

The rollback does not stop the existing management relay or alter client routing. Retire the new restricted mailbox-host SSH key separately only if collection is being removed permanently.

## Verification, October 16, 2026

The new API answered after the single service restart in 1.3 seconds. The deployed static Linux amd64 binary has SHA-256 `aec801d3b8c061022c41dcfbe7ed28c508fdeb6c9b41063668950e4246b53e8d`. The dashboard has SHA-256 `778c450bde3617e7526e2d96c9e9c961422c8acfeb4edb1ddb568ce8b399affe`.

| Check | Result |
| --- | --- |
| Backend | Seven affected Go package suites passed; auth metadata and profile backfill race tests passed |
| Backend build | Go 1.27.1, `CGO_ENABLED=0`, static Linux amd64 |
| Receipt tools | 21 tests passed, including fixed SSH configuration and retained strict host verification |
| UI | 24 focused tests passed; lint and build passed; full run had 1,537 passing tests and one unchanged baseline failure |
| Reviews | Fresh independent backend source review and repair review passed; installed UI autoreview passed |
| Account API | 13 total accounts, including 12 Claude accounts; 11 Claude profiles available and one existing disabled credential unavailable |
| Collector | 12 successful scans, 15 invoices across eight accounts, four empty scans, zero source or POST errors |
| Persistence | All 15 invoices found in durable credential metadata; all 15 PDF downloads matched their recorded SHA-256 |
| Permissions | Invoice directory 0700; PDFs, collector files and dedicated SSH private key 0600 |
| Repeat run | Completed successfully; invoice contents and counts unchanged |
| Browser | Live account page, research05 billing record, calendar dates and authenticated PDF response verified |
| Inference | Shared proxy returned HTTP 200 with the requested Haiku reply after deployment |
| Configuration | Existing proxy configuration and native Codex configuration unchanged |

The timer is enabled and persistent. It runs at 00:00, 06:00, 12:00 and 18:00 in the host timezone, with up to five minutes of jitter. User lingering is enabled so it runs without an interactive login.

The first service run exposed an SSH configuration interaction with systemd's filesystem namespace. OpenSSH rejected a system configuration file's ownership and the collector correctly recorded source errors without disrupting inference. The fixed driver starts SSH with `-F none` and supplies its target, identity and pinned host trust explicitly. The same sandbox probe and two full collection runs passed afterward. The service sandbox and strict host verification remain enabled.

Backend review caught and fixed stale profile writes after same-filename account replacement, global manager locking around external store I/O, and failed-save rollback during concurrent generation changes. Regression tests cover each case. The installed review helper rejected existing Go credential-variable references before launching; the user-approved independent full-source review was used with that guard unchanged.

The UI baseline failure is the existing provider-model-option test after another suite initializes translations. It also failed in an untouched checkout. The new account tests, lint and build passed. Chrome's download-event observer timed out, so browser proof uses the actual button request: HTTP 200, `application/pdf`, 34,604 bytes, and the expected invoice hash. All PDFs were also verified through the authenticated API.
