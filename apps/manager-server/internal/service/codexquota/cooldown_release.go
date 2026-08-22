package codexquota

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/cpaauthfiles"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

func (s *Service) releaseOwnedQuotaCooldown(
	ctx context.Context,
	setup store.Setup,
	file cpaauthfiles.File,
) error {
	if s == nil || s.quotaCooldowns == nil {
		return nil
	}
	active, err := s.quotaCooldowns.ListActive(ctx)
	if err != nil {
		return fmt.Errorf("list active quota cooldowns: %w", err)
	}
	matches := make([]model.QuotaCooldown, 0, 1)
	for _, cooldown := range active {
		if matchesOwnedCodexCooldown(cooldown, file) {
			matches = append(matches, cooldown)
		}
	}
	if len(matches) == 0 {
		return nil
	}
	if len(matches) > 1 {
		return fmt.Errorf("multiple active Codex quota cooldowns match auth index %q", file.AuthIndex)
	}
	if s.authStatuses == nil || s.authFileMutations == nil {
		return cpaauthfiles.ErrMutationCoordinatorUnavailable
	}

	cooldown := matches[0]
	releaseMutation, err := s.authFileMutations.Acquire(ctx, file.Name)
	if err != nil {
		return fmt.Errorf("coordinate quota cooldown release: %w", err)
	}
	defer releaseMutation()

	target, err := s.authStatuses.ResolveVerifiedStatusMutationTarget(
		ctx,
		setup.CPAUpstreamURL,
		setup.ManagementKey,
		cpaauthfiles.Identity{
			AuthFileName:      file.Name,
			RuntimeID:         file.ID,
			AuthIndex:         file.AuthIndex,
			Provider:          file.Provider,
			AccountSnapshot:   file.AccountSnapshot,
			AccountIDSnapshot: file.AccountID,
		},
	)
	if err != nil {
		return fmt.Errorf("resolve quota cooldown auth file: %w", err)
	}

	enabledByReset := target.File.Disabled && !cooldown.PreDisabledState
	if enabledByReset {
		if err := s.authStatuses.PatchDisabledTarget(
			ctx,
			setup.CPAUpstreamURL,
			setup.ManagementKey,
			target,
			false,
		); err != nil {
			return fmt.Errorf("enable auth file after Codex quota reset: %w", err)
		}
	}

	if err := s.quotaCooldowns.MarkRecovered(ctx, cooldown.ID, time.Now().UnixMilli()); err != nil {
		if enabledByReset {
			rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistenceTimeout)
			defer cancel()
			rollbackTarget, resolveErr := s.authStatuses.ResolveVerifiedStatusMutationTarget(
				rollbackCtx,
				setup.CPAUpstreamURL,
				setup.ManagementKey,
				cpaauthfiles.Identity{
					AuthFileName:      target.File.Name,
					RuntimeID:         target.File.ID,
					AuthIndex:         target.File.AuthIndex,
					Provider:          target.File.Provider,
					AccountSnapshot:   target.File.AccountSnapshot,
					AccountIDSnapshot: target.File.AccountID,
				},
			)
			if resolveErr == nil {
				resolveErr = s.authStatuses.PatchDisabledTarget(
					rollbackCtx,
					setup.CPAUpstreamURL,
					setup.ManagementKey,
					rollbackTarget,
					true,
				)
			}
			if resolveErr != nil {
				return fmt.Errorf("mark quota cooldown recovered: %w; rollback disable failed: %v", err, resolveErr)
			}
		}
		return fmt.Errorf("mark quota cooldown recovered: %w", err)
	}
	return nil
}

func matchesOwnedCodexCooldown(cooldown model.QuotaCooldown, file cpaauthfiles.File) bool {
	if cooldown.Owner != model.QuotaCooldownOwnerUsage429 {
		return false
	}
	if provider := strings.ToLower(strings.TrimSpace(cooldown.Provider)); provider != "" && provider != "codex" {
		return false
	}
	fileName := strings.TrimSpace(file.Name)
	runtimeID := strings.TrimSpace(file.ID)
	cooldownFileName := strings.TrimSpace(cooldown.AuthFileName)
	if cooldownFileName != fileName && cooldownFileName != runtimeID {
		return false
	}
	if authIndex := strings.TrimSpace(cooldown.AuthIndex); authIndex != "" {
		return authIndex == strings.TrimSpace(file.AuthIndex)
	}
	accountSnapshot := strings.TrimSpace(cooldown.AccountSnapshot)
	return accountSnapshot != "" && accountSnapshot == strings.TrimSpace(file.AccountSnapshot)
}
