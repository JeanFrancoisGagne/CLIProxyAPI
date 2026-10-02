package main

import (
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// sameWindow compares a parsed window with the expected one; nil means absent.
func sameWindow(t *testing.T, label string, got, want *Window) {
	t.Helper()
	if (got == nil) != (want == nil) {
		t.Errorf("%s: got %+v, want %+v", label, got, want)
		return
	}
	if got == nil {
		return
	}
	if got.HasUsed != want.HasUsed || got.Used != want.Used || !got.ResetAt.Equal(want.ResetAt) {
		t.Errorf("%s: got %+v, want %+v", label, *got, *want)
	}
}

func TestParseClaudeUsage(t *testing.T) {
	fiveReset := mustTime(t, "2026-10-02T17:00:00.123456+00:00")
	weekReset := mustTime(t, "2026-10-04T10:00:00+00:00")
	modernReset := mustTime(t, "2026-07-27T10:00:00.000000+00:00")
	legacyReset := mustTime(t, "2026-07-28T10:00:00.000000+00:00")

	tests := []struct {
		name               string
		body               string
		five, seven, fable *Window
	}{
		{
			name: "standard windows with the legacy fable field",
			body: `{"five_hour":{"utilization":12.0,"resets_at":"2026-10-02T17:00:00.123456+00:00"},
			        "seven_day":{"utilization":40,"resets_at":"2026-10-04T10:00:00+00:00"},
			        "seven_day_opus":null,
			        "iguana_necktie":{"utilization":41,"resets_at":"2026-07-28T10:00:00.000000+00:00"}}`,
			five:  &Window{Used: 12, HasUsed: true, ResetAt: fiveReset},
			seven: &Window{Used: 40, HasUsed: true, ResetAt: weekReset},
			fable: &Window{Used: 41, HasUsed: true, ResetAt: legacyReset},
		},
		{
			name: "active modern fable limit wins over the legacy field",
			body: `{"iguana_necktie":{"utilization":41,"resets_at":"2026-07-28T10:00:00.000000+00:00"},
			        "limits":[
			          {"kind":"weekly_scoped","percent":12,"resets_at":"2026-07-28T10:00:00.000000+00:00","is_active":false,"scope":{"model":{"display_name":"Fable 5"}}},
			          {"kind":"weekly_scoped","percent":64,"resets_at":"2026-07-27T10:00:00.000000+00:00","is_active":true,"scope":{"model":{"id":null,"display_name":"Fable"}}}]}`,
			fable: &Window{Used: 64, HasUsed: true, ResetAt: modernReset},
		},
		{
			name: "modern fable limit without a percent falls back to the legacy field",
			body: `{"iguana_necktie":{"utilization":41,"resets_at":"2026-07-28T10:00:00.000000+00:00"},
			        "limits":[{"kind":"weekly_scoped","percent":null,"resets_at":"2026-07-27T10:00:00.000000+00:00","is_active":true,"scope":{"model":{"display_name":"Fable"}}}]}`,
			fable: &Window{Used: 41, HasUsed: true, ResetAt: legacyReset},
		},
		{
			name: "malformed and unrelated limits are ignored",
			body: `{"five_hour":{"utilization":10,"resets_at":null},
			        "seven_day":{"utilization":"20.5","resets_at":"2026-10-04T10:00:00+00:00"},
			        "limits":[null,
			          {"kind":"weekly_scoped","percent":35,"scope":{"model":{"display_name":"Sonnet"}}},
			          {"kind":"session","percent":50,"scope":{"model":{"display_name":"Fable"}}},
			          {"kind":7,"percent":50},
			          {"kind":"weekly_scoped","percent":null,"scope":{"model":{"display_name":"Fable"}}}]}`,
			five:  &Window{Used: 10, HasUsed: true},
			seven: &Window{Used: 20.5, HasUsed: true, ResetAt: weekReset},
		},
		{
			name:  "null seven_day and null utilization",
			body:  `{"five_hour":{"utilization":null,"resets_at":null},"seven_day":null}`,
			five:  &Window{},
			seven: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, err := parseClaudeUsage([]byte(tc.body))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			sameWindow(t, "five_hour", u.FiveHour, tc.five)
			sameWindow(t, "seven_day", u.SevenDay, tc.seven)
			sameWindow(t, "fable", u.Fable, tc.fable)
		})
	}

	if _, err := parseClaudeUsage([]byte(`<html>rate limited</html>`)); err == nil {
		t.Error("non-JSON body parsed without error")
	}
}

func TestParseCodexUsage(t *testing.T) {
	now := time.Unix(1785300000, 0)
	tests := []struct {
		name        string
		body        string
		five, seven *Window
	}{
		{
			name:  "pro plan: weekly window only, in primary_window",
			body:  `{"plan_type":"pro","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":1,"limit_window_seconds":604800,"reset_after_seconds":601888,"reset_at":1785902974},"secondary_window":null}}`,
			seven: &Window{Used: 1, HasUsed: true, ResetAt: time.Unix(1785902974, 0)},
		},
		{
			name:  "windows found by length whatever the order",
			body:  `{"rate_limit":{"primary_window":{"used_percent":30,"limit_window_seconds":604800,"reset_at":1785900000},"secondary_window":{"used_percent":80,"limit_window_seconds":18000,"reset_at":1785310000}}}`,
			five:  &Window{Used: 80, HasUsed: true, ResetAt: time.Unix(1785310000, 0)},
			seven: &Window{Used: 30, HasUsed: true, ResetAt: time.Unix(1785900000, 0)},
		},
		{
			name:  "legacy payload without lengths: primary is 5h, secondary is weekly",
			body:  `{"rate_limit":{"primary_window":{"used_percent":"12","reset_at":1785310000},"secondary_window":{"used_percent":45,"reset_at":1785900000}}}`,
			five:  &Window{Used: 12, HasUsed: true, ResetAt: time.Unix(1785310000, 0)},
			seven: &Window{Used: 45, HasUsed: true, ResetAt: time.Unix(1785900000, 0)},
		},
		{
			name:  "limit reached without used_percent reads as 100%, reset from reset_after_seconds",
			body:  `{"rate_limit":{"allowed":false,"limit_reached":true,"primary_window":{"limit_window_seconds":604800,"reset_after_seconds":7200}}}`,
			seven: &Window{Used: 100, HasUsed: true, ResetAt: now.Add(2 * time.Hour)},
		},
		{
			name:  "a 30-day team window counts as the weekly window",
			body:  `{"rate_limit":{"primary_window":{"used_percent":5,"limit_window_seconds":18000,"reset_at":1785310000},"secondary_window":{"used_percent":60,"limit_window_seconds":2592000,"reset_at":1787000000}}}`,
			five:  &Window{Used: 5, HasUsed: true, ResetAt: time.Unix(1785310000, 0)},
			seven: &Window{Used: 60, HasUsed: true, ResetAt: time.Unix(1787000000, 0)},
		},
		{
			name: "no rate_limit gives no windows",
			body: `{"plan_type":"free","rate_limit":null}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, err := parseCodexUsage([]byte(tc.body), now)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			sameWindow(t, "5h", u.FiveHour, tc.five)
			sameWindow(t, "weekly", u.SevenDay, tc.seven)
			if u.Fable != nil {
				t.Errorf("codex usage has a fable window: %+v", u.Fable)
			}
		})
	}
}
