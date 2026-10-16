package claude

import (
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountmanagement"
)

func (profile *OAuthProfile) AccountSnapshot() *accountmanagement.Profile {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if profile == nil {
		return &accountmanagement.Profile{Status: "unavailable", Source: "claude_oauth_profile", Plan: "unknown", LastCheckedAt: now, ErrorCode: "profile_unavailable"}
	}
	plan := "unknown"
	switch {
	case profile.Organization.Type == "claude_team" && profile.Organization.SubscriptionStatus == "active":
		plan = "team"
	case strings.Contains(profile.Organization.Type, "enterprise"):
		plan = "unknown"
	case profile.Account.HasClaudeMax != nil && *profile.Account.HasClaudeMax:
		plan = "max"
	case profile.Account.HasClaudePro != nil && *profile.Account.HasClaudePro:
		plan = "pro"
	case profile.Account.HasClaudeMax != nil && profile.Account.HasClaudePro != nil:
		plan = "free"
	}
	started := strings.TrimSpace(profile.Organization.SubscriptionCreatedAt)
	if _, err := time.Parse(time.RFC3339, started); err != nil {
		started = ""
	}
	return &accountmanagement.Profile{
		Status: "ok", Source: "claude_oauth_profile", Plan: plan, LastCheckedAt: now, LastSuccessAt: now,
		FullName: strings.TrimSpace(profile.Account.FullName), DisplayName: strings.TrimSpace(profile.Account.DisplayName),
		OrganizationName: strings.TrimSpace(profile.Organization.Name), SubscriptionStatus: strings.TrimSpace(profile.Organization.SubscriptionStatus), SubscriptionStartedAt: started,
	}
}
