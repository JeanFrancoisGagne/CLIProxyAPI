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
// Usable, Expiring, InvalidGrant and Disabled. API key credentials are not counted.
type GrantCounts struct {
	Total int `json:"total"`
	// Usable credentials hold an access token valid past the lead time and a
	// refresh token that has not drawn invalid_grant.
	Usable int `json:"usable"`
	// Expiring credentials have no access token valid past the lead time.
	Expiring int `json:"expiring"`
	// InvalidGrant credentials hold a refresh token the provider rejected with
	// invalid_grant. Only a successful refresh or a new login clears this.
	InvalidGrant int `json:"invalid_grant"`
	// Disabled credentials were turned off by the operator.
	Disabled int `json:"disabled"`
	// InvalidGrantIDs lists the auth IDs counted in InvalidGrant, sorted.
	InvalidGrantIDs []string `json:"invalid_grant_ids"`
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
// serve a request for at least lead from now. Providers are keyed in lower case.
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
			counts = &GrantCounts{InvalidGrantIDs: []string{}}
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
		default:
			counts.Usable++
		}
	}
	for _, counts := range out {
		sort.Strings(counts.InvalidGrantIDs)
	}
	return out
}
