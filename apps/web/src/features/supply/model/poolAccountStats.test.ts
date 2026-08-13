import { describe, expect, it } from 'vitest';
import type { SupplySmartResource } from '@/services/api';
import { resolveSupplyPoolAccountStats } from './poolAccountStats';

const resource = (values: Partial<SupplySmartResource>): SupplySmartResource =>
  values as SupplySmartResource;

describe('resolveSupplyPoolAccountStats', () => {
  it('keeps healthy and at-risk counts exclusive inside the live schedulable pool', () => {
    expect(
      resolveSupplyPoolAccountStats(
        resource({
          availableAccounts: 7,
          schedulableAccounts: 13,
          healthyAccounts: 8,
          normalAccounts: 7,
          atRiskAccounts: 6,
          totalAccounts: 75,
          disabledAccounts: 62,
        }),
        undefined
      )
    ).toEqual({ schedulable: 13, healthy: 7, atRisk: 6, total: 75, disabled: 62 });
  });

  it('derives at-risk and disabled counts for an older manager response', () => {
    expect(
      resolveSupplyPoolAccountStats(
        resource({
          availableAccounts: 9,
          schedulableAccounts: 14,
          healthyAccounts: 9,
          totalAccounts: 75,
        }),
        undefined
      )
    ).toEqual({ schedulable: 14, healthy: 9, atRisk: 5, total: 75, disabled: 61 });
  });

  it('keeps the overview fallback for cold-start responses', () => {
    expect(resolveSupplyPoolAccountStats(undefined, 4)).toEqual({
      schedulable: 4,
      healthy: undefined,
      atRisk: undefined,
      total: 4,
      disabled: 0,
    });
  });
});
