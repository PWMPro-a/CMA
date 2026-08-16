import { describe, expect, it } from 'vitest';
import type { SupplyAccountPoolSummary, SupplySmartResource } from '@/services/api';
import { resolveSupplyPoolAccountStats } from './poolAccountStats';

const resource = (values: Partial<SupplySmartResource>): SupplySmartResource =>
  values as SupplySmartResource;

const summary = (values: Partial<SupplyAccountPoolSummary>): SupplyAccountPoolSummary =>
  ({
    checkedAtMs: 1,
    total: 0,
    normal: 0,
    needsAttention: 0,
    quotaRisk: 0,
    disabled: 0,
    unconfirmed: 0,
    classificationObserved: true,
    ...values,
  }) as SupplyAccountPoolSummary;

describe('resolveSupplyPoolAccountStats', () => {
  it('keeps normal and at-risk counts exclusive inside the live schedulable pool', () => {
    expect(
      resolveSupplyPoolAccountStats(
        resource({
          availableAccounts: 7,
          schedulableAccounts: 13,
          healthyAccounts: 8,
          accountClassificationObserved: true,
          normalAccounts: 7,
          atRiskAccounts: 6,
          totalAccounts: 75,
          disabledAccounts: 62,
        }),
        undefined
      )
    ).toEqual({
      schedulable: 13,
      normal: 7,
      capacityHealthy: 8,
      needsAttention: undefined,
      quotaRisk: undefined,
      unconfirmed: undefined,
      atRisk: 6,
      total: 75,
      disabled: 62,
    });
  });

  it('prefers the explicit operator buckets for risk and enabled counts', () => {
    expect(
      resolveSupplyPoolAccountStats(
        resource({
          availableAccounts: 14,
          schedulableAccounts: 17,
          healthyAccounts: 14,
          enabledAccounts: 14,
          accountClassificationObserved: true,
          normalAccounts: 9,
          needsAttentionAccounts: 2,
          quotaRiskAccounts: 3,
          unconfirmedAccounts: 0,
          totalAccounts: 75,
        }),
        undefined
      )
    ).toEqual({
      schedulable: 17,
      normal: 9,
      capacityHealthy: 14,
      needsAttention: 2,
      quotaRisk: 3,
      unconfirmed: 0,
      atRisk: 5,
      total: 75,
      disabled: 61,
    });
  });

  it('uses an explicit normal bucket for an older manager response', () => {
    expect(
      resolveSupplyPoolAccountStats(
        resource({
          availableAccounts: 9,
          schedulableAccounts: 14,
          healthyAccounts: 9,
          normalAccounts: 6,
          totalAccounts: 75,
        }),
        undefined
      )
    ).toEqual({
      schedulable: 14,
      normal: 6,
      capacityHealthy: 9,
      needsAttention: undefined,
      quotaRisk: undefined,
      unconfirmed: undefined,
      atRisk: 8,
      total: 75,
      disabled: 61,
    });
  });

  it('does not display capacity health as normal before classification is observed', () => {
    expect(
      resolveSupplyPoolAccountStats(
        resource({
          availableAccounts: 14,
          schedulableAccounts: 14,
          healthyAccounts: 14,
          totalAccounts: 75,
          enabledAccounts: 14,
          disabledAccounts: 61,
        }),
        undefined
      )
    ).toMatchObject({
      schedulable: 14,
      normal: undefined,
      capacityHealthy: 14,
      atRisk: 14,
      total: 75,
      disabled: 61,
    });
  });

  it('does not fall back to capacity health when a classification snapshot omits the normal bucket', () => {
    expect(
      resolveSupplyPoolAccountStats(
        resource({
          availableAccounts: 14,
          schedulableAccounts: 23,
          healthyAccounts: 14,
          accountClassificationObserved: true,
          totalAccounts: 95,
          enabledAccounts: 23,
          disabledAccounts: 72,
        }),
        undefined
      )
    ).toMatchObject({
      schedulable: 23,
      normal: undefined,
      capacityHealthy: 14,
      atRisk: 23,
      total: 95,
      disabled: 72,
    });
  });

  it('does not treat unobserved zero-valued operator buckets as a classification', () => {
    expect(
      resolveSupplyPoolAccountStats(
        resource({
          availableAccounts: 6,
          schedulableAccounts: 23,
          healthyAccounts: 6,
          normalAccounts: 0,
          needsAttentionAccounts: 0,
          quotaRiskAccounts: 0,
          unconfirmedAccounts: 0,
          totalAccounts: 95,
          enabledAccounts: 23,
          disabledAccounts: 72,
        }),
        undefined
      )
    ).toMatchObject({
      normal: undefined,
      capacityHealthy: 6,
      needsAttention: undefined,
      quotaRisk: undefined,
      unconfirmed: undefined,
      disabled: 72,
      atRisk: 23,
    });
  });

  it('keeps the overview fallback for cold-start responses', () => {
    expect(resolveSupplyPoolAccountStats(undefined, 4)).toEqual({
      schedulable: 4,
      normal: undefined,
      capacityHealthy: undefined,
      needsAttention: undefined,
      quotaRisk: undefined,
      unconfirmed: undefined,
      atRisk: undefined,
      total: 4,
      disabled: 0,
    });
  });

  it('uses overlapping live availability and quota-risk counts from the shared summary', () => {
    expect(
      resolveSupplyPoolAccountStats(
        resource({
          schedulableAccounts: 26,
          accountClassificationObserved: true,
          normalAccounts: 1,
          needsAttentionAccounts: 2,
          quotaRiskAccounts: 15,
          unconfirmedAccounts: 8,
          totalAccounts: 46,
          disabledAccounts: 20,
        }),
        1,
        summary({
          total: 26,
          normal: 26,
          needsAttention: 0,
          quotaRisk: 15,
          disabled: 20,
          unconfirmed: 8,
        })
      )
    ).toMatchObject({
      schedulable: 26,
      normal: 26,
      needsAttention: 0,
      quotaRisk: 15,
      unconfirmed: 8,
      atRisk: 23,
      total: 26,
      disabled: 20,
    });
  });
});
