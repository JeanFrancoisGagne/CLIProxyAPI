package claude

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestRefreshTokensCarriesAccountManagementProfile(t *testing.T) {
	client := &ClaudeAuth{httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := `{"access_token":"<fixture-proxy-token>","refresh_token":"<fixture-proxy-token>","expires_in":28800}`
		if request.URL.String() == ProfileURL {
			body = `{"account":{"uuid":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","email":"account@example.com","full_name":"Example Person","display_name":"Example","has_claude_max":true,"has_claude_pro":false},"organization":{"uuid":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","name":"Example Org","subscription_status":"active","subscription_created_at":"2026-10-16T12:00:00Z"}}`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})}}
	data, err := client.RefreshTokens(context.Background(), "<fixture-proxy-token>-account-profile")
	if err != nil {
		t.Fatal(err)
	}
	if data.AccountProfile == nil || data.AccountProfile.FullName != "Example Person" || data.AccountProfile.Plan != "max" || data.AccountProfile.SubscriptionStatus != "active" || data.AccountProfile.SubscriptionStartedAt != "2026-10-16T12:00:00Z" || data.AccountProfile.LastSuccessAt == "" {
		t.Fatalf("profile = %#v", data.AccountProfile)
	}
}
