package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestListAuthFilesReportsPersistedOAuthRefreshAfterReload(t *testing.T) {
	for _, oldRuntime := range []time.Time{{}, time.Date(2026, 10, 19, 16, 0, 0, 0, time.UTC)} {
		t.Run(oldRuntime.String(), func(t *testing.T) {
			authDir := t.TempDir()
			fileName := "claude-fixture@example.invalid.json"
			manager := coreauth.NewManager(nil, nil, nil)
			_, err := manager.Register(context.Background(), &coreauth.Auth{
				ID: fileName, FileName: fileName, Provider: "claude", Status: coreauth.StatusActive,
				LastRefreshedAt: oldRuntime,
				Attributes:      map[string]string{"path": filepath.Join(authDir, fileName), "priority": "10"},
				Metadata:        map[string]any{"email": "fixture@example.invalid", "priority": 10},
			})
			if err != nil {
				t.Fatal(err)
			}
			h := &Handler{cfg: &config.Config{AuthDir: authDir}, authManager: manager, tokenStore: sdkAuth.NewFileTokenStore()}
			h.SetPostAuthPersistHook(func(ctx context.Context, auth *coreauth.Auth) error {
				// The service preserves runtime refresh scheduling on a watcher update.
				// Exercise its persisted OAuth/synthesized metadata through the list API.
				previous, _ := manager.GetByID(auth.ID)
				auth.LastRefreshedAt = previous.LastRefreshedAt
				_, errUpdate := manager.Update(ctx, auth)
				return errUpdate
			})
			const fresh = "2026-10-20T16:00:00Z"
			storage := &claude.ClaudeTokenStorage{
				AccessToken: "fixture-new-access", RefreshToken: "fixture-new-refresh",
				Email: "fixture@example.invalid", LastRefresh: fresh, Expire: "2026-10-21T16:00:00Z",
			}
			_, err = h.saveTokenRecord(context.Background(), &coreauth.Auth{
				ID: fileName, FileName: fileName, Provider: "claude", Storage: storage,
				Metadata: map[string]any{"email": storage.Email},
			})
			if err != nil {
				t.Fatal(err)
			}
			router := gin.New()
			router.GET("/credentials", h.ListAuthFiles)
			assertRefresh := func(want string) {
				t.Helper()
				request := httptest.NewRequest(http.MethodGet, "/credentials?name="+fileName, nil)
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, request)
				var response struct {
					Files []struct {
						LastRefresh string `json:"last_refresh"`
					} `json:"files"`
				}
				if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
					t.Fatal(errDecode)
				}
				if recorder.Code != http.StatusOK || len(response.Files) != 1 || response.Files[0].LastRefresh != want {
					t.Fatalf("listed refresh after OAuth reload: status = %d, files = %+v; want last_refresh %q", recorder.Code, response.Files, want)
				}
			}
			assertRefresh(fresh)
			updated, _ := manager.GetByID(fileName)
			updated.Attributes["priority"] = "90"
			updated.UpdatedAt = time.Date(2026, 10, 20, 17, 0, 0, 0, time.UTC)
			if _, errUpdate := manager.Update(context.Background(), updated); errUpdate != nil {
				t.Fatal(errUpdate)
			}
			assertRefresh(fresh)
			for _, metadataValue := range []string{"invalid-fixture-date", "2026-10-18T16:00:00Z"} {
				updated, _ = manager.GetByID(fileName)
				updated.LastRefreshedAt, _ = time.Parse(time.RFC3339, fresh)
				updated.Metadata["last_refresh"] = metadataValue
				if _, errUpdate := manager.Update(context.Background(), updated); errUpdate != nil {
					t.Fatal(errUpdate)
				}
				assertRefresh(fresh)
			}
		})
	}
}
