import { apiClient } from './client';

export type SupplyProduct = 'oauth_30d' | 'oauth_7d' | 'team_1h';
export type SupplyStrategy = 'strong_supply' | 'balanced' | 'cost_first' | 'custom';
export type SupplyQuotaEstimationMode = 'auto' | 'fixed' | string;
export type SupplyPlatformType = 'legacy' | 'bugteam' | string;

export interface SupplyQuotaEstimationPolicy {
  mode: SupplyQuotaEstimationMode;
  fallbackM: number;
  fixedM: number;
}

export interface SupplyPlatformConfig {
  id: string;
  name?: string;
  type: SupplyPlatformType;
  enabled?: boolean;
  baseUrl: string;
  username?: string;
  password?: string;
  passwordConfigured?: boolean;
  token?: string;
  tokenConfigured?: boolean;
  product: SupplyProduct | string;
  priority?: number;
  quotaEstimationPolicies?: Record<string, SupplyQuotaEstimationPolicy>;
}

export interface SupplyConfig {
  enabled?: boolean;
  baseUrl: string;
  username: string;
  password?: string;
  passwordConfigured?: boolean;
  product: SupplyProduct | string;
  strategy?: SupplyStrategy | string;
  targetAvailableAccounts: number;
  replenishBatchSize: number;
  maxConcurrentOrders?: number;
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
  revenueMultiplier?: number;
  criticalAvailableAccounts?: number;
  healthyAvailableAccounts?: number;
  defaultEmergencyMinAccounts?: number;
  virtualDemandTtlMinutes?: number;
  accountMaxRequestsBefore401?: number;
  accountMaxUsefulSecondsBefore401?: number;
  emergencyBypassUsageRate?: boolean;
  recoveryTriggerOn401?: boolean;
  recoverySyncEnabled?: boolean;
  recoveryAutoClaim?: boolean;
  recoverySyncIntervalSeconds?: number;
  recoveryClaimBatchSize?: number;
  recoveryDisableOriginal?: boolean;
  quotaEstimationPolicies?: Record<string, SupplyQuotaEstimationPolicy>;
  platforms?: SupplyPlatformConfig[];
  platformSelectionStrategy?: string;
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
  supplierId?: string;
  product: string;
  requestedQuantity: number;
  automatic: boolean;
  strategy?: string;
  triggerReason?: string;
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
  supplierRetryUntilMs?: number;
  completedAtMs?: number;
  createdAtMs: number;
  updatedAtMs: number;
}

export interface SupplyActiveOrderStatus {
  checkedAtMs: number;
  activeOrder?: SupplyOrder;
  activeOrders?: SupplyOrder[];
  pollAttempted: boolean;
  pollInProgress: boolean;
  pollError?: string;
}

export interface SupplyOverview {
  checkedAtMs?: number;
  cpaAvailable: number;
  cpaTarget: number;
  cpaDeficit: number;
  inventory?: SupplyInventory;
  balance?: SupplyBalance;
  selectedPlatformId?: string;
  platforms?: SupplyPlatformOverview[];
  lastError?: string;
}

export interface SupplyPlatformOverview {
  id: string;
  name?: string;
  type: string;
  product: string;
  priority?: number;
  selected: boolean;
  checkedAtMs: number;
  inventory?: SupplyInventory;
  balance?: SupplyBalance;
  lastError?: string;
}

export interface SupplyAccountPoolSummary {
  checkedAtMs: number;
  total: number;
  normal: number;
  needsAttention: number;
  quotaRisk: number;
  disabled: number;
  unconfirmed: number;
  classificationObserved: boolean;
}

export interface SupplySmartResource {
  enabled: boolean;
  healthLevel: 'healthy' | 'warning' | 'critical' | 'unknown' | string;
  suggestedAction: string;
  suggestedQuantity: number;
  decisionReason: string;
  confidence: 'high' | 'medium' | 'low' | string;
  snapshotFresh: boolean;
  snapshotRefreshInProgress?: boolean;
  snapshotRefreshLastAttemptMs?: number;
  generatedAtMs: number;
  capacitySource: 'inspection_snapshot' | 'unavailable' | string;
  capacityCoverage: number;
  capacityLifetimeCoverage: number;
  capacitySnapshotAtMs: number;
  capacitySnapshotAgeSeconds: number;
  capacitySnapshotRunId?: number;
  availableAccounts: number;
  schedulableAccounts: number;
  healthyAccounts: number;
  enabledAccounts?: number;
  accountClassificationObserved?: boolean;
  normalAccounts?: number;
  needsAttentionAccounts?: number;
  quotaRiskAccounts?: number;
  unconfirmedAccounts?: number;
  atRiskAccounts?: number;
  weakAccounts: number;
  totalAccounts?: number;
  disabledAccounts?: number;
  frozenAccounts?: number;
  concurrencyLimitedAccounts?: number;
  concurrencyUnlimitedAccounts?: number;
  concurrencyMissingAccounts?: number;
  concurrencyFiniteSlots?: number;
  requiredConcurrencySlots?: number;
  concurrencyHeadroomSlots?: number;
  concurrencyAccountDeficit?: number;
  concurrencyCoverage?: number;
  concurrencyEffectiveCapacityRcu?: number;
  averageRequestLatencyMs?: number;
  concurrencyUnlimited?: boolean;
  concurrencyLimited?: boolean;
  concurrencyDemandObserved?: boolean;
  pendingInspectionAccounts?: number;
  pendingInspectionCapacityRcu?: number;
  estimatedRequiredAccounts?: number;
  projectedAvailableAccounts?: number;
  accountQuantityDeficit?: number;
  leaseEstimatedAccounts?: number;
  leaseEstimatedCapacityRcu?: number;
  configuredHealthyMinutesTarget?: number;
  effectiveHealthyMinutesTarget: number;
  accountLifetimeMinutes: number;
  estimatedSustainMinutes: number;
  emergencyShortage?: boolean;
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
  requestDemandRcuPerMinute?: number;
  tokenDemandRcuPerMinute?: number;
  demandDriver?: 'requests' | 'tokens' | 'none' | string;
  consumeRcu1m: number;
  consumeRcu5m: number;
  consumeRcu10m: number;
  demandTrend: 'rising' | 'falling' | 'stable' | 'unknown' | string;
  demandPlanningRcuPerMinute: number;
  consumeRcuPerMinute: number;
  currentCapacityRcu: number;
  availableCapacityRcu?: number;
  frozenCapacityRcu?: number;
  totalCapacityRcu?: number;
  availableSustainMinutes?: number;
  frozenSustainMinutes?: number;
  rawCapacityRcu?: number;
  timeLimitedCapacityRcu?: number;
  expiryWasteRiskRcu?: number;
  rawSustainMinutes?: number;
  expiryLimitedSustainMinutes?: number;
  nearestExpiryAtMs?: number;
  nearestExpiryMinutes?: number;
  nextCapacityDeficitAtMs?: number;
  expiringAccounts?: number;
  expiringWithinMinutes?: number;
  expiringCapacityRcu?: number;
  targetCapacityRcu: number;
  capacityGapRcu: number;
  unitCapacityRcu: number;
  recommendedCapacityRcu: number;
  prelockedCapacityRcu?: number;
  projectedCapacityAfterRefillRcu?: number;
  projectedSustainAfterRefillMinutes?: number;
  supplyPressureLevel?: 'plenty' | 'normal' | 'tight' | 'scarce' | 'unknown' | string;
  supplyPressureReason?: string;
  supplyInventoryAvailable?: number;
  supplyInventoryMissing?: number;
  supplyNeedsProduction?: boolean;
  supplyAvgFulfillSeconds?: number;
  supplyRecentWaiting?: number;
  supplyRecentOrders?: number;
  supplyRecentCancelled?: number;
  supplyRecentZeroDelivery?: number;
  supplyRecentRequestedQuantity?: number;
  supplyRecentDeliveredQuantity?: number;
  supplyFulfillmentRate?: number;
  usageSampleMinutes: number;
  lockedOrderId?: string;
  lockedOrderAgeSeconds?: number;
  lockedConfirmRounds?: number;
  strategy?: string;
  criticalAvailableAccounts?: number;
  healthyAvailableAccounts?: number;
  emergencyMinAccounts?: number;
  emergencyReason?: string;
  poolVacuumActive?: boolean;
  poolVacuumStartedAtMs?: number;
  poolVacuumDurationSeconds?: number;
  demandMemoryRcuPerMinute?: number;
  demandMemoryLastSeenMs?: number;
  demandMemoryAgeSeconds?: number;
  virtualDemandRcuPerMinute?: number;
  virtualDemandTtlMinutes?: number;
  accountMaxRequestsBefore401?: number;
  accountMaxUsefulSecondsBefore401?: number;
  estimatedNewAccountCapacityRcu?: number;
  riskAdjustedUnitCapacityRcu?: number;
  tokenCapacityMode?: string;
  accountQuotaEstimateM?: number;
  accountQuotaEstimateSource?: string;
  accountQuotaCalibrationConfidence?: string;
  accountQuotaCalibrationSamples?: number;
  accountQuotaCalibrationObservedPercent?: number;
  accountQuotaCalibrationUniqueAccounts?: number;
  accountQuotaCurrentEstimateM?: number;
  accountQuotaRecentEstimateM?: number;
  accountQuotaHistoricalEstimateM?: number;
  accountQuotaDivergencePercent?: number;
  accountQuotaPlanEstimates?: SupplyQuotaPlanEstimate[];
  quotaEstimateOrderingBlocked?: boolean;
  quotaEstimatePendingPlans?: number;
  rawCapacityTokenM?: number;
  currentCapacityTokenM?: number;
  availableCapacityTokenM?: number;
  frozenCapacityTokenM?: number;
  totalCapacityTokenM?: number;
  timeLimitedCapacityTokenM?: number;
  expiryWasteRiskTokenM?: number;
  observedTokenM1m?: number;
  observedTokenM5m?: number;
  observedTokenM10m?: number;
  observedTokenM30m?: number;
  consumeTokenM1m?: number;
  consumeTokenM5m?: number;
  consumeTokenM10m?: number;
  consumeTokenMPerMinute?: number;
  demandPlanningTokenMPerMinute?: number;
  forecastSustainMinutes?: number;
  targetCapacityTokenM?: number;
  capacityGapTokenM?: number;
  estimatedNewAccountCapacityTokenM?: number;
  prelockedCapacityTokenM?: number;
  projectedCapacityAfterRefillTokenM?: number;
}

export interface SupplyQuotaPlanEstimate {
  key: string;
  supplierId?: string;
  supplierName?: string;
  planType: string;
  mode: SupplyQuotaEstimationMode;
  accountCount: number;
  fallbackM: number;
  fixedM?: number;
  observedM?: number;
  adoptedM: number;
  source: string;
  sampleCount: number;
  uniqueAccounts: number;
  completeWindowAccounts?: number;
  minimumUniqueAccounts?: number;
  divergencePercent?: number;
  pendingConfirmation?: boolean;
  confirmationRounds?: number;
  requiredRounds?: number;
  validationState?: 'fixed' | 'insufficient' | 'confirming' | 'quarantined' | 'accepted' | string;
  usingFallback?: boolean;
  orderingBlocked?: boolean;
  lastInspectionRunId?: number;
  quotaClasses?: SupplyQuotaClassEstimate[];
}

export interface SupplyQuotaClassEstimate {
  id: string;
  centerM: number;
  minimumM: number;
  maximumM: number;
  accountCount: number;
  trustedAccounts: number;
  provisionalAccounts: number;
  sharePercent: number;
  confidence: string;
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

export interface SupplyRecoverySummary {
  enabled: boolean;
  autoClaim: boolean;
  running: boolean;
  lastSyncAtMs?: number;
  nextSyncAtMs?: number;
  lastResult?: string;
  lastError?: string;
  seen: number;
  claimable: number;
  claimed: number;
  imported: number;
  refunded: number;
  failed: number;
  total: number;
  external: number;
  importing: number;
  storedImported: number;
  storedRefunded: number;
  storedFailed: number;
}

export interface SupplyRecovery {
  id: number;
  recoveryId: string;
  product?: string;
  deliveryStatus: string;
  status: string;
  credentialVersion?: number;
  sourceOrderId?: string;
  originalFileName?: string;
  originalAuthIndex?: string;
  originalEmail?: string;
  claimOrderId?: string;
  itemCount: number;
  importedCount: number;
  refundedFen?: number;
  lastError?: string;
  lastSeenAtMs: number;
  claimedAtMs?: number;
  importedFileNames?: string[];
  importPendingCount?: number;
  importFailedCount?: number;
  lastImportedAtMs?: number;
  importStatus?:
    | 'waiting_claim'
    | 'claiming'
    | 'claimed_waiting_task'
    | 'claimed_without_local_payload'
    | 'not_this_pool'
    | 'ownership_unknown'
    | 'task_pending'
    | 'importing'
    | 'retry_scheduled'
    | 'partial'
    | 'failed'
    | 'imported'
    | 'refunded'
    | string;
  importMessage?: string;
  ownership?: 'local' | 'external' | 'unknown' | string;
  importNextRetryAtMs?: number;
  importItems?: SupplyRecoveryImportItem[];
  createdAtMs: number;
  updatedAtMs: number;
}

export interface SupplyRecoveryImportItem {
  accountName?: string;
  fileName?: string;
  importAction?: 'add' | 'replace' | string;
  replacedFileName?: string;
  status: string;
  lastError?: string;
  attemptCount: number;
  nextRetryAtMs?: number;
  importedAtMs?: number;
  updatedAtMs: number;
}

export interface SupplyRecoverySyncRequest {
  force?: boolean;
  autoClaim?: boolean;
  limit?: number;
  recoveryId?: string;
}

export interface SupplyReportRange {
  fromMs: number;
  toMs: number;
  generatedAtMs: number;
  days: number;
  truncated: boolean;
}

export interface SupplyAccountSummary {
  total: number;
  imported: number;
  pending: number;
  failed: number;
  active: number;
  disabled: number;
  expired: number;
  missing: number;
  unknown: number;
  expiringSoon: number;
  usageCalls: number;
  usageSuccessCalls: number;
  usageFailureCalls: number;
  usageTokens: number;
  usageRevenue: number;
  usageRevenueCurrency: string;
  revenueMultiplier: number;
  averageRevenuePerCall: number;
  auth401Accounts: number;
  autoQuarantined: number;
  lastUsedAtMs?: number;
  cpaStatusError?: string;
}

export interface SupplyAccountItem {
  id: number;
  fileName: string;
  orderId: string;
  source: string;
  product?: string;
  orderStatus?: string;
  status: string;
  accountStatus: string;
  accountStatusReason?: string;
  cpaProvider?: string;
  cpaAccount?: string;
  cpaAccountId?: string;
  cpaAuthIndex?: string;
  cpaStatus?: string;
  cpaDisabled?: boolean;
  usageCalls: number;
  usageSuccessCalls: number;
  usageFailureCalls: number;
  usageTokens: number;
  usageRevenue: number;
  usageRevenueCurrency: string;
  supplierBasePriceFen?: number;
  supplierChargedFen?: number;
  supplierReleasedFen?: number;
  lastUsedAtMs?: number;
  importedAtMs?: number;
  leaseExpiresAtMs?: number;
  remainingSeconds?: number;
  auth401AtMs?: number;
  auth401BeforeCalls?: number;
  auth401Reason?: string;
  autoDisabledAtMs?: number;
  recoveryId?: string;
  recoveryStatus?: string;
  attemptCount: number;
  lastError?: string;
  createdAtMs: number;
  updatedAtMs: number;
}

export interface SupplyAccountList {
  range: SupplyReportRange;
  summary: SupplyAccountSummary;
  items: SupplyAccountItem[];
}

export interface SupplyAccountLeaseItem {
  fileName: string;
  leaseExpiresAtMs: number;
}

export interface SupplyReportExecutive {
  orders: number;
  manualOrders: number;
  automaticOrders: number;
  recoveryOrders: number;
  requestedAccounts: number;
  importedAccounts: number;
  chargedFen: number;
  releasedFen: number;
  netFen: number;
  supplySpendFen: number;
  supplyNetSpendFen: number;
  averageUnitFen: number;
  usageCalls: number;
  usageTokens: number;
  usageRevenue: number;
  usageRevenueCurrency: string;
  revenueMultiplier: number;
  averageRevenuePerCall: number;
  recoveries: number;
  claimableRecoveries: number;
  claimedRecoveries: number;
  importedRecoveries: number;
  refundedRecoveries: number;
  failedRecoveries: number;
  refundedFen: number;
  recoveryClaimRate: number;
  recoveryImportRate: number;
  recoveryRefundRate: number;
  importSuccessRate: number;
  auth401Accounts: number;
  auth401Events: number;
  auth401Rate: number;
  autoQuarantined: number;
  emergencyReplenishments: number;
  virtualDemandReplenishments: number;
  vacuumReplenishments: number;
  vacuumTotalSeconds: number;
  averageVacuumRecoverySeconds: number;
}

export interface SupplyReportDimensionStat {
  key: string;
  label?: string;
  count: number;
  orders: number;
  recoveries: number;
  quantity: number;
  imported: number;
  chargedFen: number;
  releasedFen: number;
  refundedFen: number;
  successRate: number;
}

export interface SupplyReportTimelinePoint {
  bucketMs: number;
  label: string;
  orders: number;
  requested: number;
  imported: number;
  chargedFen: number;
  usageCalls: number;
  usageTokens: number;
  usageRevenue: number;
  recoveries: number;
  recoveryClaimed: number;
  recoveryImported: number;
  recoveryRefunded: number;
  importFailures: number;
}

export interface SupplyReportImportHealth {
  items: number;
  importedItems: number;
  failedItems: number;
  pendingItems: number;
  retryingItems: number;
  averageAttempts: number;
  successRate: number;
  expiringSoonItems: number;
  expiredItems: number;
}

export interface SupplyReportTiming {
  averageOrderFulfillmentSeconds: number;
  averageRecoveryClaimSeconds: number;
  averageRecoveryImportSeconds: number;
  averageImportRegistrationSeconds: number;
}

export interface SupplyReportRiskBucket {
  key: string;
  label: string;
  count: number;
}

export interface SupplyReportRisk {
  openOrders: number;
  unclaimedRecoveries: number;
  importBacklogItems: number;
  failedImportItems: number;
  partialRecoveries: number;
  staleClaimableRecoveries: number;
  claimableAgeBuckets: SupplyReportRiskBucket[];
}

export interface SupplyReportReconciliationSummary {
  orderRows: number;
  accountRows: number;
  recoveryRows: number;
  orderChargedFen: number;
  orderReleasedFen: number;
  orderNetFen: number;
  accountAllocatedChargedFen: number;
  accountAllocatedReleasedFen: number;
  accountAllocatedNetFen: number;
  accountUsageCalls: number;
  accountUsageTokens: number;
  accountUsageRevenue: number;
  refundedFen: number;
  usageRevenueCurrency: string;
  allocationMethod: string;
}

export interface SupplyReportOrderLedgerRow {
  orderId: string;
  source: string;
  strategy?: string;
  triggerReason?: string;
  product: string;
  status: string;
  requestedQuantity: number;
  itemCount: number;
  importedCount: number;
  chargedFen: number;
  releasedFen: number;
  netFen: number;
  createdAtMs: number;
  completedAtMs?: number;
}

export interface SupplyReportAccountLedgerRow {
  fileName: string;
  orderId: string;
  source: string;
  product?: string;
  status: string;
  accountStatus: string;
  importedAtMs?: number;
  leaseExpiresAtMs?: number;
  allocatedChargedFen: number;
  allocatedReleasedFen: number;
  allocatedNetFen: number;
  supplierBasePriceFen?: number;
  supplierChargedFen?: number;
  supplierReleasedFen?: number;
  usageCalls: number;
  usageSuccessCalls: number;
  usageFailureCalls: number;
  usageTokens: number;
  usageRevenue: number;
  lastUsedAtMs?: number;
  auth401AtMs?: number;
  autoDisabledAtMs?: number;
}

export interface SupplyReportRecoveryLedgerRow {
  recoveryId: string;
  product?: string;
  deliveryStatus: string;
  status: string;
  originalFileName?: string;
  claimOrderId?: string;
  itemCount: number;
  importedCount: number;
  refundedFen: number;
  lastSeenAtMs?: number;
  claimedAtMs?: number;
  updatedAtMs: number;
}

export interface SupplyReportReconciliation {
  summary: SupplyReportReconciliationSummary;
  orders: SupplyReportOrderLedgerRow[];
  accounts: SupplyReportAccountLedgerRow[];
  recoveries: SupplyReportRecoveryLedgerRow[];
}

export interface SupplyReportUsageModelStat {
  model: string;
  billingModel: string;
  serviceTier?: string;
  calls: number;
  successCalls: number;
  tokens: number;
  revenue: number;
}

export interface SupplyReport {
  range: SupplyReportRange;
  executive: SupplyReportExecutive;
  importHealth: SupplyReportImportHealth;
  timing: SupplyReportTiming;
  risk: SupplyReportRisk;
  reconciliation: SupplyReportReconciliation;
  timeline: SupplyReportTimelinePoint[];
  products: SupplyReportDimensionStat[];
  strategies: SupplyReportDimensionStat[];
  triggerReasons: SupplyReportDimensionStat[];
  orderStatuses: SupplyReportDimensionStat[];
  recoveryStatuses: SupplyReportDimensionStat[];
  deliveryStatuses: SupplyReportDimensionStat[];
  sources: SupplyReportDimensionStat[];
  usageModels: SupplyReportUsageModelStat[];
}

export interface SupplyStatus {
  config: SupplyConfig;
  running: boolean;
  overview: SupplyOverview;
  accountPool?: SupplyAccountPoolSummary;
  smartResource: SupplySmartResource;
  automation?: SupplyAutomationExecution;
  recovery?: SupplyRecoverySummary;
  activeOrder?: SupplyOrder;
  activeOrders?: SupplyOrder[];
  orders: SupplyOrder[];
}

export const supplyApi = {
  getStatus: (limit = 50): Promise<SupplyStatus> => apiClient.get('/supply', { params: { limit } }),

  getAccountPoolSummary: (): Promise<SupplyAccountPoolSummary> =>
    apiClient.get('/supply/account-pool'),

  getActiveOrder: (): Promise<SupplyActiveOrderStatus> => apiClient.get('/supply/active'),

  saveConfig: (config: SupplyConfig): Promise<SupplyStatus> =>
    apiClient.put('/supply/config', { config }),

  check: (): Promise<SupplyStatus> => apiClient.post('/supply/check'),

  getReport: (
    params: { fromMs?: number; toMs?: number; limit?: number } = {}
  ): Promise<SupplyReport> => apiClient.get('/supply/reports', { params }),

  listAccounts: (
    params: { fromMs?: number; toMs?: number; limit?: number; status?: string } = {}
  ): Promise<SupplyAccountList> => apiClient.get('/supply/accounts', { params }),

  listAccountLeases: (): Promise<SupplyAccountLeaseItem[]> =>
    apiClient.get('/supply/account-leases'),

  listRecoveries: (params: { limit?: number; status?: string } = {}): Promise<SupplyRecovery[]> =>
    apiClient.get('/supply/recoveries', { params }),

  syncRecoveries: (payload: SupplyRecoverySyncRequest = {}): Promise<SupplyRecoverySummary> =>
    apiClient.post('/supply/recoveries/sync', payload),

  claimRecovery: (recoveryId: string): Promise<SupplyRecoverySummary> =>
    apiClient.post(`/supply/recoveries/${encodeURIComponent(recoveryId)}/claim`),

  retryRecoveryImport: (recoveryId: string): Promise<SupplyRecovery> =>
    apiClient.post(`/supply/recoveries/${encodeURIComponent(recoveryId)}/retry-import`),

  replenish: (quantity: number): Promise<SupplyStatus> =>
    apiClient.post('/supply/replenish', { quantity }),

  dismissCreateUncertain: (orderId: string): Promise<SupplyStatus> =>
    apiClient.post(`/supply/orders/${encodeURIComponent(orderId)}/dismiss-uncertain`),
};
