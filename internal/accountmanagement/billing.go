package accountmanagement

import (
	"fmt"
	"reflect"
	"sort"
	"time"
)

func MergeBilling(current Billing, receipts []Receipt, observedAt, errorCode string) (Billing, int, error) {
	observed, _ := time.Parse(time.RFC3339, observedAt)
	last, _ := time.Parse(time.RFC3339, current.LastCheckedAt)
	if observed.Before(last) {
		return current, 0, fmt.Errorf("stale_observation")
	}
	current.LastCheckedAt = observedAt
	current.ErrorCode = errorCode
	if errorCode != "" {
		current.Status = "unavailable"
		if current.LastSuccessAt != "" {
			current.Status = "stale"
		}
		return current, 0, nil
	}
	byID := make(map[string]Receipt, len(current.Invoices))
	for _, receipt := range current.Invoices {
		byID[receipt.InvoiceID] = receipt
	}
	imported := 0
	changed := false
	for _, receipt := range receipts {
		old, exists := byID[receipt.InvoiceID]
		if exists && (old.AmountMinor != receipt.AmountMinor || old.Currency != receipt.Currency || !invoiceDate(old.IssuedAt).Equal(invoiceDate(receipt.IssuedAt))) {
			return current, 0, fmt.Errorf("invoice_conflict")
		}
		if !exists {
			imported++
		}
		if exists {
			receipt = preserveReceiptEvidence(old, receipt)
		}
		changed = changed || !exists || !reflect.DeepEqual(old, receipt)
		byID[receipt.InvoiceID] = receipt
	}
	if len(byID) > 1000 {
		return current, 0, fmt.Errorf("invoice_limit")
	}
	current.Invoices = make([]Receipt, 0, len(byID))
	for _, receipt := range byID {
		current.Invoices = append(current.Invoices, receipt)
	}
	sort.Slice(current.Invoices, func(i, j int) bool {
		left := invoiceDate(current.Invoices[i].IssuedAt)
		right := invoiceDate(current.Invoices[j].IssuedAt)
		if left.Equal(right) {
			return current.Invoices[i].InvoiceID < current.Invoices[j].InvoiceID
		}
		return left.After(right)
	})
	current.Status, current.Source, current.LastSuccessAt = "empty", "gmail_receipt", observedAt
	if changed {
		current.UpdatedAt = observedAt
	}
	if len(current.Invoices) > 0 {
		current.Status = "ok"
		latest := current.Invoices[0]
		current.BillingName, current.BillingEmail = latest.BillingName, latest.BillingEmail
		current.RenewalAt, current.PeriodEnd = latest.RenewalAt, latest.PeriodEnd
	}
	return current, imported, nil
}

func invoiceDate(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err == nil {
		return parsed
	}
	parsed, _ = time.Parse("2006-01-02", value)
	return parsed
}

// A repeated receipt may add evidence, but omitted fields cannot delete facts
// already verified for the same invoice. A new invoice gets its own dates.
func preserveReceiptEvidence(old, incoming Receipt) Receipt {
	fields := []struct {
		next     *string
		previous string
	}{
		{&incoming.PDFRef, old.PDFRef}, {&incoming.BillingName, old.BillingName}, {&incoming.BillingEmail, old.BillingEmail},
		{&incoming.PeriodStart, old.PeriodStart}, {&incoming.PeriodEnd, old.PeriodEnd}, {&incoming.RenewalAt, old.RenewalAt},
		{&incoming.SourceAuthentication, old.SourceAuthentication},
	}
	for _, field := range fields {
		if *field.next == "" {
			*field.next = field.previous
		}
	}
	return incoming
}
