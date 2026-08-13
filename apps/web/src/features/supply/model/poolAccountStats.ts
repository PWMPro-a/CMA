import type { SupplySmartResource } from '@/services/api';

export interface SupplyPoolAccountStats {
  schedulable: number | undefined;
  healthy: number | undefined;
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
  const healthy =
    finiteNonNegative(resource?.normalAccounts) ?? finiteNonNegative(resource?.healthyAccounts);
  const atRisk =
    finiteNonNegative(resource?.atRiskAccounts) ??
    (schedulable !== undefined && healthy !== undefined
      ? Math.max(0, schedulable - healthy)
      : undefined);
  const total = finiteNonNegative(resource?.totalAccounts) ?? schedulable;
  const disabled =
    finiteNonNegative(resource?.disabledAccounts) ??
    (total !== undefined && schedulable !== undefined ? Math.max(0, total - schedulable) : undefined);

  return { schedulable, healthy, atRisk, total, disabled };
};
