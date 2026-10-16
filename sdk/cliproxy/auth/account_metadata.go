package auth

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountmanagement"
	log "github.com/sirupsen/logrus"
)

type AccountProfileRefresher interface {
	AccountProfileNeedsBackfill(*Auth) bool
	FetchAccountProfile(context.Context, *Auth) (map[string]any, error)
}

// BackfillAccountProfiles enriches older credentials without rotating tokens or
// adding provider calls to inference. Normal token refresh owns later updates.
func (m *Manager) BackfillAccountProfiles(ctx context.Context) {
	type job struct {
		auth      *Auth
		refresher AccountProfileRefresher
	}
	var jobs []job
	m.mu.RLock()
	for _, auth := range m.auths {
		executor, _ := m.executorLocked(executorKeyFromAuth(auth))
		refresher, ok := executor.(AccountProfileRefresher)
		if ok && refresher.AccountProfileNeedsBackfill(auth) {
			jobs = append(jobs, job{auth.Clone(), refresher})
		}
	}
	m.mu.RUnlock()
	queue := make(chan job, len(jobs))
	for _, item := range jobs {
		queue <- item
	}
	close(queue)
	var workers sync.WaitGroup
	for i := 0; i < 2; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for item := range queue {
				if ctx.Err() != nil {
					return
				}
				values, err := item.refresher.FetchAccountProfile(ctx, item.auth)
				if err != nil {
					continue
				}
				_, err = m.updateAccountProfile(ctx, item.auth, values)
				if err != nil {
					log.WithField("auth_id", item.auth.ID).Warn("account profile backfill could not persist")
				}
			}
		}()
	}
	workers.Wait()
}

// UpdateAccountMetadata serializes account writes with token refresh. Storage
// runs outside the global lock; a failed save rolls back only its own values.
func (m *Manager) UpdateAccountMetadata(ctx context.Context, id string, epoch uint64, update func(map[string]any) error) (*Auth, error) {
	return m.updateAccountMetadata(ctx, id, epoch, nil, update)
}

func (m *Manager) updateAccountProfile(ctx context.Context, source *Auth, values map[string]any) (*Auth, error) {
	guard := func(current *Auth) error {
		if source.Provider != current.Provider || CredentialsChanged(source, current) {
			return fmt.Errorf("profile credential changed")
		}
		for _, key := range []string{"email", "account_uuid", "organization_uuid"} {
			if authMetadataString(source, key) != authMetadataString(current, key) {
				return fmt.Errorf("profile identity changed")
			}
		}
		return nil
	}
	return m.updateAccountMetadata(ctx, source.ID, source.RegistrationEpoch, guard, func(metadata map[string]any) error {
		current := accountmanagement.ReadProfile(metadata, "claude")
		observed := accountmanagement.ReadProfile(values, "claude")
		last, _ := time.Parse(time.RFC3339, current.LastCheckedAt)
		checked, _ := time.Parse(time.RFC3339, observed.LastCheckedAt)
		if checked.Before(last) {
			return fmt.Errorf("profile observation superseded")
		}
		for key, value := range values {
			if key != accountmanagement.ProfileKey {
				metadata[key] = value
			}
		}
		return accountmanagement.Put(metadata, accountmanagement.ProfileKey, accountmanagement.MergeProfile(current, observed))
	})
}

func (m *Manager) updateAccountMetadata(ctx context.Context, id string, epoch uint64, guard func(*Auth) error, update func(map[string]any) error) (*Auth, error) {
	lockValue, _ := m.refreshLocks.LoadOrStore(id, &authRefreshLock{})
	lock := lockValue.(*authRefreshLock)
	lock.mu.Lock()
	defer lock.mu.Unlock()
	m.mu.Lock()
	current := m.auths[id]
	if current == nil || current.RegistrationEpoch != epoch {
		m.mu.Unlock()
		return nil, fmt.Errorf("credential changed during account update")
	}
	if m.store == nil || IsConfigAPIKeyAuth(current) || IsPluginVirtualAuth(current) || current.Attributes["runtime_only"] == "true" {
		m.mu.Unlock()
		return nil, fmt.Errorf("credential has no durable account store")
	}
	if guard != nil {
		if err := guard(current); err != nil {
			m.mu.Unlock()
			return nil, err
		}
	}
	before := current.Clone()
	updated := current.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	if err := update(updated.Metadata); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	updated.Generation++
	updated.UpdatedAt = time.Now()
	m.auths[id] = updated.Clone()
	m.mu.Unlock()
	if err := m.persist(ctx, updated); err != nil {
		m.rollbackAccountMetadata(ctx, before, updated)
		return nil, err
	}
	currentResult, ok := m.GetByID(id)
	if !ok || currentResult.RegistrationEpoch != epoch {
		return nil, fmt.Errorf("credential changed during account update")
	}
	m.hook.OnAuthUpdated(ctx, currentResult.Clone())
	return currentResult, nil
}

func (m *Manager) rollbackAccountMetadata(ctx context.Context, before, candidate *Auth) {
	m.mu.Lock()
	current := m.auths[candidate.ID]
	if current == nil || current.RegistrationEpoch != candidate.RegistrationEpoch {
		m.mu.Unlock()
		return
	}
	restored := current.Clone()
	keys := make(map[string]struct{}, len(before.Metadata)+len(candidate.Metadata))
	for key := range before.Metadata {
		keys[key] = struct{}{}
	}
	for key := range candidate.Metadata {
		keys[key] = struct{}{}
	}
	changed := false
	for key := range keys {
		old, hadOld := before.Metadata[key]
		value, hadValue := candidate.Metadata[key]
		if hadOld == hadValue && reflect.DeepEqual(old, value) {
			continue
		}
		present, hasPresent := current.Metadata[key]
		// Inference results and unrelated updates advance the generation too.
		// Revert our values only while no later writer has replaced those keys.
		if hasPresent != hadValue || !reflect.DeepEqual(present, value) {
			continue
		}
		if hadOld {
			if restored.Metadata == nil {
				restored.Metadata = make(map[string]any)
			}
			restored.Metadata[key] = old
		} else {
			delete(restored.Metadata, key)
		}
		changed = true
	}
	if !changed {
		m.mu.Unlock()
		return
	}
	restored.Generation++
	m.auths[candidate.ID] = restored
	m.mu.Unlock()
	if err := m.persist(ctx, restored); err != nil {
		log.WithField("auth_id", candidate.ID).Warn("account metadata rollback could not persist")
	}
}
