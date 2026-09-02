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
  status: () => apiClient.get<LicenseStatus>('/v0/management/license/status'),
  startShopAuthorization: (callbackUrl: string, origin: string) =>
    apiClient.get<ShopAuthorization>('/v0/management/license/shop/start', {
      params: { callback_url: callbackUrl, origin },
    }),
  exchangeShopCode: (state: string, code: string) =>
    apiClient.post<LicenseMutationResponse>('/v0/management/license/shop/exchange', {
      state,
      code,
    }),
  refresh: () => apiClient.post<LicenseMutationResponse>('/v0/management/license/refresh'),
  activate: (code: string) =>
    apiClient.post<LicenseMutationResponse>('/v0/management/license/activate', { code }),
};
