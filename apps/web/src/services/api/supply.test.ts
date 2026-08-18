import { beforeEach, describe, expect, it, vi } from 'vitest';

const { post } = vi.hoisted(() => ({
  post: vi.fn(),
}));

vi.mock('./client', () => ({
  apiClient: { post },
}));

import { SUPPLY_REPLENISH_TIMEOUT_MS, supplyApi } from './supply';

describe('supplyApi', () => {
  beforeEach(() => {
    post.mockReset();
    post.mockResolvedValue({});
  });

  it('keeps manual replenishment alive for bulk CPA initialization', async () => {
    await supplyApi.replenish(10);

    expect(post).toHaveBeenCalledWith(
      '/supply/replenish',
      { quantity: 10 },
      { timeout: SUPPLY_REPLENISH_TIMEOUT_MS }
    );
    expect(SUPPLY_REPLENISH_TIMEOUT_MS).toBe(15 * 60 * 1000);
  });

  it('sends the explicitly selected supply platform', async () => {
    await supplyApi.replenish(3, ' bugteam ');

    expect(post).toHaveBeenCalledWith(
      '/supply/replenish',
      { quantity: 3, supplierId: 'bugteam' },
      { timeout: SUPPLY_REPLENISH_TIMEOUT_MS }
    );
  });
});
