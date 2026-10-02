package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func win(used float64, reset time.Time) *Window {
	return &Window{Used: used, HasUsed: true, ResetAt: reset}
}

func claude(name string, five, seven *Window) Credential {
	return Credential{Name: name, Provider: "claude", Usage: &Usage{FiveHour: five, SevenDay: seven}}
}

func days(n float64) time.Time { return t0.Add(time.Duration(n * 24 * float64(time.Hour))) }

func TestRank(t *testing.T) {
	tests := []struct {
		name  string
		creds []Credential
		want  map[string]int
		// reasons maps a credential to a substring its reason must contain.
		reasons map[string]string
	}{
		{
			name: "soonest weekly reset wins",
			creds: []Credential{
				claude("a", win(10, days(0.1)), win(10, days(3))),
				claude("b", win(10, days(0.1)), win(60, days(1))),
			},
			want:    map[string]int{"b": 100, "a": 90},
			reasons: map[string]string{"b": "rank 1 of 2", "a": "rank 2 of 2"},
		},
		{
			name: "soonest reset with 5h at 97% falls behind",
			creds: []Credential{
				claude("a", win(10, days(0.1)), win(10, days(3))),
				claude("b", win(97, days(0.1)), win(60, days(1))),
			},
			want:    map[string]int{"a": 100, "b": 10},
			reasons: map[string]string{"b": "5h at 97%"},
		},
		{
			name: "5h at exactly 95% sits out",
			creds: []Credential{
				claude("a", win(95, days(0.1)), win(10, days(1))),
			},
			want: map[string]int{"a": 10},
		},
		{
			name: "7d at 99% sits out",
			creds: []Credential{
				claude("a", win(0, days(0.1)), win(99, days(1))),
				claude("b", win(0, days(0.1)), win(98.9, days(2))),
			},
			want:    map[string]int{"a": 10, "b": 100},
			reasons: map[string]string{"a": "7d at 99%"},
		},
		{
			name: "all exhausted gives all 10",
			creds: []Credential{
				claude("a", win(10, days(0.1)), win(99, days(1))),
				claude("b", win(96, days(0.1)), win(20, days(2))),
				claude("c", win(100, days(0.1)), win(100, days(3))),
			},
			want: map[string]int{"a": 10, "b": 10, "c": 10},
		},
		{
			name: "missing reset time sorts last among eligible",
			creds: []Credential{
				claude("a", win(0, time.Time{}), win(0, time.Time{})),
				claude("b", win(5, days(0.1)), win(50, days(5))),
				claude("c", win(5, days(0.1)), win(70, days(2))),
			},
			want:    map[string]int{"c": 100, "b": 90, "a": 80},
			reasons: map[string]string{"a": "no reset time so last"},
		},
		{
			name: "failed usage fetch stays at 10 and the others still rank",
			creds: []Credential{
				{Name: "a", Provider: "claude", Priority: 100, UsageError: "upstream 401: token expired"},
				claude("b", win(5, days(0.1)), win(50, days(1))),
				claude("c", win(5, days(0.1)), win(50, days(2))),
			},
			want:    map[string]int{"a": 10, "b": 100, "c": 90},
			reasons: map[string]string{"a": "usage unknown: upstream 401"},
		},
		{
			name: "equal resets break on lower 7d use, then name",
			creds: []Credential{
				claude("a", nil, win(50, days(1))),
				claude("c", nil, win(20, days(1))),
				claude("b", nil, win(20, days(1))),
			},
			want: map[string]int{"b": 100, "c": 90, "a": 80},
		},
		{
			name: "providers rank independently",
			creds: []Credential{
				claude("a", nil, win(10, days(4))),
				{Name: "x", Provider: "codex", Usage: &Usage{SevenDay: win(10, days(6))}},
				{Name: "y", Provider: "codex", Usage: &Usage{SevenDay: win(10, days(1))}},
			},
			want: map[string]int{"a": 100, "y": 100, "x": 90},
		},
		{
			name: "fable at 99% sits out only when 7d is missing",
			creds: []Credential{
				{Name: "no7d", Provider: "claude", Usage: &Usage{FiveHour: win(1, days(0.1)), Fable: win(99, days(1))}},
				{Name: "has7d", Provider: "claude", Usage: &Usage{FiveHour: win(1, days(0.1)), SevenDay: win(40, days(2)), Fable: win(99, days(1))}},
			},
			want:    map[string]int{"no7d": 10, "has7d": 100},
			reasons: map[string]string{"no7d": "no 7d window and fable at 99%"},
		},
		{
			name: "usage with no windows counts as unknown",
			creds: []Credential{
				{Name: "a", Provider: "codex", Usage: &Usage{}},
			},
			want:    map[string]int{"a": 10},
			reasons: map[string]string{"a": "no windows"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Rank(tc.creds)
			if len(got) != len(tc.creds) {
				t.Fatalf("got %d decisions, want %d", len(got), len(tc.creds))
			}
			for i, d := range got {
				if d.Name != tc.creds[i].Name {
					t.Fatalf("decision %d is %q, want %q (order must follow input)", i, d.Name, tc.creds[i].Name)
				}
				if want := tc.want[d.Name]; d.New != want {
					t.Errorf("%s: priority %d, want %d (reason %q)", d.Name, d.New, want, d.Reason)
				}
				if d.Old != tc.creds[i].Priority {
					t.Errorf("%s: old %d, want %d", d.Name, d.Old, tc.creds[i].Priority)
				}
				if sub, ok := tc.reasons[d.Name]; ok && !strings.Contains(d.Reason, sub) {
					t.Errorf("%s: reason %q lacks %q", d.Name, d.Reason, sub)
				}
			}
		})
	}
}

func TestRankFloorsAtTwenty(t *testing.T) {
	var creds []Credential
	for i := range 10 {
		creds = append(creds, claude(fmt.Sprintf("c%02d", i), nil, win(0, days(float64(i+1)))))
	}
	got := Rank(creds)
	want := []int{100, 90, 80, 70, 60, 50, 40, 30, 20, 20}
	for i, d := range got {
		if d.New != want[i] {
			t.Errorf("%s: priority %d, want %d", d.Name, d.New, want[i])
		}
	}
}

func TestDecisionChanged(t *testing.T) {
	got := Rank([]Credential{
		{Name: "same", Provider: "claude", Priority: 100, Usage: &Usage{SevenDay: win(1, days(1))}},
		{Name: "unset", Provider: "codex", Usage: &Usage{SevenDay: win(1, days(1))}},
	})
	if got[0].Changed() {
		t.Errorf("same: 100 -> %d reported as a change", got[0].New)
	}
	if !got[1].Changed() {
		t.Errorf("unset: 0 -> %d not reported as a change", got[1].New)
	}
}

// wham/usage as a ChatGPT Pro account returns it today: one weekly window in
// primary_window, no 5-hour window, plus fields the pacer ignores. Shape taken
// from the dashboard's tests/codexQuota.test.ts.
const whamProPayload = `{
  "plan_type": "pro",
  "rate_limit": {
    "allowed": true,
    "limit_reached": false,
    "primary_window": {"used_percent": %d, "limit_window_seconds": 604800, "reset_after_seconds": 601888, "reset_at": %d},
    "secondary_window": null
  },
  "code_review_rate_limit": null,
  "additional_rate_limits": [
    {"limit_name": "GPT-5.3-Codex-Spark", "metered_feature": "codex_bengalfox",
     "rate_limit": {"allowed": true, "limit_reached": false,
       "primary_window": {"used_percent": 0, "limit_window_seconds": 604800, "reset_after_seconds": 602111, "reset_at": 1785903197},
       "secondary_window": null}}
  ],
  "rate_limit_reset_credits": {"available_count": 1, "applicable_available_count": 0}
}`

// The older wham shape: a 5-hour primary window and a weekly secondary window.
const whamTwoWindowPayload = `{
  "rate_limit": {
    "allowed": true,
    "limit_reached": false,
    "primary_window": {"used_percent": %d, "limit_window_seconds": 18000, "reset_after_seconds": 3600, "reset_at": %d},
    "secondary_window": {"used_percent": 30, "limit_window_seconds": 604800, "reset_after_seconds": 90000, "reset_at": %d}
  }
}`

func TestRankCodexFromWhamUsage(t *testing.T) {
	now := time.Unix(1785300000, 0)
	later := int64(1785902974)
	sooner := later - 86400
	parse := func(body string) *Usage {
		t.Helper()
		u, err := parseCodexUsage([]byte(body), now)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		return u
	}
	late := parse(fmt.Sprintf(whamProPayload, 1, later))
	early := parse(fmt.Sprintf(whamProPayload, 40, sooner))
	busy := parse(fmt.Sprintf(whamTwoWindowPayload, 97, sooner-3600, sooner-7200))

	if late.FiveHour != nil {
		t.Errorf("pro payload: 5h window %+v, want none", late.FiveHour)
	}
	if w := late.SevenDay; w == nil || !w.HasUsed || w.Used != 1 || !w.ResetAt.Equal(time.Unix(later, 0)) {
		t.Fatalf("pro payload: weekly window %+v, want 1%% resetting at %d", w, later)
	}
	if w := busy.FiveHour; w == nil || w.Used != 97 {
		t.Fatalf("two-window payload: 5h window %+v, want 97%%", w)
	}

	got := Rank([]Credential{
		{Name: "late", Provider: "codex", Usage: late},
		{Name: "early", Provider: "codex", Usage: early},
		{Name: "busy", Provider: "codex", Usage: busy},
	})
	want := map[string]int{"early": 100, "late": 90, "busy": 10}
	for _, d := range got {
		if d.New != want[d.Name] {
			t.Errorf("%s: priority %d, want %d (reason %q)", d.Name, d.New, want[d.Name], d.Reason)
		}
	}
}
