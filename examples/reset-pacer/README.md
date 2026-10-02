# reset-pacer

reset-pacer makes CLIProxyAPI spend the Claude or Codex account whose weekly
limit resets soonest first. Weekly quota left unused at the reset is lost, so
the account closest to its reset should take the next new session. An account
whose 5-hour window is nearly full sits out until it recovers.

It runs beside the proxy and changes nothing in the proxy itself. It only
rewrites each credential's `priority` field through the management API.

## What one pass does

1. `GET /v0/management/auth-files` lists the credentials.
2. For every enabled Claude and Codex OAuth credential, `POST /v0/management/api-call`
   asks the proxy to fetch that account's usage with the account's own token:
   - Claude: `https://api.anthropic.com/api/oauth/usage` (windows `five_hour`,
     `seven_day`, and the weekly Fable bucket)
   - Codex: `https://chatgpt.com/backend-api/wham/usage`
3. Per provider, an account is eligible when its usage came back, its 5-hour
   window is under 95%, and its weekly window is under 99%. A Claude account
   with no weekly window is also out when its Fable bucket is at 99% or more.
4. Eligible accounts are ordered by weekly reset time, soonest first (ties:
   lower weekly use, then name) and get priorities 100, 90, 80, and so on,
   never below 20. An account with no weekly reset time goes last. Every
   other account gets 10, so the proxy can still fall back to it.
5. `PATCH /v0/management/auth-files/fields` with `{"name": ..., "priority": N}`
   runs only for credentials whose priority changes. reset-pacer never
   disables a credential.

The proxy sends each new session to the highest priority tier that has an
available credential. A session already bound to an account stays on it,
which keeps its prompt cache. A credential in cooldown after a 429 is skipped
by the proxy until it recovers.

reset-pacer owns the `priority` field of every enabled Claude and Codex OAuth
credential. A priority set by hand in the dashboard is overwritten within 5
minutes. Other providers and disabled credentials are left alone.

## Install on desktop-c

From Windows, after building (`GOOS=linux GOARCH=amd64 go build -o E:\agents\out\reset-pacer ./cmd/reset-pacer/`):

```sh
ssh jf@100.95.181.77 'mkdir -p ~/.local/bin ~/.config/systemd/user'
scp E:/agents/out/reset-pacer jf@100.95.181.77:.local/bin/reset-pacer
scp examples/reset-pacer/reset-pacer.service examples/reset-pacer/reset-pacer.timer jf@100.95.181.77:.config/systemd/user/
```

On the box, check the plan first. `-dry-run` prints the ranking and writes nothing:

```sh
chmod +x ~/.local/bin/reset-pacer
set -a; . ~/.cli-proxy-api/secrets.env; set +a
CPA_BASE=http://100.95.181.77:8317 ~/.local/bin/reset-pacer -once -dry-run -verbose
```

Then start the timer (every 5 minutes, on the clock):

```sh
systemctl --user daemon-reload
systemctl --user enable --now reset-pacer.timer
loginctl enable-linger jf    # user timers keep running with nobody logged in
systemctl --user list-timers reset-pacer.timer
```

Settings:

| Variable / flag | Meaning |
|---|---|
| `CPA_MGMT_KEY` | management key; `CLIPROXY_MGMT_KEY` is read when it is unset |
| `CPA_BASE`, `-base` | proxy URL, default `http://127.0.0.1:8317` |
| `-once` | one pass, then exit (the timer uses this) |
| `-interval 5m` | time between passes when `-once` is not set |
| `-dry-run` | print the plan, write nothing |
| `-verbose` | also print each usage call and its result |

`secrets.env` must be plain `KEY=value` lines for systemd to read it. If it
also sets `CPA_BASE`, that value wins over the one in the unit.

The proxy treats a request from any address other than 127.0.0.1 or ::1 as
remote, so calling it at `100.95.181.77` needs `remote-management.allow-remote: true`
in its config. If the dashboard already works from another machine, it is on.

## Reading the output

Each pass prints a summary line, then one row per credential (the journal
keeps them: `journalctl --user -u reset-pacer -n 30`):

```
2026-10-02T14:05:00Z reset-pacer: 4 credentials, 2 patched, 0 failed
PROVIDER  ACCOUNT            5H   7D   FABLE  7D RESET (UTC)                PRIORITY   REASON
claude    bo***@example.com  10%  60%  -      2026-10-03 14:03 (in 23h59m)  10 -> 100  rank 1 of 2 by 7d reset
claude    al***@example.com  10%  20%  -      2026-10-05 14:03 (in 2d23h)   90 -> 90   rank 2 of 2 by 7d reset
claude    of***@example.com  -    -    -      -                             50 -> 50   disabled, left alone
codex     xa***@example.com  -    -    -      -                             0 -> 10    usage unknown: upstream 429: ...
```

- `ACCOUNT` is the email with most of the local part hidden.
- `5H`, `7D`, `FABLE` are percent used. `-` means the payload had no such
  window (a ChatGPT Pro account reports no 5-hour window today); `?` means the
  window came back without a number.
- `PRIORITY` is old -> new. A row whose two numbers match was not written.
- `REASON` gives the rank, or why the account sits out: `5h at 97%`,
  `7d at 99%`, `no 7d window and fable at 99%`, `usage unknown: ...`. A
  failed write shows `PATCH failed: ...` and makes the run exit 1.

Exit codes: 0 done (a failed usage call only lowers that account to 10), 1 the
credential list or a write failed, 2 bad flags, no key, or the proxy refused
the key. On a refused key the pass stops at the first
request: the proxy bans a client address for 30 minutes after 5 bad keys.

## Stop

```sh
systemctl --user disable --now reset-pacer.timer
```

Priorities stay where the last pass left them. Reset them in the dashboard
if you want the proxy's plain fill-first order back.
