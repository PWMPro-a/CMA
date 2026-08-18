import type {
  SupplyAccountPoolPlanSummary,
  SupplyQuotaEstimationPolicy,
  SupplyQuotaPlanEstimate,
} from '../../../services/api/supply';

const normalizePlanKey = (supplierId: string | undefined, planType: string) => {
  const supplier = supplierId?.trim().toLowerCase() || 'unassigned';
  return `${supplier}:${planType.trim().toLowerCase() || 'unknown'}`;
};

export const reconcileLiveQuotaPlanAccounts = (
  estimates: SupplyQuotaPlanEstimate[],
  livePlans: SupplyAccountPoolPlanSummary[],
  resolvePolicy: (supplierId: string | undefined, planType: string) => SupplyQuotaEstimationPolicy
): SupplyQuotaPlanEstimate[] => {
  const remaining = new Map(
    livePlans.map((plan) => [
      normalizePlanKey(plan.supplierId, plan.planType),
      {
        ...plan,
        planType: plan.planType.trim().toLowerCase() || 'unknown',
      },
    ])
  );
  const reconciled = estimates.map((estimate) => {
    const key = normalizePlanKey(estimate.supplierId, estimate.planType);
    const live = remaining.get(key);
    if (!live) return estimate;
    remaining.delete(key);
    return {
      ...estimate,
      supplierName: live.supplierName || estimate.supplierName,
      accountCount: live.accountCount,
    };
  });

  remaining.forEach((live, key) => {
    const policy = resolvePolicy(live.supplierId, live.planType);
    const fixed = policy.mode === 'fixed';
    reconciled.push({
      key: live.key || key,
      supplierId: live.supplierId,
      supplierName: live.supplierName,
      planType: live.planType,
      mode: policy.mode,
      accountCount: live.accountCount,
      fallbackM: policy.fallbackM,
      fixedM: policy.fixedM,
      adoptedM: fixed ? policy.fixedM : policy.fallbackM,
      source: 'live_pool',
      sampleCount: 0,
      uniqueAccounts: 0,
      minimumUniqueAccounts: 3,
      validationState: fixed ? 'fixed' : 'insufficient',
      usingFallback: !fixed,
    });
  });
  return reconciled;
};
