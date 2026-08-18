import { describe, expect, it } from 'vitest';

import type {
  SupplyAccountPoolPlanSummary,
  SupplyQuotaPlanEstimate,
} from '../../../services/api/supply';
import { reconcileLiveQuotaPlanAccounts } from './quotaPlanEstimates';

const estimate = (planType: string, accountCount: number): SupplyQuotaPlanEstimate => ({
  key: `legacy:${planType}`,
  supplierId: 'legacy',
  supplierName: 'Legacy supplier',
  planType,
  mode: 'auto',
  accountCount,
  fallbackM: 10,
  adoptedM: 10,
  source: 'inspection',
  sampleCount: 0,
  uniqueAccounts: 0,
});

describe('reconcileLiveQuotaPlanAccounts', () => {
  it('keeps capacity estimates but replaces their stale counts and publishes unclassified accounts', () => {
    const inspected = [estimate('free', 0), estimate('plus', 11), estimate('team', 3)];
    const live: SupplyAccountPoolPlanSummary[] = [
      { key: 'legacy:plus', supplierId: 'legacy', planType: 'plus', accountCount: 11 },
      { key: 'legacy:team', supplierId: 'legacy', planType: 'team', accountCount: 3 },
      { key: 'legacy:unknown', supplierId: 'legacy', planType: 'unknown', accountCount: 7 },
    ];
    const result = reconcileLiveQuotaPlanAccounts(inspected, live, () => ({
      mode: 'auto',
      fallbackM: 10,
      fixedM: 10,
    }));

    expect(result.map((item) => [item.planType, item.accountCount])).toEqual([
      ['free', 0],
      ['plus', 11],
      ['team', 3],
      ['unknown', 7],
    ]);
    expect(result.reduce((total, item) => total + item.accountCount, 0)).toBe(21);
    expect(result[3]).toMatchObject({
      source: 'live_pool',
      validationState: 'insufficient',
      usingFallback: true,
    });
  });

  it('uses the live Team count instead of the stale inspection count', () => {
    const result = reconcileLiveQuotaPlanAccounts(
      [estimate('plus', 11), estimate('team', 3)],
      [
        { key: 'legacy:plus', supplierId: 'legacy', planType: 'plus', accountCount: 11 },
        { key: 'legacy:team', supplierId: 'legacy', planType: 'team', accountCount: 10 },
      ],
      () => ({ mode: 'auto', fallbackM: 10, fixedM: 10 })
    );
    expect(result.find((item) => item.planType === 'team')?.accountCount).toBe(10);
  });
});
