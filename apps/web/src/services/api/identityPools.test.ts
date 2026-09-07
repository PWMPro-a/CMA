import { beforeEach, describe, expect, it, vi } from 'vitest';

const mocks = vi.hoisted(() => ({
  get: vi.fn(),
  post: vi.fn(),
  delete: vi.fn(),
  patch: vi.fn(),
}));

vi.mock('./client', async (importOriginal) => ({
  ...(await importOriginal<typeof import('./client')>()),
  apiClient: mocks,
}));

import {
  identityPoolsApi,
  normalizeCacheAffinityStats,
  normalizeIdentityProfile,
} from './identityPools';

beforeEach(() => {
  vi.resetAllMocks();
});

describe('identity pool API normalization', () => {
  it('normalizes profile metadata without retaining unknown fields', () => {
    expect(
      normalizeIdentityProfile({ id: ' p1 ', version: 'v1', observed: 1 as never, enabled: true })
    ).toEqual({
      id: 'p1',
      version: 'v1',
      observed: true,
      enabled: true,
      platform: '',
      architecture: '',
    });
  });
  it('preserves measured rates without fabricating missing counters', () => {
    expect(
      normalizeCacheAffinityStats({ token_weighted_hit_rate: 0.91 })?.token_weighted_hit_rate
    ).toBe(0.91);
    expect(normalizeCacheAffinityStats(undefined)).toBeNull();
    expect(
      normalizeCacheAffinityStats({ token_weighted_hit_rate: 0.91 })?.route_rebinds
    ).toBeNull();
  });
});

describe('identity pool API isolation', () => {
  it('encodes the requested pool without changing its value', async () => {
    mocks.get.mockResolvedValue({ accounts: [] });
    await identityPoolsApi.accounts('fixture/pool');
    expect(mocks.get).toHaveBeenCalledWith('/identity-pools/accounts?pool=fixture%2Fpool');
  });

  it('preserves opaque account references in scoped session queries', async () => {
    const response = {
      sessions: [{ account_id: 'acct:0123456789abcdef', logical_session_id: 'session' }],
    };
    mocks.get.mockResolvedValue(response);
    await expect(identityPoolsApi.sessions('fixture', 'acct:0123456789abcdef')).resolves.toBe(
      response
    );
    expect(mocks.get).toHaveBeenCalledWith(
      '/identity-pools/sessions?pool=fixture&account=acct%3A0123456789abcdef'
    );
  });

  it('does not retry an unknown account as an all-accounts query', async () => {
    const error = Object.assign(new Error('account_not_found'), { status: 404 });
    mocks.get.mockRejectedValue(error);
    await expect(identityPoolsApi.sessions('fixture', 'acct:missing')).rejects.toBe(error);
    expect(mocks.get).toHaveBeenCalledTimes(1);
    expect(mocks.get).toHaveBeenCalledWith(
      '/identity-pools/sessions?pool=fixture&account=acct%3Amissing'
    );
  });

  it('keeps deletion scoped while encoding a session path segment', async () => {
    mocks.delete.mockResolvedValue({ ok: true });
    await identityPoolsApi.deleteSession('fixture', 'acct:0123456789abcdef', 'folder/session?1');
    expect(mocks.delete).toHaveBeenCalledWith('/identity-pools/sessions/folder%2Fsession%3F1', {
      params: { pool: 'fixture', account: 'acct:0123456789abcdef' },
    });
  });

  it('preserves explicit false when disabling a profile', async () => {
    mocks.patch.mockResolvedValue({ ok: true });
    await identityPoolsApi.patchProfile('fixture', 'profile', { enabled: false });
    expect(mocks.patch).toHaveBeenCalledWith('/identity-pools/fixture/profiles/profile', {
      enabled: false,
    });
  });
});

describe('identity pool captured connection scope', () => {
  const scope = { apiBase: 'http://old-cpa.test:8317/', managementKey: 'fixture-key' };
  const expectedConfig = {
    baseURL: 'http://old-cpa.test:8317/v0/management',
    headers: { Authorization: 'Bearer fixture-key' },
    cpampScopedRequest: true,
  };
  it('passes the captured connection to every read endpoint', async () => {
    mocks.get.mockResolvedValue({ stats: { token_weighted_hit_rate: 0.92 } });
    await identityPoolsApi.get(scope);
    await identityPoolsApi.accounts('codex', scope);
    await identityPoolsApi.sessions('codex', 'acct:scope', scope);
    await expect(identityPoolsApi.stats(scope)).resolves.toMatchObject({
      token_weighted_hit_rate: 0.92,
    });
    expect(mocks.get.mock.calls).toEqual([
      ['/identity-pools', expectedConfig],
      ['/identity-pools/accounts?pool=codex', expectedConfig],
      ['/identity-pools/sessions?pool=codex&account=acct%3Ascope', expectedConfig],
      ['/codex/cache-affinity/stats', expectedConfig],
    ]);
  });

  it('prefers the explicit five-minute window over the cumulative snapshot', async () => {
    mocks.get.mockResolvedValue({
      stats: { cache_read_tokens: 1, uncached_input_tokens: 99 },
      window_5m: { cache_read_tokens: 90, uncached_input_tokens: 10 },
    });
    await expect(identityPoolsApi.stats(scope)).resolves.toMatchObject({
      cache_read_tokens: 90,
      uncached_input_tokens: 10,
      token_weighted_hit_rate: 0.9,
    });
  });
  it('preserves captured scope and resource scope on writes', async () => {
    await identityPoolsApi.deleteSession('codex', 'acct:scope', 'logical/session', scope);
    await identityPoolsApi.patchProfile('codex', 'profile', { enabled: false }, scope);
    expect(mocks.delete).toHaveBeenCalledWith('/identity-pools/sessions/logical%2Fsession', {
      ...expectedConfig,
      params: { pool: 'codex', account: 'acct:scope' },
    });
    expect(mocks.patch).toHaveBeenCalledWith(
      '/identity-pools/codex/profiles/profile',
      { enabled: false },
      expectedConfig
    );
  });
  it.each([{}, { stats: undefined }, { stats: null }, { stats: {} }, { stats: [] }])(
    'does not invent a zero hit rate for an absent diagnostics snapshot: %j',
    async (response) => {
      mocks.get.mockResolvedValue(response);
      await expect(identityPoolsApi.stats(scope)).resolves.toBeNull();
    }
  );
});

describe('cache diagnostics measurement semantics', () => {
  it.each([null, undefined, [], '', 0, true, { new_field: 1 }])(
    'ignores non-snapshots and unknown-only payloads: %j',
    (value) => {
      expect(normalizeCacheAffinityStats(value)).toBeNull();
    }
  );

  it('uses token counts rather than a rounded rate, excluding cache writes', () => {
    const value = normalizeCacheAffinityStats({
      cache_read_tokens: 910,
      uncached_input_tokens: 90,
      cache_write_tokens: 8000,
      token_weighted_hit_rate: 0.9,
    });
    expect(value?.token_weighted_hit_rate).toBeCloseTo(0.91);
    expect(value?.cache_write_tokens).toBe(8000);
  });

  it('does not report a measured hit rate when no input or request samples exist', () => {
    const value = normalizeCacheAffinityStats({
      cache_read_tokens: 0,
      uncached_input_tokens: 0,
      cache_write_tokens: 100,
      usage_requests: 0,
      token_weighted_hit_rate: 0,
      request_hit_rate: 0,
      route_rebinds: 0,
    });
    expect(value?.token_weighted_hit_rate).toBeNull();
    expect(value?.request_hit_rate).toBeNull();
    expect(value?.route_rebinds).toBe(0);
  });

  it('preserves a real zero rate with positive samples', () => {
    expect(
      normalizeCacheAffinityStats({
        cache_read_tokens: 0,
        uncached_input_tokens: 250,
        usage_requests: 4,
        request_hit_rate: 0,
      })
    ).toMatchObject({ token_weighted_hit_rate: 0, request_hit_rate: 0 });
  });

  it('does not infer cache request hits from route affinity hits', () => {
    expect(
      normalizeCacheAffinityStats({ route_hits: 100, route_misses: 0, usage_requests: 100 })
        ?.request_hit_rate
    ).toBeNull();
  });

  it.each([
    null,
    undefined,
    '',
    '   ',
    true,
    false,
    [],
    {},
    -1,
    1.2,
    NaN,
    Infinity,
    Number.MAX_SAFE_INTEGER + 1,
    '0x10',
    '9x',
  ])('keeps an invalid counter unknown: %j', (value) => {
    const result = normalizeCacheAffinityStats({
      route_rebinds: value,
      token_weighted_hit_rate: 0.91,
    });
    expect(result?.route_rebinds).toBeNull();
    expect(result?.token_weighted_hit_rate).toBe(0.91);
  });

  it.each([null, '', false, [], {}, -0.1, 1.1, 90, NaN, Infinity, '91%', '0x1'])(
    'keeps an invalid reported ratio unknown: %j',
    (value) => {
      const result = normalizeCacheAffinityStats({ request_hit_rate: value, route_hits: 0 });
      expect(result?.request_hit_rate).toBeNull();
      expect(result?.route_hits).toBe(0);
    }
  );

  it('supports finite decimal numeric strings from older builds', () => {
    expect(
      normalizeCacheAffinityStats({
        cache_read_tokens: ' 910 ',
        uncached_input_tokens: '9e1',
        request_hit_rate: '0.5',
        route_rebinds: '0',
      })
    ).toMatchObject({ token_weighted_hit_rate: 0.91, request_hit_rate: 0.5, route_rebinds: 0 });
  });

  it('treats an explicitly reported legacy zero ratio as measured when sample counts are absent', () => {
    expect(normalizeCacheAffinityStats({ request_hit_rate: 0 })?.request_hit_rate).toBe(0);
  });

  it('drops unrecognized fields and does not mutate the response', () => {
    const raw = Object.freeze({ route_rebinds: 0, secret: 'not-a-metric' });
    const result = normalizeCacheAffinityStats(raw);
    expect(result).not.toHaveProperty('secret');
    expect(raw.secret).toBe('not-a-metric');
  });
});
