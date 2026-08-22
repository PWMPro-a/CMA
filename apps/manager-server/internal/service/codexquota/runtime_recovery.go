package codexquota

import (
	"context"
	"fmt"
	"strings"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/cpaauthfiles"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

// recoverRuntimeQuotaPreempt clears a native CPA quota freeze after a fresh
// upstream quota check has confirmed recovery. CPA versions have emitted both
// quota_preempt and usage_limit_reached for this state. It deliberately
// requires the runtime reason and zero concurrency so manual disables and live
// requests are left untouched.
func (s *Service) recoverRuntimeQuotaPreempt(ctx context.Context, setup store.Setup, file cpaauthfiles.File) error {
	if s == nil || !file.Disabled || !runtimeQuotaPreempted(file.Raw) || !currentRequestCountZero(file.Raw) {
		return nil
	}
	if s.authStatuses == nil || s.authFileMutations == nil {
		return cpaauthfiles.ErrMutationCoordinatorUnavailable
	}
	release, err := s.authFileMutations.Acquire(ctx, file.Name)
	if err != nil {
		return fmt.Errorf("coordinate quota_preempt recovery: %w", err)
	}
	defer release()
	target, err := s.authStatuses.ResolveVerifiedStatusMutationTarget(ctx, setup.CPAUpstreamURL, setup.ManagementKey, cpaauthfiles.Identity{
		AuthFileName:      file.Name,
		RuntimeID:         file.ID,
		AuthIndex:         file.AuthIndex,
		Provider:          file.Provider,
		AccountSnapshot:   file.AccountSnapshot,
		AccountIDSnapshot: file.AccountID,
	})
	if err != nil {
		return fmt.Errorf("resolve quota_preempt recovery target: %w", err)
	}
	if !target.File.Disabled || !runtimeQuotaPreempted(target.File.Raw) || !currentRequestCountZero(target.File.Raw) {
		return nil
	}
	if err := s.authStatuses.PatchDisabledTarget(ctx, setup.CPAUpstreamURL, setup.ManagementKey, target, false); err != nil {
		return fmt.Errorf("enable recovered quota_preempt auth file: %w", err)
	}
	return nil
}

func runtimeQuotaPreempted(raw map[string]any) bool {
	for _, key := range []string{"runtime_last_skip_reason", "runtimeLastSkipReason"} {
		value, ok := raw[key]
		if !ok {
			continue
		}
		reason := strings.ToLower(strings.TrimSpace(fmt.Sprint(value)))
		reason = strings.NewReplacer("-", "_", " ", "_").Replace(reason)
		switch reason {
		case "quota_preempt", "usage_limit_reached", "codex_usage_limit_reached":
			return true
		default:
			return false
		}
	}
	return false
}
