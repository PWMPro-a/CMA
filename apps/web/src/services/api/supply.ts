import { apiClient } from './client';

export type SupplyProduct = 'oauth_30d' | 'oauth_7d';

export interface SupplyConfig {
  enabled?: boolean;
  baseUrl: string;
  username: string;
  password?: string;
  passwordConfigured?: boolean;
  product: SupplyProduct | string;
  targetAvailableAccounts: number;
  replenishBatchSize: number;
  checkIntervalSeconds: number;
  pollIntervalSeconds: number;
  defaultWebsockets: boolean;
}

export interface SupplyInventory {
  product: string;
  requestedQuantity: number;
  available: number;
  missing: number;
  needsProduction: boolean;
  estimatedTotalFen: number;
  estimatedUnitPriceFen: number;
  minimumRemainingSeconds: number;
  maximumRemainingSeconds: number;
}

export interface SupplyBalance {
  balanceFen: number;
  heldFen: number;
  availableFen: number;
  currency: string;
}

export interface SupplyOrder {
  id: number;
  orderId: string;
  product: string;
  requestedQuantity: number;
  automatic: boolean;
  status: string;
  remoteStatus?: string;
  readyQuantity: number;
  progress: number;
  statusUrl?: string;
  takeUrl?: string;
  chargedFen: number;
  releasedFen: number;
  itemCount: number;
  importedCount: number;
  lastError?: string;
  nextPollAtMs?: number;
  completedAtMs?: number;
  createdAtMs: number;
  updatedAtMs: number;
}

export interface SupplyOverview {
  checkedAtMs?: number;
  cpaAvailable: number;
  cpaTarget: number;
  cpaDeficit: number;
  inventory?: SupplyInventory;
  balance?: SupplyBalance;
  lastError?: string;
}

export interface SupplyStatus {
  config: SupplyConfig;
  running: boolean;
  overview: SupplyOverview;
  activeOrder?: SupplyOrder;
  orders: SupplyOrder[];
}

export const supplyApi = {
  getStatus: (limit = 50): Promise<SupplyStatus> =>
    apiClient.get('/supply', { params: { limit } }),

  saveConfig: (config: SupplyConfig): Promise<SupplyStatus> =>
    apiClient.put('/supply/config', { config }),

  check: (): Promise<SupplyStatus> => apiClient.post('/supply/check'),

  replenish: (quantity: number): Promise<SupplyStatus> =>
    apiClient.post('/supply/replenish', { quantity }),

  dismissCreateUncertain: (orderId: string): Promise<SupplyStatus> =>
    apiClient.post(`/supply/orders/${encodeURIComponent(orderId)}/dismiss-uncertain`),

  cancelOrder: (orderId: string): Promise<SupplyStatus> =>
    apiClient.post(`/supply/orders/${encodeURIComponent(orderId)}/cancel`),
};
