// Package accountmanagement defines safe account summaries stored with credentials.
package accountmanagement

import (
	"encoding/json"
	"time"
)

const (
	ProfileKey = "account_profile"
	BillingKey = "account_billing"
)

type Profile struct {
	Status                string `json:"status"`
	Source                string `json:"source,omitempty"`
	LastCheckedAt         string `json:"last_checked_at,omitempty"`
	LastSuccessAt         string `json:"last_success_at,omitempty"`
	FullName              string `json:"full_name,omitempty"`
	DisplayName           string `json:"display_name,omitempty"`
	OrganizationName      string `json:"organization_name,omitempty"`
	Plan                  string `json:"plan"`
	SubscriptionStatus    string `json:"subscription_status,omitempty"`
	SubscriptionStartedAt string `json:"subscription_started_at,omitempty"`
	ErrorCode             string `json:"error_code,omitempty"`
}

type Receipt struct {
	InvoiceID            string `json:"invoice_id"`
	IssuedAt             string `json:"issued_at"`
	Currency             string `json:"currency"`
	AmountMinor          int64  `json:"amount_minor"`
	BillingName          string `json:"billing_name,omitempty"`
	BillingEmail         string `json:"billing_email,omitempty"`
	PeriodStart          string `json:"period_start,omitempty"`
	PeriodEnd            string `json:"period_end,omitempty"`
	RenewalAt            string `json:"renewal_at,omitempty"`
	PDFRef               string `json:"pdf_ref,omitempty"`
	SourceMailbox        string `json:"source_mailbox"`
	SourceMessageID      string `json:"source_message_id"`
	SourceAuthentication string `json:"source_authentication,omitempty"`
	EvidenceSHA256       string `json:"evidence_sha256"`
}

type Billing struct {
	Status        string    `json:"status"`
	Source        string    `json:"source"`
	LastCheckedAt string    `json:"last_checked_at,omitempty"`
	LastSuccessAt string    `json:"last_success_at,omitempty"`
	UpdatedAt     string    `json:"updated_at,omitempty"`
	ErrorCode     string    `json:"error_code,omitempty"`
	BillingName   string    `json:"billing_name,omitempty"`
	BillingEmail  string    `json:"billing_email,omitempty"`
	RenewalAt     string    `json:"renewal_at,omitempty"`
	PeriodEnd     string    `json:"period_end,omitempty"`
	Invoices      []Receipt `json:"invoices"`
}

func ReadProfile(metadata map[string]any, provider string) Profile {
	profile := Profile{Status: "pending", Source: "claude_oauth_profile", Plan: "unknown"}
	if provider != "claude" {
		return Profile{Status: "unavailable", Plan: "unknown", ErrorCode: "unsupported_provider"}
	}
	decode(metadata[ProfileKey], &profile)
	return profile
}

func ReadBilling(metadata map[string]any) Billing {
	billing := Billing{Status: "unavailable", Source: "gmail_receipt", Invoices: []Receipt{}}
	decode(metadata[BillingKey], &billing)
	if billing.Invoices == nil {
		billing.Invoices = []Receipt{}
	}
	return billing
}

func ProfileAt(metadata map[string]any, provider string, now time.Time) Profile {
	profile := ReadProfile(metadata, provider)
	if profile.Status == "ok" && expiredObservation(profile.LastSuccessAt, now, 24*time.Hour) {
		profile.Status, profile.ErrorCode = "stale", "profile_overdue"
	}
	return profile
}

func BillingAt(metadata map[string]any, now time.Time) Billing {
	billing := ReadBilling(metadata)
	if (billing.Status == "ok" || billing.Status == "empty") && expiredObservation(billing.LastSuccessAt, now, 13*time.Hour) {
		billing.Status, billing.ErrorCode = "stale", "scan_overdue"
	}
	return billing
}

func expiredObservation(raw string, now time.Time, maximumAge time.Duration) bool {
	observed, err := time.Parse(time.RFC3339, raw)
	return err != nil || now.Sub(observed) > maximumAge
}

func MergeProfile(current Profile, observed Profile) Profile {
	if observed.Status == "ok" {
		return observed
	}
	current.LastCheckedAt, current.ErrorCode = observed.LastCheckedAt, observed.ErrorCode
	current.Status, current.Source = "unavailable", "claude_oauth_profile"
	if current.LastSuccessAt != "" {
		current.Status = "stale"
	}
	return current
}

func decode(value any, target any) {
	if value == nil {
		return
	}
	data, err := json.Marshal(value)
	if err == nil {
		_ = json.Unmarshal(data, target)
	}
}

// Store detached JSON values. Readers decode snapshots before replacing them.
func Put(metadata map[string]any, key string, snapshot any) error {
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	var value any
	if err = json.Unmarshal(data, &value); err != nil {
		return err
	}
	metadata[key] = value
	return nil
}
