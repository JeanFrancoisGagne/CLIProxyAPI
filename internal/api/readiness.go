package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const (
	// readyAccessLead is how long a Claude access token must stay valid for the
	// proxy to count it as able to serve.
	readyAccessLead = 10 * time.Minute
	// readyResolveTimeout bounds the DNS checks so a dead resolver answers 503
	// before a 3 s probe gives up.
	readyResolveTimeout = 2 * time.Second
	// readyOverrideFile, in the auth dir, forces the answer for drills. It holds
	// "ready" or "not-ready". Any other content is reported and ignored.
	readyOverrideFile = "ready-override"
)

// readyUpstreamHosts lists the names each provider's requests and refreshes
// need. Claude's are always checked; another provider's only while it has
// credentials loaded.
var readyUpstreamHosts = map[string][]string{
	"claude": {"api.anthropic.com", "platform.claude.com"},
	"codex":  {"chatgpt.com", "auth.openai.com"},
}

type hostResolver func(ctx context.Context, host string) error

func resolveHost(ctx context.Context, host string) error {
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return err
	}
	if len(addrs) == 0 {
		return errors.New("no addresses")
	}
	return nil
}

type readyReport struct {
	Ready     bool                         `json:"ready"`
	Reasons   []string                     `json:"reasons"`
	Override  string                       `json:"override,omitempty"`
	CheckedAt string                       `json:"checked_at"`
	Providers map[string]*auth.GrantCounts `json:"providers"`
	DNS       map[string]string            `json:"dns"`
}

// readyChecker answers GET /healthz/ready. Its fields are seams for tests.
type readyChecker struct {
	grants  func(now time.Time, lead time.Duration) map[string]*auth.GrantCounts
	authDir func() string
	resolve hostResolver
	now     func() time.Time
}

func (s *Server) newReadyChecker() *readyChecker {
	return &readyChecker{
		grants: func(now time.Time, lead time.Duration) map[string]*auth.GrantCounts {
			if s.handlers == nil || s.handlers.AuthManager == nil {
				return map[string]*auth.GrantCounts{}
			}
			return s.handlers.AuthManager.GrantReadiness(now, lead)
		},
		authDir: func() string {
			if s.cfg == nil {
				return ""
			}
			return s.cfg.AuthDir
		},
		resolve: resolveHost,
		now:     time.Now,
	}
}

func (rc *readyChecker) handle(c *gin.Context) {
	report := rc.check(c.Request.Context())
	status := http.StatusOK
	if !report.Ready {
		status = http.StatusServiceUnavailable
	}
	c.JSON(status, report)
}

func (rc *readyChecker) check(ctx context.Context) readyReport {
	now := rc.now()
	report := readyReport{
		Reasons:   []string{},
		CheckedAt: now.UTC().Format(time.RFC3339),
		Providers: rc.grants(now, readyAccessLead),
		DNS:       map[string]string{},
	}

	claude := report.Providers["claude"]
	if claude == nil || claude.Usable == 0 {
		report.Reasons = append(report.Reasons, "no claude grant with an access token valid 10 min ahead and no invalid_grant")
	}

	hosts := append([]string{}, readyUpstreamHosts["claude"]...)
	for provider, names := range readyUpstreamHosts {
		if provider != "claude" && report.Providers[provider] != nil && report.Providers[provider].Total > 0 {
			hosts = append(hosts, names...)
		}
	}
	sort.Strings(hosts)
	for host, errResolve := range rc.resolveAll(ctx, hosts) {
		if errResolve != nil {
			report.DNS[host] = errResolve.Error()
			report.Reasons = append(report.Reasons, "cannot resolve "+host)
			continue
		}
		report.DNS[host] = "ok"
	}
	sort.Strings(report.Reasons)
	report.Ready = len(report.Reasons) == 0

	switch override := rc.readOverride(); override {
	case "":
	case "ready":
		report.Override = override
		report.Ready = true
	case "not-ready":
		report.Override = override
		report.Ready = false
	default:
		report.Override = "ignored: " + override
	}
	return report
}

func (rc *readyChecker) resolveAll(ctx context.Context, hosts []string) map[string]error {
	ctx, cancel := context.WithTimeout(ctx, readyResolveTimeout)
	defer cancel()
	results := make(map[string]error, len(hosts))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, host := range hosts {
		wg.Add(1)
		go func(host string) {
			defer wg.Done()
			errResolve := rc.resolve(ctx, host)
			mu.Lock()
			results[host] = errResolve
			mu.Unlock()
		}(host)
	}
	wg.Wait()
	return results
}

// readOverride returns the trimmed, lower-cased override file contents, or ""
// when the file is absent or unreadable.
func (rc *readyChecker) readOverride() string {
	dir, err := util.ResolveAuthDir(rc.authDir())
	if err != nil || dir == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(dir, readyOverrideFile))
	if err != nil {
		return ""
	}
	value := strings.ToLower(strings.TrimSpace(string(raw)))
	if len(value) > 32 {
		value = value[:32]
	}
	return value
}
