package management

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const (
	maxAPICallReadEntries = 1024
	maxAPICallRetryBody   = 64 << 10
	maxAPICallRetryHeader = 16 << 10
)

var errAPICallReadCapacity = errors.New("management read coordination capacity reached")

type apiCallReadEntry struct {
	done     chan struct{}
	until    time.Time
	response apiCallResponse
}

// apiCallReadBackoff shares upstream read embargoes between management callers.
// Successes are never shared: each caller still receives its own representation.
type apiCallReadBackoff struct {
	mu      sync.Mutex
	entries map[[32]byte]*apiCallReadEntry
	now     func() time.Time
}

func (b *apiCallReadBackoff) currentTime() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

func apiCallReadKey(req *http.Request, data, proxyRoute, authIndex string, auth *coreauth.Auth) [32]byte {
	// Cosmetic headers cannot defeat a provider's account/resource embargo.
	// Other headers remain in scope because generic callers can carry identity there.
	headers := req.Header.Clone()
	for _, name := range []string{"User-Agent", "Accept", "Accept-Encoding", "Content-Type"} {
		headers.Del(name)
	}
	identity := struct {
		Index, ID, Provider, Token string
		Account, Organization      string
	}{Index: authIndex}
	if auth != nil {
		identity.ID = auth.ID
		identity.Provider = auth.Provider
		identity.Token = tokenValueForAuth(auth)
		identity.Account = stringValue(auth.Metadata, "account_uuid")
		identity.Organization = stringValue(auth.Metadata, "organization_uuid")
	}
	payload, _ := json.Marshal(struct {
		Method, URL, Host, Body, Proxy string
		Identity                       any
		Headers                        http.Header
	}{req.Method, req.URL.String(), req.Host, data, proxyRoute, identity, headers})
	// The map retains a digest, never raw credential values or signed query URLs.
	return sha256.Sum256(payload)
}

func (b *apiCallReadBackoff) acquire(ctx context.Context, key [32]byte) (*apiCallReadEntry, *apiCallResponse, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		b.mu.Lock()
		now := b.currentTime()
		if b.entries == nil {
			b.entries = make(map[[32]byte]*apiCallReadEntry)
		}
		entry := b.entries[key]
		if entry != nil && entry.done != nil {
			done := entry.done
			b.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-done:
				continue
			}
		}
		if entry != nil && now.Before(entry.until) {
			response := entry.response
			response.Header = http.Header(response.Header).Clone()
			remaining := entry.until.Sub(now)
			seconds := int64(remaining / time.Second)
			if remaining%time.Second != 0 {
				seconds++
			}
			http.Header(response.Header).Set("Retry-After", strconv.FormatInt(seconds, 10))
			b.mu.Unlock()
			return nil, &response, nil
		}
		// Never evict a live embargo just to admit another resource. At capacity,
		// fail locally instead of making an upstream call before its known deadline.
		for k, candidate := range b.entries {
			if candidate.done == nil && !now.Before(candidate.until) {
				delete(b.entries, k)
			}
		}
		if len(b.entries) >= maxAPICallReadEntries {
			b.mu.Unlock()
			return nil, nil, errAPICallReadCapacity
		}
		entry = &apiCallReadEntry{done: make(chan struct{})}
		b.entries[key] = entry
		b.mu.Unlock()
		return entry, nil, nil
	}
}

func (b *apiCallReadBackoff) release(key [32]byte, entry *apiCallReadEntry, response *apiCallResponse, receivedAt time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if response != nil && response.StatusCode == http.StatusTooManyRequests {
		if until, ok := apiCallRetryDeadline(http.Header(response.Header), receivedAt); ok {
			entry.until = until
			entry.response = apiCallResponse{StatusCode: response.StatusCode, Header: http.Header(response.Header).Clone(), Body: response.Body}
			if len(entry.response.Body) > maxAPICallRetryBody {
				entry.response.Body = `{"error":"upstream rate limited"}`
			}
			headerBytes := 0
			for name, values := range entry.response.Header {
				headerBytes += len(name)
				for _, value := range values {
					headerBytes += len(value)
				}
			}
			if headerBytes > maxAPICallRetryHeader {
				entry.response.Header = http.Header{"Content-Type": []string{"application/json"}}
			}
		}
	}
	close(entry.done)
	entry.done = nil
	if entry.until.IsZero() {
		delete(b.entries, key)
	}
}

func apiCallRetryDeadline(header http.Header, now time.Time) (time.Time, bool) {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return time.Time{}, false
	}
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil {
		const maxSeconds = uint64((1<<63 - 1) / int64(time.Second))
		if seconds > maxSeconds {
			seconds = maxSeconds
		}
		if seconds == 0 {
			return time.Time{}, false
		}
		return now.Add(time.Duration(seconds) * time.Second), true
	}
	until, err := http.ParseTime(value)
	if err != nil {
		return time.Time{}, false
	}
	// Compare absolute Retry-After values on the provider's clock. A local clock
	// ahead of Date must not shorten the embargo and restart the polling loop.
	if providerNow, errDate := http.ParseTime(header.Get("Date")); errDate == nil {
		delay := until.Sub(providerNow)
		return now.Add(delay), delay > 0
	}
	delay := until.Sub(now)
	return now.Add(delay), delay > 0
}
