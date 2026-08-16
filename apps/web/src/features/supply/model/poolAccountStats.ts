import type { SupplyAccountPoolSummary, SupplySmartResource } from '@/services/api';

export interface SupplyPoolAccountStats {
  schedulable: number | undefined;
  // `normal` is live scheduling availability. Quota risk is an overlapping
  // warning, so a low-quota credential can be both normal and at risk.
  normal: number | undefined;
  capacityHealthy: number | undefined;
  needsAttention: number | undefined;
  quotaRisk: number | undefined;
  unconfirmed: number | undefined;
  atRisk: number | undefined;
  total: number | undefined;
  disabled: number | undefined;
}

const finiteNonNegative = (value: number | undefined): number | undefined =>
  typeof value === 'number' && Number.isFinite(value) ? Math.max(0, value) : undefined;

// Account-pool cards describe the live CPA pool. Capacity planning has a
// separate, stricter count (availableAccounts), which remains visible in the
// capacity widgets and continues to drive replenishment decisions.
export const resolveSupplyPoolAccountStats = (
  resource: SupplySmartResource | undefined,
  fallbackAvailable: number | undefined,
  summary?: SupplyAccountPoolSummary
): SupplyPoolAccountStats => {
  const available = finiteNonNegative(resource?.availableAccounts ?? fallbackAvailable);
  const summaryObserved = summary?.classificationObserved === true;
  const schedulable =
    (summaryObserved ? finiteNonNegative(summary.normal) : undefined) ??
    finiteNonNegative(resource?.schedulableAccounts) ??
    available;
  const classificationObserved =
    summaryObserved || resource?.accountClassificationObserved === true;
  const legacyNormal = finiteNonNegative(resource?.normalAccounts);
  const capacityHealthy = finiteNonNegative(resource?.healthyAccounts);
  const normal = summaryObserved
    ? finiteNonNegative(summary.normal)
    : classificationObserved
      ? legacyNormal
      : // `healthyAccounts` is an inspection-backed capacity count. It can be
        // larger than the credential page's normal bucket (for example, a
        // recently cooling or low-quota credential can still contribute usable
        // capacity). Never present that planning value as a normal account when
        // the matching classification snapshot is absent.
        // A zero bucket is what the manager emits while it has no matching
        // inspection evidence. Treat it as unknown rather than rendering
        // `0 normal` or falling back to the capacity-planning count.
        legacyNormal !== undefined && legacyNormal > 0
        ? legacyNormal
        : undefined;
  const needsAttention = summaryObserved
    ? finiteNonNegative(summary.needsAttention)
    : classificationObserved
      ? finiteNonNegative(resource?.needsAttentionAccounts)
      : undefined;
  const quotaRisk = summaryObserved
    ? finiteNonNegative(summary.quotaRisk)
    : classificationObserved
      ? finiteNonNegative(resource?.quotaRiskAccounts)
      : undefined;
  const unconfirmed = summaryObserved
    ? finiteNonNegative(summary.unconfirmed)
    : classificationObserved
      ? finiteNonNegative(resource?.unconfirmedAccounts)
      : undefined;
  const explicitAtRisk =
    needsAttention !== undefined && quotaRisk !== undefined && unconfirmed !== undefined
      ? needsAttention + quotaRisk + unconfirmed
      : undefined;
  const atRisk =
    explicitAtRisk ??
    finiteNonNegative(resource?.atRiskAccounts) ??
    (resource && schedulable !== undefined
      ? normal !== undefined
        ? Math.max(0, schedulable - normal)
        : schedulable
      : undefined);
  const total =
    (summaryObserved ? finiteNonNegative(summary.total) : undefined) ??
    finiteNonNegative(resource?.totalAccounts) ??
    schedulable;
  const enabled = finiteNonNegative(resource?.enabledAccounts);
  const disabled =
    (summaryObserved ? finiteNonNegative(summary.disabled) : undefined) ??
    finiteNonNegative(resource?.disabledAccounts) ??
    (total !== undefined && enabled !== undefined
      ? Math.max(0, total - enabled)
      : total !== undefined && schedulable !== undefined
        ? Math.max(0, total - schedulable)
        : undefined);

  return {
    schedulable,
    normal,
    capacityHealthy,
    needsAttention,
    quotaRisk,
    unconfirmed,
    atRisk,
    total,
    disabled,
  };
};
