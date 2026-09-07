import axios from 'axios';

export type QuotaThresholdRule = {
  id: number;
  fileName: string;
  authIndex?: string;
  provider?: string;
  accountSnapshot?: string;
  accountId?: string;
  thresholdPercent: number;
  enabled: boolean;
  lastObservedRemainingPercent?: number;
  lastDisabled: boolean;
  lastTriggeredAtMs?: number;
  lastInspectionAtMs?: number;
  lastError?: string;
  createdAtMs: number;
  updatedAtMs: number;
};

const buildUrl = (base: string, path: string) => `${String(base ?? '').replace(/\/+$/, '')}${path}`;
const headers = (managementKey?: string) => (managementKey ? { Authorization: `Bearer ${managementKey}` } : undefined);

export const quotaThresholdRulesApi = {
  list: async (base: string, managementKey?: string): Promise<QuotaThresholdRule[]> => {
    const response = await axios.get<{ items?: QuotaThresholdRule[] }>(buildUrl(base, '/v0/management/quota-threshold-rules'), { headers: headers(managementKey), timeout: 20_000 });
    return Array.isArray(response.data?.items) ? response.data.items : [];
  },
  save: async (base: string, managementKey: string | undefined, rules: Array<Omit<QuotaThresholdRule, 'id' | 'lastObservedRemainingPercent' | 'lastDisabled' | 'lastTriggeredAtMs' | 'lastInspectionAtMs' | 'lastError' | 'createdAtMs' | 'updatedAtMs'>>): Promise<QuotaThresholdRule[]> => {
    const response = await axios.put<{ items?: QuotaThresholdRule[] }>(buildUrl(base, '/v0/management/quota-threshold-rules'), { rules }, { headers: headers(managementKey), timeout: 20_000 });
    return Array.isArray(response.data?.items) ? response.data.items : [];
  },
  remove: async (base: string, managementKey: string | undefined, id: number): Promise<void> => {
    await axios.delete(buildUrl(base, `/v0/management/quota-threshold-rules/${encodeURIComponent(String(id))}`), { headers: headers(managementKey), timeout: 20_000 });
  },
};
