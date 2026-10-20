# T3 handoff: one-account quota refresh failure

The affected credential is `claude-773c1595-jf-research04@nera.software.json`, auth index `d544da88d2e03b9c`. It belongs to `jf-research04@nera.software`. The separate `jeff-research04@nera.software` account is unaffected by this credential repair.

## Confirmed sequence

All times below are UTC on October 20, 2026.

| Time | Confirmed observation |
| --- | --- |
| 17:54:44 | The affected credential refreshed successfully and its profile loaded. |
| 18:35 | Weekly usage reached 99%; the pacer reduced priority from 100 to 10. |
| 18:40 | Weekly usage reached 100%. |
| 18:47:06 | An Opus inference request received a rate-limit response. |
| 20:10:08 | Last successful quota poll: session 53%, weekly 100%, Fable 0% used. |
| 20:15–20:25 | Three scheduled polls received HTTP 401, “OAuth access token has been revoked.” |
| 20:30–21:25 | Twelve scheduled polls received HTTP 429. |
| 21:24:05 | A single pinned management usage read reproduced HTTP 429 with `Retry-After: 360`. |
| 21:27:33 | Only `reset-pacer.timer` was stopped to prevent further automatic polling. |
| 21:31:44 | After the six-minute quiet window, one usage read still received HTTP 429, now with `Retry-After: 3600`. |
| 21:32:34 | A separate profile read confirmed HTTP 401 and the revoked-token error. |
| During diagnosis | A controlled refresh of this exact credential failed with `invalid_grant`. Interactive renewal is required. |
| 21:37:21 | The revoked credential was set `disabled: true`; `reset-pacer.timer` resumed for healthy credentials. |
| 21:45:15 | User completed a fresh OAuth authorization; the exact credential saved renewed tokens. |
| 21:46:32 | Reviewed backend and dashboard deployed; proxy running under PID 1008747. |
| 21:47:49 | Renewed credential profile returned HTTP 200 and the expected email. |
| 21:48:19 | Original pinned quota repro returned HTTP 200 using the renewed credential. Weekly usage remained 100%. |
| 21:48:59 | Temporary isolation removed; credential enabled again. |
| After deployment | Two natural scheduled polls loaded this credential successfully; priority remained 10 because weekly usage was 100%. |

The logs establish early revocation between 20:10 and 20:15, before the stored expiry of October 21 at 01:54 UTC. They do not identify who or what revoked the tokens. Do not attribute the revocation to T3 or a proxy restart without additional evidence. The proxy process had remained running since October 16.

Weekly exhaustion, revoked authentication, and throttled quota reads are separate conditions. Renewing authentication does not reset subscription usage. Do not clear the account's inference cooldown to make a quota test pass.

## Retry defects and repairs

The installed reset-pacer requests `/api/oauth/usage` every 300 seconds and discards upstream response headers. That violated the observed 360-second wait. Its installed source is commit `29f30c248390efd2eed526cdeec9cd67e72cba40` in the CLIProxy repository; `cmd/reset-pacer` is absent from the account-management branch.

Both legacy `/v0/management/api-call` and `/v8/management/requests/api-call` use the same handler. The repair belongs in that shared handler, covering the existing scheduler and every dashboard client without replacing the scheduler or creating machine-specific tunnels.

- Backend GET/HEAD coordination retains upstream 429 responses until `Retry-After` expires, per credential and request resource. Other accounts and writes remain independent.
- Concurrent callers cannot defeat the wait through different User-Agent values. Successful responses are not cached or shared between callers.
- Delta seconds and HTTP dates use a local monotonic deadline. Provider `Date` handles clock skew. Waiting and the HTTP request share the existing 60-second management request budget.
- Storage is bounded. Active embargoes are not evicted merely to admit another resource.
- The dashboard shares per-account backoff between ordinary usage and reset-grant reads. Failed quota loads no longer trigger a second reset-grant usage request.
- Successful token renewal changes the public `last_refresh` marker. Inventory projection uses the newest valid persisted/runtime timestamp because OAuth watcher reloads can retain an older runtime schedule.
- The dashboard observes that marker, rejects old-token replies, and clears only the renewed credential's state. Priority-only updates do not clear a provider wait.

## Verification

Backend source: `E:/wt/cliproxy-quota-backoff`, branch `codex/quota-backoff`, based on `b2033788`.

Dashboard source: `E:/wt/cpamc-quota-backoff`, branch `codex/claude-quota-backoff`, based on `7209f8d`.

| Proof | Result |
| --- | --- |
| Baseline backend public handler replay | RED: two upstream calls during one embargo; independently reproduced by root. |
| Baseline dashboard provider replay | RED: repeated usage request ignored `Retry-After`; independently reproduced by root. |
| Baseline reset-grant hook replay | RED: failed quota triggered a second usage-family request; independently reproduced by root. |
| Backend final management suite | Passed with the race detector on Desktop C, Go 1.27.1. |
| Backend static Linux amd64 build | Passed with `CGO_ENABLED=0`. |
| Dashboard focused suite | 90 tests passed using pinned Bun 1.3.14. |
| Dashboard lint, TypeScript, single-file build | Passed. |
| Dashboard full suite | Final patch: 1,552 passed and one existing i18n-order failure. Untouched baseline: 1,537 passed and the same failure. |
| Independent review | Three findings repaired: clock skew, stale credential cache, and OAuth inventory refresh marker. Final source review and cold focused tests passed. |

AutoReview's first dashboard pass completed without findings. Its final expanded dashboard bundle and the backend bundle were refused by the source guard before review. The guard remains enabled. The user's existing incident approval permits independent full-source review instead; that review covered the changed modules and their persistence, watcher, service, inventory and quota callers.

Working evidence is under `E:/scratch/cliproxy-quota-20261020/`. It contains sanitized repros, proof commands, test logs and the decision log. Never add live credentials, OAuth callback codes, management keys or raw auth files to this document or Git.

## Deployment and recovery

The shared endpoint remains `http://100.113.156.52:8317`. Keep the native Codex recovery harness on its direct account.

The independent control path was verified through LAN SSH to NVR at `192.168.1.205`, from `192.168.1.85`. A private backup of the installed binary, dashboard and configuration exists on NVR at `/home/jf/.local/state/cliproxy-quota-backoff/20261020T214245Z`. It contains no credential snapshots.

Rollback over the existing LAN SSH route:

```sh
install -m 700 /home/jf/.local/state/cliproxy-quota-backoff/20261020T214245Z/binary /home/jf/.local/bin/cli-proxy-api.rollback
mv /home/jf/.local/bin/cli-proxy-api.rollback /home/jf/.local/bin/cli-proxy-api
install -m 600 /home/jf/.local/state/cliproxy-quota-backoff/20261020T214245Z/ui /home/jf/.cli-proxy-api/static/management.html.rollback
mv /home/jf/.cli-proxy-api/static/management.html.rollback /home/jf/.cli-proxy-api/static/management.html
systemctl --user restart cliproxy.service
systemctl --user is-active cliproxy.service
```

Do not restore old credential files during rollback. OAuth refresh tokens can rotate independently of the deployed binary.

## Final deployed state

The account's authentication and quota refresh are repaired. The deployed dashboard was checked in Chrome: the affected row loads successfully, shows 47% session allowance remaining and 0% overall weekly allowance remaining, and identifies the weekly reset as October 25 at 09:00 America/Toronto. This is a real subscription limit, not a failed quota load. Extra usage was disabled in the successful provider response; no plan or spending setting was changed.

| Artifact or check | Verified value |
| --- | --- |
| Backend code commit | `914e9bca`, branch `codex/quota-backoff` |
| Dashboard code commit | `f5817ac`, branch `codex/claude-quota-backoff` |
| Deployed backend SHA-256 | `607aed062225bd171eb159a80062767e1d5c2501b583ae95f1ac4f8f85c5817e` |
| Deployed dashboard SHA-256 | `e6aba2f2e467ab2f5b01d7861032b78dd2205c5e40f46c163ecbef74b1e0a6f0` |
| Proxy configuration SHA-256, unchanged | `5703a1e074fd9d0fa3693c468268c6e85a409b67107879986d45263f1e2eb866` |
| Native Codex configuration SHA-256, unchanged during this repair | `718570fffd574aa24dfe5bbe4ed0e76857b656598c8afd4be67af3fa9ed6e7bd` |
| Native Codex provider | `openai`, direct account; does not point at the shared proxy |
| Shared inference smoke | HTTP 200 from `claude-haiku-5-5`, exact reply `PROXY_OK` |
| Deployed coordination fixture | v8 read429, v0 cached429, independent credential200, write200; exactly three fixture upstream calls |
| Screenshot | `E:/scratch/cliproxy-quota-20261020/repaired-quota.png` |

The fixture ran against the actual deployed management handler and a temporary loopback HTTP server. No credential headers were sent to it and it made no provider calls. The server was stopped after verification.

The proxy intentionally listens at `100.113.156.52:8317`, not loopback port 8317. Verify readiness at the shared address or existing loopback relay `127.0.0.1:8318`. A loopback8317 refusal does not prove the shared service is down.

Do not restart the proxy during a pending OAuth flow: its PKCE session is process-local. The completed renewal was saved before this deployment. Do not rotate working credentials merely to clear a provider wait; this renewal was required because both old tokens were rejected.

The private temporary management-key file retained at the user's request remains unchanged. Cleanup is outside this repair. No machine-specific tunnel, new client route, recurring automation, subscription change or paid service was introduced.
