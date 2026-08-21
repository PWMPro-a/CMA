package quotasnapshot_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
	quotasnapshot "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/quotasnapshot"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

func TestListCandidatesKeepsLatestEvidenceForEveryActiveWindow(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "usage.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const (
		windowCount = 251
		historySize = 8
	)
	scopeFingerprint := quotasnapshot.ScopeFingerprint("all", "", nil)
	writes := make([]model.AccountQuotaObservationWrite, 0, windowCount*historySize)
	for windowIndex := 0; windowIndex < windowCount; windowIndex++ {
		providerWindowID := fmt.Sprintf("window-%03d", windowIndex)
		for historyIndex := 0; historyIndex < historySize; historyIndex++ {
			observedAtMS := int64(windowIndex*100 + historyIndex + 1)
			observationHash := fmt.Sprintf("observation-%03d-%d", windowIndex, historyIndex)
			writes = append(writes, model.AccountQuotaObservationWrite{
				Observation: model.AccountQuotaObservation{
					ObservationHash:     observationHash,
					AccountKey:          "account-1",
					Provider:            "antigravity",
					Source:              "api_query",
					SourceObservationID: observationHash,
					InventoryScopeKey:   "antigravity:quota-windows",
					InventoryMode:       "partial",
					ObservedAtMS:        observedAtMS,
					WindowCount:         1,
					CreatedAtMS:         observedAtMS,
				},
				Snapshots: []model.AccountQuotaSnapshot{{
					AccountKey:          "account-1",
					Provider:            "antigravity",
					ProviderWindowID:    providerWindowID,
					WindowKind:          "model_quota",
					WindowMode:          "unknown",
					ModelScopeKind:      "all",
					ScopeFingerprint:    scopeFingerprint,
					ContentHash:         fmt.Sprintf("content-%03d-%d", windowIndex, historyIndex),
					InventoryScopeKey:   "antigravity:quota-windows",
					Source:              "api_query",
					SourceObservationID: observationHash,
					ObservedAtMS:        observedAtMS,
					BoundaryAccuracy:    "unknown",
					CreatedAtMS:         observedAtMS,
				}},
			})
		}
	}
	if err := st.QuotaSnapshots.InsertObservationWrites(context.Background(), writes); err != nil {
		t.Fatalf("insert quota evidence: %v", err)
	}

	candidates, err := st.QuotaSnapshots.ListCandidates(
		context.Background(),
		"account-1",
		"antigravity",
		2000,
	)
	if err != nil {
		t.Fatalf("list quota candidates: %v", err)
	}
	windows := make(map[string]struct{}, windowCount)
	for _, candidate := range candidates {
		windows[candidate.ProviderWindowID] = struct{}{}
	}
	if len(windows) != windowCount {
		t.Fatalf("candidate windows = %d, want %d", len(windows), windowCount)
	}
}

func TestInactiveContainerClearsChildRelationship(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "usage.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const (
		accountKey        = "account-relationship-clear"
		provider          = "codex"
		inventoryScopeKey = "codex:quota-windows"
	)
	scopeFingerprint := quotasnapshot.ScopeFingerprint("all", "", nil)
	buildSnapshot := func(providerWindowID, windowKind string, observedAtMS int64) model.AccountQuotaSnapshot {
		return model.AccountQuotaSnapshot{
			AccountKey:          accountKey,
			Provider:            provider,
			ProviderWindowID:    providerWindowID,
			WindowKind:          windowKind,
			WindowMode:          "unknown",
			ModelScopeKind:      "all",
			ScopeFingerprint:    scopeFingerprint,
			ContentHash:         fmt.Sprintf("%s-%d", providerWindowID, observedAtMS),
			InventoryScopeKey:   inventoryScopeKey,
			Source:              "api_query",
			SourceObservationID: fmt.Sprintf("observation-%d", observedAtMS),
			ObservedAtMS:        observedAtMS,
			BoundaryAccuracy:    "unknown",
			CreatedAtMS:         observedAtMS,
		}
	}
	buildWrite := func(observedAtMS int64, includeWeekly bool) model.AccountQuotaObservationWrite {
		snapshots := []model.AccountQuotaSnapshot{
			buildSnapshot("five-hour", "five_hour", observedAtMS),
		}
		if includeWeekly {
			snapshots = append(snapshots, buildSnapshot("weekly", "weekly", observedAtMS))
		}
		return model.AccountQuotaObservationWrite{
			Observation: model.AccountQuotaObservation{
				ObservationHash:     fmt.Sprintf("observation-%d", observedAtMS),
				AccountKey:          accountKey,
				Provider:            provider,
				Source:              "api_query",
				SourceObservationID: fmt.Sprintf("observation-%d", observedAtMS),
				InventoryScopeKey:   inventoryScopeKey,
				InventoryMode:       "complete",
				ObservedAtMS:        observedAtMS,
				WindowCount:         len(snapshots),
				CreatedAtMS:         observedAtMS,
			},
			Snapshots: snapshots,
		}
	}

	writes := []model.AccountQuotaObservationWrite{
		buildWrite(1_000, true),
		buildWrite(2_000, false),
		buildWrite(3_000, false),
	}
	if err := st.QuotaSnapshots.InsertObservationWrites(context.Background(), writes); err != nil {
		t.Fatalf("insert quota observations: %v", err)
	}

	states, err := st.QuotaSnapshots.ListWindowStates(context.Background(), accountKey, provider)
	if err != nil {
		t.Fatalf("list window states: %v", err)
	}
	byID := make(map[string]model.AccountQuotaWindowState, len(states))
	for _, state := range states {
		byID[state.ProviderWindowID] = state
	}
	if weekly := byID["weekly"]; weekly.Availability != "inactive" {
		t.Fatalf("weekly availability = %q, want inactive", weekly.Availability)
	}
	if child := byID["five-hour"]; child.RelationshipKind != "" || child.ContainerProviderWindowID != "" {
		t.Fatalf(
			"five-hour relationship = %q/%q, want cleared",
			child.RelationshipKind,
			child.ContainerProviderWindowID,
		)
	}
}
