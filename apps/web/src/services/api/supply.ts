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
  smartEnabled?: boolean;
  healthyMinutesTarget: number;
  warningMinutes: number;
  criticalMinutes: number;
  prelockEnabled?: boolean;
  prelockMinQuantity: number;
  prelockMaxQuantity: number;
  criticalTakeConfirmRounds: number;
  createCooldownSeconds: number;
  releaseCooldownSeconds: number;
  authFilesCacheTTLSeconds: number;
  minHoldSeconds: number;
  newAccountConfidence: number;
  minBalanceReserveFen?: number;
  dailyMaxHoldFen?: number;
  dailyMaxReplenishQuantity?: number;
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

export interface SupplySmartResource {
  enabled: boolean;
  healthLevel: 'healthy' | 'warning' | 'critical' | 'unknown' | string;
  suggestedAction: string;
  suggestedQuantity: number;
  decisionReason: string;
  confidence: 'high' | 'medium' | 'low' | string;
  snapshotFresh: boolean;
  generatedAtMs: number;
  availableAccounts: number;
  schedulableAccounts: number;
  healthyAccounts: number;
  weakAccounts: number;
  targetAvailableAccounts: number;
  estimatedSustainMinutes: number;
  healthyMinutesTarget: number;
  warningMinutes: number;
  criticalMinutes: number;
  rpm30m: number;
  rpm5mPeak: number;
  tpm30m: number;
  consumeRcuPerMinute: number;
  currentCapacityRcu: number;
  targetCapacityRcu: number;
  capacityGapRcu: number;
  unitCapacityRcu: number;
  recommendedCapacityRcu: number;
  prelockedCapacityRcu?: number;
  usageSampleMinutes: number;
  accountCacheAgeSeconds: number;
  lockedOrderId?: string;
  lockedOrderAgeSeconds?: number;
  lockedConfirmRounds?: number;
}

export interface SupplyStatus {
  config: SupplyConfig;
  running: boolean;
  overview: SupplyOverview;
  smartResource: SupplySmartResource;
  activeOrder?: SupplyOrder;
  orders: SupplyOrder[];
}

export const supplyApi = {
  getStatus: (limit = 50): Promise<SupplyStatus> => apiClient.get('/supply', { params: { limit } }),

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
