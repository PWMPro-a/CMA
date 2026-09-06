import { apiClient, createScopedApiRequestConfig, type ApiClientRequestScope } from './client';

export type IdentityProfile = {
  id: string;
  version: string;
  observed: boolean;
  enabled: boolean;
  platform?: string;
  architecture?: string;
  user_agent?: string;
  originator?: string;
  terminal?: string;
  client_mode?: string;
  protocol?: string;
  source?: string;
  evidence_status?: string;
  eligible?: boolean;
  routable?: boolean;
  artifact_package?: string;
  artifact_ref?: string;
  artifact_integrity?: string;
  capture_method?: string;
  evidence_hash?: string;
  observed_at?: string;
  account_count?: number;
  session_count?: number;
};
export type IdentityCatalogEntry = IdentityProfile & { id: string; version: string; observed: boolean; enabled: boolean };
export type IdentityValidationRecord = {
  id: number;
  at: string;
  method: string;
  path: string;
  mode: string;
  profile_id?: string;
  client_mode?: string;
  valid: boolean;
  exact_match: boolean;
  score: number;
  proxy_exposure: boolean;
  issue_codes?: string[];
  issues?: Array<{ code: string; severity: string; field: string; expected?: string; actual?: string; message: string }>;
};
export type IdentityPool = {
  id: string;
  provider: string;
  protocol: string;
  profiles: IdentityProfile[];
};
export type IdentityAccount = {
  pool_id: string;
  account_id: string;
  fingerprint: string;
  installation_id: string;
  identity_id?: string;
  profile_id: string;
  identity_version: number;
  session_count?: number;
  account_label?: string;
  email?: string;
  auth_index?: string;
  auth_file?: string;
  runtime_status?: string;
  disabled?: boolean;
  unavailable?: boolean;
  source_ip?: string;
  updated_at?: string;
  environment?: IdentityProfile;
  identity_bound?: boolean;
  account_state?: string;
};
export type IdentitySession = {
  pool_id: string;
  account_id: string;
  logical_session_id: string;
  session_id: string;
  thread_id: string;
  window_id: string;
  prompt_cache_key: string;
  identity_version: number;
};
export type CacheAffinityStats = {
  cache_read_tokens: number | null;
  uncached_input_tokens: number | null;
  cache_write_tokens: number | null;
  token_weighted_hit_rate: number | null;
  request_hit_rate: number | null;
  usage_requests: number | null;
  route_hits: number | null;
  route_misses: number | null;
  route_rebinds: number | null;
  prefix_heat_matches: number | null;
  temporary_failovers: number | null;
  tail_burst_fallbacks: number | null;
  engine_fingerprint_rejections: number | null;
  ttft_p50: number | null;
  ttft_p95: number | null;
};
type PoolsResponse = { enabled: boolean; runtime?: string; pools?: IdentityPool[] };
export type IdentityPoolsApiScope = ApiClientRequestScope;

export const normalizeIdentityProfile = (value: Partial<IdentityProfile>): IdentityProfile => ({
  id: String(value.id ?? '').trim(),
  version: String(value.version ?? '').trim(),
  observed: Boolean(value.observed),
  enabled: Boolean(value.enabled),
  platform: String(value.platform ?? '').trim(),
  architecture: String(value.architecture ?? '').trim(),
});

// Missing measurements stay null; coercion must not turn blanks, booleans or
// objects into apparent observations. Decimal numeric strings are supported for
// older management builds, but non-decimal and non-finite values are rejected.
const metricNumber = (value: unknown): number | null => {
  if (typeof value === 'string') {
    const decimal = value.trim();
    if (!/^(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?$/.test(decimal)) return null;
    value = Number(decimal);
  }
  return typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : null;
};

export const normalizeCacheAffinityStats = (value: unknown): CacheAffinityStats | null => {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return null;
  const raw = value as Record<string, unknown>;
  const read = (key: string) =>
    metricNumber(Object.prototype.hasOwnProperty.call(raw, key) ? raw[key] : undefined);
  const counter = (key: string) => {
    const number = read(key);
    return number !== null && Number.isSafeInteger(number) ? number : null;
  };
  const ratio = (key: string) => {
    const number = read(key);
    return number !== null && number <= 1 ? number : null;
  };
  const cacheRead = counter('cache_read_tokens');
  const uncachedInput = counter('uncached_input_tokens');
  const usageRequests = counter('usage_requests');
  // Prefer the exact counter-based formula over a rounded reported rate.
  // Cache writes are a separate bucket and are not added to its denominator.
  const tokenRate =
    cacheRead !== null && uncachedInput !== null
      ? cacheRead + uncachedInput > 0
        ? cacheRead / (cacheRead + uncachedInput)
        : null
      : ratio('token_weighted_hit_rate');
  const stats: CacheAffinityStats = {
    cache_read_tokens: cacheRead,
    uncached_input_tokens: uncachedInput,
    cache_write_tokens: counter('cache_write_tokens'),
    token_weighted_hit_rate: tokenRate,
    request_hit_rate: usageRequests === 0 ? null : ratio('request_hit_rate'),
    usage_requests: usageRequests,
    route_hits: counter('route_hits'),
    route_misses: counter('route_misses'),
    route_rebinds: counter('route_rebinds'),
    prefix_heat_matches: counter('prefix_heat_matches'),
    temporary_failovers: counter('temporary_failovers'),
    tail_burst_fallbacks: counter('tail_burst_fallbacks'),
    engine_fingerprint_rejections: counter('engine_fingerprint_rejections'),
    ttft_p50: counter('ttft_p50'),
    ttft_p95: counter('ttft_p95'),
  };
  return Object.values(stats).some((field) => field !== null) ? stats : null;
};

export const identityPoolsApi = {
  get: (scope?: IdentityPoolsApiScope) =>
    scope
      ? apiClient.get<PoolsResponse>('/identity-pools', createScopedApiRequestConfig(scope))
      : apiClient.get<PoolsResponse>('/identity-pools'),
  accounts: (pool = 'codex', scope?: IdentityPoolsApiScope) =>
    scope
      ? apiClient.get<{ accounts?: IdentityAccount[] }>(
          `/identity-pools/accounts?pool=${encodeURIComponent(pool)}`,
          createScopedApiRequestConfig(scope)
        )
      : apiClient.get<{ accounts?: IdentityAccount[] }>(
          `/identity-pools/accounts?pool=${encodeURIComponent(pool)}`
        ),
  sessions: (pool = 'codex', account = '', scope?: IdentityPoolsApiScope) =>
    scope
      ? apiClient.get<{ sessions?: IdentitySession[] }>(
          `/identity-pools/sessions?pool=${encodeURIComponent(pool)}&account=${encodeURIComponent(account)}`,
          createScopedApiRequestConfig(scope)
        )
      : apiClient.get<{ sessions?: IdentitySession[] }>(
          `/identity-pools/sessions?pool=${encodeURIComponent(pool)}&account=${encodeURIComponent(account)}`
        ),
  rotate: (pool: string, account: string, scope?: IdentityPoolsApiScope) =>
    scope
      ? apiClient.post(
          `/identity-pools/${encodeURIComponent(pool)}/accounts/${encodeURIComponent(account)}/rotate`,
          undefined,
          createScopedApiRequestConfig(scope)
        )
      : apiClient.post(
          `/identity-pools/${encodeURIComponent(pool)}/accounts/${encodeURIComponent(account)}/rotate`
        ),
  deleteSession: (pool: string, account: string, session: string, scope?: IdentityPoolsApiScope) =>
    apiClient.delete(`/identity-pools/sessions/${encodeURIComponent(session)}`, {
      ...(scope ? createScopedApiRequestConfig(scope) : {}),
      params: { pool, account },
    }),
  patchProfile: (
    pool: string,
    profile: string,
    patch: { enabled?: boolean; observed?: boolean },
    scope?: IdentityPoolsApiScope
  ) =>
    scope
      ? apiClient.patch(
          `/identity-pools/${encodeURIComponent(pool)}/profiles/${encodeURIComponent(profile)}`,
          patch,
          createScopedApiRequestConfig(scope)
        )
      : apiClient.patch(
          `/identity-pools/${encodeURIComponent(pool)}/profiles/${encodeURIComponent(profile)}`,
        patch
      ),
  patchAccountProfile: (pool: string, account: string, profileId: string, scope?: IdentityPoolsApiScope) =>
    scope
      ? apiClient.patch(
          `/identity-pools/${encodeURIComponent(pool)}/accounts/${encodeURIComponent(account)}`,
          { profile_id: profileId },
          createScopedApiRequestConfig(scope)
        )
      : apiClient.patch(
          `/identity-pools/${encodeURIComponent(pool)}/accounts/${encodeURIComponent(account)}`,
          { profile_id: profileId }
        ),
  stats: async (scope?: IdentityPoolsApiScope): Promise<CacheAffinityStats | null> => {
    const response = await (scope
      ? apiClient.get<{ stats?: unknown; window_5m?: unknown }>(
          '/codex/cache-affinity/stats',
          createScopedApiRequestConfig(scope)
        )
      : apiClient.get<{ stats?: unknown; window_5m?: unknown }>('/codex/cache-affinity/stats'));
    return normalizeCacheAffinityStats(response?.window_5m ?? response?.stats);
  },
  catalog: (scope?: IdentityPoolsApiScope) =>
    scope
      ? apiClient.get<{ count?: number; items?: IdentityCatalogEntry[] }>('/identity-pools/catalog', createScopedApiRequestConfig(scope))
      : apiClient.get<{ count?: number; items?: IdentityCatalogEntry[] }>('/identity-pools/catalog'),
  validation: (limit = 100, scope?: IdentityPoolsApiScope) =>
    scope
      ? apiClient.get<{ total?: number; valid?: number; invalid?: number; proxy_exposure?: number; records?: IdentityValidationRecord[] }>(
          `/identity-pools/validation?limit=${encodeURIComponent(String(limit))}`,
          createScopedApiRequestConfig(scope)
        )
      : apiClient.get<{ total?: number; valid?: number; invalid?: number; proxy_exposure?: number; records?: IdentityValidationRecord[] }>(
          `/identity-pools/validation?limit=${encodeURIComponent(String(limit))}`
        ),
};
