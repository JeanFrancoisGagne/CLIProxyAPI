package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// The usage endpoints and request headers match what the management dashboard
// (Cli-Proxy-API-Management-Center, src/utils/quota/constants.ts) sends through
// /api-call today. The proxy replaces $TOKEN$ with the credential's access token.
const (
	claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"
	codexUsageURL  = "https://chatgpt.com/backend-api/wham/usage"
)

func claudeUsageHeaders() map[string]string {
	return map[string]string{
		"User-Agent":     "claude-cli/2.1.280 (external, cli)",
		"Authorization":  "Bearer $TOKEN$",
		"Content-Type":   "application/json",
		"anthropic-beta": "oauth-2025-04-20",
	}
}

func codexUsageHeaders(chatgptAccountID string) map[string]string {
	h := map[string]string{
		"Authorization": "Bearer $TOKEN$",
		"Content-Type":  "application/json",
		"User-Agent":    "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)",
	}
	if chatgptAccountID != "" {
		h["Chatgpt-Account-Id"] = chatgptAccountID
	}
	return h
}

// flexNumber decodes a JSON number or a numeric string, like the dashboard's
// normalizeNumberValue. null, absent and anything else leave it invalid.
type flexNumber struct {
	Value float64
	Valid bool
}

func (n *flexNumber) UnmarshalJSON(b []byte) error {
	var raw any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil
	}
	switch v := raw.(type) {
	case float64:
		n.Value, n.Valid = v, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err == nil && !math.IsInf(f, 0) && !math.IsNaN(f) {
			n.Value, n.Valid = f, true
		}
	}
	return nil
}

// flexTime decodes a reset instant the way the dashboard's resolveResetMs does:
// an ISO-8601 string first, then a Unix timestamp in seconds or milliseconds.
type flexTime struct {
	Time time.Time
}

func (t *flexTime) UnmarshalJSON(b []byte) error {
	var raw any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil
	}
	var unix float64
	switch v := raw.(type) {
	case string:
		s := strings.TrimSpace(v)
		if parsed, err := time.Parse(time.RFC3339Nano, s); err == nil {
			t.Time = parsed
			return nil
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil
		}
		unix = f
	case float64:
		unix = v
	default:
		return nil
	}
	if unix <= 0 || math.IsInf(unix, 0) || math.IsNaN(unix) {
		return nil
	}
	if unix < 1e11 {
		t.Time = time.Unix(0, int64(unix*float64(time.Second)))
	} else {
		t.Time = time.UnixMilli(int64(unix))
	}
	return nil
}

type claudeWindowJSON struct {
	Utilization flexNumber `json:"utilization"`
	ResetsAt    flexTime   `json:"resets_at"`
}

func (w *claudeWindowJSON) window() *Window {
	if w == nil {
		return nil
	}
	return &Window{Used: w.Utilization.Value, HasUsed: w.Utilization.Valid, ResetAt: w.ResetsAt.Time}
}

type claudeLimitJSON struct {
	Kind     string     `json:"kind"`
	Percent  flexNumber `json:"percent"`
	ResetsAt flexTime   `json:"resets_at"`
	IsActive bool       `json:"is_active"`
	Scope    struct {
		Model struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

type claudeUsageJSON struct {
	FiveHour      *claudeWindowJSON `json:"five_hour"`
	SevenDay      *claudeWindowJSON `json:"seven_day"`
	IguanaNecktie *claudeWindowJSON `json:"iguana_necktie"` // the weekly Fable bucket, legacy field
	Limits        []json.RawMessage `json:"limits"`
}

// parseClaudeUsage reads GET https://api.anthropic.com/api/oauth/usage.
// The Fable bucket comes from the active "weekly_scoped" Fable entry in
// limits when one has a percent, else from iguana_necktie, as in the
// dashboard's buildClaudeQuotaWindows.
func parseClaudeUsage(body []byte) (*Usage, error) {
	var p claudeUsageJSON
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("claude usage is not JSON: %w", err)
	}
	u := &Usage{FiveHour: p.FiveHour.window(), SevenDay: p.SevenDay.window()}
	if fable := findFableLimit(p.Limits); fable != nil {
		u.Fable = &Window{Used: fable.Percent.Value, HasUsed: true, ResetAt: fable.ResetsAt.Time}
	} else {
		u.Fable = p.IguanaNecktie.window()
	}
	return u, nil
}

func findFableLimit(raw []json.RawMessage) *claudeLimitJSON {
	var first *claudeLimitJSON
	for _, item := range raw {
		var limit claudeLimitJSON
		if err := json.Unmarshal(item, &limit); err != nil {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(limit.Kind))
		model := strings.ToLower(strings.TrimSpace(limit.Scope.Model.DisplayName))
		if kind != "weekly_scoped" || (model != "fable" && model != "fable 5") || !limit.Percent.Valid {
			continue
		}
		if limit.IsActive {
			return &limit
		}
		if first == nil {
			first = &limit
		}
	}
	return first
}

const (
	codexFiveHourSeconds = 18000
	codexWeekSeconds     = 604800
	codexMinMonthSeconds = 28 * 24 * 60 * 60
	codexMaxMonthSeconds = 31 * 24 * 60 * 60
)

type codexWindowJSON struct {
	UsedPercent        flexNumber `json:"used_percent"`
	LimitWindowSeconds flexNumber `json:"limit_window_seconds"`
	ResetAfterSeconds  flexNumber `json:"reset_after_seconds"`
	ResetAt            flexTime   `json:"reset_at"`
}

type codexRateLimitJSON struct {
	Allowed         *bool            `json:"allowed"`
	LimitReached    bool             `json:"limit_reached"`
	PrimaryWindow   *codexWindowJSON `json:"primary_window"`
	SecondaryWindow *codexWindowJSON `json:"secondary_window"`
}

type codexUsageJSON struct {
	RateLimit *codexRateLimitJSON `json:"rate_limit"`
}

// parseCodexUsage reads GET https://chatgpt.com/backend-api/wham/usage.
// Windows are classified by limit_window_seconds (18000 = 5 hours, 604800 or
// 28 to 31 days = weekly), falling back to primary = 5h and secondary = weekly
// for payloads without lengths, as in the dashboard's buildCodexQuotaWindows.
// A Pro plan today sends only the weekly window, in primary_window. now turns
// reset_after_seconds into an instant.
func parseCodexUsage(body []byte, now time.Time) (*Usage, error) {
	var p codexUsageJSON
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("codex usage is not JSON: %w", err)
	}
	if p.RateLimit == nil {
		return &Usage{}, nil
	}
	rl := p.RateLimit
	five, week := classifyCodexWindows(rl.PrimaryWindow, rl.SecondaryWindow)
	limitReached := rl.LimitReached || (rl.Allowed != nil && !*rl.Allowed)
	return &Usage{
		FiveHour: codexWindow(five, limitReached, now),
		SevenDay: codexWindow(week, limitReached, now),
	}, nil
}

func classifyCodexWindows(primary, secondary *codexWindowJSON) (five, week *codexWindowJSON) {
	for _, w := range []*codexWindowJSON{primary, secondary} {
		if w == nil || !w.LimitWindowSeconds.Valid {
			continue
		}
		secs := w.LimitWindowSeconds.Value
		switch {
		case secs == codexFiveHourSeconds && five == nil:
			five = w
		case (secs == codexWeekSeconds || (secs >= codexMinMonthSeconds && secs <= codexMaxMonthSeconds)) && week == nil:
			week = w
		}
	}
	if five == nil && primary != nil && primary != week {
		five = primary
	}
	if week == nil && secondary != nil && secondary != five {
		week = secondary
	}
	return five, week
}

func codexWindow(w *codexWindowJSON, limitReached bool, now time.Time) *Window {
	if w == nil {
		return nil
	}
	out := &Window{Used: w.UsedPercent.Value, HasUsed: w.UsedPercent.Valid, ResetAt: w.ResetAt.Time}
	if out.ResetAt.IsZero() && w.ResetAfterSeconds.Valid && w.ResetAfterSeconds.Value > 0 {
		out.ResetAt = now.Add(time.Duration(w.ResetAfterSeconds.Value * float64(time.Second)))
	}
	// The dashboard shows a window without used_percent as 100% when the
	// limit is reached and a reset time is known.
	if !out.HasUsed && limitReached && !out.ResetAt.IsZero() {
		out.Used, out.HasUsed = 100, true
	}
	return out
}
