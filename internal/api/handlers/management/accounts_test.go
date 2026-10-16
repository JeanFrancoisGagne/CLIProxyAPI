package management

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountmanagement"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestAccountOverviewDoesNotExposeCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: "claude-account.json", FileName: "claude-account.json", Provider: "claude",
		Metadata: map[string]any{"email": "account@example.com", "access_token": "<fixture-proxy-token>", "refresh_token": "<fixture-proxy-token>"},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandlerWithoutConfigFilePath(&config.Config{}, manager)
	router := gin.New()
	router.GET("/credentials/accounts", handler.GetAccountOverview)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/credentials/accounts", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if strings.Contains(response.Body.String(), "fixture-proxy-token") || strings.Contains(response.Body.String(), "access_token") || strings.Contains(response.Body.String(), "refresh_token") {
		t.Fatal("overview exposed credentials")
	}
	var body struct {
		Accounts []struct {
			AuthIndex string `json:"auth_index"`
			Email     string `json:"email"`
			Profile   struct {
				Status string `json:"status"`
				Plan   string `json:"plan"`
			} `json:"profile"`
			Billing struct {
				Status   string            `json:"status"`
				Invoices []json.RawMessage `json:"invoices"`
			} `json:"billing"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Accounts) != 1 {
		t.Fatalf("accounts = %d", len(body.Accounts))
	}
	got := body.Accounts[0]
	if got.AuthIndex != auth.EnsureIndex() || got.Email != "account@example.com" || got.Profile.Status != "pending" || got.Profile.Plan != "unknown" || got.Billing.Status != "unavailable" || got.Billing.Invoices == nil {
		t.Fatalf("unexpected account overview: %s", response.Body.String())
	}
}

func accountReceiptFixture(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	dir := t.TempDir()
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(store, nil, nil)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{ID: "account.json", FileName: "account.json", Provider: "claude", Attributes: map[string]string{"path": filepath.Join(dir, "account.json")}, Metadata: map[string]any{"type": "claude", "email": "account@example.com", "access_token": "<fixture-proxy-token>"}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, manager)
	router := gin.New()
	router.POST("/receipts", handler.ImportAccountReceipts)
	router.GET("/accounts", handler.GetAccountOverview)
	router.GET("/invoice", handler.DownloadAccountInvoice)
	return router, auth.EnsureIndex()
}

func postReceiptJSON(router *gin.Engine, body string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/receipts", strings.NewReader(body)))
	return response
}

func TestAccountReceiptScanFreshnessPreservesHistory(t *testing.T) {
	router, index := accountReceiptFixture(t)
	first := `{"auth_index":"` + index + `","account_email":"account@example.com","observed_at":"2026-10-16T15:00:00Z","source":"gmail","receipts":[{"invoice_id":"receipt-1","issued_at":"2026-10-16T00:00:00Z","currency":"USD","amount_minor":0,"period_start":"2026-10-16","period_end":"2026-11-16","source_mailbox":"mailbox@example.com","source_message_id":"message-1","source_authentication":"gmail_arc","evidence_sha256":"` + strings.Repeat("a", 64) + `"}]}`
	for i := 0; i < 2; i++ {
		response := postReceiptJSON(router, first)
		if response.Code != 200 {
			t.Fatalf("import=%d %s", response.Code, response.Body.String())
		}
	}
	for _, scan := range []struct{ at, code, status string }{
		{"2026-10-16T16:00:00Z", "", "ok"},
		{"2026-10-16T17:00:00Z", "gmail_read_failed", "stale"},
		{"2026-10-16T18:00:00Z", "receipt_sender_unverified", "stale"},
	} {
		body := `{"auth_index":"` + index + `","account_email":"account@example.com","observed_at":"` + scan.at + `","source":"gmail","receipts":[],"error_code":"` + scan.code + `"}`
		response := postReceiptJSON(router, body)
		if response.Code != 200 {
			t.Fatalf("scan=%d %s", response.Code, response.Body.String())
		}
		var result struct {
			Billing accountmanagement.Billing `json:"billing"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Billing.Status != scan.status || len(result.Billing.Invoices) != 1 || result.Billing.RenewalAt != "" || result.Billing.PeriodEnd != "2026-11-16" || result.Billing.LastCheckedAt != scan.at {
			t.Fatalf("billing=%+v", result.Billing)
		}
		if scan.code != "" && result.Billing.LastSuccessAt != "2026-10-16T16:00:00Z" {
			t.Fatal("failed scan replaced successful freshness")
		}
	}
}

func TestAccountReceiptRejectsInvalidEvidence(t *testing.T) {
	for _, test := range []struct {
		name, extra, email string
		status             int
	}{
		{"missing amount", `"currency":"USD",`, "account@example.com", 400},
		{"bad authentication", `"currency":"USD","amount_minor":1,"source_authentication":"pretend",`, "account@example.com", 400},
		{"caller reference", `"currency":"USD","amount_minor":1,"pdf_ref":"sha256:` + strings.Repeat("a", 64) + `",`, "account@example.com", 400},
		{"bad PDF hash", `"currency":"USD","amount_minor":1,"pdf_base64":"JVBERi0xLjQK",`, "account@example.com", 400},
		{"wrong account", `"currency":"USD","amount_minor":1,`, "other@example.com", 409},
	} {
		t.Run(test.name, func(t *testing.T) {
			router, index := accountReceiptFixture(t)
			body := `{"auth_index":"` + index + `","account_email":"` + test.email + `","observed_at":"2026-10-16T15:00:00Z","source":"gmail","receipts":[{"invoice_id":"receipt-1","issued_at":"2026-10-16T00:00:00Z",` + test.extra + `"source_mailbox":"mailbox@example.com","source_message_id":"message-1","evidence_sha256":"` + strings.Repeat("a", 64) + `"}]}`
			response := postReceiptJSON(router, body)
			if response.Code != test.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			download := httptest.NewRecorder()
			router.ServeHTTP(download, httptest.NewRequest(http.MethodGet, "/invoice?auth_index="+index+"&invoice_id=receipt-1", nil))
			if download.Code != 404 {
				t.Fatal("invalid invoice became downloadable")
			}
		})
	}
}

func TestAccountReceiptAcceptsCollectorExportContract(t *testing.T) {
	router, index := accountReceiptFixture(t)
	raw, err := os.ReadFile("../../../../tools/account_billing/tests/fixtures/exported_scan.json")
	if err != nil {
		t.Fatal(err)
	}
	var batch map[string]any
	if err = json.Unmarshal(raw, &batch); err != nil {
		t.Fatal(err)
	}
	batch["auth_index"], batch["account_email"] = index, "account@example.com"
	raw, _ = json.Marshal(batch)
	response := postReceiptJSON(router, string(raw))
	if response.Code != 200 {
		t.Fatalf("collector JSON status=%d: %s", response.Code, response.Body.String())
	}
	var result struct {
		Billing accountmanagement.Billing `json:"billing"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Billing.Invoices) != 1 || result.Billing.Invoices[0].SourceAuthentication != "gmail_direct" || result.Billing.PeriodEnd != "2026-11-16" || result.Billing.RenewalAt != "" || result.Billing.Invoices[0].AmountMinor != 32193 {
		t.Fatalf("collector data changed: %+v", result.Billing)
	}
	batch["receipts"] = []any{}
	for i, code := range []string{"source_error", "source_unavailable", "mailbox_auth_required", "receipt_parse_failed", "incomplete_scan", "rate_limited", "receipt_sender_unverified", "gmail_read_failed"} {
		batch["error_code"] = code
		batch["observed_at"] = time.Date(2026, 10, 16, 16, i, 0, 0, time.UTC).Format(time.RFC3339)
		raw, _ = json.Marshal(batch)
		response = postReceiptJSON(router, string(raw))
		if response.Code != 200 {
			t.Fatalf("collector error %s rejected: %d %s", code, response.Code, response.Body.String())
		}
	}
}

func TestAccountReceiptBatchSurvivesReloadAndDownloadsOwnedPDF(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(store, nil, nil)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: "claude-receipt.json", FileName: "claude-receipt.json", Provider: "claude",
		Attributes: map[string]string{"path": filepath.Join(dir, "claude-receipt.json")},
		Metadata:   map[string]any{"email": "account@example.com", "type": "claude", "access_token": "<fixture-proxy-token>", "priority": 7.0},
	})
	if err != nil {
		t.Fatal(err)
	}
	pdf := []byte("%PDF-1.4\nfixture invoice\n%%EOF\n")
	digest := sha256.Sum256(pdf)
	batch := map[string]any{
		"auth_index": auth.EnsureIndex(), "account_email": "account@example.com", "source": "gmail", "observed_at": time.Now().UTC().Format(time.RFC3339),
		"receipts": []any{map[string]any{
			"invoice_id": "invoice-123", "issued_at": "2026-10-16T00:00:00Z", "currency": "USD", "amount_minor": 20000,
			"billing_name": "Example Customer", "billing_email": "billing@example.com", "period_end": "2026-11-16T00:00:00Z",
			"source_mailbox": "mailbox@example.com", "source_message_id": "message-123", "evidence_sha256": hex.EncodeToString(digest[:]), "pdf_base64": base64.StdEncoding.EncodeToString(pdf),
		}},
	}
	raw, _ := json.Marshal(batch)
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, manager)
	router := gin.New()
	router.POST("/credentials/accounts/receipts", handler.ImportAccountReceipts)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/credentials/accounts/receipts", strings.NewReader(string(raw))))
	if response.Code != http.StatusOK {
		t.Fatalf("import status=%d: %s", response.Code, response.Body.String())
	}
	reloaded := coreauth.NewManager(store, nil, nil)
	if err := reloaded.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	handler = NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, reloaded)
	router = gin.New()
	router.GET("/credentials/accounts", handler.GetAccountOverview)
	router.GET("/credentials/accounts/invoice", handler.DownloadAccountInvoice)
	overview := httptest.NewRecorder()
	router.ServeHTTP(overview, httptest.NewRequest(http.MethodGet, "/credentials/accounts", nil))
	var body struct {
		Accounts []struct {
			Billing struct {
				Status    string `json:"status"`
				RenewalAt string `json:"renewal_at"`
				PeriodEnd string `json:"period_end"`
				Invoices  []struct {
					InvoiceID string `json:"invoice_id"`
					PDFRef    string `json:"pdf_ref"`
				} `json:"invoices"`
			} `json:"billing"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(overview.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Accounts) != 1 || body.Accounts[0].Billing.Status != "ok" || body.Accounts[0].Billing.RenewalAt != "" || body.Accounts[0].Billing.PeriodEnd != "2026-11-16T00:00:00Z" || len(body.Accounts[0].Billing.Invoices) != 1 || body.Accounts[0].Billing.Invoices[0].PDFRef != "sha256:"+hex.EncodeToString(digest[:]) {
		t.Fatalf("bad persisted overview: %s", overview.Body.String())
	}
	download := httptest.NewRecorder()
	router.ServeHTTP(download, httptest.NewRequest(http.MethodGet, "/credentials/accounts/invoice?auth_index="+auth.EnsureIndex()+"&invoice_id=invoice-123", nil))
	if download.Code != http.StatusOK || download.Body.String() != string(pdf) || download.Header().Get("Content-Type") != "application/pdf" || download.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("download status=%d headers=%v", download.Code, download.Header())
	}
	saved, ok := reloaded.GetByID(auth.ID)
	if !ok || saved.Metadata["access_token"] != "<fixture-proxy-token>" || saved.Metadata["priority"] != 7.0 {
		t.Fatal("receipt replaced credential fields")
	}
}
