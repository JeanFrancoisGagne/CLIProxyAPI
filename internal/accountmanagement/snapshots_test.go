package accountmanagement

import (
	"testing"
	"time"
)

func TestAccountFreshnessAgesWithoutDiscardingEvidence(t *testing.T) {
	now := time.Date(2026, 10, 16, 18, 0, 0, 0, time.UTC)
	metadata := make(map[string]any)
	if err := Put(metadata, ProfileKey, Profile{Status: "ok", Plan: "max", FullName: "Example Person", LastSuccessAt: "2026-10-15T17:59:59Z"}); err != nil {
		t.Fatal(err)
	}
	if err := Put(metadata, BillingKey, Billing{Status: "ok", Source: "gmail_receipt", LastSuccessAt: "2026-10-16T04:59:59Z", Invoices: []Receipt{{InvoiceID: "invoice-1"}}}); err != nil {
		t.Fatal(err)
	}
	profile := ProfileAt(metadata, "claude", now)
	billing := BillingAt(metadata, now)
	if profile.Status != "stale" || profile.ErrorCode != "profile_overdue" || profile.FullName != "Example Person" {
		t.Fatalf("profile=%+v", profile)
	}
	if billing.Status != "stale" || billing.ErrorCode != "scan_overdue" || len(billing.Invoices) != 1 || billing.Invoices[0].InvoiceID != "invoice-1" {
		t.Fatalf("billing=%+v", billing)
	}
	if ReadProfile(metadata, "claude").Status != "ok" || ReadBilling(metadata).Status != "ok" {
		t.Fatal("projection mutated stored snapshots")
	}
}

func TestProfileFailureRetainsConfirmedSubscription(t *testing.T) {
	previous := Profile{Status: "ok", Source: "claude_oauth_profile", Plan: "max", FullName: "Example Person", LastSuccessAt: "2026-10-16T01:00:00Z"}
	got := MergeProfile(previous, Profile{Status: "unavailable", Plan: "unknown", LastCheckedAt: "2026-10-16T02:00:00Z", ErrorCode: "profile_unavailable"})
	if got.Status != "stale" || got.Plan != "max" || got.FullName != "Example Person" || got.LastSuccessAt != previous.LastSuccessAt || got.LastCheckedAt != "2026-10-16T02:00:00Z" {
		t.Fatalf("profile=%+v", got)
	}
}
