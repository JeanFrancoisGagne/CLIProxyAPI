package auth

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"
)

type accountMetadataStore struct {
	mu      sync.Mutex
	auth    *Auth
	fail    bool
	entered chan struct{}
	release chan struct{}
}

func (store *accountMetadataStore) List(context.Context) ([]*Auth, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return []*Auth{store.auth.Clone()}, nil
}
func (store *accountMetadataStore) Delete(context.Context, string) error { return nil }
func (store *accountMetadataStore) Save(_ context.Context, auth *Auth) (string, error) {
	if auth.Metadata["account_billing"] != nil && store.entered != nil {
		close(store.entered)
		<-store.release
		store.entered = nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.fail {
		return "", errors.New("disk unavailable")
	}
	store.auth = auth.Clone()
	return auth.ID, nil
}

type accountMetadataRefreshExecutor struct{ countingRefreshExecutor }

func (executor *accountMetadataRefreshExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	auth.Metadata["access_token"] = "<fixture-proxy-token>-rotated"
	auth.Metadata["expired"] = time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	return auth, nil
}

func TestAccountMetadataPersistsAlongsideConcurrentRefresh(t *testing.T) {
	store := &accountMetadataStore{}
	manager := NewManager(store, nil, nil)
	manager.RegisterExecutor(&accountMetadataRefreshExecutor{countingRefreshExecutor: countingRefreshExecutor{id: "claude"}})
	auth, err := manager.Register(context.Background(), &Auth{ID: "account", Provider: "claude", Metadata: map[string]any{"access_token": "<fixture-proxy-token>", "refresh_token": "<fixture-proxy-token>", "priority": 7.0}})
	if err != nil {
		t.Fatal(err)
	}
	store.entered, store.release = make(chan struct{}), make(chan struct{})
	updated := make(chan error, 1)
	go func() {
		_, err := manager.UpdateAccountMetadata(context.Background(), auth.ID, auth.RegistrationEpoch, func(metadata map[string]any) error {
			metadata["account_billing"] = map[string]any{"status": "ok"}
			return nil
		})
		updated <- err
	}()
	<-store.entered
	refreshed := make(chan error, 1)
	go func() { _, err := manager.ForceRefreshAuth(context.Background(), auth.ID); refreshed <- err }()
	close(store.release)
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	if err := <-refreshed; err != nil {
		t.Fatal(err)
	}
	saved, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if saved[0].Metadata["access_token"] != "<fixture-proxy-token>-rotated" || saved[0].Metadata["account_billing"] == nil || saved[0].Metadata["priority"] != 7.0 {
		t.Fatal("account update lost billing, priority, or token rotation")
	}
}

func TestAccountMetadataSaveFailureDoesNotPublish(t *testing.T) {
	store := &accountMetadataStore{}
	manager := NewManager(store, nil, nil)
	auth, err := manager.Register(context.Background(), &Auth{ID: "account", Provider: "claude", Metadata: map[string]any{"access_token": "<fixture-proxy-token>"}})
	if err != nil {
		t.Fatal(err)
	}
	store.fail = true
	_, err = manager.UpdateAccountMetadata(context.Background(), auth.ID, auth.RegistrationEpoch, func(metadata map[string]any) error {
		metadata["account_billing"] = map[string]any{"status": "ok"}
		return nil
	})
	if err == nil {
		t.Fatal("failed save reported success")
	}
	current, _ := manager.GetByID(auth.ID)
	if current.Metadata["account_billing"] != nil {
		t.Fatal("failed save published unpersisted account data")
	}
}

func TestAccountMetadataDoesNotBlockReadersDuringSave(t *testing.T) {
	store := &accountMetadataStore{}
	manager := NewManager(store, nil, nil)
	auth, err := manager.Register(context.Background(), &Auth{ID: "account", Provider: "claude", Metadata: map[string]any{"access_token": "<fixture-proxy-token>"}})
	if err != nil {
		t.Fatal(err)
	}
	store.entered, store.release = make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := manager.UpdateAccountMetadata(context.Background(), auth.ID, auth.RegistrationEpoch, func(metadata map[string]any) error {
			metadata["account_billing"] = map[string]any{"status": "ok"}
			return nil
		})
		done <- err
	}()
	<-store.entered
	read := make(chan struct{})
	go func() { manager.GetByID(auth.ID); close(read) }()
	blocked := false
	select {
	case <-read:
	case <-time.After(time.Second):
		blocked = true
	}
	close(store.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if blocked {
		t.Fatal("account persistence blocked unrelated credential reads")
	}
}

func TestAccountMetadataFailedSavePreservesConcurrentUpdate(t *testing.T) {
	store := &accountMetadataStore{}
	manager := NewManager(store, nil, nil)
	auth, err := manager.Register(context.Background(), &Auth{ID: "account", Provider: "claude", Metadata: map[string]any{
		"access_token":    "<fixture-proxy-token>",
		"priority":        1.0,
		"account_billing": map[string]any{"status": "empty"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	store.fail = true
	store.entered, store.release = make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := manager.UpdateAccountMetadata(context.Background(), auth.ID, auth.RegistrationEpoch, func(metadata map[string]any) error {
			metadata["account_billing"] = map[string]any{"status": "ok"}
			metadata["account_profile"] = map[string]any{"full_name": "Pending profile"}
			return nil
		})
		done <- err
	}()
	<-store.entered
	concurrent, _ := manager.GetByID(auth.ID)
	concurrent.Metadata["priority"] = 9.0
	concurrent.Metadata["account_profile"] = map[string]any{"full_name": "Newer profile"}
	updated := make(chan error, 1)
	go func() {
		_, err := manager.Update(WithSkipPersist(context.Background()), concurrent)
		updated <- err
	}()
	// Update publishes before waiting for the same per-auth persistence lock.
	// Wait for that public state so the failed save races with a newer generation.
	deadline := time.Now().Add(time.Second)
	published := false
	for time.Now().Before(deadline) {
		current, _ := manager.GetByID(auth.ID)
		if current.Metadata["priority"] == 9.0 {
			published = true
			break
		}
		runtime.Gosched()
	}
	close(store.release)
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("failed save reported success")
	}
	if !published {
		t.Fatal("concurrent update did not publish before the save completed")
	}
	current, _ := manager.GetByID(auth.ID)
	if current.Metadata["priority"] != 9.0 {
		t.Fatal("rollback discarded the concurrent priority change")
	}
	if current.Metadata["account_billing"].(map[string]any)["status"] != "empty" {
		t.Fatal("failed save kept unpersisted billing after a concurrent generation advance")
	}
	if current.Metadata["account_profile"].(map[string]any)["full_name"] != "Newer profile" {
		t.Fatal("rollback discarded a newer value from another writer")
	}
}
