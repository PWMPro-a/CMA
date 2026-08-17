import { describe, expect, it } from 'vitest';
import { resolvePurchasePlatformLabel } from './purchasePlatform';

const platforms = [
  { id: 'legacy', name: 'Legacy supplier', type: 'legacy', product: 'oauth_7d' },
  { id: 'bugteam', name: 'BugTeam', type: 'bugteam', product: 'team_1h' },
];

describe('resolvePurchasePlatformLabel', () => {
  it('prefers the persisted supplier id', () => {
    expect(
      resolvePurchasePlatformLabel(
        { orderId: 'cus_new', supplierId: 'bugteam', product: 'team_1h' },
        platforms
      )
    ).toBe('BugTeam');
  });

  it('keeps an unknown persisted supplier id visible', () => {
    expect(
      resolvePurchasePlatformLabel({ orderId: '123', supplierId: 'manual-source' }, platforms)
    ).toBe('manual-source');
  });

  it('infers BugTeam for legacy customer order ids', () => {
    expect(
      resolvePurchasePlatformLabel({ orderId: 'cus_old', product: 'team_1h' }, platforms)
    ).toBe('BugTeam');
  });

  it('infers the only platform for a product', () => {
    expect(
      resolvePurchasePlatformLabel({ orderId: 'legacy-order', product: 'oauth_7d' }, [
        ...platforms,
        { ...platforms[0] },
      ])
    ).toBe('Legacy supplier');
  });

  it('returns a stable fallback when the source cannot be inferred', () => {
    expect(
      resolvePurchasePlatformLabel({ orderId: 'legacy-order', product: 'unknown' }, platforms)
    ).toBe('-');
  });
});
