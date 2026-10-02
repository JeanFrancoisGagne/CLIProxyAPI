package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const managementPrefix = "/v0/management"

// errManagementAuth means the proxy refused the management key. A pass stops
// at the first refusal: the proxy bans a client IP for 30 minutes after 5
// failed keys (internal/api/handlers/management/handler.go).
var errManagementAuth = errors.New("management key refused")

// mgmtClient talks to the CLIProxyAPI management API. The middleware accepts
// the key as "Authorization: Bearer <key>" or as "X-Management-Key: <key>";
// this client sends the first.
type mgmtClient struct {
	base string
	key  string
	http *http.Client
}

func newMgmtClient(base, key string) *mgmtClient {
	return &mgmtClient{
		base: strings.TrimRight(base, "/"),
		key:  key,
		// The proxy gives an /api-call upstream request 60 seconds.
		http: &http.Client{Timeout: 90 * time.Second},
	}
}

// authFile is the part of a GET /auth-files entry the pacer reads.
type authFile struct {
	Name        string          `json:"name"`
	Provider    string          `json:"provider"`
	Type        string          `json:"type"`
	Email       string          `json:"email"`
	AuthIndex   string          `json:"auth_index"`
	Disabled    bool            `json:"disabled"`
	AccountType string          `json:"account_type"`
	Priority    int             `json:"priority"`
	IDToken     json.RawMessage `json:"id_token"` // Codex only: decoded claims object
}

func (f authFile) provider() string {
	p := f.Provider
	if p == "" {
		p = f.Type
	}
	return strings.ToLower(strings.TrimSpace(p))
}

// chatgptAccountID returns id_token.chatgpt_account_id, which the dashboard
// sends as the Chatgpt-Account-Id header on Codex usage calls.
func (f authFile) chatgptAccountID() string {
	var claims struct {
		ChatgptAccountID string `json:"chatgpt_account_id"`
	}
	if len(f.IDToken) == 0 || json.Unmarshal(f.IDToken, &claims) != nil {
		return ""
	}
	return strings.TrimSpace(claims.ChatgptAccountID)
}

func (c *mgmtClient) listAuthFiles(ctx context.Context) ([]authFile, error) {
	var out struct {
		Files []json.RawMessage `json:"files"`
	}
	if err := c.do(ctx, http.MethodGet, "/auth-files", nil, &out); err != nil {
		return nil, fmt.Errorf("list auth files: %w", err)
	}
	files := make([]authFile, 0, len(out.Files))
	for _, raw := range out.Files {
		var f authFile
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("list auth files: unexpected entry %.200s: %w", raw, err)
		}
		files = append(files, f)
	}
	return files, nil
}

type apiCallRequest struct {
	AuthIndex string            `json:"auth_index"`
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Header    map[string]string `json:"header"`
}

type apiCallResponse struct {
	StatusCode int    `json:"status_code"`
	Body       string `json:"body"`
}

// upstreamGet makes the proxy GET url with the credential at authIndex and
// returns the upstream body. A non-2xx upstream status is an error.
func (c *mgmtClient) upstreamGet(ctx context.Context, authIndex, url string, header map[string]string) ([]byte, error) {
	var resp apiCallResponse
	req := apiCallRequest{AuthIndex: authIndex, Method: http.MethodGet, URL: url, Header: header}
	if err := c.do(ctx, http.MethodPost, "/api-call", req, &resp); err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, shorten(resp.Body))
	}
	return []byte(resp.Body), nil
}

// setPriority writes the credential's top-level "priority" field. The body
// carries only name and priority, so nothing else on the credential changes.
func (c *mgmtClient) setPriority(ctx context.Context, name string, priority int) error {
	body := struct {
		Name     string `json:"name"`
		Priority int    `json:"priority"`
	}{name, priority}
	return c.do(ctx, http.MethodPatch, "/auth-files/fields", body, nil)
}

func (c *mgmtClient) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+managementPrefix+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w: HTTP %d %s", errManagementAuth, resp.StatusCode, errorText(data))
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d %s", resp.StatusCode, errorText(data))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode %s response: %w", path, err)
	}
	return nil
}

// errorText pulls {"error": "..."} out of a management error response.
func errorText(data []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &e) == nil && e.Error != "" {
		return e.Error
	}
	return shorten(string(data))
}

func shorten(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		return s[:160] + "..."
	}
	return s
}
