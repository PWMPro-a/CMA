import { beforeEach, describe, expect, it, vi } from 'vitest';

const mocks = vi.hoisted(() => ({
  get: vi.fn(),
  post: vi.fn(),
}));

vi.mock('./client', () => ({
  apiClient: mocks,
}));

import { licenseApi } from './license';

describe('licenseApi paths', () => {
  beforeEach(() => {
    mocks.get.mockReset();
    mocks.post.mockReset();
  });

  it('uses paths relative to the management API base', () => {
    licenseApi.status();
    licenseApi.startShopAuthorization('http://cpa.test/license/shop/callback', 'http://cpa.test');
    licenseApi.exchangeShopCode('state', 'code');
    licenseApi.refresh();
    licenseApi.activate('activation-code');

    expect(mocks.get.mock.calls).toEqual([
      ['/license/status'],
      [
        '/license/shop/start',
        {
          params: {
            callback_url: 'http://cpa.test/license/shop/callback',
            origin: 'http://cpa.test',
          },
        },
      ],
    ]);
    expect(mocks.post.mock.calls).toEqual([
      ['/license/shop/exchange', { state: 'state', code: 'code' }],
      ['/license/refresh'],
      ['/license/activate', { code: 'activation-code' }],
    ]);
  });
});
