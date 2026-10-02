// Command reset-pacer steers CLIProxyAPI toward the Claude or Codex account
// whose weekly limit resets soonest, so weekly quota that would be lost at the
// reset gets used first. It never touches the proxy core: every pass it reads
// each enabled Claude and Codex credential's usage through the management API
// (POST /v0/management/api-call), ranks them, and rewrites only the
// credential "priority" field (PATCH /v0/management/auth-files/fields).
//
// The proxy serves each new session from the highest priority tier with an
// available credential. Sessions already bound to a credential stay there.
//
// Usage:
//
//	CPA_MGMT_KEY=... reset-pacer -once [-dry-run] [-verbose]
//	CPA_MGMT_KEY=... reset-pacer -interval 5m
//
// Environment:
//
//	CPA_BASE            proxy base URL (default http://127.0.0.1:8317); -base overrides
//	CPA_MGMT_KEY        management key; CLIPROXY_MGMT_KEY is read when CPA_MGMT_KEY is unset
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

const defaultBase = "http://127.0.0.1:8317"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reset-pacer", flag.ContinueOnError)
	fs.SetOutput(stderr)
	base := fs.String("base", envOr("CPA_BASE", defaultBase), "proxy base URL (env CPA_BASE)")
	once := fs.Bool("once", false, "run one pass and exit")
	interval := fs.Duration("interval", 5*time.Minute, "time between passes when -once is not set")
	dryRun := fs.Bool("dry-run", false, "print the plan without writing any priority")
	verbose := fs.Bool("verbose", false, "also print each usage call")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	key := firstEnv("CPA_MGMT_KEY", "CLIPROXY_MGMT_KEY")
	if key == "" {
		fmt.Fprintln(stderr, "reset-pacer: set CPA_MGMT_KEY (or CLIPROXY_MGMT_KEY) to the proxy's management key")
		return 2
	}
	if *interval <= 0 {
		fmt.Fprintln(stderr, "reset-pacer: -interval must be positive")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// 20 s per usage call keeps one hung upstream from eating the systemd
	// TimeoutStartSec budget for every credential behind it.
	p := &pacer{client: newMgmtClient(*base, key), out: stdout, dryRun: *dryRun, verbose: *verbose, now: time.Now, usageTimeout: 20 * time.Second}
	for {
		err := p.pass(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "reset-pacer: %v\n", err)
		}
		if errors.Is(err, errManagementAuth) {
			return 2
		}
		if *once {
			if err != nil {
				return 1
			}
			return 0
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(*interval):
		}
	}
}

type pacer struct {
	client       *mgmtClient
	out          io.Writer
	dryRun       bool
	verbose      bool
	now          func() time.Time
	usageTimeout time.Duration // deadline for one usage call; 0 means none
}

// line is one credential's row in the pass report.
type line struct {
	file     authFile
	usage    *Usage
	decision Decision
	note     string
}

// pass runs one list, fetch, rank, patch cycle and prints the report.
func (p *pacer) pass(ctx context.Context) error {
	now := p.now()
	files, err := p.client.listAuthFiles(ctx)
	if err != nil {
		return err
	}

	var ranked, untouched []authFile
	var creds []Credential
	for _, f := range files {
		if pr := f.provider(); pr != "claude" && pr != "codex" {
			continue
		}
		if f.Disabled || f.AccountType == "api_key" {
			untouched = append(untouched, f)
			continue
		}
		usage, err := p.fetchUsage(ctx, f, now)
		if errors.Is(err, errManagementAuth) {
			return err
		}
		c := Credential{Name: f.Name, Provider: f.provider(), Priority: f.Priority, Usage: usage}
		if err != nil {
			c.UsageError = err.Error()
		}
		if p.verbose {
			status := "ok"
			if err != nil {
				status = err.Error()
			}
			fmt.Fprintf(p.out, "usage %s %s (auth_index %s): %s\n", c.Provider, maskAccount(f.Email, f.Name), f.AuthIndex, status)
		}
		ranked = append(ranked, f)
		creds = append(creds, c)
	}

	decisions := Rank(creds)
	lines := make([]line, 0, len(files))
	var patched, failed int
	for i, d := range decisions {
		l := line{file: ranked[i], usage: creds[i].Usage, decision: d}
		switch {
		case !d.Changed():
		case p.dryRun:
			l.note = "dry-run, not written"
		default:
			if err := p.client.setPriority(ctx, d.Name, d.New); err != nil {
				if errors.Is(err, errManagementAuth) {
					return err
				}
				failed++
				l.note = "PATCH failed: " + err.Error()
			} else {
				patched++
			}
		}
		lines = append(lines, l)
	}
	for _, f := range untouched {
		reason := "disabled, left alone"
		if !f.Disabled {
			reason = "API key credential, left alone"
		}
		lines = append(lines, line{
			file:     f,
			decision: Decision{Name: f.Name, Provider: f.provider(), Old: f.Priority, New: f.Priority, Reason: reason},
		})
	}

	mode := ""
	if p.dryRun {
		mode = " (dry-run)"
	}
	fmt.Fprintf(p.out, "%s reset-pacer%s: %d credentials, %d patched, %d failed\n",
		now.UTC().Format(time.RFC3339), mode, len(lines), patched, failed)
	p.report(now, lines)
	if failed > 0 {
		return fmt.Errorf("%d priority update(s) failed", failed)
	}
	return nil
}

func (p *pacer) fetchUsage(ctx context.Context, f authFile, now time.Time) (*Usage, error) {
	if f.AuthIndex == "" {
		return nil, errors.New("credential has no auth_index")
	}
	if p.usageTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.usageTimeout)
		defer cancel()
	}
	if f.provider() == "claude" {
		body, err := p.client.upstreamGet(ctx, f.AuthIndex, claudeUsageURL, claudeUsageHeaders())
		if err != nil {
			return nil, err
		}
		return parseClaudeUsage(body)
	}
	body, err := p.client.upstreamGet(ctx, f.AuthIndex, codexUsageURL, codexUsageHeaders(f.chatgptAccountID()))
	if err != nil {
		return nil, err
	}
	return parseCodexUsage(body, now)
}

// report prints one row per credential: provider, masked account, usage,
// weekly reset, old -> new priority, and the reason.
func (p *pacer) report(now time.Time, lines []line) {
	sort.SliceStable(lines, func(i, j int) bool {
		a, b := lines[i].decision, lines[j].decision
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.New != b.New {
			return a.New > b.New
		}
		return a.Name < b.Name
	})
	tw := tabwriter.NewWriter(p.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PROVIDER\tACCOUNT\t5H\t7D\tFABLE\t7D RESET (UTC)\tPRIORITY\tREASON")
	for _, l := range lines {
		var five, seven, fable *Window
		if l.usage != nil {
			five, seven, fable = l.usage.FiveHour, l.usage.SevenDay, l.usage.Fable
		}
		reason := l.decision.Reason
		if l.note != "" {
			reason += "; " + l.note
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d -> %d\t%s\n",
			l.decision.Provider, maskAccount(l.file.Email, l.file.Name),
			percent(five), percent(seven), percent(fable),
			resetText(seven, now), l.decision.Old, l.decision.New, reason)
	}
	tw.Flush()
}

func percent(w *Window) string {
	switch {
	case w == nil:
		return "-"
	case !w.HasUsed:
		return "?"
	default:
		return fmt.Sprintf("%.0f%%", w.Used)
	}
}

func resetText(w *Window, now time.Time) string {
	if w == nil || w.ResetAt.IsZero() {
		return "-"
	}
	at := w.ResetAt.UTC().Format("2006-01-02 15:04")
	d := w.ResetAt.Sub(now)
	if d <= 0 {
		return at + " (passed)"
	}
	days, hours, mins := int(d/(24*time.Hour)), int(d/time.Hour)%24, int(d/time.Minute)%60
	if days > 0 {
		return fmt.Sprintf("%s (in %dd%dh)", at, days, hours)
	}
	return fmt.Sprintf("%s (in %dh%02dm)", at, hours, mins)
}

// maskAccount shows an account without its full email. Auth file names often
// embed the email, so a name with "@" is masked the same way.
func maskAccount(email, name string) string {
	if email = strings.TrimSpace(email); email != "" {
		return maskEmail(email)
	}
	if strings.Contains(name, "@") {
		return maskEmail(name)
	}
	return name
}

func maskEmail(s string) string {
	at := strings.LastIndex(s, "@")
	if at < 0 {
		return s
	}
	// Keep the last two characters too: accounts like jf-research04 and
	// jf-research05 differ only at the end.
	local := []rune(s[:at])
	switch {
	case len(local) > 4:
		return string(local[:2]) + "***" + string(local[len(local)-2:]) + "@" + s[at+1:]
	case len(local) > 2:
		return string(local[:2]) + "***@" + s[at+1:]
	default:
		return string(local[:min(1, len(local))]) + "***@" + s[at+1:]
	}
}

func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return ""
}
