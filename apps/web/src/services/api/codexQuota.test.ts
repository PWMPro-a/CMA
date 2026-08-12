import { beforeEach, describe, expect, it, vi } from 'vitest';

const { mocks } = vi.hoisted(() => ({
  mocks: {
    request: vi.fn(),
    post: vi.fn(),
  },
}));

vi.mock('@/stores/useAuthStore', () => ({
  useAuthStore: {
    getState: () => ({
      apiBase: 'http://manager.local:18317',
      managementKey: 'manager-key',
      sessionMode: 'manager_embedded',
    }),
  },
}));

vi.mock('@/stores/useUsageServiceStore', () => ({
  useUsageServiceStore: {
    getState: () => ({
      enabled: true,
      serviceBase: 'http://manager.local:18317',
    }),
  },
}));

vi.mock('./usageService', () => ({
  usageServiceApi: {
    resetCodexQuota: mocks.post,
  },
}));

vi.mock('./apiCall', () => ({
  apiCallApi: {
    request: mocks.request,
  },
  getApiCallErrorMessage: (result: { statusCode: number; bodyText?: string }) =>
    `${result.statusCode} ${result.bodyText ?? ''}`.trim(),
}));

import { buildCodexUsageRequestHeaders, requestCodexQuotaReset } from './codexQuota';

beforeEach(() => {
  mocks.request.mockReset();
  mocks.post.mockReset();
});

describe('buildCodexUsageRequestHeaders', () => {
  it('does not include Chatgpt-Account-Id when account id is missing', () => {
    const headers = buildCodexUsageRequestHeaders(null);

    expect(headers).not.toHaveProperty('Chatgpt-Account-Id');
    expect(headers.Authorization).toBe('Bearer $TOKEN$');
  });

  it('includes trimmed account id when available', () => {
    const headers = buildCodexUsageRequestHeaders(' account-123 ');

    expect(headers['Chatgpt-Account-Id']).toBe('account-123');
  });

  it('allows Codex inspection to override User-Agent', () => {
    const headers = buildCodexUsageRequestHeaders('account-123', {
      userAgent: 'codex-test-agent',
    });

    expect(headers['User-Agent']).toBe('codex-test-agent');
  });
});

describe('requestCodexQuotaReset', () => {
  it('posts an idempotent reset operation to the CPAM controller', async () => {
    mocks.post.mockResolvedValue({
      operation_id: 'operation-1',
      account_key: 'codex:account-id:acct-1',
      auth_index: 'auth-1',
      state: 'completed',
      consumed: true,
      warning_codes: [],
    });

    await requestCodexQuotaReset({
      name: 'codex-auth.json',
      type: 'codex',
      authIndex: ' auth-1 ',
      id_token: { account_id: 'acct-1' },
    });

    expect(mocks.post).toHaveBeenCalledTimes(1);
    expect(mocks.post).toHaveBeenCalledWith(
      'http://manager.local:18317',
      'manager-key',
      'auth-1',
      expect.any(String)
    );
  });

  it('reuses the same operation id when the first request times out', async () => {
    mocks.post
      .mockRejectedValueOnce(Object.assign(new Error('timeout'), { code: 'ECONNABORTED' }))
      .mockResolvedValueOnce({
        operation_id: 'operation-1',
        account_key: 'codex:account-id:acct-1',
        auth_index: 'auth-1',
        state: 'completed',
        consumed: true,
        warning_codes: [],
      });

    await requestCodexQuotaReset({
      name: 'codex-auth.json',
      type: 'codex',
      authIndex: 'auth-1',
      id_token: { account_id: 'acct-1' },
    });

    expect(mocks.post).toHaveBeenCalledTimes(2);
    expect(mocks.post.mock.calls[0][3]).toBe(mocks.post.mock.calls[1][3]);
  });

  it('does not retry a definitive HTTP failure', async () => {
    mocks.post.mockRejectedValueOnce(Object.assign(new Error('conflict'), { status: 409 }));

    await expect(
      requestCodexQuotaReset({
        name: 'codex-auth.json',
        type: 'codex',
        authIndex: 'auth-1',
        id_token: { account_id: 'acct-1' },
      })
    ).rejects.toThrow('conflict');

    expect(mocks.post).toHaveBeenCalledTimes(1);
  });
});
