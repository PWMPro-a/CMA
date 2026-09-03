import { apiClient } from './client';

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
    apiClient.post<LicenseMutationResponse>('/license/activate', { code }),
};
