import type { SupplySmartResource } from '@/services/api';

export interface SupplyPoolAccountStats {
  schedulable: number | undefined;
  healthy: number | undefined;
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
  fallbackAvailable: number | undefined
): SupplyPoolAccountStats => {
  const available = finiteNonNegative(resource?.availableAccounts ?? fallbackAvailable);
  const schedulable = finiteNonNegative(resource?.schedulableAccounts) ?? available;
  const classificationObserved = resource?.accountClassificationObserved === true;
  const legacyNormal = finiteNonNegative(resource?.normalAccounts);
  const healthy = classificationObserved
    ? finiteNonNegative(resource?.normalAccounts) ?? finiteNonNegative(resource?.healthyAccounts)
    // `healthyAccounts` is an inspection-backed capacity count. It can be
    // larger than the credential page's normal bucket (for example, a
    // recently cooling or low-quota credential can still contribute usable
    // capacity). Never present that planning value as a normal account when
    // the matching classification snapshot is absent.
    // A zero bucket is what the manager emits while it has no matching
    // inspection evidence. Treat it as unknown rather than rendering
    // `0 normal` or falling back to the capacity-planning count.
    : legacyNormal !== undefined && legacyNormal > 0
      ? legacyNormal
      : undefined;
  const needsAttention = classificationObserved
    ? finiteNonNegative(resource?.needsAttentionAccounts)
    : undefined;
  const quotaRisk = classificationObserved ? finiteNonNegative(resource?.quotaRiskAccounts) : undefined;
  const unconfirmed = classificationObserved ? finiteNonNegative(resource?.unconfirmedAccounts) : undefined;
  const explicitAtRisk =
    needsAttention !== undefined && quotaRisk !== undefined && unconfirmed !== undefined
      ? needsAttention + quotaRisk + unconfirmed
      : undefined;
  const atRisk =
    explicitAtRisk ??
    finiteNonNegative(resource?.atRiskAccounts) ??
    (resource && schedulable !== undefined
      ? healthy !== undefined
        ? Math.max(0, schedulable - healthy)
        : schedulable
      : undefined);
  const total = finiteNonNegative(resource?.totalAccounts) ?? schedulable;
  const enabled = finiteNonNegative(resource?.enabledAccounts);
  const disabled =
    finiteNonNegative(resource?.disabledAccounts) ??
    (total !== undefined && enabled !== undefined
      ? Math.max(0, total - enabled)
      : total !== undefined && schedulable !== undefined
        ? Math.max(0, total - schedulable)
        : undefined);

  return { schedulable, healthy, needsAttention, quotaRisk, unconfirmed, atRisk, total, disabled };
};
