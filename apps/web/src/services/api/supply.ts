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
  capacitySource: 'inspection_snapshot' | 'unavailable' | string;
  capacityCoverage: number;
  capacityLifetimeCoverage: number;
  capacitySnapshotAtMs: number;
  capacitySnapshotAgeSeconds: number;
  capacitySnapshotRunId?: number;
  pendingInspectionAccounts?: number;
  pendingInspectionCapacityRcu?: number;
  configuredHealthyMinutesTarget?: number;
  effectiveHealthyMinutesTarget: number;
  accountLifetimeMinutes: number;
  estimatedSustainMinutes: number;
  healthyMinutesTarget: number;
  warningMinutes: number;
  criticalMinutes: number;
  rpm30m: number;
  rpm5mPeak: number;
  tpm30m: number;
  rpm1m: number;
  rpm5m: number;
  rpm10m: number;
  tpm1m: number;
  tpm5m: number;
  tpm10m: number;
  consumeRcu1m: number;
  consumeRcu5m: number;
  consumeRcu10m: number;
  demandTrend: 'rising' | 'falling' | 'stable' | 'unknown' | string;
  demandPlanningRcuPerMinute: number;
  consumeRcuPerMinute: number;
  currentCapacityRcu: number;
  rawCapacityRcu?: number;
  timeLimitedCapacityRcu?: number;
  expiryWasteRiskRcu?: number;
  targetCapacityRcu: number;
  capacityGapRcu: number;
  unitCapacityRcu: number;
  recommendedCapacityRcu: number;
  prelockedCapacityRcu?: number;
  supplyPressureLevel?: 'plenty' | 'normal' | 'tight' | 'scarce' | 'unknown' | string;
  supplyPressureReason?: string;
  supplyInventoryAvailable?: number;
  supplyInventoryMissing?: number;
  supplyNeedsProduction?: boolean;
  supplyAvgFulfillSeconds?: number;
  supplyRecentWaiting?: number;
  usageSampleMinutes: number;
  lockedOrderId?: string;
  lockedOrderAgeSeconds?: number;
  lockedConfirmRounds?: number;
}

export interface SupplyAutomationExecution {
  enabled: boolean;
  running: boolean;
  nextExecutionAtMs?: number;
  intervalSeconds?: number;
  lastStartedAtMs?: number;
  lastFinishedAtMs?: number;
  lastResult?: 'scheduled' | 'completed' | 'failed' | 'disabled' | string;
  lastAction?: string;
  lastReason?: string;
  lastError?: string;
}

export interface SupplyStatus {
  config: SupplyConfig;
  running: boolean;
  overview: SupplyOverview;
  smartResource: SupplySmartResource;
  automation?: SupplyAutomationExecution;
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
