import { apiClient } from './client';

const LICENSE_ERROR_CODES = new Set([
  'feature_not_enabled',
  'purchase_required',
  'expired',
  'revoked',
  'authorization_expired',
  'authorization_invalid',
  'provider_unavailable',
  'provider_rejected',
  'not_activated',
  'license_expired',
  'lease_expired',
  'integrity_failed',
  'configuration_error',
  'instance_mismatch',
  'product_mismatch',
  'state_mismatch',
  'code_used',
  'code_expired',
  'authorization_error',
  'invalid_request',
]);

const isRecord = (value: unknown): value is Record<string, unknown> =>
  value !== null && typeof value === 'object';

const asLicenseErrorCode = (value: unknown): string => {
  if (typeof value !== 'string') return '';
  const code = value.trim().toLowerCase();
  return LICENSE_ERROR_CODES.has(code) ? code : '';
};

/**
 * Normalize an activation code copied from Markdown or wrapped terminal text.
 * The signed payload is base64url, so whitespace and escaped '-'/'_' are
 * formatting artifacts rather than meaningful bytes.
 */
export const normalizeActivationCode = (value: string): string => {
  let normalized = String(value ?? '').trim();
  normalized = normalized
    .replace(/^```[a-z0-9_-]*\s*/i, '')
    .replace(/\s*```$/i, '')
    .replace(/\\([_-])/g, '$1');
  return normalized.replace(/[\s\u200B-\u200D\uFEFF]+/g, '');
};

/** Extract the provider's stable license error code from an API error. */
export const getLicenseErrorCode = (error: unknown): string => {
  if (typeof error === 'string') return asLicenseErrorCode(error);
  if (!isRecord(error)) return '';

  const direct = asLicenseErrorCode(error.code);
  if (direct) return direct;

  for (const payload of [error.details, error.data]) {
    if (!isRecord(payload)) continue;
    const code = asLicenseErrorCode(payload.code);
    if (code) return code;
    if (isRecord(payload.error)) {
      const nestedCode = asLicenseErrorCode(payload.error.code);
      if (nestedCode) return nestedCode;
    }
    const nestedError = asLicenseErrorCode(payload.error);
    if (nestedError) return nestedError;
  }

  return asLicenseErrorCode(error.message);
};

/** Return a server-provided human-readable message when no translation exists. */
export const getLicenseErrorMessage = (error: unknown): string => {
  const candidates: unknown[] = [];
  if (isRecord(error)) {
    candidates.push(error.details, error.data);
    if (typeof error.message === 'string') candidates.push(error.message);
  } else if (error instanceof Error) {
    candidates.push(error.message);
  }

  for (const payload of candidates) {
    if (typeof payload === 'string') {
      if (payload.trim() && !asLicenseErrorCode(payload)) return payload.trim();
      continue;
    }
    if (!isRecord(payload)) continue;
    const message = typeof payload.message === 'string' ? payload.message : payload.error;
    if (typeof message === 'string' && message.trim() && !asLicenseErrorCode(message)) {
      return message.trim();
    }
    if (isRecord(payload.error) && typeof payload.error.message === 'string') {
      const nested = payload.error.message.trim();
      if (nested && !asLicenseErrorCode(nested)) return nested;
    }
  }
  return '';
};

export interface LicenseStatus {
  enabled: boolean;
  configured: boolean;
  valid: boolean;
  in_grace: boolean;
  allowed: boolean;
  reason?: string;
  provider: string;
  product_code: string;
  product_name?: string;
  license_id?: string;
  activation_mode?: string;
  features?: string[];
  instance_id: string;
  instance_bound: boolean;
  expires_at?: number;
  lease_expires_at?: number;
  last_verified_at?: number;
  last_refresh_at?: number;
  last_refresh_error?: string;
  grace_period_seconds?: number;
  grace_started_at?: number;
  grace_until?: number;
  grace_remaining_seconds?: number;
  expiry_grace?: boolean;
  expiry_grace_started_at?: number;
  expiry_grace_until?: number;
  expiry_grace_remaining_seconds?: number;
}

export interface ShopAuthorization {
  url: string;
  state: string;
  expires_at: string;
}

interface LicenseMutationResponse {
  status: 'ok';
  license: LicenseStatus;
}

export const licenseApi = {
  // apiClient already prefixes requests with the management API path. Keep
  // these endpoint paths relative so they resolve to /v0/management/license/*
  // instead of duplicating the prefix as /v0/management/v0/management/*.
  status: () => apiClient.get<LicenseStatus>('/license/status'),
  startShopAuthorization: (callbackUrl: string, origin: string) =>
    apiClient.get<ShopAuthorization>('/license/shop/start', {
      params: { callback_url: callbackUrl, origin },
    }),
  exchangeShopCode: (state: string, code: string) =>
    apiClient.post<LicenseMutationResponse>('/license/shop/exchange', {
      state,
      code,
    }),
  refresh: () => apiClient.post<LicenseMutationResponse>('/license/refresh'),
  activate: (code: string) =>
    apiClient.post<LicenseMutationResponse>('/license/activate', {
      code: normalizeActivationCode(code),
    }),
};
