package management

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountmanagement"
)

type accountOverview struct {
	AuthIndex string                    `json:"auth_index"`
	Name      string                    `json:"name"`
	Provider  string                    `json:"provider"`
	Email     string                    `json:"email"`
	Disabled  bool                      `json:"disabled"`
	Profile   accountmanagement.Profile `json:"profile"`
	Billing   accountmanagement.Billing `json:"billing"`
}

// GetAccountOverview reads stored summaries only; dashboard loads never contact providers.
func (h *Handler) GetAccountOverview(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return
	}
	accounts := make([]accountOverview, 0)
	for _, auth := range h.authManager.List() {
		if auth == nil {
			continue
		}
		name := strings.TrimSpace(auth.FileName)
		if name == "" {
			name = auth.ID
		}
		accounts = append(accounts, accountOverview{
			AuthIndex: auth.EnsureIndex(), Name: name, Provider: auth.Provider,
			Email: authEmail(auth), Disabled: auth.Disabled,
			Profile: accountmanagement.ProfileAt(auth.Metadata, auth.Provider, time.Now()),
			Billing: accountmanagement.BillingAt(auth.Metadata, time.Now()),
		})
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].Name < accounts[j].Name })
	c.JSON(http.StatusOK, gin.H{"accounts": accounts})
}
