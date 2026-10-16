package management

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountmanagement"
)

type receiptInput struct {
	accountmanagement.Receipt
	AmountMinor *int64 `json:"amount_minor"`
	PDFBase64   string `json:"pdf_base64,omitempty"`
}

type receiptBatch struct {
	AuthIndex    string         `json:"auth_index"`
	AccountEmail string         `json:"account_email"`
	ObservedAt   string         `json:"observed_at"`
	Source       string         `json:"source"`
	Receipts     []receiptInput `json:"receipts"`
	ErrorCode    string         `json:"error_code,omitempty"`
}

func receiptText(value string, limit int) bool {
	return strings.TrimSpace(value) == value && len(value) <= limit && strings.IndexFunc(value, unicode.IsControl) < 0
}

func receiptTime(value string) bool {
	_, err := time.Parse(time.RFC3339, value)
	return err == nil
}

func receiptDate(value string) bool {
	if receiptTime(value) {
		return true
	}
	_, err := time.Parse("2006-01-02", value)
	return err == nil
}

func validateReceiptBatch(batch *receiptBatch) ([]accountmanagement.Receipt, map[string][]byte, error) {
	if batch.AuthIndex == "" || batch.AccountEmail == "" || batch.Source != "gmail" || !receiptTime(batch.ObservedAt) || batch.Receipts == nil || len(batch.Receipts) > 100 {
		return nil, nil, fmt.Errorf("invalid_batch")
	}
	switch batch.ErrorCode {
	case "", "source_error", "source_unavailable", "mailbox_auth_required", "receipt_parse_failed", "incomplete_scan", "rate_limited", "receipt_sender_unverified", "gmail_read_failed":
	default:
		return nil, nil, fmt.Errorf("invalid_error_code")
	}
	if batch.ErrorCode != "" && len(batch.Receipts) != 0 {
		return nil, nil, fmt.Errorf("error_batch_contains_receipts")
	}
	receipts := make([]accountmanagement.Receipt, 0, len(batch.Receipts))
	pdfs := make(map[string][]byte)
	seen := make(map[string]bool)
	for _, input := range batch.Receipts {
		receipt := input.Receipt
		if receipt.InvoiceID == "" || seen[receipt.InvoiceID] || !receiptDate(receipt.IssuedAt) || input.AmountMinor == nil || len(receipt.Currency) != 3 || receipt.Currency != strings.ToUpper(receipt.Currency) || strings.Trim(receipt.Currency, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
			return nil, nil, fmt.Errorf("invalid_receipt")
		}
		for value, limit := range map[string]int{receipt.InvoiceID: 200, receipt.BillingName: 512, receipt.BillingEmail: 320, receipt.SourceMailbox: 320, receipt.SourceMessageID: 512} {
			if !receiptText(value, limit) {
				return nil, nil, fmt.Errorf("invalid_receipt_text")
			}
		}
		if receipt.SourceMailbox == "" || receipt.SourceMessageID == "" || len(receipt.EvidenceSHA256) != 64 || receipt.EvidenceSHA256 != strings.ToLower(receipt.EvidenceSHA256) {
			return nil, nil, fmt.Errorf("invalid_evidence")
		}
		if _, err := hex.DecodeString(receipt.EvidenceSHA256); err != nil {
			return nil, nil, fmt.Errorf("invalid_evidence")
		}
		switch receipt.SourceAuthentication {
		case "", "gmail_direct", "gmail_arc":
		default:
			return nil, nil, fmt.Errorf("invalid_source_authentication")
		}
		for _, value := range []string{receipt.PeriodStart, receipt.PeriodEnd, receipt.RenewalAt} {
			if value != "" && !receiptDate(value) {
				return nil, nil, fmt.Errorf("invalid_receipt_date")
			}
		}
		if receipt.PDFRef != "" {
			return nil, nil, fmt.Errorf("pdf_ref_is_server_owned")
		}
		if input.PDFBase64 != "" {
			data, err := base64.StdEncoding.DecodeString(input.PDFBase64)
			if err != nil {
				return nil, nil, fmt.Errorf("invalid_pdf_base64")
			}
			if err := accountmanagement.ValidatePDF(data, receipt.EvidenceSHA256); err != nil {
				return nil, nil, err
			}
			pdfs[receipt.EvidenceSHA256] = data
			receipt.PDFRef = "sha256:" + receipt.EvidenceSHA256
		}
		receipt.AmountMinor = *input.AmountMinor
		seen[receipt.InvoiceID] = true
		receipts = append(receipts, receipt)
	}
	return receipts, pdfs, nil
}

func (h *Handler) ImportAccountReceipts(c *gin.Context) {
	if h.authManager == nil || h.cfg == nil || h.cfg.AuthDir == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "account_store_unavailable"})
		return
	}
	var batch receiptBatch
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 8<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&batch); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_batch"})
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_batch"})
		return
	}
	receipts, pdfs, err := validateReceiptBatch(&batch)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	auth := h.authByIndex(batch.AuthIndex)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "account_not_found"})
		return
	}
	if !strings.EqualFold(authEmail(auth), batch.AccountEmail) {
		c.JSON(http.StatusConflict, gin.H{"error": "account_email_mismatch"})
		return
	}
	for digest, data := range pdfs {
		if err := accountmanagement.StorePDF(h.cfg.AuthDir, digest, data); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "invoice_store_failed"})
			return
		}
	}
	var billing accountmanagement.Billing
	var imported int
	_, err = h.authManager.UpdateAccountMetadata(c.Request.Context(), auth.ID, auth.RegistrationEpoch, func(metadata map[string]any) error {
		if email, _ := metadata["email"].(string); !strings.EqualFold(email, batch.AccountEmail) {
			return fmt.Errorf("account_email_mismatch")
		}
		var mergeErr error
		billing, imported, mergeErr = accountmanagement.MergeBilling(accountmanagement.ReadBilling(metadata), receipts, batch.ObservedAt, batch.ErrorCode)
		if mergeErr != nil {
			return mergeErr
		}
		return accountmanagement.Put(metadata, accountmanagement.BillingKey, billing)
	})
	if err != nil {
		code := "account_store_failed"
		status := http.StatusInternalServerError
		switch err.Error() {
		case "stale_observation", "invoice_conflict", "invoice_limit", "account_email_mismatch":
			code, status = err.Error(), http.StatusConflict
		}
		c.JSON(status, gin.H{"error": code})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "imported": imported, "total": len(billing.Invoices), "billing": billing})
}

func (h *Handler) DownloadAccountInvoice(c *gin.Context) {
	if h.authManager == nil || h.cfg == nil || h.cfg.AuthDir == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "account_store_unavailable"})
		return
	}
	auth := h.authByIndex(c.Query("auth_index"))
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "invoice_not_found"})
		return
	}
	for _, receipt := range accountmanagement.ReadBilling(auth.Metadata).Invoices {
		if receipt.InvoiceID != c.Query("invoice_id") || !strings.HasPrefix(receipt.PDFRef, "sha256:") {
			continue
		}
		data, err := accountmanagement.ReadPDF(h.cfg.AuthDir, strings.TrimPrefix(receipt.PDFRef, "sha256:"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "invoice_not_found"})
			return
		}
		c.Header("Content-Disposition", "attachment; filename=invoice.pdf")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Cache-Control", "no-store")
		c.DataFromReader(http.StatusOK, int64(len(data)), "application/pdf", bytes.NewReader(data), nil)
		return
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "invoice_not_found"})
}
