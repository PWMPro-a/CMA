import { beforeEach, describe, expect, it, vi } from 'vitest';

const mocks = vi.hoisted(() => ({
  get: vi.fn(),
  post: vi.fn(),
}));

vi.mock('./client', () => ({
  apiClient: mocks,
}));

import {
  getLicenseErrorCode,
  getLicenseErrorMessage,
  licenseApi,
  normalizeActivationCode,
} from './license';

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

  it('normalizes copied activation codes before sending them', () => {
    expect(normalizeActivationCode('  ```text\nabc\\_def\\-ghi\n```  ')).toBe('abc_def-ghi');
    licenseApi.activate('  abc\\_def\n');
    expect(mocks.post).toHaveBeenLastCalledWith('/license/activate', { code: 'abc_def' });
  });

  it('extracts stable provider codes and preserves human-readable messages', () => {
    const error = Object.assign(new Error('授权操作未完成'), {
      details: { code: 'authorization_invalid', error: '授权链接已使用或与当前 CPA 不匹配' },
    });
    expect(getLicenseErrorCode(error)).toBe('authorization_invalid');
    expect(getLicenseErrorMessage(error)).toBe('授权链接已使用或与当前 CPA 不匹配');
    expect(getLicenseErrorCode('provider_rejected')).toBe('provider_rejected');
  });
});
