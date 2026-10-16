package executor

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountmanagement"
	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestClaudeAccountBackfillEnrichesIdleCredentialWithoutRotation(t *testing.T) {
	dir := t.TempDir()
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(store, nil, nil)
	executor := NewClaudeExecutor(&config.Config{})
	executor.oauthProfileFetcher = func(context.Context, *coreauth.Auth, string) (*claudeauth.OAuthProfile, error) {
		profile := &claudeauth.OAuthProfile{}
		profile.Account.UUID, profile.Account.Email, profile.Account.FullName = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "account@example.com", "Example Person"
		max := true
		profile.Account.HasClaudeMax = &max
		return profile, nil
	}
	manager.RegisterExecutor(executor)
	_, err := manager.Register(context.Background(), &coreauth.Auth{ID: "idle.json", FileName: "idle.json", Provider: "claude", Attributes: map[string]string{"path": filepath.Join(dir, "idle.json")}, Metadata: map[string]any{"email": "account@example.com", "type": "claude", "access_token": "sk-ant-oat-<fixture-proxy-token>", "refresh_token": "<fixture-proxy-token>", "account_uuid": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "priority": 7.0}})
	if err != nil {
		t.Fatal(err)
	}
	manager.BackfillAccountProfiles(context.Background())
	reloaded := coreauth.NewManager(store, nil, nil)
	if err := reloaded.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	auth, ok := reloaded.GetByID("idle.json")
	if !ok {
		t.Fatal("credential disappeared")
	}
	profile := accountmanagement.ReadProfile(auth.Metadata, "claude")
	if profile.Status != "ok" || profile.FullName != "Example Person" || profile.Plan != "max" || auth.Metadata["refresh_token"] != "<fixture-proxy-token>" || auth.Metadata["priority"] != 7.0 {
		t.Fatalf("profile=%+v or credentials changed", profile)
	}
}

func TestClaudeAccountBackfillFailurePreservesConcurrentProfile(t *testing.T) {
	dir := t.TempDir()
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(store, nil, nil)
	executor := NewClaudeExecutor(&config.Config{})
	entered, release := make(chan struct{}), make(chan struct{})
	executor.oauthProfileFetcher = func(context.Context, *coreauth.Auth, string) (*claudeauth.OAuthProfile, error) {
		close(entered)
		<-release
		return nil, errors.New("temporary profile failure")
	}
	manager.RegisterExecutor(executor)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{ID: "profile.json", FileName: "profile.json", Provider: "claude", Attributes: map[string]string{"path": filepath.Join(dir, "profile.json")}, Metadata: map[string]any{"email": "account@example.com", "type": "claude", "access_token": "sk-ant-oat-<fixture-proxy-token>"}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { manager.BackfillAccountProfiles(context.Background()); close(done) }()
	<-entered
	checked := time.Now().UTC().Format(time.RFC3339Nano)
	if err = accountmanagement.Put(auth.Metadata, accountmanagement.ProfileKey, accountmanagement.Profile{Status: "ok", Plan: "max", FullName: "Confirmed Person", LastCheckedAt: checked, LastSuccessAt: checked}); err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Update(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	current, _ := manager.GetByID(auth.ID)
	profile := accountmanagement.ReadProfile(current.Metadata, "claude")
	if profile.Status != "stale" || profile.Plan != "max" || profile.FullName != "Confirmed Person" || profile.LastSuccessAt != checked {
		t.Fatalf("profile=%+v", profile)
	}
}

func TestClaudeAccountBackfillCannotReplaceNewSameIDIdentity(t *testing.T) {
	dir := t.TempDir()
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(store, nil, nil)
	executor := NewClaudeExecutor(&config.Config{})
	entered, release := make(chan struct{}), make(chan struct{})
	executor.oauthProfileFetcher = func(context.Context, *coreauth.Auth, string) (*claudeauth.OAuthProfile, error) {
		close(entered)
		<-release
		profile := &claudeauth.OAuthProfile{}
		profile.Account.UUID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		profile.Account.Email = "first@example.com"
		return profile, nil
	}
	manager.RegisterExecutor(executor)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{ID: "shared.json", FileName: "shared.json", Provider: "claude", Attributes: map[string]string{"path": filepath.Join(dir, "shared.json")}, Metadata: map[string]any{"email": "first@example.com", "type": "claude", "access_token": "sk-ant-oat-<fixture-proxy-token>-first", "account_uuid": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { manager.BackfillAccountProfiles(context.Background()); close(done) }()
	<-entered
	auth.Metadata["email"], auth.Metadata["account_uuid"], auth.Metadata["access_token"] = "second@example.com", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "sk-ant-oat-<fixture-proxy-token>-second"
	if _, err = manager.Update(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	current, _ := manager.GetByID(auth.ID)
	if current.Metadata["email"] != "second@example.com" || current.Metadata["account_uuid"] != "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb" || accountmanagement.ReadProfile(current.Metadata, "claude").Status != "pending" {
		t.Fatal("old profile overwrote replacement account identity")
	}
}
