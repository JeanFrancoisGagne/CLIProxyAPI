package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"
)

// GrantCounts summarizes one provider's OAuth credentials for a readiness check.
// Every credential with an access or refresh token lands in exactly one of
// Usable, Expiring, InvalidGrant, Blocked and Disabled. API key credentials
// are not counted.
type GrantCounts struct {
	Total int `json:"total"`
	// Usable credentials hold an access token valid past the lead time and a
	// refresh token that has not drawn invalid_grant, and the manager would
	// select them for a request at the end of the lead time.
	Usable int `json:"usable"`
	// Expiring credentials have no access token valid past the lead time.
	Expiring int `json:"expiring"`
	// InvalidGrant credentials hold a refresh token the provider rejected with
	// invalid_grant. Only a successful refresh or a new login clears this.
	InvalidGrant int `json:"invalid_grant"`
	// Blocked credentials have a good token but the manager will not select
	// them: they are held after a terminal 401, or cooling down (quota, 429)
	// past the lead time.
	Blocked int `json:"blocked"`
	// Disabled credentials were turned off by the operator.
	Disabled int `json:"disabled"`
	// InvalidGrantIDs lists the auth IDs counted in InvalidGrant, sorted.
	InvalidGrantIDs []string `json:"invalid_grant_ids"`
	// BlockedIDs lists the auth IDs counted in Blocked, sorted.
	BlockedIDs []string `json:"blocked_ids"`
}

func refreshTokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// markRefreshTokenRejected records that the provider answered invalid_grant
// for this auth's refresh token. The mark sticks to the token, not to the
// auth's status fields, so later request errors, priority patches and file
// reloads that keep the same token do not erase it.
func (m *Manager) markRefreshTokenRejected(id, refreshToken string) {
	if m == nil || strings.TrimSpace(id) == "" || refreshToken == "" {
		return
	}
	m.rejectedRefreshTokens.Store(id, refreshTokenFingerprint(refreshToken))
}

// clearRefreshTokenRejected drops the mark after the provider accepted a
// refresh, even when the response kept the same refresh token.
func (m *Manager) clearRefreshTokenRejected(id string) {
	if m == nil {
		return
	}
	m.rejectedRefreshTokens.Delete(id)
}

// refreshTokenRejected reports whether the auth still holds the refresh token
// that last drew invalid_grant.
func (m *Manager) refreshTokenRejected(a *Auth) bool {
	if m == nil || a == nil {
		return false
	}
	value, ok := m.rejectedRefreshTokens.Load(a.ID)
	if !ok {
		return false
	}
	token := authRefreshToken(a)
	if token == "" {
		return false
	}
	fingerprint, _ := value.(string)
	return fingerprint == refreshTokenFingerprint(token)
}

// GrantReadiness counts each provider's OAuth credentials by whether they can
// serve a request for at least lead from now. A credential counts as Blocked
// when the selector would skip it, with no model named, at now+lead.
// Providers are keyed in lower case.
func (m *Manager) GrantReadiness(now time.Time, lead time.Duration) map[string]*GrantCounts {
	out := make(map[string]*GrantCounts)
	if m == nil {
		return out
	}
	horizon := now.Add(lead)
	for _, a := range m.List() {
		if a == nil || (authAccessToken(a) == "" && authRefreshToken(a) == "") {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(a.Provider))
		counts := out[provider]
		if counts == nil {
			counts = &GrantCounts{InvalidGrantIDs: []string{}, BlockedIDs: []string{}}
			out[provider] = counts
		}
		counts.Total++
		switch {
		case a.Disabled || a.Status == StatusDisabled:
			counts.Disabled++
		case m.refreshTokenRejected(a):
			counts.InvalidGrant++
			counts.InvalidGrantIDs = append(counts.InvalidGrantIDs, a.ID)
		case !a.HasValidAccessToken(horizon):
			counts.Expiring++
		case grantBlocked(a, horizon):
			counts.Blocked++
			counts.BlockedIDs = append(counts.BlockedIDs, a.ID)
		default:
			counts.Usable++
		}
	}
	for _, counts := range out {
		sort.Strings(counts.InvalidGrantIDs)
		sort.Strings(counts.BlockedIDs)
	}
	return out
}

// grantBlocked reports whether the selector would skip a for every model at
// horizon: a terminal 401, a credential-wide quota, or an auth-level cooldown
// that lasts past horizon.
func grantBlocked(a *Auth, horizon time.Time) bool {
	blocked, _, _ := isAuthBlockedForModel(a, "", horizon)
	return blocked
}
