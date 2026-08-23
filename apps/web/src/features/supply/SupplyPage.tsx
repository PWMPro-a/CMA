import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import { SegmentedTabs, type SegmentedTabItem } from '@/components/ui/SegmentedTabs';
import { Select } from '@/components/ui/Select';
import { ToggleSwitch } from '@/components/ui/ToggleSwitch';
import {
  IconDatabaseZap,
  IconDollarSign,
  IconInbox,
  IconPlus,
  IconRefreshCw,
  IconTimer,
  IconTrash2,
  IconTrendingUp,
  IconX,
} from '@/components/ui/icons';
import {
  supplyApi,
  type SupplyAccountList,
  type SupplyActiveOrderStatus,
  type SupplyConfig,
  type SupplyOrder,
  type SupplyPlatformConfig,
  type SupplyPlatformOverview,
  type SupplyPlatformProduct,
  type SupplyPlatformProductCatalog,
  type SupplyPurchaseTask,
  type SupplyQuotaEstimationPolicy,
  type SupplyReport,
  type SupplyReportDimensionStat,
  type SupplyReportUsageModelStat,
  type SupplyRecovery,
  type SupplySmartResource,
  type SupplyStatus,
  type SupplyStrategy,
  type SupplySupplierQuotaScore,
} from '@/services/api';
import { useNotificationStore } from '@/stores';
import { resolveSupplyPoolAccountStats } from './model/poolAccountStats';
import { reconcileLiveQuotaPlanAccounts } from './model/quotaPlanEstimates';
import { resolvePurchasePlatformLabel } from './model/purchasePlatform';
import { isSupplyRuntimeErrorRetrying, localizeSupplyRuntimeError } from './model/runtimeError';
import styles from './SupplyPage.module.scss';

const DEFAULT_QUOTA_ESTIMATION_POLICIES: Record<string, SupplyQuotaEstimationPolicy> = {
  team: { mode: 'auto', fallbackM: 60, fixedM: 60 },
  plus: { mode: 'auto', fallbackM: 160, fixedM: 160 },
  pro: { mode: 'auto', fallbackM: 3000, fixedM: 3000 },
  free: { mode: 'auto', fallbackM: 10, fixedM: 10 },
};

const PLATFORM_CATALOG_REFRESH_INTERVAL_MS = 10_000;

const platformCatalogIdentity = (platform: SupplyPlatformConfig) =>
  [
    platform.id.trim().toLowerCase(),
    platform.type.trim().toLowerCase(),
    platform.baseUrl.trim(),
    platform.username?.trim() ?? '',
    platform.password ?? '',
    platform.passwordConfigured === true ? '1' : '0',
    platform.token?.trim() ?? '',
    platform.tokenConfigured === true ? '1' : '0',
    platform.product.trim().toLowerCase(),
    platform.supplierQuotaGateEnabled === true ? '1' : '0',
    String(platform.supplierQuotaMinimumM ?? 30),
    String(platform.supplierQuotaTrialQuantity ?? 1),
  ].join('\u0000');

const canLoadPlatformCatalog = (platform: SupplyPlatformConfig) => {
  if (platform.type !== 'nvtokens' || platform.enabled === false || !platform.baseUrl.trim()) {
    return false;
  }
  return (
    Boolean(platform.token?.trim() || platform.tokenConfigured) ||
    (Boolean(platform.username?.trim()) &&
      Boolean(platform.password?.trim() || platform.passwordConfigured))
  );
};

const newSupplyPlatform = (
  type: 'legacy' | 'bugteam' | 'nvtokens',
  index: number
): SupplyPlatformConfig => ({
  id: `${type}-${Date.now().toString(36)}-${index + 1}`,
  name: type === 'bugteam' ? 'BugTeam' : type === 'nvtokens' ? 'nvtokens' : `Supply ${index + 1}`,
  type,
  enabled: true,
  baseUrl:
    type === 'bugteam'
      ? 'https://bugteam.team'
      : type === 'nvtokens'
        ? 'https://nvtokens.com'
        : 'https://sogouedu.cc',
  username: '',
  clearUsername: false,
  password: '',
  passwordConfigured: false,
  token: '',
  tokenConfigured: false,
  product: type === 'bugteam' ? 'team_1h' : type === 'nvtokens' ? 'plus' : 'oauth_30d',
  purchaseAccountType: type === 'nvtokens' ? 'all' : undefined,
  maxUnitPriceFen: type === 'nvtokens' ? 0 : undefined,
  supplierQuotaGateEnabled: type === 'nvtokens' ? false : undefined,
  supplierQuotaMinimumM: type === 'nvtokens' ? 30 : undefined,
  supplierQuotaTrialQuantity: type === 'nvtokens' ? 1 : undefined,
  sessionRefreshEnabled: type === 'nvtokens' ? false : undefined,
  challengeProvider: type === 'nvtokens' ? 'capsolver' : undefined,
  challengeApiBase: type === 'nvtokens' ? 'https://api.capsolver.com' : undefined,
  challengeApiKey: '',
  challengeApiKeyConfigured: false,
  clearChallengeApiKey: false,
  refreshCooldownSeconds: type === 'nvtokens' ? 300 : undefined,
  priority: index + 1,
  emergencyOnly: type === 'bugteam',
  quotaEstimationPolicies: {},
});

const normalizeSupplyConfigForEditor = (config: SupplyConfig): SupplyConfig => {
  const configuredPlatforms = config.platforms;
  const platforms =
    configuredPlatforms !== undefined
      ? configuredPlatforms.map((platform) => ({
          ...platform,
          clearUsername: false,
          password: '',
          token: '',
          challengeApiKey: '',
          clearChallengeApiKey: false,
          purchaseAccountType:
            platform.type === 'nvtokens' ? platform.purchaseAccountType || 'all' : undefined,
          supplierQuotaGateEnabled:
            platform.type === 'nvtokens' ? platform.supplierQuotaGateEnabled === true : undefined,
          supplierQuotaMinimumM:
            platform.type === 'nvtokens' ? platform.supplierQuotaMinimumM || 30 : undefined,
          supplierQuotaTrialQuantity:
            platform.type === 'nvtokens' ? platform.supplierQuotaTrialQuantity || 1 : undefined,
        }))
      : config.baseUrl || config.username || config.passwordConfigured
        ? [
            {
              id: 'legacy',
              name: 'Legacy supplier',
              type: 'legacy',
              enabled: true,
              baseUrl: config.baseUrl,
              username: config.username,
              clearUsername: false,
              password: '',
              passwordConfigured: config.passwordConfigured,
              product: config.product,
              priority: 1,
              quotaEstimationPolicies: {},
            },
          ]
        : [];
  return { ...config, password: '', platforms };
};

const emptyConfig: SupplyConfig = {
  enabled: false,
  baseUrl: 'https://sogouedu.cc',
  username: '',
  clearUsername: false,
  password: '',
  passwordConfigured: false,
  product: 'oauth_30d',
  strategy: 'strong_supply',
  targetAvailableAccounts: 100,
  replenishBatchSize: 10,
  lowPriceReserveEnabled: false,
  lowPriceReserveMaxUnitPriceFen: 0,
  lowPriceReserveTargetAccounts: 200,
  lowPriceReserveCheckIntervalMilliseconds: 1000,
  maxConcurrentOrders: 3,
  checkIntervalSeconds: 60,
  pollIntervalSeconds: 3,
  defaultWebsockets: false,
  smartEnabled: true,
  healthyMinutesTarget: 120,
  warningMinutes: 60,
  criticalMinutes: 30,
  prelockEnabled: true,
  prelockMinQuantity: 1,
  prelockMaxQuantity: 10,
  criticalTakeConfirmRounds: 2,
  createCooldownSeconds: 120,
  releaseCooldownSeconds: 60,
  authFilesCacheTTLSeconds: 60,
  minHoldSeconds: 30,
  newAccountConfidence: 0.7,
  minBalanceReserveFen: 0,
  revenueMultiplier: 0.06,
  criticalAvailableAccounts: 2,
  healthyAvailableAccounts: 10,
  startupAvailableAccounts: 5,
  defaultEmergencyMinAccounts: 5,
  virtualDemandTtlMinutes: 60,
  accountMaxRequestsBefore401: 30,
  accountMaxUsefulSecondsBefore401: 120,
  emergencyBypassUsageRate: true,
  recoveryTriggerOn401: true,
  recoverySyncEnabled: true,
  recoveryAutoClaim: true,
  recoverySyncIntervalSeconds: 60,
  recoveryClaimBatchSize: 20,
  recoveryDisableOriginal: true,
  quotaEstimationPolicies: DEFAULT_QUOTA_ESTIMATION_POLICIES,
  platforms: [newSupplyPlatform('legacy', 0)],
  platformSelectionStrategy: 'best_available',
};

type SupplyWorkspaceTab =
  | 'overview'
  | 'automation'
  | 'orders'
  | 'accounts'
  | 'recoveries'
  | 'reports'
  | 'history';
type ReportRangePreset = 'today' | 'yesterday' | 'last7' | 'last30';
type AccountStatusFilter =
  | 'all'
  | 'active'
  | 'imported'
  | 'cooldown'
  | 'disabled'
  | 'expired'
  | 'missing'
  | 'pending'
  | 'failed'
  | 'unknown';

const SUPPLY_STRATEGIES: SupplyStrategy[] = ['strong_supply', 'balanced', 'cost_first', 'custom'];

const SUPPLY_STRATEGY_PRESETS: Record<
  Exclude<SupplyStrategy, 'custom'>,
  Pick<
    SupplyConfig,
    | 'criticalAvailableAccounts'
    | 'healthyAvailableAccounts'
    | 'defaultEmergencyMinAccounts'
    | 'virtualDemandTtlMinutes'
    | 'accountMaxRequestsBefore401'
    | 'accountMaxUsefulSecondsBefore401'
    | 'emergencyBypassUsageRate'
    | 'recoveryTriggerOn401'
  >
> = {
  strong_supply: {
    criticalAvailableAccounts: 2,
    healthyAvailableAccounts: 10,
    defaultEmergencyMinAccounts: 5,
    virtualDemandTtlMinutes: 60,
    accountMaxRequestsBefore401: 30,
    accountMaxUsefulSecondsBefore401: 120,
    emergencyBypassUsageRate: true,
    recoveryTriggerOn401: true,
  },
  balanced: {
    criticalAvailableAccounts: 1,
    healthyAvailableAccounts: 5,
    defaultEmergencyMinAccounts: 3,
    virtualDemandTtlMinutes: 30,
    accountMaxRequestsBefore401: 40,
    accountMaxUsefulSecondsBefore401: 150,
    emergencyBypassUsageRate: true,
    recoveryTriggerOn401: true,
  },
  cost_first: {
    criticalAvailableAccounts: 0,
    healthyAvailableAccounts: 2,
    defaultEmergencyMinAccounts: 1,
    virtualDemandTtlMinutes: 15,
    accountMaxRequestsBefore401: 50,
    accountMaxUsefulSecondsBefore401: 180,
    emergencyBypassUsageRate: true,
    recoveryTriggerOn401: true,
  },
};

const REPORT_RANGE_PRESETS: Array<{ id: ReportRangePreset; labelKey: string }> = [
  { id: 'today', labelKey: 'supply.report_range_today' },
  { id: 'yesterday', labelKey: 'supply.report_range_yesterday' },
  { id: 'last7', labelKey: 'supply.report_range_last7' },
  { id: 'last30', labelKey: 'supply.report_range_last30' },
];

const ACCOUNT_STATUS_FILTERS: Array<{ id: AccountStatusFilter; labelKey: string }> = [
  { id: 'all', labelKey: 'supply.account_filter_all' },
  { id: 'active', labelKey: 'supply.account_status_active' },
  { id: 'imported', labelKey: 'supply.account_status_imported' },
  { id: 'cooldown', labelKey: 'supply.account_status_cooldown' },
  { id: 'disabled', labelKey: 'supply.account_status_disabled' },
  { id: 'expired', labelKey: 'supply.account_status_expired' },
  { id: 'missing', labelKey: 'supply.account_status_missing' },
  { id: 'pending', labelKey: 'supply.account_status_pending' },
  { id: 'failed', labelKey: 'supply.account_status_failed' },
  { id: 'unknown', labelKey: 'supply.account_status_unknown' },
];

const SUPPLY_AUTO_REFRESH_MS = 10_000;
const SUPPLY_ACTIVE_ORDER_REFRESH_MS = 2_000;
const SUPPLY_REPORT_REFRESH_MS = 60_000;

const startOfLocalDay = (value: Date) => {
  const next = new Date(value);
  next.setHours(0, 0, 0, 0);
  return next;
};

const addLocalDays = (value: Date, days: number) => {
  const next = new Date(value);
  next.setDate(next.getDate() + days);
  return next;
};

const reportRangeForPreset = (preset: ReportRangePreset, now = new Date()) => {
  const todayStart = startOfLocalDay(now);
  const currentMs = Math.max(now.getTime(), todayStart.getTime() + 1);
  switch (preset) {
    case 'yesterday': {
      const yesterdayStart = addLocalDays(todayStart, -1);
      return { fromMs: yesterdayStart.getTime(), toMs: todayStart.getTime() };
    }
    case 'last7':
      return { fromMs: addLocalDays(todayStart, -6).getTime(), toMs: currentMs };
    case 'last30':
      return { fromMs: addLocalDays(todayStart, -29).getTime(), toMs: currentMs };
    case 'today':
    default:
      return { fromMs: todayStart.getTime(), toMs: currentMs };
  }
};

const formatMoney = (fen?: number) => `¥${((fen ?? 0) / 100).toFixed(2)}`;

const formatCostMultiplier = (value?: number) =>
  typeof value === 'number' && Number.isFinite(value) && value > 0
    ? `¥${value.toFixed(3)} / M`
    : '-';

const hasSupplierCost = (basePriceFen?: number, chargedFen?: number) =>
  (basePriceFen ?? 0) > 0 || (chargedFen ?? 0) > 0;

const formatMultiplier = (value?: number) => {
  const multiplier =
    typeof value === 'number' && Number.isFinite(value) && value > 0 ? value : 0.06;
  return `${multiplier.toLocaleString(undefined, { maximumFractionDigits: 6 })}x`;
};

const formatUsd = (value?: number) =>
  typeof value === 'number' && Number.isFinite(value) ? `$${value.toFixed(2)}` : '$0.00';

const formatNumber = (value?: number, digits = 1) =>
  typeof value === 'number' && Number.isFinite(value) ? value.toFixed(digits) : '-';

const formatInteger = (value?: number) =>
  typeof value === 'number' && Number.isFinite(value) ? value.toLocaleString() : '-';

const formatPercent = (value?: number) =>
  typeof value === 'number' && Number.isFinite(value) ? `${(value * 100).toFixed(1)}%` : '-';

const formatTokens = (value?: number) => {
  if (typeof value !== 'number' || !Number.isFinite(value)) return '-';
  if (Math.abs(value) >= 1_000_000) return `${(value / 1_000_000).toFixed(2)}M`;
  if (Math.abs(value) >= 1_000) return `${(value / 1_000).toFixed(1)}K`;
  return value.toLocaleString();
};

const formatSeconds = (value?: number) => {
  if (typeof value !== 'number' || !Number.isFinite(value) || value <= 0) return '-';
  if (value >= 3600) return `${(value / 3600).toFixed(1)}h`;
  if (value >= 60) return `${(value / 60).toFixed(1)}m`;
  return `${value.toFixed(0)}s`;
};

const formatTokenM = (value?: number, digits = 1) =>
  typeof value === 'number' && Number.isFinite(value) ? `${value.toFixed(digits)} M` : '-';

const formatTokenMRate = (value?: number) =>
  typeof value === 'number' && Number.isFinite(value) ? `${value.toFixed(2)} M/min` : '-';

const supplierQuotaStatusClass = (score: SupplySupplierQuotaScore) => {
  switch (score.status) {
    case 'approved':
      return styles.supplierQuotaStatusApproved;
    case 'blocked':
      return styles.supplierQuotaStatusBlocked;
    case 'observing':
      return styles.supplierQuotaStatusObserving;
    default:
      return styles.supplierQuotaStatusUntried;
  }
};

const formatMinutes = (value?: number) => {
  if (typeof value !== 'number' || !Number.isFinite(value) || value <= 0) return '-';
  if (value >= 1440) return `${(value / 1440).toFixed(1)}d`;
  if (value >= 60) return `${(value / 60).toFixed(1)}h`;
  return `${value.toFixed(1)}m`;
};

const formatTime = (value?: number) =>
  value && value > 0 ? new Date(value).toLocaleString() : '-';

const formatCountdown = (targetMs?: number, nowMs = Date.now()) => {
  if (!targetMs || targetMs <= 0) return '-';
  const totalSeconds = Math.max(0, Math.ceil((targetMs - nowMs) / 1000));
  const hours = Math.floor(totalSeconds / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = totalSeconds % 60;
  if (hours > 0) {
    return `${String(hours).padStart(2, '0')}:${String(minutes).padStart(2, '0')}:${String(seconds).padStart(2, '0')}`;
  }
  return `${String(minutes).padStart(2, '0')}:${String(seconds).padStart(2, '0')}`;
};

const clampPercent = (value: number) => Math.min(100, Math.max(0, value));

const shortOrderId = (value?: string) => {
  if (!value) return '-';
  return value.length > 10 ? `…${value.slice(-8)}` : value;
};

const purchaseTaskRemaining = (task: SupplyPurchaseTask) =>
  Math.max(0, task.targetQuantity - task.fulfilledQuantity);

const purchaseTaskActive = (task: SupplyPurchaseTask) =>
  task.status === 'pending' || task.status === 'running';

const orderTone = (status: string) => {
  if (status === 'completed' || status === 'released' || status === 'imported')
    return styles.success;
  if (status === 'failed' || status === 'cancelled' || status === 'dismissed') return styles.error;
  if (
    status === 'partial' ||
    status === 'completed_partial' ||
    status === 'recovery_partial' ||
    status === 'create_uncertain' ||
    status === 'retry_scheduled' ||
    status === 'claimed_waiting_task' ||
    status === 'claimed_without_local_payload' ||
    status === 'not_this_pool' ||
    status === 'ownership_unknown' ||
    status === 'refunded'
  )
    return styles.warning;
  return styles.active;
};

const accountTone = (status: string) => {
  if (status === 'active' || status === 'imported') return styles.success;
  if (status === 'disabled' || status === 'missing' || status === 'failed') return styles.error;
  if (status === 'cooldown' || status === 'expired' || status === 'pending' || status === 'unknown')
    return styles.warning;
  return styles.active;
};

const smartTone = (resource?: SupplySmartResource) => {
  if (!resource?.enabled) return styles.warning;
  if (!resource.snapshotFresh) return styles.warning;
  if (resource.emergencyShortage || resource.suggestedAction === 'emergency_replenish')
    return styles.error;
  if (resource.healthLevel === 'healthy') return styles.success;
  if (resource.healthLevel === 'critical') return styles.error;
  if (resource.healthLevel === 'warning') return styles.warning;
  return styles.active;
};

const smartPanelTone = (resource?: SupplySmartResource) => {
  if (!resource?.enabled || !resource.snapshotFresh) return styles.smartPanelWarning;
  if (resource.emergencyShortage || resource.suggestedAction === 'emergency_replenish')
    return styles.smartPanelCritical;
  if (resource.healthLevel === 'healthy') return styles.smartPanelHealthy;
  if (resource.healthLevel === 'critical') return styles.smartPanelCritical;
  if (resource.healthLevel === 'warning') return styles.smartPanelWarning;
  return styles.smartPanelUnknown;
};

const snapshotLabelKey = (resource?: SupplySmartResource) => {
  if (!resource) return 'supply.no_snapshot';
  if (resource.snapshotFresh) return 'supply.snapshot_fresh';
  if (resource.snapshotRefreshInProgress) return 'supply.snapshot_refreshing';
  if (
    resource.decisionReason === 'snapshot_not_ready' ||
    resource.decisionReason === 'inspection_snapshot_unavailable' ||
    resource.decisionReason === 'inspection_snapshot_incomplete' ||
    resource.decisionReason === 'usage_rate_not_ready'
  ) {
    return 'supply.snapshot_not_ready';
  }
  if (resource.decisionReason === 'using_stale_inspection_snapshot') {
    return 'supply.snapshot_read_failed';
  }
  if (
    resource.decisionReason.startsWith('inspection_quota_incomplete') ||
    resource.decisionReason.startsWith('inspection_usability_incomplete') ||
    resource.pendingInspectionAccounts
  ) {
    if (resource.capacityCoverage > 0 && resource.capacityCoverage < 100) {
      return 'supply.snapshot_coverage_pending';
    }
    return 'supply.snapshot_partial';
  }
  return 'supply.snapshot_stale';
};

export function SupplyPage() {
  const { t } = useTranslation();
  const localizeRuntimeError = useCallback(
    (value?: string | null) => localizeSupplyRuntimeError(value, t),
    [t]
  );
  const { showNotification, showConfirmation } = useNotificationStore();
  const [status, setStatus] = useState<SupplyStatus | null>(null);
  const [draft, setDraft] = useState<SupplyConfig>(emptyConfig);
  const [accounts, setAccounts] = useState<SupplyAccountList | null>(null);
  const [accountsLoading, setAccountsLoading] = useState(false);
  const [accountStatusFilter, setAccountStatusFilter] = useState<AccountStatusFilter>('all');
  const [recoveries, setRecoveries] = useState<SupplyRecovery[]>([]);
  const [recoveriesLoading, setRecoveriesLoading] = useState(false);
  const [report, setReport] = useState<SupplyReport | null>(null);
  const [reportLoading, setReportLoading] = useState(false);
  const [manualQuantity, setManualQuantity] = useState(10);
  const [manualSupplierId, setManualSupplierId] = useState('');
  const [manualProduct, setManualProduct] = useState('');
  const [manualQuote, setManualQuote] = useState<SupplyPlatformOverview | null>(null);
  const [manualQuoteLoading, setManualQuoteLoading] = useState(false);
  const [manualQuoteError, setManualQuoteError] = useState('');
  const [platformCatalogs, setPlatformCatalogs] = useState<
    Record<string, { loading: boolean; error?: string; catalog?: SupplyPlatformProductCatalog }>
  >({});
  const [cancellingTaskId, setCancellingTaskId] = useState('');
  const [refreshingPlatformId, setRefreshingPlatformId] = useState('');
  const [loading, setLoading] = useState(true);
  const [activeTab, setActiveTab] = useState<SupplyWorkspaceTab>('overview');
  const [reportRangePreset, setReportRangePreset] = useState<ReportRangePreset>('today');
  const [nowMs, setNowMs] = useState(() => Date.now());
  const [action, setAction] = useState<
    | 'save'
    | 'check'
    | 'replenish'
    | 'dismiss'
    | 'syncRecoveries'
    | 'claimRecovery'
    | 'retryRecoveryImport'
    | null
  >(null);
  const configDirtyRef = useRef(false);
  const loadInFlightRef = useRef(false);
  const activeOrderLoadInFlightRef = useRef(false);
  const actionInFlightRef = useRef(false);
  const refreshGenerationRef = useRef(0);
  const catalogLoadedIdentityRef = useRef(new Map<string, string>());
  const catalogInFlightRef = useRef(new Set<string>());
  const catalogLatestRequestRef = useRef(new Map<string, number>());
  const catalogRequestSequenceRef = useRef(0);
  const catalogPlatformsRef = useRef<SupplyPlatformConfig[]>([]);
  const manualPlatformConfigRef = useRef<SupplyPlatformConfig | undefined>(undefined);
  const hasActiveOrder = Boolean(status?.activeOrder);
  const catalogRefreshIdentity = useMemo(
    () =>
      (draft.platforms ?? [])
        .filter(canLoadPlatformCatalog)
        .map(platformCatalogIdentity)
        .join('\u0001'),
    [draft.platforms]
  );
  catalogPlatformsRef.current = draft.platforms ?? [];

  const updateDraft = useCallback((patch: Partial<SupplyConfig>) => {
    configDirtyRef.current = true;
    setDraft((current) => ({ ...current, ...patch }));
  }, []);

  const updateQuotaEstimationPolicy = useCallback(
    (planType: string, patch: Partial<SupplyQuotaEstimationPolicy>) => {
      configDirtyRef.current = true;
      setDraft((current) => {
        const normalizedPlan = planType.trim().toLowerCase();
        const defaultPolicy = DEFAULT_QUOTA_ESTIMATION_POLICIES[normalizedPlan] ?? {
          mode: 'auto',
          fallbackM: 10,
          fixedM: 10,
        };
        const currentPolicies = current.quotaEstimationPolicies ?? {};
        return {
          ...current,
          quotaEstimationPolicies: {
            ...currentPolicies,
            [normalizedPlan]: {
              ...defaultPolicy,
              ...currentPolicies[normalizedPlan],
              ...patch,
            },
          },
        };
      });
    },
    []
  );

  const updateSupplyPlatform = useCallback(
    (index: number, patch: Partial<SupplyPlatformConfig>) => {
      configDirtyRef.current = true;
      const catalogIdentityChanged =
        patch.type !== undefined ||
        patch.baseUrl !== undefined ||
        patch.username !== undefined ||
        patch.password !== undefined ||
        patch.token !== undefined;
      const existingCatalogKey = draft.platforms?.[index]?.id.trim().toLowerCase();
      if (catalogIdentityChanged && existingCatalogKey) {
        catalogLoadedIdentityRef.current.delete(existingCatalogKey);
        setPlatformCatalogs((catalogs) => {
          const updated = { ...catalogs };
          delete updated[existingCatalogKey];
          return updated;
        });
      }
      setDraft((current) => {
        const platforms = [...(current.platforms ?? [])];
        const existing = platforms[index];
        if (!existing) return current;
        let next = { ...existing, ...patch };
        if (patch.type === 'bugteam') {
          next = {
            ...next,
            id: `bugteam-${Date.now().toString(36)}-${index + 1}`,
            type: 'bugteam',
            baseUrl: 'https://bugteam.team',
            product: 'team_1h',
            emergencyOnly: true,
            username: '',
            clearUsername: false,
            password: '',
            passwordConfigured: false,
            token: '',
            tokenConfigured: false,
            purchaseAccountType: undefined,
            maxUnitPriceFen: undefined,
            supplierQuotaGateEnabled: undefined,
            supplierQuotaMinimumM: undefined,
            supplierQuotaTrialQuantity: undefined,
            sessionRefreshEnabled: undefined,
            challengeProvider: undefined,
            challengeApiBase: undefined,
            challengeApiKey: '',
            challengeApiKeyConfigured: false,
            clearChallengeApiKey: false,
            refreshCooldownSeconds: undefined,
          };
        } else if (patch.type === 'nvtokens') {
          next = {
            ...next,
            id: `nvtokens-${Date.now().toString(36)}-${index + 1}`,
            type: 'nvtokens',
            baseUrl: 'https://nvtokens.com',
            product: 'plus',
            emergencyOnly: false,
            username: '',
            clearUsername: false,
            password: '',
            passwordConfigured: false,
            token: '',
            tokenConfigured: false,
            purchaseAccountType: 'all',
            maxUnitPriceFen: 0,
            supplierQuotaGateEnabled: false,
            supplierQuotaMinimumM: 30,
            supplierQuotaTrialQuantity: 1,
            sessionRefreshEnabled: false,
            challengeProvider: 'capsolver',
            challengeApiBase: 'https://api.capsolver.com',
            challengeApiKey: '',
            challengeApiKeyConfigured: false,
            clearChallengeApiKey: false,
            refreshCooldownSeconds: 300,
          };
        } else if (patch.type === 'legacy') {
          next = {
            ...next,
            id: `legacy-${Date.now().toString(36)}-${index + 1}`,
            type: 'legacy',
            baseUrl: 'https://sogouedu.cc',
            product: 'oauth_30d',
            emergencyOnly: false,
            username: '',
            clearUsername: false,
            password: '',
            passwordConfigured: false,
            token: '',
            tokenConfigured: false,
            purchaseAccountType: undefined,
            maxUnitPriceFen: undefined,
            supplierQuotaGateEnabled: undefined,
            supplierQuotaMinimumM: undefined,
            supplierQuotaTrialQuantity: undefined,
            sessionRefreshEnabled: undefined,
            challengeProvider: undefined,
            challengeApiBase: undefined,
            challengeApiKey: '',
            challengeApiKeyConfigured: false,
            clearChallengeApiKey: false,
            refreshCooldownSeconds: undefined,
          };
        }
        platforms[index] = next;

        return { ...current, platforms };
      });
    },
    [draft.platforms]
  );

  const updatePlatformQuotaEstimationPolicy = useCallback(
    (index: number, planType: string, patch: Partial<SupplyQuotaEstimationPolicy>) => {
      configDirtyRef.current = true;
      setDraft((current) => {
        const platforms = [...(current.platforms ?? [])];
        const existing = platforms[index];
        if (!existing) return current;
        const normalizedPlan = planType.trim().toLowerCase();
        const globalPolicy = current.quotaEstimationPolicies?.[normalizedPlan];
        const defaultPolicy = globalPolicy ??
          DEFAULT_QUOTA_ESTIMATION_POLICIES[normalizedPlan] ?? {
            mode: 'auto',
            fallbackM: 10,
            fixedM: 10,
          };
        platforms[index] = {
          ...existing,
          quotaEstimationPolicies: {
            ...(existing.quotaEstimationPolicies ?? {}),
            [normalizedPlan]: {
              ...defaultPolicy,
              ...existing.quotaEstimationPolicies?.[normalizedPlan],
              ...patch,
            },
          },
        };
        return { ...current, platforms };
      });
    },
    []
  );

  const addSupplyPlatform = useCallback(() => {
    configDirtyRef.current = true;
    setDraft((current) => {
      const platforms = current.platforms ?? [];
      return {
        ...current,
        platforms: [...platforms, newSupplyPlatform('bugteam', platforms.length)],
      };
    });
  }, []);

  const removeSupplyPlatform = useCallback((index: number) => {
    configDirtyRef.current = true;
    setDraft((current) => ({
      ...current,
      platforms: (current.platforms ?? []).filter((_, platformIndex) => platformIndex !== index),
    }));
  }, []);

  const loadPlatformCatalog = useCallback(
    async (platform: SupplyPlatformConfig, force = false) => {
      const key = platform.id.trim().toLowerCase();
      const identity = platformCatalogIdentity(platform);
      if (
        !key ||
        catalogInFlightRef.current.has(identity) ||
        (!force && catalogLoadedIdentityRef.current.get(key) === identity)
      ) {
        return;
      }
      catalogInFlightRef.current.add(identity);
      const requestId = ++catalogRequestSequenceRef.current;
      catalogLatestRequestRef.current.set(key, requestId);
      setPlatformCatalogs((current) => ({
        ...current,
        [key]: { ...current[key], loading: true, error: undefined },
      }));
      try {
        const catalog = await supplyApi.getPlatformCatalog(platform);
        if (catalogLatestRequestRef.current.get(key) !== requestId) return;
        catalogLoadedIdentityRef.current.set(key, identity);
        setPlatformCatalogs((current) => ({
          ...current,
          [key]: { loading: false, catalog },
        }));
      } catch (error) {
        if (catalogLatestRequestRef.current.get(key) !== requestId) return;
        catalogLoadedIdentityRef.current.delete(key);
        setPlatformCatalogs((current) => ({
          ...current,
          [key]: {
            ...current[key],
            loading: false,
            error: error instanceof Error ? error.message : t('common.unknown_error'),
          },
        }));
      } finally {
        catalogInFlightRef.current.delete(identity);
      }
    },
    [t]
  );

  useEffect(() => {
    for (const platform of draft.platforms ?? []) {
      if (canLoadPlatformCatalog(platform)) {
        void loadPlatformCatalog(platform);
      }
    }
  }, [draft.platforms, loadPlatformCatalog]);

  useEffect(() => {
    if (activeTab !== 'automation' && activeTab !== 'orders') return undefined;

    const refreshCatalogs = () => {
      if (document.visibilityState !== 'visible') return;
      for (const platform of catalogPlatformsRef.current) {
        if (canLoadPlatformCatalog(platform)) {
          void loadPlatformCatalog(platform, true);
        }
      }
    };

    refreshCatalogs();
    const timer = window.setInterval(refreshCatalogs, PLATFORM_CATALOG_REFRESH_INTERVAL_MS);
    document.addEventListener('visibilitychange', refreshCatalogs);
    return () => {
      window.clearInterval(timer);
      document.removeEventListener('visibilitychange', refreshCatalogs);
    };
  }, [activeTab, catalogRefreshIdentity, loadPlatformCatalog]);

  const selectSupplyStrategy = useCallback((strategy: SupplyStrategy) => {
    configDirtyRef.current = true;
    setDraft((current) => ({
      ...current,
      ...(strategy === 'custom' ? {} : SUPPLY_STRATEGY_PRESETS[strategy]),
      strategy,
    }));
  }, []);

  const applyStatus = useCallback((next: SupplyStatus) => {
    setStatus(next);
    if (!configDirtyRef.current && next.config) {
      setDraft(normalizeSupplyConfigForEditor(next.config));
    }
    setManualQuantity((current) =>
      current > 0 ? current : Math.max(1, next.config?.replenishBatchSize || 10)
    );
    setManualSupplierId((current) => {
      const platforms = (next.config?.platforms ?? []).filter(
        (platform) => platform.enabled !== false
      );
      const configuredIds = new Map(
        platforms.map((platform) => [platform.id.trim().toLowerCase(), platform.id])
      );
      const currentID = current.trim().toLowerCase();
      if (currentID && configuredIds.has(currentID)) {
        return configuredIds.get(currentID) ?? current;
      }
      const recommendedID = next.overview?.selectedPlatformId?.trim().toLowerCase() ?? '';
      if (recommendedID && configuredIds.has(recommendedID)) {
        return configuredIds.get(recommendedID) ?? '';
      }
      return platforms[0]?.id ?? next.overview?.platforms?.[0]?.id ?? '';
    });
  }, []);

  const load = useCallback(
    async (quiet = false, force = false) => {
      // Polling must never overlap an ongoing request or a state-changing
      // operation. Otherwise an earlier response can overwrite the newer
      // order/check result and make the workspace appear to jump backwards.
      if (loadInFlightRef.current || (quiet && actionInFlightRef.current && !force)) return;
      loadInFlightRef.current = true;
      const generation = ++refreshGenerationRef.current;
      if (!quiet) setLoading(true);
      try {
        const next = await supplyApi.getStatus();
        if (generation === refreshGenerationRef.current) {
          applyStatus(next);
        }
      } catch (error) {
        if (!quiet) {
          showNotification(
            error instanceof Error ? error.message : t('supply.load_failed'),
            'error'
          );
        }
      } finally {
        loadInFlightRef.current = false;
        if (!quiet) setLoading(false);
      }
    },
    [applyStatus, showNotification, t]
  );

  const refreshPlatformSession = useCallback(
    async (platformId: string) => {
      setRefreshingPlatformId(platformId);
      try {
        await supplyApi.refreshPlatformSession(platformId);
        catalogLoadedIdentityRef.current.delete(platformId.trim().toLowerCase());
        await load(true, true);
        showNotification(t('supply.session_refresh_success'), 'success');
      } catch (error) {
        showNotification(
          error instanceof Error ? error.message : t('common.unknown_error'),
          'error'
        );
      } finally {
        setRefreshingPlatformId('');
      }
    },
    [load, showNotification, t]
  );

  const applyActiveOrderStatus = useCallback((snapshot: SupplyActiveOrderStatus) => {
    setStatus((current) => {
      if (!current) return current;
      const activeOrder = snapshot.activeOrder;
      const activeOrders = snapshot.activeOrders ?? (activeOrder ? [activeOrder] : []);
      let orders = current.orders ?? [];
      for (const nextOrder of activeOrders) {
        let replaced = false;
        orders = orders.map((order) => {
          if (order.orderId !== nextOrder.orderId) return order;
          replaced = true;
          return nextOrder;
        });
        if (!replaced) orders = [nextOrder, ...orders];
      }
      return { ...current, activeOrder, activeOrders, orders };
    });
  }, []);

  const loadActiveOrder = useCallback(async () => {
    if (
      activeOrderLoadInFlightRef.current ||
      loadInFlightRef.current ||
      actionInFlightRef.current ||
      !hasActiveOrder
    ) {
      return;
    }
    activeOrderLoadInFlightRef.current = true;
    try {
      const snapshot = await supplyApi.getActiveOrder();
      if (!actionInFlightRef.current) applyActiveOrderStatus(snapshot);
    } catch {
      // The next full status poll remains the recovery path. A transient fast
      // status error should not interrupt the operator's current workflow.
    } finally {
      activeOrderLoadInFlightRef.current = false;
    }
  }, [applyActiveOrderStatus, hasActiveOrder]);

  const loadRecoveries = useCallback(
    async (quiet = false) => {
      if (!quiet) setRecoveriesLoading(true);
      try {
        const items = await supplyApi.listRecoveries({ limit: 100 });
        setRecoveries(items);
      } catch (error) {
        if (!quiet) {
          showNotification(
            error instanceof Error ? error.message : t('supply.recovery_load_failed'),
            'error'
          );
        }
      } finally {
        if (!quiet) setRecoveriesLoading(false);
      }
    },
    [showNotification, t]
  );

  const loadReport = useCallback(
    async (quiet = false) => {
      if (!quiet) setReportLoading(true);
      try {
        const { fromMs, toMs } = reportRangeForPreset(reportRangePreset);
        const next = await supplyApi.getReport({ fromMs, toMs });
        setReport(next);
      } catch (error) {
        if (!quiet) {
          showNotification(
            error instanceof Error ? error.message : t('supply.report_load_failed'),
            'error'
          );
        }
      } finally {
        if (!quiet) setReportLoading(false);
      }
    },
    [reportRangePreset, showNotification, t]
  );

  const loadAccounts = useCallback(
    async (quiet = false) => {
      if (!quiet) setAccountsLoading(true);
      try {
        const { fromMs, toMs } = reportRangeForPreset(reportRangePreset);
        const next = await supplyApi.listAccounts({
          fromMs,
          toMs,
          limit: 200,
          status: accountStatusFilter === 'all' ? undefined : accountStatusFilter,
        });
        setAccounts(next);
      } catch (error) {
        if (!quiet) {
          showNotification(
            error instanceof Error ? error.message : t('supply.accounts_load_failed'),
            'error'
          );
        }
      } finally {
        if (!quiet) setAccountsLoading(false);
      }
    },
    [accountStatusFilter, reportRangePreset, showNotification, t]
  );

  useEffect(() => {
    let disposed = false;
    let timer: number | undefined;

    const schedule = () => {
      timer = window.setTimeout(async () => {
        // Hidden tabs used to keep rebuilding the heaviest supply snapshot
        // every ten seconds. Resume through the visibility/focus handler
        // instead, so multiple parked management tabs do not amplify SQLite
        // read pressure while the page is not being observed.
        if (document.visibilityState !== 'hidden') {
          await load(true);
        }
        if (!disposed) schedule();
      }, SUPPLY_AUTO_REFRESH_MS);
    };

    void load();
    schedule();
    return () => {
      disposed = true;
      if (timer !== undefined) window.clearTimeout(timer);
    };
  }, [load]);

  useEffect(() => {
    const refreshVisibleSnapshot = () => {
      if (document.visibilityState === 'hidden') return;
      void load(true, true);
    };
    document.addEventListener('visibilitychange', refreshVisibleSnapshot);
    window.addEventListener('focus', refreshVisibleSnapshot);
    return () => {
      document.removeEventListener('visibilitychange', refreshVisibleSnapshot);
      window.removeEventListener('focus', refreshVisibleSnapshot);
    };
  }, [load]);

  useEffect(() => {
    if (!hasActiveOrder) return undefined;
    void loadActiveOrder();
    const timer = window.setInterval(() => {
      void loadActiveOrder();
    }, SUPPLY_ACTIVE_ORDER_REFRESH_MS);
    return () => window.clearInterval(timer);
  }, [hasActiveOrder, loadActiveOrder]);

  useEffect(() => {
    const timer = window.setInterval(() => setNowMs(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, []);

  useEffect(() => {
    if (activeTab !== 'recoveries') {
      return undefined;
    }
    void loadRecoveries(true);
    const timer = window.setInterval(() => {
      void loadRecoveries(true);
    }, SUPPLY_AUTO_REFRESH_MS);
    return () => window.clearInterval(timer);
  }, [activeTab, loadRecoveries]);

  useEffect(() => {
    if (activeTab !== 'accounts') {
      return undefined;
    }
    void loadAccounts(false);
    const timer = window.setInterval(() => {
      void loadAccounts(true);
    }, SUPPLY_REPORT_REFRESH_MS);
    return () => window.clearInterval(timer);
  }, [activeTab, loadAccounts]);

  useEffect(() => {
    if (activeTab !== 'reports') {
      return undefined;
    }
    void loadReport(false);
    const timer = window.setInterval(() => {
      void loadReport(true);
    }, SUPPLY_REPORT_REFRESH_MS);
    return () => window.clearInterval(timer);
  }, [activeTab, loadReport]);

  const runAction = async (
    kind: 'save' | 'check' | 'replenish' | 'dismiss',
    operation: () => Promise<SupplyStatus>,
    successMessage: string,
    refreshAfterSuccess = false
  ) => {
    // Invalidate a pending read before changing state. The action result is
    // authoritative and cannot be replaced by a response started earlier.
    refreshGenerationRef.current += 1;
    actionInFlightRef.current = true;
    setAction(kind);
    try {
      const result = await operation();
      if (kind === 'save') {
        configDirtyRef.current = false;
      }
      applyStatus(result);
      // Replenishment may create or advance an order while its action
      // response is being generated. Read the status again immediately so
      // capacity, inventory, balance and order cards show the latest state.
      if (refreshAfterSuccess) {
        await load(true, true);
      }
      showNotification(successMessage, 'success');
    } catch (error) {
      showNotification(error instanceof Error ? error.message : t('common.unknown_error'), 'error');
    } finally {
      actionInFlightRef.current = false;
      setAction(null);
    }
  };

  const save = () => runAction('save', () => supplyApi.saveConfig(draft), t('supply.save_success'));

  const toggleAutoSupply = (enabled: boolean) => {
    // Keep the header switch independent from unsaved fields in the
    // automation form: only the currently persisted configuration plus the
    // new enabled state is submitted.
    const current = status?.config ?? draft;
    const next = { ...current, enabled, password: '' };
    setDraft((previous) => ({ ...previous, enabled }));
    runAction(
      'save',
      () => supplyApi.saveConfig(next),
      enabled ? t('supply.auto_enabled') : t('supply.auto_disabled')
    );
  };

  const check = () => runAction('check', () => supplyApi.check(), t('supply.check_success'));
  const replenish = () =>
    runAction(
      'replenish',
      () => supplyApi.replenish(manualQuantity, manualSupplierId, manualProduct),
      t('supply.replenish_started'),
      true
    );

  const syncRecoveries = async () => {
    setAction('syncRecoveries');
    try {
      await supplyApi.syncRecoveries({ force: true, autoClaim: true, limit: 50 });
      await Promise.all([
        load(true, true),
        loadRecoveries(true),
        activeTab === 'accounts' ? loadAccounts(true) : Promise.resolve(),
        activeTab === 'reports' ? loadReport(true) : Promise.resolve(),
      ]);
      showNotification(t('supply.recovery_sync_success'), 'success');
    } catch (error) {
      showNotification(error instanceof Error ? error.message : t('common.unknown_error'), 'error');
    } finally {
      setAction(null);
    }
  };

  const claimRecovery = async (recoveryId: string) => {
    setAction('claimRecovery');
    try {
      await supplyApi.claimRecovery(recoveryId);
      await Promise.all([
        load(true, true),
        loadRecoveries(true),
        activeTab === 'accounts' ? loadAccounts(true) : Promise.resolve(),
        activeTab === 'reports' ? loadReport(true) : Promise.resolve(),
      ]);
      showNotification(t('supply.recovery_claim_success'), 'success');
    } catch (error) {
      showNotification(error instanceof Error ? error.message : t('common.unknown_error'), 'error');
    } finally {
      setAction(null);
    }
  };

  const retryRecoveryImport = async (recoveryId: string) => {
    setAction('retryRecoveryImport');
    try {
      await supplyApi.retryRecoveryImport(recoveryId);
      await Promise.all([
        load(true, true),
        loadRecoveries(true),
        activeTab === 'accounts' ? loadAccounts(true) : Promise.resolve(),
        activeTab === 'reports' ? loadReport(true) : Promise.resolve(),
      ]);
      showNotification(t('supply.recovery_import_retry_success'), 'success');
    } catch (error) {
      showNotification(error instanceof Error ? error.message : t('common.unknown_error'), 'error');
      await loadRecoveries(true);
    } finally {
      setAction(null);
    }
  };

  const dismissUncertain = (order: SupplyOrder) => {
    showConfirmation({
      title: t('supply.dismiss_uncertain_title'),
      message: t('supply.dismiss_uncertain_confirm', { orderId: order.orderId }),
      variant: 'danger',
      confirmText: t('supply.dismiss_uncertain_action'),
      onConfirm: () =>
        runAction(
          'dismiss',
          () => supplyApi.dismissCreateUncertain(order.orderId),
          t('supply.dismiss_uncertain_success')
        ),
    });
  };

  const cancelPurchaseTask = (task: SupplyPurchaseTask) => {
    showConfirmation({
      title: t('supply.purchase_task_cancel_title'),
      message: t('supply.purchase_task_cancel_confirm', {
        remaining: purchaseTaskRemaining(task),
      }),
      variant: 'danger',
      confirmText: t('supply.purchase_task_cancel_action'),
      onConfirm: async () => {
        setCancellingTaskId(task.taskId);
        try {
          const next = await supplyApi.cancelPurchaseTask(task.taskId);
          applyStatus(next);
          showNotification(t('supply.purchase_task_cancel_success'), 'success');
        } catch (error) {
          showNotification(
            error instanceof Error ? error.message : t('common.unknown_error'),
            'error'
          );
        } finally {
          setCancellingTaskId('');
        }
      },
    });
  };

  const overview = status?.overview;
  const inventory = overview?.inventory;
  const balance = overview?.balance;
  const smart = status?.smartResource;
  const platformOverviewById = useMemo(
    () =>
      new Map(
        (overview?.platforms ?? []).map((platform) => [platform.id.trim().toLowerCase(), platform])
      ),
    [overview?.platforms]
  );
  const configuredManualPlatforms = useMemo(
    () => (draft.platforms ?? []).filter((platform) => platform.enabled !== false),
    [draft.platforms]
  );
  const manualPlatformOptions = useMemo(
    () =>
      configuredManualPlatforms.map((platform) => {
        const live = platformOverviewById.get(platform.id.trim().toLowerCase());
        return {
          value: platform.id,
          label: `${platform.name || platform.id}${
            platform.emergencyOnly ? ` · ${t('supply.platform_emergency_only_short')}` : ''
          }${live?.lastError ? ` · ${t('supply.platform_quote_failed_short')}` : ''}`,
        };
      }),
    [configuredManualPlatforms, platformOverviewById, t]
  );
  const manualPlatformConfig = manualSupplierId
    ? configuredManualPlatforms.find(
        (platform) => platform.id.trim().toLowerCase() === manualSupplierId.trim().toLowerCase()
      )
    : undefined;
  manualPlatformConfigRef.current = manualPlatformConfig;
  const manualCatalogState = manualPlatformConfig
    ? platformCatalogs[manualPlatformConfig.id.trim().toLowerCase()]
    : undefined;
  const manualProductOptions = useMemo(() => {
    if (!manualPlatformConfig) return [];
    const products = manualCatalogState?.catalog?.products ?? [];
    const fallback: SupplyPlatformProduct[] =
      manualPlatformConfig.type === 'bugteam'
        ? [{ code: 'team_1h', label: t('supply.product_team_1h'), available: 0 }]
        : manualPlatformConfig.type === 'nvtokens'
          ? [
              {
                code: manualPlatformConfig.product || 'plus',
                label: manualPlatformConfig.product || 'Plus',
                available: 0,
              },
            ]
          : [
              { code: 'oauth_30d', label: t('supply.product_30d'), available: 0 },
              { code: 'oauth_7d', label: t('supply.product_7d'), available: 0 },
              { code: 'team_1h', label: t('supply.product_team_1h'), available: 0 },
            ];
    return (products.length > 0 ? products : fallback).map((product) => {
      const min = product.minUnitPriceFen ?? 0;
      const max = product.maxUnitPriceFen ?? 0;
      const price =
        min > 0 && max > min
          ? `${formatMoney(min)}–${formatMoney(max)}`
          : min > 0 || max > 0
            ? formatMoney(min || max)
            : '';
      return {
        value: product.code,
        label: `${product.label}${
          products.length > 0
            ? ` · ${t('supply.catalog_inventory', { value: product.available })}`
            : ''
        }${price ? ` · ${price}` : ''}`,
      };
    });
  }, [manualCatalogState?.catalog?.products, manualPlatformConfig, t]);
  useEffect(() => {
    const values = new Set(manualProductOptions.map((option) => option.value));
    if (manualProduct && values.has(manualProduct)) return;
    setManualProduct(
      values.has(manualPlatformConfig?.product ?? '')
        ? (manualPlatformConfig?.product ?? '')
        : (manualProductOptions[0]?.value ?? '')
    );
  }, [manualPlatformConfig?.product, manualProduct, manualProductOptions]);
  useEffect(() => {
    if (
      activeTab !== 'orders' ||
      !manualSupplierId ||
      !manualProduct ||
      manualQuantity < 1 ||
      manualQuantity > 10000
    ) {
      setManualQuote(null);
      setManualQuoteLoading(false);
      setManualQuoteError('');
      return undefined;
    }
    let cancelled = false;
    let refreshing = false;
    let hasQuote = false;
    let timer = 0;
    setManualQuote(null);
    setManualQuoteError('');

    const scheduleRefresh = (delay: number) => {
      window.clearTimeout(timer);
      timer = window.setTimeout(() => void refreshQuote(), delay);
    };
    const refreshQuote = async () => {
      if (cancelled || refreshing) return;
      if (document.visibilityState !== 'visible') {
        scheduleRefresh(PLATFORM_CATALOG_REFRESH_INTERVAL_MS);
        return;
      }
      refreshing = true;
      if (!hasQuote) setManualQuoteLoading(true);
      setManualQuoteError('');
      try {
        const quote = await supplyApi.quote(manualQuantity, manualSupplierId, manualProduct);
        if (!cancelled) {
          const inventory = quote.inventory;
          if (
            !inventory ||
            inventory.available <= 0 ||
            inventory.estimatedTotalFen <= 0 ||
            inventory.estimatedUnitPriceFen <= 0
          ) {
            hasQuote = false;
            setManualQuote(null);
            setManualQuoteError(t('supply.quote_unavailable'));
            return;
          }
          hasQuote = true;
          setManualQuote(quote);
          const platform = manualPlatformConfigRef.current;
          if (platform?.type === 'nvtokens') {
            void loadPlatformCatalog(platform, true);
          }
        }
      } catch (error) {
        if (!cancelled) {
          hasQuote = false;
          setManualQuote(null);
          setManualQuoteError(error instanceof Error ? error.message : t('common.unknown_error'));
        }
      } finally {
        refreshing = false;
        if (!cancelled) {
          setManualQuoteLoading(false);
          scheduleRefresh(PLATFORM_CATALOG_REFRESH_INTERVAL_MS);
        }
      }
    };
    const handleVisibilityChange = () => {
      if (document.visibilityState === 'visible' && !refreshing) {
        scheduleRefresh(0);
      }
    };

    scheduleRefresh(350);
    document.addEventListener('visibilitychange', handleVisibilityChange);
    return () => {
      cancelled = true;
      window.clearTimeout(timer);
      document.removeEventListener('visibilitychange', handleVisibilityChange);
    };
  }, [activeTab, loadPlatformCatalog, manualProduct, manualQuantity, manualSupplierId, t]);
  const manualInventory = manualQuote?.inventory;
  const recommendedPlatform = overview?.selectedPlatformId
    ? platformOverviewById.get(overview.selectedPlatformId.trim().toLowerCase())
    : undefined;
  const purchasePlatforms = useMemo(
    () => [...(status?.config?.platforms ?? draft.platforms ?? []), ...(overview?.platforms ?? [])],
    [draft.platforms, overview?.platforms, status?.config?.platforms]
  );
  const poolAccounts = resolveSupplyPoolAccountStats(
    smart,
    overview?.cpaAvailable,
    status?.accountPool
  );
  const poolClassificationObserved =
    status?.accountPool?.classificationObserved === true ||
    smart?.accountClassificationObserved === true;
  const displayedCPAAvailable = poolAccounts.normal ?? overview?.cpaAvailable;
  const displayedCPADeficit =
    displayedCPAAvailable === undefined
      ? overview?.cpaDeficit
      : Math.max(0, (overview?.cpaTarget ?? draft.targetAvailableAccounts) - displayedCPAAvailable);
  const automation = status?.automation;
  const lowPriceReserve = status?.lowPriceReserve;
  const recovery = status?.recovery;
  const autoSupplyEnabled = status?.config?.enabled ?? draft.enabled ?? false;
  const smartModeEnabled = smart?.enabled ?? draft.smartEnabled !== false;
  const activeOrder = status?.activeOrder;
  const activeOrders = status?.activeOrders ?? (activeOrder ? [activeOrder] : []);
  const purchaseTasks = status?.purchaseTasks ?? [];
  const activePurchaseTasks = purchaseTasks.filter(purchaseTaskActive);
  const activeTaskIDs = new Set(activePurchaseTasks.map((task) => task.taskId));
  const unlinkedActiveOrderCount = activeOrders.filter(
    (order) => !order.taskId || !activeTaskIDs.has(order.taskId)
  ).length;
  const orderTabCount = activePurchaseTasks.length + unlinkedActiveOrderCount;
  const latestAutomaticOrder = status?.orders?.find(
    (order) => order.automatic && order.strategy !== 'recovery'
  );
  const orderCount = status?.orders?.length ?? 0;
  const recoveryCount = recovery?.total ?? recoveries.length;
  const revenueMultiplier = status?.config?.revenueMultiplier ?? draft.revenueMultiplier ?? 0.06;
  const healthLevel = smart?.healthLevel || 'unknown';
  const suggestedAction = smart?.suggestedAction || 'unknown';
  const decisionReason = smart?.decisionReason || 'unknown';
  const confidence = smart?.confidence || 'low';
  const supplyPressureLevel = smart?.supplyPressureLevel || 'unknown';
  const demandTrend = smart?.demandTrend || 'unknown';
  const concurrencySlotsLabel = smart?.concurrencyUnlimited
    ? t('supply.concurrency_unlimited')
    : smart?.concurrencyDemandObserved
      ? t('supply.concurrency_slots_value', {
          current: formatInteger(smart?.concurrencyFiniteSlots),
          required: formatInteger(smart?.requiredConcurrencySlots),
        })
      : t('supply.concurrency_slots_waiting', {
          current: formatInteger(smart?.concurrencyFiniteSlots),
        });
  const concurrencySlotsTitle = t('supply.concurrency_slots_hint', {
    limited: formatInteger(smart?.concurrencyLimitedAccounts),
    unlimited: formatInteger(smart?.concurrencyUnlimitedAccounts),
    missing: formatInteger(smart?.concurrencyMissingAccounts),
    deficit: formatInteger(smart?.concurrencyAccountDeficit),
  });
  const draftStrategy = (draft.strategy || 'strong_supply') as SupplyStrategy;
  const effectiveHealthTargetMinutes =
    smart?.effectiveHealthyMinutesTarget ??
    smart?.healthyMinutesTarget ??
    draft.healthyMinutesTarget;
  const configuredHealthTargetMinutes =
    smart?.configuredHealthyMinutesTarget ??
    smart?.healthyMinutesTarget ??
    draft.healthyMinutesTarget;
  const tokenUnit = Math.max(1, smart?.unitCapacityRcu ?? 40);
  const rcuToTokenM = (value?: number) =>
    typeof value === 'number' && Number.isFinite(value) ? (value * tokenUnit) / 1000 : undefined;
  const currentCapacityTokenM =
    smart?.currentCapacityTokenM ?? rcuToTokenM(smart?.currentCapacityRcu);
  const totalCapacityTokenM = smart?.totalCapacityTokenM ?? currentCapacityTokenM;
  const availableCapacityTokenM =
    smart?.availableCapacityTokenM ??
    rcuToTokenM(smart?.availableCapacityRcu) ??
    totalCapacityTokenM;
  const frozenCapacityTokenM =
    smart?.frozenCapacityTokenM ??
    rcuToTokenM(smart?.frozenCapacityRcu) ??
    Math.max(0, (totalCapacityTokenM ?? 0) - (availableCapacityTokenM ?? 0));
  const timeLimitedCapacityTokenM = smart?.timeLimitedCapacityTokenM ?? currentCapacityTokenM;
  const targetCapacityTokenM = smart?.targetCapacityTokenM ?? rcuToTokenM(smart?.targetCapacityRcu);
  const capacityGapTokenM = smart?.capacityGapTokenM ?? rcuToTokenM(smart?.capacityGapRcu);
  const rawCapacityTokenM =
    smart?.rawCapacityTokenM ?? rcuToTokenM(smart?.rawCapacityRcu ?? smart?.currentCapacityRcu);
  const expiryWasteRiskTokenM =
    smart?.expiryWasteRiskTokenM ?? rcuToTokenM(smart?.expiryWasteRiskRcu ?? 0);
  const consumeTokenMPerMinute =
    smart?.consumeTokenMPerMinute ?? rcuToTokenM(smart?.consumeRcuPerMinute);
  const planningTokenMPerMinute =
    smart?.demandPlanningTokenMPerMinute ?? rcuToTokenM(smart?.demandPlanningRcuPerMinute);
  const forecastTokenMPerMinute = Math.max(
    consumeTokenMPerMinute ?? 0,
    planningTokenMPerMinute ?? 0
  );
  const forecastSustainMinutes =
    smart?.forecastSustainMinutes ??
    (forecastTokenMPerMinute > 0 && (timeLimitedCapacityTokenM ?? 0) > 0
      ? (timeLimitedCapacityTokenM ?? 0) / forecastTokenMPerMinute
      : undefined);
  const availableSustainMinutes =
    smart?.availableSustainMinutes ??
    (forecastTokenMPerMinute > 0 && (availableCapacityTokenM ?? 0) > 0
      ? (availableCapacityTokenM ?? 0) / forecastTokenMPerMinute
      : undefined);
  const rawSustainMinutes =
    smart?.rawSustainMinutes ??
    (forecastTokenMPerMinute > 0 && (rawCapacityTokenM ?? 0) > 0
      ? (rawCapacityTokenM ?? 0) / forecastTokenMPerMinute
      : undefined);
  const nearestExpiryAtMs = smart?.nearestExpiryAtMs;
  const nearestExpiryMinutes =
    smart?.nearestExpiryMinutes ??
    (nearestExpiryAtMs && nearestExpiryAtMs > nowMs
      ? (nearestExpiryAtMs - nowMs) / 60_000
      : undefined);
  const nextCapacityDeficitAtMs = smart?.nextCapacityDeficitAtMs;
  const forecastConsumptionTokenM = (minutes: number) =>
    forecastTokenMPerMinute > 0 ? forecastTokenMPerMinute * minutes : 0;
  const forecastPressureTone = (minutes: number) => {
    const usable = timeLimitedCapacityTokenM ?? 0;
    const projected = forecastConsumptionTokenM(minutes);
    if (usable <= 0 || projected <= 0) return '';
    if (projected >= usable) return styles.forecastCritical;
    if (projected >= usable * 0.6) return styles.forecastWarning;
    return styles.forecastHealthy;
  };
  const capacityPercent = smart
    ? clampPercent(
        ((timeLimitedCapacityTokenM ?? 0) / Math.max(0.001, targetCapacityTokenM ?? 0.001)) * 100
      )
    : 0;
  const snapshotLabel = t(snapshotLabelKey(smart), {
    coverage: formatNumber(smart?.capacityCoverage, 0),
  });
  const smartActionLabel = (action: string, reason: string) => {
    if (action !== 'snapshot_stale') {
      return t(`supply.smart_action_${action}`, {
        defaultValue: action,
      });
    }
    const resourceForReason = smart ? { ...smart, decisionReason: reason } : smart;
    return t(snapshotLabelKey(resourceForReason), {
      coverage: formatNumber(resourceForReason?.capacityCoverage, 0),
    });
  };
  const nextExecutionDue = Boolean(
    autoSupplyEnabled &&
    !automation?.running &&
    !status?.running &&
    automation?.nextExecutionAtMs &&
    automation.nextExecutionAtMs <= nowMs
  );
  const nextExecutionCountdown = !autoSupplyEnabled
    ? t('supply.automation_disabled_short')
    : automation?.running || status?.running
      ? t('supply.automation_running')
      : nextExecutionDue
        ? t('supply.automation_checking')
        : automation?.nextExecutionAtMs
          ? formatCountdown(automation.nextExecutionAtMs, nowMs)
          : t('supply.automation_waiting');
  const nextExecutionDetail = !autoSupplyEnabled
    ? t('supply.automation_disabled_detail')
    : automation?.running || status?.running || nextExecutionDue
      ? t('supply.automation_checking_detail')
      : automation?.nextExecutionAtMs
        ? t('supply.automation_next_execution_detail', {
            value: formatTime(automation.nextExecutionAtMs),
            seconds: automation.intervalSeconds ?? draft.checkIntervalSeconds,
          })
        : t('supply.automation_waiting_detail');
  const lastExecutionResult = automation?.lastResult || 'scheduled';
  const lastExecutionRetrying = isSupplyRuntimeErrorRetrying(automation?.lastError);
  const lastExecutionAction = automation?.lastAction || suggestedAction;
  const lastExecutionReason = automation?.lastReason || decisionReason;
  const lastExecutionDetail = automation?.lastFinishedAtMs
    ? t('supply.automation_last_execution_detail', {
        value: formatTime(automation.lastFinishedAtMs),
      })
    : t('supply.automation_no_execution');
  const lastExecutionActionLabel = smartActionLabel(lastExecutionAction, lastExecutionReason);
  const lastExecutionReasonLabel = t(`supply.smart_reason_${lastExecutionReason}`, {
    defaultValue: lastExecutionReason,
  });
  const lastExecutionResultLabel = lastExecutionRetrying
    ? t('supply.automation_result_retrying')
    : t(`supply.automation_result_${lastExecutionResult}`, {
        defaultValue: lastExecutionResult,
      });
  const lastExecutionCreatedOrder = Boolean(
    latestAutomaticOrder &&
    automation?.lastStartedAtMs &&
    latestAutomaticOrder.createdAtMs >= automation.lastStartedAtMs - 1_000 &&
    latestAutomaticOrder.createdAtMs <=
      (automation.lastFinishedAtMs ?? nowMs) + Math.max(5_000, automation.intervalSeconds ?? 0)
  );
  const lastExecutionOutcome = lastExecutionRetrying
    ? t('supply.automation_execution_retrying')
    : automation?.lastError || lastExecutionResult === 'failed'
      ? t('supply.automation_execution_failed')
      : lastExecutionResult === 'price_wait'
        ? t('supply.automation_execution_price_wait')
        : lastExecutionResult === 'quota_wait'
          ? t('supply.automation_execution_quota_wait')
          : lastExecutionCreatedOrder && latestAutomaticOrder
            ? t('supply.automation_execution_order_created', {
                quantity: latestAutomaticOrder.requestedQuantity,
              })
            : activeOrder
              ? t('supply.automation_execution_order_continued', {
                  orderId: shortOrderId(activeOrder.orderId),
                })
              : t('supply.automation_execution_no_order');
  const lastExecutionSummary = t('supply.automation_last_execution_summary', {
    result: lastExecutionResultLabel,
    value: automation?.lastFinishedAtMs ? formatTime(automation.lastFinishedAtMs) : '-',
    action: lastExecutionActionLabel,
    reason: lastExecutionReasonLabel,
  });
  const lastExecutionTooltip = `${lastExecutionDetail} · ${lastExecutionOutcome} · ${lastExecutionActionLabel} · ${lastExecutionReasonLabel}`;
  const activeOrderDetail = activeOrder
    ? activeOrder.nextPollAtMs && activeOrder.nextPollAtMs > nowMs
      ? t('supply.automation_order_poll_detail', {
          value: formatCountdown(activeOrder.nextPollAtMs, nowMs),
        })
      : t('supply.automation_order_processing_detail')
    : t('supply.automation_no_active_order_detail');
  const displayedOrder = activeOrder || latestAutomaticOrder;
  const displayedOrderStatus = displayedOrder
    ? t(`supply.status_${displayedOrder.status}`, { defaultValue: displayedOrder.status })
    : t('supply.no_active_order_short');
  const orderExecutionTitle = activeOrder
    ? t('supply.automation_active_order_status', { status: displayedOrderStatus })
    : latestAutomaticOrder
      ? t('supply.automation_latest_order_status', { status: displayedOrderStatus })
      : displayedOrderStatus;
  const displayedOrderSummary = displayedOrder
    ? t('supply.automation_order_summary', {
        orderId: shortOrderId(displayedOrder.orderId),
        quantity: displayedOrder.requestedQuantity,
        imported: displayedOrder.importedCount,
        cost: formatMoney(displayedOrder.chargedFen),
        time: formatTime(displayedOrder.updatedAtMs),
      })
    : activeOrderDetail;
  const orderExecutionDetail = displayedOrder?.lastError
    ? `${displayedOrderSummary} · ${t('supply.automation_order_error')}: ${localizeRuntimeError(displayedOrder.lastError)}`
    : activeOrder
      ? `${displayedOrderSummary} · ${activeOrderDetail}`
      : displayedOrderSummary;
  const emergencyShortage = smart?.emergencyShortage || suggestedAction === 'emergency_replenish';
  const displayDemandStrategy = emergencyShortage ? 'emergency' : demandTrend;
  const demandStrategy = t(`supply.demand_strategy_${displayDemandStrategy}`, {
    defaultValue: displayDemandStrategy,
  });
  const demandBasisKey = emergencyShortage
    ? 'emergency'
    : demandTrend === 'falling' && (smart?.capacityGapRcu ?? 0) > 0
      ? 'falling_target_gap'
      : demandTrend;
  const demandBasis = t(`supply.demand_basis_${demandBasisKey}`, {
    defaultValue: t('supply.demand_basis_unknown'),
  });
  const reportExecutive = report?.executive;
  const reportImportHealth = report?.importHealth;
  const reportTiming = report?.timing;
  const reportRisk = report?.risk;
  const reportRange = report?.range;
  const selectedReportRangeLabel = t(
    REPORT_RANGE_PRESETS.find((preset) => preset.id === reportRangePreset)?.labelKey ??
      'supply.report_range_today'
  );
  const reportRangeLabel = reportRange
    ? t('supply.report_range_value', {
        from: new Date(reportRange.fromMs).toLocaleDateString(),
        to: new Date(Math.max(reportRange.fromMs, reportRange.toMs - 1)).toLocaleDateString(),
        days: reportRange.days,
      })
    : selectedReportRangeLabel;
  const accountSummary = accounts?.summary;
  const accountRevenueMultiplier = accountSummary?.revenueMultiplier ?? revenueMultiplier;
  const reportRevenueMultiplier = reportExecutive?.revenueMultiplier ?? revenueMultiplier;
  const accountRange = accounts?.range;
  const accountRangeLabel = accountRange
    ? t('supply.report_range_value', {
        from: new Date(accountRange.fromMs).toLocaleDateString(),
        to: new Date(Math.max(accountRange.fromMs, accountRange.toMs - 1)).toLocaleDateString(),
        days: accountRange.days,
      })
    : selectedReportRangeLabel;
  const accountProblemCount =
    (accountSummary?.disabled ?? 0) +
    (accountSummary?.expired ?? 0) +
    (accountSummary?.missing ?? 0) +
    (accountSummary?.failed ?? 0);
  const accountMetrics = [
    {
      label: t('supply.account_total'),
      value: formatInteger(accountSummary?.total),
      detail: t('supply.account_total_hint', {
        imported: formatInteger(accountSummary?.imported),
        pending: formatInteger(accountSummary?.pending),
      }),
    },
    {
      label: t('supply.account_active'),
      value: formatInteger(accountSummary?.active),
      detail: t('supply.account_active_hint', {
        expiring: formatInteger(accountSummary?.expiringSoon),
      }),
    },
    {
      label: t('supply.account_problem'),
      value: formatInteger(accountProblemCount),
      detail: t('supply.account_problem_hint', {
        disabled: formatInteger(accountSummary?.disabled),
        missing: formatInteger(accountSummary?.missing),
        expired: formatInteger(accountSummary?.expired),
      }),
    },
    {
      label: t('supply.account_auth_401'),
      value: formatInteger(accountSummary?.auth401Accounts),
      detail: t('supply.account_auth_401_hint', {
        quarantined: formatInteger(accountSummary?.autoQuarantined),
      }),
    },
    {
      label: t('supply.account_usage_calls'),
      value: formatInteger(accountSummary?.usageCalls),
      detail: t('supply.account_usage_calls_hint', {
        success: formatInteger(accountSummary?.usageSuccessCalls),
        failure: formatInteger(accountSummary?.usageFailureCalls),
      }),
    },
    {
      label: t('supply.account_usage_tokens'),
      value: formatTokens(accountSummary?.usageTokens),
      detail: t('supply.account_usage_tokens_hint', {
        lastUsed: formatTime(accountSummary?.lastUsedAtMs),
      }),
    },
    {
      label: t('supply.account_usage_revenue'),
      value: formatUsd(accountSummary?.usageRevenue),
      detail: t('supply.account_usage_revenue_hint', {
        value: formatUsd(accountSummary?.averageRevenuePerCall),
        multiplier: formatMultiplier(accountRevenueMultiplier),
      }),
    },
  ];
  const reportFinanceMetrics = [
    {
      label: t('supply.report_supply_spend'),
      value: formatMoney(reportExecutive?.supplySpendFen),
      detail: t('supply.report_supply_spend_hint'),
    },
    {
      label: t('supply.report_supply_net_spend'),
      value: formatMoney(reportExecutive?.supplyNetSpendFen),
      detail: t('supply.report_supply_net_spend_hint', {
        released: formatMoney(reportExecutive?.releasedFen),
        refunded: formatMoney(reportExecutive?.refundedFen),
      }),
    },
    {
      label: t('supply.report_usage_revenue'),
      value: formatUsd(reportExecutive?.usageRevenue),
      detail: t('supply.report_usage_revenue_hint', {
        currency: reportExecutive?.usageRevenueCurrency || 'USD',
        multiplier: formatMultiplier(reportRevenueMultiplier),
      }),
    },
    {
      label: t('supply.report_average_revenue_per_call'),
      value: formatUsd(reportExecutive?.averageRevenuePerCall),
      detail: t('supply.report_average_revenue_per_call_hint'),
    },
    {
      label: t('supply.report_usage_calls'),
      value: formatInteger(reportExecutive?.usageCalls),
      detail: t('supply.report_successful_calls', {
        value: formatInteger(
          report?.usageModels?.reduce((sum, item) => sum + item.successCalls, 0)
        ),
      }),
    },
    {
      label: t('supply.report_usage_tokens'),
      value: formatTokens(reportExecutive?.usageTokens),
      detail: t('supply.report_usage_tokens_hint'),
    },
    {
      label: t('supply.report_average_unit_cost'),
      value: formatMoney(reportExecutive?.averageUnitFen),
      detail: t('supply.report_average_unit_cost_hint'),
    },
    {
      label: t('supply.report_refunded_amount'),
      value: formatMoney(reportExecutive?.refundedFen),
      detail: t('supply.report_refunded_amount_hint'),
    },
  ];
  const reportOperationsMetrics = [
    {
      label: t('supply.report_orders'),
      value: formatInteger(reportExecutive?.orders),
      detail: t('supply.report_orders_hint'),
    },
    {
      label: t('supply.report_requested_accounts'),
      value: formatInteger(reportExecutive?.requestedAccounts),
      detail: t('supply.report_requested_accounts_hint'),
    },
    {
      label: t('supply.report_imported_accounts'),
      value: formatInteger(reportExecutive?.importedAccounts),
      detail: t('supply.report_imported_accounts_hint'),
    },
    {
      label: t('supply.report_recoveries'),
      value: formatInteger(reportExecutive?.recoveries),
      detail: t('supply.report_recoveries_hint'),
    },
    {
      label: t('supply.report_recovery_claim_rate'),
      value: formatPercent(reportExecutive?.recoveryClaimRate),
      detail: t('supply.report_recovery_claim_rate_hint'),
    },
    {
      label: t('supply.report_recovery_import_rate'),
      value: formatPercent(reportExecutive?.recoveryImportRate),
      detail: t('supply.report_recovery_import_rate_hint'),
    },
    {
      label: t('supply.report_recovery_refund_rate'),
      value: formatPercent(reportExecutive?.recoveryRefundRate),
      detail: t('supply.report_recovery_refund_rate_hint'),
    },
    {
      label: t('supply.report_import_success_rate'),
      value: formatPercent(reportExecutive?.importSuccessRate),
      detail: t('supply.report_import_success_rate_hint'),
    },
    {
      label: t('supply.report_auth_401_accounts'),
      value: formatInteger(reportExecutive?.auth401Accounts),
      detail: t('supply.report_auth_401_events_hint', {
        events: formatInteger(reportExecutive?.auth401Events),
        rate: formatPercent(reportExecutive?.auth401Rate),
      }),
    },
    {
      label: t('supply.report_auto_quarantined'),
      value: formatInteger(reportExecutive?.autoQuarantined),
      detail: t('supply.report_auto_quarantined_hint'),
    },
    {
      label: t('supply.report_emergency_replenishments'),
      value: formatInteger(reportExecutive?.emergencyReplenishments),
      detail: t('supply.report_vacuum_replenishments_hint', {
        value: formatInteger(reportExecutive?.vacuumReplenishments),
      }),
    },
    {
      label: t('supply.report_virtual_demand_replenishments'),
      value: formatInteger(reportExecutive?.virtualDemandReplenishments),
      detail: t('supply.report_virtual_demand_replenishments_hint'),
    },
    {
      label: t('supply.report_vacuum_total_duration'),
      value: formatSeconds(reportExecutive?.vacuumTotalSeconds),
      detail: t('supply.report_vacuum_average_recovery_hint', {
        value: formatSeconds(reportExecutive?.averageVacuumRecoverySeconds),
      }),
    },
  ];
  const reportProductMetrics = [
    {
      label: t('supply.report_avg_order_seconds'),
      value: formatSeconds(reportTiming?.averageOrderFulfillmentSeconds),
      detail: t('supply.report_avg_order_seconds_hint'),
    },
    {
      label: t('supply.report_avg_recovery_claim_seconds'),
      value: formatSeconds(reportTiming?.averageRecoveryClaimSeconds),
      detail: t('supply.report_avg_recovery_claim_seconds_hint'),
    },
    {
      label: t('supply.report_avg_recovery_import_seconds'),
      value: formatSeconds(reportTiming?.averageRecoveryImportSeconds),
      detail: t('supply.report_avg_recovery_import_seconds_hint'),
    },
    {
      label: t('supply.report_avg_import_registration_seconds'),
      value: formatSeconds(reportTiming?.averageImportRegistrationSeconds),
      detail: t('supply.report_avg_import_registration_seconds_hint'),
    },
    {
      label: t('supply.report_import_items'),
      value: formatInteger(reportImportHealth?.items),
      detail: t('supply.report_import_items_hint'),
    },
    {
      label: t('supply.report_average_attempts'),
      value: formatNumber(reportImportHealth?.averageAttempts),
      detail: t('supply.report_average_attempts_hint'),
    },
    {
      label: t('supply.report_expiring_soon_items'),
      value: formatInteger(reportImportHealth?.expiringSoonItems),
      detail: t('supply.report_expiring_soon_items_hint'),
    },
    {
      label: t('supply.report_expired_items'),
      value: formatInteger(reportImportHealth?.expiredItems),
      detail: t('supply.report_expired_items_hint'),
    },
  ];
  const reportRiskMetrics = [
    {
      label: t('supply.report_open_orders'),
      value: formatInteger(reportRisk?.openOrders),
      detail: t('supply.report_open_orders_hint'),
    },
    {
      label: t('supply.report_unclaimed_recoveries'),
      value: formatInteger(reportRisk?.unclaimedRecoveries),
      detail: t('supply.report_unclaimed_recoveries_hint'),
    },
    {
      label: t('supply.report_import_backlog_items'),
      value: formatInteger(reportRisk?.importBacklogItems),
      detail: t('supply.report_import_backlog_items_hint'),
    },
    {
      label: t('supply.report_failed_import_items'),
      value: formatInteger(reportRisk?.failedImportItems),
      detail: t('supply.report_failed_import_items_hint'),
    },
    {
      label: t('supply.report_partial_recoveries'),
      value: formatInteger(reportRisk?.partialRecoveries),
      detail: t('supply.report_partial_recoveries_hint'),
    },
    {
      label: t('supply.report_stale_claimable_recoveries'),
      value: formatInteger(reportRisk?.staleClaimableRecoveries),
      detail: t('supply.report_stale_claimable_recoveries_hint'),
    },
  ];

  const metrics = useMemo(() => {
    if (smart?.enabled ?? draft.smartEnabled !== false) {
      return [
        {
          label: t('supply.effective_capacity_1h'),
          // Keep the actual sum of per-account remaining quota separate from
          // the expiry-aware amount that the planner can consume in time.
          value: formatTokenM(rawCapacityTokenM),
          detail: t('supply.capacity_split_detail', {
            available: formatTokenM(availableCapacityTokenM),
            frozen: formatTokenM(frozenCapacityTokenM),
            effective: formatTokenM(timeLimitedCapacityTokenM),
          }),
          icon: <IconDatabaseZap size={18} />,
          tone: 'teal',
        },
        {
          label: t('supply.consume_rate'),
          value: formatTokenMRate(consumeTokenMPerMinute),
          detail: t('supply.consume_rate_detail', {
            rpm: formatNumber(smart?.rpm30m),
            tpm: formatTokenM(smart?.observedTokenM30m ?? (smart?.tpm30m ?? 0) / 1_000_000, 2),
            request: formatNumber(smart?.requestDemandRcuPerMinute),
            token: formatTokenMRate(consumeTokenMPerMinute),
            driver: t(`supply.demand_driver_${smart?.demandDriver || 'none'}`),
          }),
          icon: <IconTrendingUp size={18} />,
          tone: 'orange',
        },
        {
          label: t('supply.estimated_depletion'),
          value: formatMinutes(smart?.forecastSustainMinutes ?? smart?.estimatedSustainMinutes),
          detail: t('supply.effective_health_target_minutes', {
            value:
              smart?.effectiveHealthyMinutesTarget ??
              smart?.healthyMinutesTarget ??
              draft.healthyMinutesTarget,
            configured:
              smart?.configuredHealthyMinutesTarget ??
              smart?.healthyMinutesTarget ??
              draft.healthyMinutesTarget,
          }),
          icon: <IconTimer size={18} />,
          tone: 'blue',
        },
        {
          label: t('supply.available_balance'),
          value: balance ? formatMoney(balance.availableFen) : '-',
          detail: balance
            ? t('supply.held_value', { value: formatMoney(balance.heldFen) })
            : t('supply.supply_inventory_value', { value: inventory?.available ?? '-' }),
          icon: <IconDollarSign size={18} />,
          tone: 'violet',
        },
      ];
    }
    return [
      {
        label: t('supply.cpa_available'),
        value: displayedCPAAvailable ?? '-',
        detail: t('supply.target_value', {
          value: overview?.cpaTarget ?? draft.targetAvailableAccounts,
        }),
        icon: <IconDatabaseZap size={18} />,
        tone: 'teal',
      },
      {
        label: t('supply.deficit'),
        value: displayedCPADeficit ?? '-',
        detail: t('supply.auto_order_hint'),
        icon: <IconInbox size={18} />,
        tone: 'orange',
      },
      {
        label: t('supply.supply_inventory'),
        value: inventory?.available ?? '-',
        detail: inventory?.needsProduction
          ? t('supply.production_required', { value: inventory.missing })
          : t('supply.ready_delivery'),
        icon: <IconInbox size={18} />,
        tone: 'blue',
      },
      {
        label: t('supply.available_balance'),
        value: balance ? formatMoney(balance.availableFen) : '-',
        detail: balance ? t('supply.held_value', { value: formatMoney(balance.heldFen) }) : '-',
        icon: <IconDollarSign size={18} />,
        tone: 'violet',
      },
    ];
  }, [
    balance,
    consumeTokenMPerMinute,
    draft.healthyMinutesTarget,
    draft.smartEnabled,
    draft.targetAvailableAccounts,
    displayedCPAAvailable,
    displayedCPADeficit,
    availableCapacityTokenM,
    frozenCapacityTokenM,
    inventory,
    overview,
    totalCapacityTokenM,
    smart,
    t,
  ]);

  const tabItems = useMemo<SegmentedTabItem<SupplyWorkspaceTab>[]>(
    () => [
      { id: 'overview', label: t('supply.tabs_overview') },
      { id: 'automation', label: t('supply.tabs_automation') },
      {
        id: 'orders',
        label: (
          <span className={styles.tabLabel}>
            {t('supply.tabs_orders')}
            {orderTabCount > 0 ? <span className={styles.tabBadge}>{orderTabCount}</span> : null}
          </span>
        ),
      },
      {
        id: 'accounts',
        label: (
          <span className={styles.tabLabel}>
            {t('supply.tabs_accounts')}
            {accounts?.summary?.total ? (
              <span className={styles.tabBadge}>{accounts.summary.total}</span>
            ) : null}
          </span>
        ),
      },
      {
        id: 'recoveries',
        label: (
          <span className={styles.tabLabel}>
            {t('supply.tabs_recoveries')}
            {recoveryCount > 0 ? <span className={styles.tabBadge}>{recoveryCount}</span> : null}
          </span>
        ),
      },
      { id: 'reports', label: t('supply.tabs_reports') },
      {
        id: 'history',
        label: (
          <span className={styles.tabLabel}>
            {t('supply.tabs_history')}
            {orderCount > 0 ? <span className={styles.tabBadge}>{orderCount}</span> : null}
          </span>
        ),
      },
    ],
    [accounts?.summary?.total, orderCount, orderTabCount, recoveryCount, t]
  );
  const reportRangeItems = useMemo<SegmentedTabItem<ReportRangePreset>[]>(
    () =>
      REPORT_RANGE_PRESETS.map((preset) => ({
        id: preset.id,
        label: t(preset.labelKey),
      })),
    [t]
  );
  const supplyStrategyItems = useMemo<SegmentedTabItem<SupplyStrategy>[]>(
    () =>
      SUPPLY_STRATEGIES.map((strategy) => ({
        id: strategy,
        label: t(`supply.strategy_${strategy}`),
      })),
    [t]
  );
  const accountStatusOptions = useMemo(
    () =>
      ACCOUNT_STATUS_FILTERS.map((item) => ({
        value: item.id,
        label: t(item.labelKey),
      })),
    [t]
  );
  const quotaPlanTypes = useMemo(() => {
    const planTypes = new Set<string>(['team', 'plus', 'pro', 'free']);
    Object.keys(draft.quotaEstimationPolicies ?? {}).forEach((planType) => {
      if (planType.trim()) planTypes.add(planType.trim().toLowerCase());
    });
    smart?.accountQuotaPlanEstimates?.forEach((estimate) => {
      if (estimate.planType.trim()) planTypes.add(estimate.planType.trim().toLowerCase());
    });
    return [...planTypes].sort((left, right) => {
      const rank = (value: string) =>
        value === 'team'
          ? 0
          : value === 'free'
            ? 1
            : value === 'plus'
              ? 2
              : value === 'pro'
                ? 3
                : 4;
      return rank(left) - rank(right) || left.localeCompare(right);
    });
  }, [draft.quotaEstimationPolicies, smart?.accountQuotaPlanEstimates]);
  const quotaPolicyForPlan = (planType: string): SupplyQuotaEstimationPolicy => {
    const defaultPolicy = DEFAULT_QUOTA_ESTIMATION_POLICIES[planType] ?? {
      mode: 'auto',
      fallbackM: 10,
      fixedM: 10,
    };
    return {
      ...defaultPolicy,
      ...draft.quotaEstimationPolicies?.[planType],
    };
  };
  const quotaPolicyForSupplier = (
    supplierId: string | undefined,
    planType: string
  ): SupplyQuotaEstimationPolicy => {
    const globalPolicy = quotaPolicyForPlan(planType);
    const platform = (draft.platforms ?? []).find(
      (item) => item.id.trim().toLowerCase() === (supplierId ?? '').trim().toLowerCase()
    );
    return {
      ...globalPolicy,
      ...platform?.quotaEstimationPolicies?.[planType],
    };
  };
  const quotaEstimateItems = reconcileLiveQuotaPlanAccounts(
    smart?.accountQuotaPlanEstimates ?? [],
    status?.accountPool?.plans ?? [],
    quotaPolicyForSupplier
  );

  if (loading && !status) {
    return <div className={styles.loading}>{t('common.loading')}</div>;
  }

  return (
    <div className={styles.page}>
      <section className={styles.hero}>
        <div className={styles.heroCopy}>
          <div className={styles.eyebrow}>{t('supply.eyebrow')}</div>
          <h1>{t('supply.title')}</h1>
          <p>{t('supply.subtitle')}</p>
        </div>
        <div className={styles.heroActions}>
          <div className={styles.autoSupplyControl}>
            <ToggleSwitch
              checked={autoSupplyEnabled}
              disabled={action === 'save'}
              label={t('supply.enable_auto')}
              labelPosition="left"
              onChange={toggleAutoSupply}
            />
          </div>
          <div className={styles.heroSummary}>
            <span className={`${styles.serviceBadge} ${autoSupplyEnabled ? styles.success : ''}`}>
              <span />
              {autoSupplyEnabled ? t('supply.auto_enabled') : t('supply.auto_disabled')}
            </span>
            <span className={`${styles.statusPill} ${smartTone(smart)}`}>
              {t(`supply.smart_health_${healthLevel}`, {
                defaultValue: smart?.healthLevel || '-',
              })}
            </span>
            <span className={`${styles.statusPill} ${activeOrder ? styles.active : ''}`}>
              {activeOrder
                ? t('supply.active_order_short', { value: shortOrderId(activeOrder.orderId) })
                : t('supply.no_active_order_short')}
            </span>
          </div>
          <Button
            variant="secondary"
            size="sm"
            loading={action === 'check' || status?.running}
            onClick={() => void check()}
          >
            <IconRefreshCw size={15} /> {t('supply.check_now')}
          </Button>
        </div>
      </section>

      <section className={styles.poolSummaryGrid} aria-label={t('supply.pool_summary')}>
        <div className={styles.poolSummaryItem}>
          <span>{t('supply.pool_total_accounts')}</span>
          <strong>{formatInteger(poolAccounts.total)}</strong>
          <small>
            {t('supply.pool_total_accounts_hint', {
              total: formatInteger(poolAccounts.total),
              disabled: formatInteger(poolAccounts.disabled),
            })}
          </small>
        </div>
        <div className={styles.poolSummaryItem}>
          <span>{t('supply.pool_available_accounts')}</span>
          <strong>{formatInteger(poolAccounts.normal)}</strong>
          <small>
            {t('supply.pool_normal_accounts_hint', { value: formatInteger(poolAccounts.normal) })}
          </small>
        </div>
        <div className={styles.poolSummaryItem}>
          <span>{t('supply.pool_attention_accounts')}</span>
          <strong>
            {poolClassificationObserved ? formatInteger(poolAccounts.needsAttention) : '-'}
          </strong>
          <small>{t('supply.pool_attention_accounts_hint')}</small>
        </div>
        <div className={styles.poolSummaryItem}>
          <span>{t('supply.pool_quota_risk_accounts')}</span>
          <strong>
            {poolClassificationObserved ? formatInteger(poolAccounts.quotaRisk) : '-'}
          </strong>
          <small>{t('supply.pool_quota_risk_accounts_hint')}</small>
        </div>
        <div className={styles.poolSummaryItem}>
          <span>{t('supply.pool_disabled_accounts')}</span>
          <strong>{formatInteger(poolAccounts.disabled)}</strong>
          <small>{t('supply.pool_disabled_accounts_hint')}</small>
        </div>
        <div className={styles.poolSummaryItem}>
          <span>{t('supply.pool_unconfirmed_accounts')}</span>
          <strong>
            {poolClassificationObserved ? formatInteger(poolAccounts.unconfirmed) : '-'}
          </strong>
          <small>{t('supply.pool_unconfirmed_accounts_hint')}</small>
        </div>
      </section>

      <section
        className={styles.quotaEstimateSection}
        aria-label={t('supply.quota_plan_estimates_title')}
      >
        <div className={styles.quotaEstimateHeader}>
          <div>
            <strong>{t('supply.quota_plan_estimates_title')}</strong>
            <span>{t('supply.quota_plan_estimates_hint')}</span>
          </div>
          {smart?.quotaEstimateOrderingBlocked ? (
            <span className={`${styles.statusPill} ${styles.error}`}>
              {t('supply.quota_plan_ordering_blocked')}
            </span>
          ) : smart?.quotaEstimatePendingPlans ? (
            <span className={`${styles.statusPill} ${styles.warning}`}>
              {t('supply.quota_plan_pending_count', {
                count: smart.quotaEstimatePendingPlans,
              })}
            </span>
          ) : null}
        </div>
        <div className={styles.quotaEstimateGrid}>
          {quotaEstimateItems.map((estimate) => {
            const planType = estimate.planType.trim().toLowerCase();
            const policy = quotaPolicyForSupplier(estimate.supplierId, planType);
            const mode = estimate?.mode ?? policy.mode;
            const adoptedM =
              estimate?.adoptedM ?? (mode === 'fixed' ? policy.fixedM : policy.fallbackM);
            const pending = estimate?.pendingConfirmation === true;
            const blocked = estimate?.orderingBlocked === true;
            const usingFallback = estimate?.usingFallback === true;
            const validationState =
              estimate?.validationState ??
              (mode === 'fixed' ? 'fixed' : pending ? 'confirming' : 'accepted');
            const insufficient = validationState === 'insufficient';
            const quarantined = validationState === 'quarantined';
            const rejectedAccounts = estimate?.rejectedAccounts ?? 0;
            const hasRejectedAccounts = rejectedAccounts > 0;
            const upwardPending =
              pending &&
              !blocked &&
              !usingFallback &&
              validationState === 'confirming' &&
              (estimate?.confirmationRounds ?? 0) < (estimate?.requiredRounds ?? 2) &&
              (estimate?.observedM ?? 0) > adoptedM;
            const downwardPending = pending && usingFallback;
            const showValidationNotice = pending || insufficient || hasRejectedAccounts;
            return (
              <article
                className={`${styles.quotaEstimateCard} ${
                  blocked
                    ? styles.quotaEstimateBlocked
                    : showValidationNotice
                      ? styles.quotaEstimateWarning
                      : ''
                }`}
                key={estimate.key || `${estimate.supplierId || 'unassigned'}:${planType}`}
              >
                <div className={styles.quotaEstimateCardHeader}>
                  <div>
                    <span>
                      {estimate.supplierName ||
                        estimate.supplierId ||
                        t('supply.platform_unassigned')}
                    </span>
                    <strong>
                      {t(`supply.quota_plan_type_${planType}`, {
                        defaultValue: planType.toUpperCase(),
                      })}
                    </strong>
                  </div>
                  <span
                    className={`${styles.statusPill} ${pending ? styles.warning : styles.active}`}
                  >
                    {t(`supply.quota_plan_mode_${mode}`, { defaultValue: mode })}
                  </span>
                </div>
                <div className={styles.quotaEstimateValues}>
                  <div>
                    <span>{t('supply.quota_plan_observed')}</span>
                    <strong>
                      {(estimate?.observedM ?? 0) > 0
                        ? formatTokenM(estimate?.observedM, 2)
                        : t('supply.quota_plan_no_data')}
                    </strong>
                  </div>
                  <div>
                    <span>{t('supply.quota_plan_adopted')}</span>
                    <strong>{formatTokenM(adoptedM, 2)}</strong>
                  </div>
                </div>
                <div className={styles.quotaEstimateMeta}>
                  <span>
                    {t('supply.quota_plan_accounts')}: {formatInteger(estimate?.accountCount ?? 0)}
                  </span>
                  <span>
                    {t('supply.quota_plan_samples')}: {formatInteger(estimate?.uniqueAccounts ?? 0)}
                  </span>
                  <span>
                    {t('supply.quota_plan_minimum_samples')}:{' '}
                    {formatInteger(estimate?.minimumUniqueAccounts ?? 3)}
                  </span>
                  <span>
                    {t('supply.quota_plan_fallback')}:{' '}
                    {formatTokenM(estimate?.fallbackM ?? policy.fallbackM, 1)}
                  </span>
                </div>
                {(estimate.quotaClasses?.length ?? 0) > 0 ? (
                  <div className={styles.quotaClassList}>
                    {estimate.quotaClasses?.map((quotaClass) => (
                      <div className={styles.quotaClassItem} key={quotaClass.id}>
                        <div>
                          <strong>
                            {t('supply.quota_class_center', {
                              value: formatTokenM(quotaClass.centerM, 1),
                            })}
                          </strong>
                          <span>
                            {t('supply.quota_class_accounts', {
                              count: quotaClass.accountCount,
                              share: formatNumber(quotaClass.sharePercent, 1),
                            })}
                          </span>
                        </div>
                        <small>
                          {t('supply.quota_class_evidence', {
                            trusted: quotaClass.trustedAccounts,
                            provisional: quotaClass.provisionalAccounts,
                            minimum: formatTokenM(quotaClass.minimumM, 1),
                            maximum: formatTokenM(quotaClass.maximumM, 1),
                          })}
                        </small>
                      </div>
                    ))}
                  </div>
                ) : null}
                {showValidationNotice ? (
                  <div className={blocked ? styles.quotaEstimateAlert : styles.quotaEstimateNotice}>
                    {t(
                      insufficient
                        ? 'supply.quota_plan_warning_insufficient'
                        : quarantined
                          ? 'supply.quota_plan_warning_quarantined'
                          : hasRejectedAccounts
                            ? 'supply.quota_plan_warning_rejected'
                            : blocked || downwardPending
                              ? 'supply.quota_plan_warning_pending'
                              : upwardPending
                                ? 'supply.quota_plan_warning_upward_pending'
                                : 'supply.quota_plan_warning_staged',
                      {
                        divergence: formatNumber(estimate?.divergencePercent, 1),
                        current: estimate?.confirmationRounds ?? 0,
                        required: estimate?.requiredRounds ?? 2,
                        observedAccounts: estimate?.uniqueAccounts ?? 0,
                        minimumAccounts: estimate?.minimumUniqueAccounts ?? 3,
                        completeAccounts: estimate?.completeWindowAccounts ?? 0,
                        rejectedAccounts,
                      }
                    )}
                  </div>
                ) : (
                  <small>{t('supply.quota_plan_default_scope')}</small>
                )}
              </article>
            );
          })}
        </div>
      </section>

      <section className={styles.metricGrid}>
        {metrics.map((metric) => (
          <article className={`${styles.metricCard} ${styles[metric.tone]}`} key={metric.label}>
            <div className={styles.metricIcon}>{metric.icon}</div>
            <div>
              <span>{metric.label}</span>
              <strong>{metric.value}</strong>
              <small>{metric.detail}</small>
            </div>
          </article>
        ))}
      </section>

      {smart?.enabled ? (
        <section
          className={styles.consumptionForecast}
          aria-label={t('supply.consumption_forecast_title')}
        >
          <div className={styles.consumptionForecastHeader}>
            <div>
              <strong>{t('supply.consumption_forecast_title')}</strong>
              <span>{t('supply.consumption_forecast_hint')}</span>
            </div>
            <span className={`${styles.statusPill} ${styles.active}`}>
              {t('supply.forecast_rate_basis', {
                value: formatTokenMRate(forecastTokenMPerMinute),
              })}
            </span>
          </div>
          <div className={styles.consumptionForecastGrid}>
            <article className={`${styles.consumptionForecastItem} ${styles.forecastRate}`}>
              <span>{t('supply.forecast_current_rate')}</span>
              <strong>{formatTokenMRate(consumeTokenMPerMinute)}</strong>
              <small>
                {t('supply.forecast_current_rate_hint', {
                  minutes: smart?.usageSampleMinutes ?? 0,
                })}
              </small>
            </article>
            {[10, 30, 60].map((minutes) => (
              <article
                className={`${styles.consumptionForecastItem} ${forecastPressureTone(minutes)}`}
                key={minutes}
              >
                <span>{t('supply.forecast_consumption_window', { minutes })}</span>
                <strong>{formatTokenM(forecastConsumptionTokenM(minutes))}</strong>
                <small>
                  {t('supply.forecast_consumption_hint', {
                    rate: formatTokenMRate(forecastTokenMPerMinute),
                  })}
                </small>
              </article>
            ))}
            <article className={`${styles.consumptionForecastItem} ${styles.forecastHealthy}`}>
              <span>{t('supply.forecast_available_balance')}</span>
              <strong>{formatTokenM(availableCapacityTokenM)}</strong>
              <small>
                {t('supply.forecast_available_balance_hint', {
                  accounts: formatInteger(smart?.availableAccounts),
                })}
              </small>
            </article>
            <article className={`${styles.consumptionForecastItem} ${styles.forecastWarning}`}>
              <span>{t('supply.forecast_frozen_balance')}</span>
              <strong>{formatTokenM(frozenCapacityTokenM)}</strong>
              <small>
                {t('supply.forecast_frozen_balance_hint', {
                  accounts: formatInteger(smart?.frozenAccounts),
                })}
              </small>
            </article>
            <article className={`${styles.consumptionForecastItem} ${styles.forecastRunway}`}>
              <span>{t('supply.forecast_available_runway')}</span>
              <strong>{formatMinutes(availableSustainMinutes)}</strong>
              <small>
                {t('supply.forecast_available_runway_hint', {
                  critical: formatMinutes(smart?.criticalMinutes),
                })}
              </small>
            </article>
            <article className={`${styles.consumptionForecastItem} ${styles.forecastBalance}`}>
              <span>{t('supply.forecast_usable_balance')}</span>
              <strong>{formatTokenM(totalCapacityTokenM)}</strong>
              <small>
                {t('supply.forecast_usable_balance_hint', {
                  raw: formatTokenM(totalCapacityTokenM),
                  waste: formatTokenM(expiryWasteRiskTokenM ?? 0),
                })}
              </small>
            </article>
            <article className={`${styles.consumptionForecastItem} ${styles.forecastRunway}`}>
              <span>{t('supply.forecast_runway')}</span>
              <strong>{formatMinutes(forecastSustainMinutes)}</strong>
              <small>
                {t('supply.forecast_runway_comparison_hint', {
                  raw: formatMinutes(rawSustainMinutes),
                })}
              </small>
            </article>
            <article className={`${styles.consumptionForecastItem} ${styles.forecastBalance}`}>
              <span>{t('supply.forecast_next_expiry')}</span>
              <strong>{nearestExpiryAtMs ? formatTime(nearestExpiryAtMs) : '-'}</strong>
              <small>
                {nearestExpiryAtMs
                  ? t('supply.forecast_next_expiry_hint', {
                      minutes: formatMinutes(nearestExpiryMinutes),
                    })
                  : t('supply.forecast_expiry_unknown')}
              </small>
            </article>
            <article className={`${styles.consumptionForecastItem} ${styles.forecastRunway}`}>
              <span>{t('supply.forecast_next_deficit')}</span>
              <strong>{nextCapacityDeficitAtMs ? formatTime(nextCapacityDeficitAtMs) : '-'}</strong>
              <small>{t('supply.forecast_next_deficit_hint')}</small>
            </article>
            <article className={`${styles.consumptionForecastItem} ${styles.forecastHealthy}`}>
              <span>{t('supply.forecast_purchase_lead')}</span>
              <strong>{formatMinutes(smart?.purchaseLeadMinutes)}</strong>
              <small>{t('supply.forecast_purchase_lead_hint')}</small>
            </article>
            <article className={`${styles.consumptionForecastItem} ${styles.forecastRunway}`}>
              <span>{t('supply.forecast_purchase_trigger')}</span>
              <strong>{formatMinutes(smart?.purchaseTimingTriggerMinutes)}</strong>
              <small>
                {(smart?.purchaseTimingWaitMinutes ?? 0) > 0
                  ? t('supply.forecast_purchase_trigger_wait_hint', {
                      minutes: formatMinutes(smart?.purchaseTimingWaitMinutes),
                    })
                  : t('supply.forecast_purchase_trigger_ready_hint', {
                      quantity: formatInteger(smart?.purchaseTimingEligibleQuantity),
                    })}
              </small>
            </article>
          </div>
        </section>
      ) : null}

      {overview?.lastError ? (
        <div
          className={
            isSupplyRuntimeErrorRetrying(overview.lastError)
              ? styles.retryBanner
              : styles.errorBanner
          }
        >
          {localizeRuntimeError(overview.lastError)}
        </div>
      ) : null}

      <section className={styles.workspace}>
        <div className={styles.workspaceHeader}>
          <SegmentedTabs
            items={tabItems}
            activeTab={activeTab}
            ariaLabel={t('supply.tabs_aria')}
            onChange={setActiveTab}
            idBase="supply-workspace-tabs"
            fullWidth
            equalWidth
          />
          <div className={styles.workspaceMeta}>
            {t('supply.last_checked', { value: formatTime(overview?.checkedAtMs) })}
          </div>
        </div>

        <div className={styles.tabPanel} role="tabpanel" id={`supply-workspace-${activeTab}`}>
          {activeTab === 'overview' ? (
            <section className={styles.overviewGrid}>
              <article className={`${styles.decisionPanel} ${smartPanelTone(smart)}`}>
                <div className={styles.compactHeader}>
                  <div>
                    <div className={styles.eyebrow}>{t('supply.ops_next_action')}</div>
                    <h2>{t('supply.runtime_summary')}</h2>
                  </div>
                  <span className={`${styles.statusPill} ${smartTone(smart)}`}>
                    {t(`supply.smart_health_${healthLevel}`, {
                      defaultValue: smart?.healthLevel || '-',
                    })}
                  </span>
                </div>
                <div className={styles.decisionBody}>
                  <span>{t('supply.ops_next_action')}</span>
                  <strong>{smartActionLabel(suggestedAction, decisionReason)}</strong>
                  <p>
                    {t('supply.decision_reason')}:{' '}
                    {t(`supply.smart_reason_${decisionReason}`, {
                      defaultValue: decisionReason,
                    })}
                  </p>
                </div>
                <div className={styles.demandStrategy}>
                  <div className={styles.demandStrategyHeader}>
                    <div>
                      <span>{t('supply.demand_strategy')}</span>
                      <strong>{demandStrategy}</strong>
                    </div>
                    <small>{demandBasis}</small>
                  </div>
                  <div className={styles.demandMetricGrid}>
                    <div>
                      <span>{t('supply.demand_actual_1m')}</span>
                      <strong>
                        {formatTokenMRate(
                          smart?.consumeTokenM1m ?? rcuToTokenM(smart?.consumeRcu1m)
                        )}
                      </strong>
                    </div>
                    <div>
                      <span>{t('supply.demand_reference_5m')}</span>
                      <strong>
                        {formatTokenMRate(
                          smart?.consumeTokenM5m ?? rcuToTokenM(smart?.consumeRcu5m)
                        )}
                      </strong>
                    </div>
                    <div>
                      <span>{t('supply.demand_reference_10m')}</span>
                      <strong>
                        {formatTokenMRate(
                          smart?.consumeTokenM10m ?? rcuToTokenM(smart?.consumeRcu10m)
                        )}
                      </strong>
                    </div>
                    <div>
                      <span>{t('supply.demand_purchase_basis')}</span>
                      <strong>
                        {formatTokenMRate(
                          smart?.demandPlanningTokenMPerMinute ??
                            rcuToTokenM(smart?.demandPlanningRcuPerMinute)
                        )}
                      </strong>
                    </div>
                  </div>
                </div>
                <div className={styles.executionStrip} aria-live="polite">
                  <div className={`${styles.executionCell} ${styles.executionCountdown}`}>
                    <div className={styles.executionIcon}>
                      <IconTimer size={17} />
                    </div>
                    <div>
                      <span>{t('supply.automation_next_execution')}</span>
                      <strong className={styles.countdownValue}>{nextExecutionCountdown}</strong>
                      <small title={nextExecutionDetail}>{nextExecutionDetail}</small>
                    </div>
                  </div>
                  <div className={styles.executionCell}>
                    <div className={styles.executionIcon}>
                      <IconRefreshCw size={17} />
                    </div>
                    <div>
                      <span>{t('supply.automation_last_execution')}</span>
                      <strong>{lastExecutionOutcome}</strong>
                      <small title={lastExecutionTooltip}>{lastExecutionSummary}</small>
                    </div>
                  </div>
                  <div className={styles.executionCell}>
                    <div className={styles.executionIcon}>
                      <IconInbox size={17} />
                    </div>
                    <div>
                      <span>{t('supply.automation_order_execution')}</span>
                      <strong>{orderExecutionTitle}</strong>
                      <small title={orderExecutionDetail}>{orderExecutionDetail}</small>
                    </div>
                  </div>
                </div>
                {automation?.lastError ? (
                  <div
                    className={
                      lastExecutionRetrying ? styles.executionRetry : styles.executionError
                    }
                  >
                    {t('supply.automation_last_error')}:{' '}
                    {localizeRuntimeError(automation.lastError)}
                  </div>
                ) : null}
                <div className={styles.decisionFooter}>
                  <div>
                    <span>{t('supply.suggested_quantity')}</span>
                    <strong>{smart?.suggestedQuantity ?? '-'}</strong>
                  </div>
                  <div>
                    <span>{t('supply.projected_sustain_after_refill')}</span>
                    <strong>
                      {smart?.projectedSustainAfterRefillMinutes != null
                        ? formatMinutes(smart.projectedSustainAfterRefillMinutes)
                        : '-'}
                    </strong>
                  </div>
                  <div>
                    <span>{t('supply.confidence')}</span>
                    <strong>
                      {t(`supply.smart_confidence_${confidence}`, {
                        defaultValue: confidence,
                      })}
                    </strong>
                  </div>
                  <div>
                    <span>{t('supply.snapshot_status')}</span>
                    <strong>{snapshotLabel}</strong>
                  </div>
                </div>
              </article>

              <article className={`${styles.panel} ${styles.capacityPanel}`}>
                <div className={styles.panelHeader}>
                  <div>
                    <h2>{t('supply.capacity_summary')}</h2>
                    <p>
                      {t('supply.effective_health_target_minutes', {
                        value: effectiveHealthTargetMinutes,
                        configured: configuredHealthTargetMinutes,
                      })}
                    </p>
                  </div>
                </div>
                <div className={styles.capacityOverview}>
                  <div>
                    <span>{t('supply.current_capacity')}</span>
                    <strong>{formatTokenM(timeLimitedCapacityTokenM)}</strong>
                  </div>
                  <div>
                    <span>{t('supply.target_capacity')}</span>
                    <strong>{formatTokenM(targetCapacityTokenM)}</strong>
                  </div>
                </div>
                <div className={styles.progressTrack}>
                  <span style={{ width: `${capacityPercent}%` }} />
                </div>
                <div className={styles.miniMetricGrid}>
                  <div>
                    <span>{t('supply.capacity_gap_label')}</span>
                    <strong>{formatTokenM(capacityGapTokenM)}</strong>
                  </div>
                  <div>
                    <span>{t('supply.supply_pressure')}</span>
                    <strong>
                      {t(`supply.supply_pressure_${supplyPressureLevel}`, {
                        defaultValue: supplyPressureLevel,
                      })}
                    </strong>
                  </div>
                  <div>
                    <span>{t('supply.capacity_coverage_label')}</span>
                    <strong>{formatNumber(smart?.capacityCoverage, 0)}%</strong>
                  </div>
                  <div>
                    <span>{t('supply.capacity_source_label')}</span>
                    <strong>
                      {t(`supply.capacity_source_${smart?.capacitySource || 'unavailable'}`, {
                        defaultValue: smart?.capacitySource || '-',
                      })}
                    </strong>
                  </div>
                  <div title={concurrencySlotsTitle}>
                    <span>{t('supply.concurrency_capacity')}</span>
                    <strong>{concurrencySlotsLabel}</strong>
                  </div>
                  <div>
                    <span>{t('supply.average_request_latency')}</span>
                    <strong>
                      {(smart?.averageRequestLatencyMs ?? 0) > 0
                        ? `${formatNumber(smart?.averageRequestLatencyMs, 0)} ms`
                        : '-'}
                    </strong>
                  </div>
                </div>
              </article>

              <article className={styles.panel}>
                <div className={styles.panelHeader}>
                  <div>
                    <h2>{t('supply.supply_summary')}</h2>
                    <p>
                      {smartModeEnabled ? t('supply.smart_enabled') : t('supply.smart_disabled')}
                    </p>
                  </div>
                </div>
                <div className={styles.summaryList}>
                  <div>
                    <span>{t('supply.supply_inventory')}</span>
                    <strong>{inventory?.available ?? '-'}</strong>
                  </div>
                  <div>
                    <span>{t('supply.deficit')}</span>
                    <strong>{inventory?.missing ?? overview?.cpaDeficit ?? '-'}</strong>
                  </div>
                  <div>
                    <span>{t('supply.available_balance')}</span>
                    <strong>{balance ? formatMoney(balance.availableFen) : '-'}</strong>
                  </div>
                  <div>
                    <span>{t('supply.estimated_total')}</span>
                    <strong>{inventory ? formatMoney(inventory.estimatedTotalFen) : '-'}</strong>
                  </div>
                  <div>
                    <span>{t('supply.recent_fulfillment')}</span>
                    <strong>
                      {(smart?.supplyRecentRequestedQuantity ?? 0) > 0
                        ? `${formatInteger(smart?.supplyRecentDeliveredQuantity)} / ${formatInteger(smart?.supplyRecentRequestedQuantity)} (${formatNumber(smart?.supplyFulfillmentRate, 1)}%)`
                        : '-'}
                    </strong>
                  </div>
                  <div>
                    <span>{t('supply.recent_cancelled_orders')}</span>
                    <strong>
                      {formatInteger(smart?.supplyRecentCancelled)} /{' '}
                      {formatInteger(smart?.supplyRecentOrders)}
                    </strong>
                  </div>
                </div>
              </article>

              <article className={styles.panel}>
                <div className={styles.panelHeader}>
                  <div>
                    <h2>{t('supply.traffic_summary')}</h2>
                    <p>
                      {t('supply.usage_sample')}: {smart?.usageSampleMinutes ?? 0}m
                    </p>
                  </div>
                </div>
                <div className={styles.summaryList}>
                  <div>
                    <span>{t('supply.consume_rate')}</span>
                    <strong>{formatTokenMRate(consumeTokenMPerMinute)}</strong>
                  </div>
                  <div>
                    <span>{t('supply.rpm30m')}</span>
                    <strong>{formatNumber(smart?.rpm30m)}</strong>
                  </div>
                  <div>
                    <span>{t('supply.tpm30m')}</span>
                    <strong>{formatNumber(smart?.tpm30m, 0)}</strong>
                  </div>
                  <div>
                    <span>{t('supply.quota_snapshot_age_label')}</span>
                    <strong>{smart ? `${smart.capacitySnapshotAgeSeconds ?? 0}s` : '-'}</strong>
                  </div>
                </div>
              </article>
            </section>
          ) : null}

          {activeTab === 'automation' ? (
            <section className={styles.automationGrid}>
              <article className={`${styles.panel} ${styles.fullSpan}`}>
                <div className={styles.panelHeader}>
                  <div>
                    <h2>{t('supply.supply_connection_title')}</h2>
                    <p>{t('supply.supply_connection_hint')}</p>
                  </div>
                  <Button variant="secondary" size="sm" onClick={addSupplyPlatform}>
                    <IconPlus size={15} />
                    {t('supply.platform_add')}
                  </Button>
                </div>
                <div className={styles.platformList}>
                  {(draft.platforms ?? []).map((platform, platformIndex) => {
                    const live = platformOverviewById.get(platform.id.trim().toLowerCase());
                    const sessionRefresh = status?.sessionRefresh?.find(
                      (item) =>
                        item.platformId.trim().toLowerCase() === platform.id.trim().toLowerCase()
                    );
                    const catalogState = platformCatalogs[platform.id.trim().toLowerCase()];
                    const catalogProducts = catalogState?.catalog?.products ?? [];
                    const supplierQuotaScores =
                      catalogState?.catalog?.supplierQuotaScores ?? live?.supplierQuotaScores ?? [];
                    const platformProductOptions = (() => {
                      if (platform.type === 'bugteam') {
                        return [{ value: 'team_1h', label: t('supply.product_team_1h') }];
                      }
                      if (platform.type !== 'nvtokens') {
                        return [
                          { value: 'oauth_30d', label: t('supply.product_30d') },
                          { value: 'oauth_7d', label: t('supply.product_7d') },
                          { value: 'team_1h', label: t('supply.product_team_1h') },
                        ];
                      }
                      const options = catalogProducts.map((product) => {
                        const min = product.minUnitPriceFen ?? 0;
                        const max = product.maxUnitPriceFen ?? 0;
                        const price =
                          min > 0 && max > min
                            ? `${formatMoney(min)}–${formatMoney(max)}`
                            : min > 0 || max > 0
                              ? formatMoney(min || max)
                              : '';
                        return {
                          value: product.code,
                          label: `${product.label} · ${t('supply.catalog_inventory', {
                            value: product.available,
                          })}${price ? ` · ${price}` : ''}`,
                        };
                      });
                      if (options.length === 0) {
                        return [
                          {
                            value: platform.product || 'plus',
                            label: platform.product || 'Plus',
                          },
                        ];
                      }
                      if (!options.some((option) => option.value === platform.product)) {
                        options.unshift({
                          value: platform.product,
                          label: `${platform.product} · ${t('supply.catalog_product_unavailable')}`,
                        });
                      }
                      return options;
                    })();
                    return (
                      <section className={styles.platformRow} key={platform.id || platformIndex}>
                        <div className={styles.platformRowHeader}>
                          <div className={styles.platformIdentity}>
                            <strong>{platform.name || platform.id}</strong>
                            <span>{platform.id}</span>
                            {live?.selected ? (
                              <span className={`${styles.statusPill} ${styles.active}`}>
                                {t('supply.platform_selected')}
                              </span>
                            ) : null}
                          </div>
                          <div className={styles.platformActions}>
                            <ToggleSwitch
                              checked={platform.enabled !== false}
                              onChange={(enabled) =>
                                updateSupplyPlatform(platformIndex, { enabled })
                              }
                              label={t('supply.platform_enabled')}
                            />
                            <ToggleSwitch
                              checked={platform.emergencyOnly === true}
                              onChange={(emergencyOnly) =>
                                updateSupplyPlatform(platformIndex, { emergencyOnly })
                              }
                              label={t('supply.platform_emergency_only')}
                            />
                            {platform.type === 'nvtokens' ? (
                              <Button
                                type="button"
                                variant="secondary"
                                size="xs"
                                loading={refreshingPlatformId === platform.id}
                                disabled={
                                  platform.sessionRefreshEnabled !== true ||
                                  !platform.challengeApiKeyConfigured ||
                                  !platform.passwordConfigured
                                }
                                onClick={() => void refreshPlatformSession(platform.id)}
                              >
                                <IconRefreshCw size={13} />
                                {t('supply.session_refresh_now')}
                              </Button>
                            ) : null}
                            <Button
                              variant="ghost"
                              size="xs"
                              iconOnly
                              disabled={(draft.platforms?.length ?? 0) <= 1}
                              title={t('supply.platform_remove')}
                              aria-label={t('supply.platform_remove')}
                              onClick={() => removeSupplyPlatform(platformIndex)}
                            >
                              <IconTrash2 size={14} />
                            </Button>
                          </div>
                        </div>
                        <div className={styles.platformFormGrid}>
                          <div className={styles.field}>
                            <label>{t('supply.platform_type')}</label>
                            <Select
                              value={platform.type}
                              options={[
                                { value: 'legacy', label: t('supply.platform_type_legacy') },
                                { value: 'bugteam', label: t('supply.platform_type_bugteam') },
                                { value: 'nvtokens', label: t('supply.platform_type_nvtokens') },
                              ]}
                              onChange={(type) => updateSupplyPlatform(platformIndex, { type })}
                            />
                          </div>
                          <Input
                            label={t('supply.platform_name')}
                            value={platform.name ?? ''}
                            onChange={(event) =>
                              updateSupplyPlatform(platformIndex, { name: event.target.value })
                            }
                          />
                          <Input
                            label={t('supply.base_url')}
                            value={platform.baseUrl}
                            disabled={platform.type === 'bugteam'}
                            onChange={(event) =>
                              updateSupplyPlatform(platformIndex, { baseUrl: event.target.value })
                            }
                          />
                          <Input
                            label={t('supply.username')}
                            value={platform.username ?? ''}
                            onChange={(event) => {
                              const username = event.target.value;
                              const clearUsername = username.trim() === '';
                              updateSupplyPlatform(platformIndex, {
                                username,
                                clearUsername,
                                ...(clearUsername
                                  ? { password: '', passwordConfigured: false }
                                  : {}),
                              });
                            }}
                            rightElement={
                              platform.username?.trim() ? (
                                <Button
                                  type="button"
                                  variant="ghost"
                                  size="xs"
                                  iconOnly
                                  title={t('common.reset')}
                                  aria-label={t('common.reset')}
                                  onClick={() =>
                                    updateSupplyPlatform(platformIndex, {
                                      username: '',
                                      clearUsername: true,
                                      password: '',
                                      passwordConfigured: false,
                                    })
                                  }
                                >
                                  <IconX size={14} />
                                </Button>
                              ) : undefined
                            }
                            autoComplete="username"
                          />
                          <Input
                            label={t('supply.password')}
                            type="password"
                            value={platform.password ?? ''}
                            onChange={(event) =>
                              updateSupplyPlatform(platformIndex, { password: event.target.value })
                            }
                            placeholder={
                              platform.passwordConfigured
                                ? t('supply.password_saved')
                                : t('supply.password_placeholder')
                            }
                            autoComplete="new-password"
                          />
                          <Input
                            label={t('supply.platform_token')}
                            type="password"
                            value={platform.token ?? ''}
                            onChange={(event) =>
                              updateSupplyPlatform(platformIndex, { token: event.target.value })
                            }
                            placeholder={
                              platform.tokenConfigured
                                ? t('supply.platform_token_saved')
                                : t('supply.platform_token_placeholder')
                            }
                            autoComplete="off"
                          />
                          <div className={styles.field}>
                            <div className={styles.fieldLabelRow}>
                              <label>{t('supply.platform_default_product')}</label>
                              {platform.type === 'nvtokens' ? (
                                <Button
                                  type="button"
                                  variant="ghost"
                                  size="xs"
                                  loading={catalogState?.loading === true}
                                  onClick={() => void loadPlatformCatalog(platform, true)}
                                >
                                  <IconRefreshCw size={13} />
                                  {t('supply.catalog_refresh')}
                                </Button>
                              ) : null}
                            </div>
                            <Select
                              value={platform.product}
                              options={platformProductOptions}
                              onChange={(product) =>
                                updateSupplyPlatform(platformIndex, { product })
                              }
                            />
                            {platform.type === 'nvtokens' ? (
                              <small
                                className={catalogState?.error ? styles.fieldError : undefined}
                              >
                                {catalogState?.error ||
                                  (catalogState?.catalog
                                    ? `${t('supply.catalog_loaded', {
                                        count: catalogProducts.length,
                                      })} · ${t('supply.catalog_checked_at', {
                                        value: formatTime(catalogState.catalog.checkedAtMs),
                                      })}`
                                    : t('supply.catalog_auth_hint'))}
                              </small>
                            ) : null}
                          </div>
                          {platform.type === 'nvtokens' ? (
                            <>
                              <div className={styles.field}>
                                <label>{t('supply.session_refresh_enabled')}</label>
                                <ToggleSwitch
                                  checked={platform.sessionRefreshEnabled === true}
                                  onChange={(sessionRefreshEnabled) =>
                                    updateSupplyPlatform(platformIndex, { sessionRefreshEnabled })
                                  }
                                  label={
                                    platform.sessionRefreshEnabled
                                      ? t('common.enabled')
                                      : t('common.disabled')
                                  }
                                />
                              </div>
                              <div className={styles.field}>
                                <label>{t('supply.challenge_provider')}</label>
                                <Select
                                  value={platform.challengeProvider || 'capsolver'}
                                  options={[
                                    { value: 'capsolver', label: 'CapSolver' },
                                    { value: 'capmonster', label: 'CapMonster' },
                                    { value: '2captcha', label: '2Captcha' },
                                    {
                                      value: 'custom',
                                      label: t('supply.challenge_provider_custom'),
                                    },
                                    {
                                      value: 'session_sidecar',
                                      label: t('supply.challenge_provider_sidecar'),
                                    },
                                  ]}
                                  onChange={(challengeProvider) => {
                                    const defaults: Record<string, string> = {
                                      capsolver: 'https://api.capsolver.com',
                                      capmonster: 'https://api.capmonster.cloud',
                                      '2captcha': 'https://api.2captcha.com',
                                    };
                                    updateSupplyPlatform(platformIndex, {
                                      challengeProvider,
                                      challengeApiBase:
                                        defaults[challengeProvider] ??
                                        platform.challengeApiBase ??
                                        '',
                                    });
                                  }}
                                />
                              </div>
                              <Input
                                label={t('supply.challenge_api_base')}
                                value={platform.challengeApiBase ?? ''}
                                onChange={(event) =>
                                  updateSupplyPlatform(platformIndex, {
                                    challengeApiBase: event.target.value,
                                  })
                                }
                                placeholder="https://api.capsolver.com"
                              />
                              <Input
                                label={t('supply.challenge_api_key')}
                                type="password"
                                value={platform.challengeApiKey ?? ''}
                                onChange={(event) =>
                                  updateSupplyPlatform(platformIndex, {
                                    challengeApiKey: event.target.value,
                                    clearChallengeApiKey: false,
                                  })
                                }
                                placeholder={
                                  platform.challengeApiKeyConfigured
                                    ? t('supply.challenge_api_key_saved')
                                    : t('supply.challenge_api_key_placeholder')
                                }
                                rightElement={
                                  platform.challengeApiKeyConfigured ? (
                                    <Button
                                      type="button"
                                      variant="ghost"
                                      size="xs"
                                      iconOnly
                                      title={t('common.reset')}
                                      aria-label={t('common.reset')}
                                      onClick={() =>
                                        updateSupplyPlatform(platformIndex, {
                                          challengeApiKey: '',
                                          challengeApiKeyConfigured: false,
                                          clearChallengeApiKey: true,
                                        })
                                      }
                                    >
                                      <IconX size={14} />
                                    </Button>
                                  ) : undefined
                                }
                                autoComplete="off"
                              />
                              <Input
                                label={t('supply.session_refresh_cooldown')}
                                type="number"
                                min={30}
                                max={3600}
                                value={platform.refreshCooldownSeconds ?? 300}
                                onChange={(event) =>
                                  updateSupplyPlatform(platformIndex, {
                                    refreshCooldownSeconds: Number(event.target.value),
                                  })
                                }
                              />
                              <div className={styles.field}>
                                <label>{t('supply.purchase_account_type')}</label>
                                <Select
                                  value={platform.purchaseAccountType || 'all'}
                                  options={[
                                    {
                                      value: 'all',
                                      label: t('supply.purchase_account_type_all'),
                                    },
                                    {
                                      value: 'has_refresh_token',
                                      label: t('supply.purchase_account_type_has_refresh_token'),
                                    },
                                    {
                                      value: 'without_refresh_token',
                                      label: t(
                                        'supply.purchase_account_type_without_refresh_token'
                                      ),
                                    },
                                  ]}
                                  onChange={(purchaseAccountType) =>
                                    updateSupplyPlatform(platformIndex, { purchaseAccountType })
                                  }
                                />
                              </div>
                              <Input
                                label={t('supply.max_unit_price')}
                                type="number"
                                min={0}
                                step={0.01}
                                value={
                                  (platform.maxUnitPriceFen ?? 0) > 0
                                    ? (platform.maxUnitPriceFen ?? 0) / 100
                                    : ''
                                }
                                placeholder={t('supply.max_unit_price_placeholder')}
                                onChange={(event) => {
                                  const yuan = Number(event.target.value);
                                  updateSupplyPlatform(platformIndex, {
                                    maxUnitPriceFen:
                                      Number.isFinite(yuan) && yuan > 0
                                        ? Math.round(yuan * 100)
                                        : 0,
                                  });
                                }}
                              />
                              <div className={styles.field}>
                                <label>{t('supply.supplier_quota_gate_enabled')}</label>
                                <ToggleSwitch
                                  checked={platform.supplierQuotaGateEnabled === true}
                                  onChange={(supplierQuotaGateEnabled) =>
                                    updateSupplyPlatform(platformIndex, {
                                      supplierQuotaGateEnabled,
                                    })
                                  }
                                  label={
                                    platform.supplierQuotaGateEnabled
                                      ? t('common.enabled')
                                      : t('common.disabled')
                                  }
                                />
                              </div>
                              <Input
                                label={t('supply.supplier_quota_minimum')}
                                type="number"
                                min={0.5}
                                step={0.5}
                                disabled={platform.supplierQuotaGateEnabled !== true}
                                value={platform.supplierQuotaMinimumM ?? 30}
                                onChange={(event) =>
                                  updateSupplyPlatform(platformIndex, {
                                    supplierQuotaMinimumM: Number(event.target.value),
                                  })
                                }
                              />
                              <Input
                                label={t('supply.supplier_quota_trial_quantity')}
                                type="number"
                                min={1}
                                max={5}
                                step={1}
                                disabled={platform.supplierQuotaGateEnabled !== true}
                                value={platform.supplierQuotaTrialQuantity ?? 1}
                                onChange={(event) =>
                                  updateSupplyPlatform(platformIndex, {
                                    supplierQuotaTrialQuantity: Number(event.target.value),
                                  })
                                }
                              />
                              <div className={styles.supplierQuotaGatePanel}>
                                <div className={styles.supplierQuotaGateHeader}>
                                  <div>
                                    <strong>{t('supply.supplier_quota_scores_title')}</strong>
                                    <small>{t('supply.supplier_quota_scores_hint')}</small>
                                  </div>
                                  {platform.supplierQuotaGateEnabled ? (
                                    <span>
                                      {t('supply.supplier_quota_threshold_summary', {
                                        value: formatTokenM(platform.supplierQuotaMinimumM ?? 30),
                                      })}
                                    </span>
                                  ) : (
                                    <span>{t('supply.supplier_quota_gate_disabled_hint')}</span>
                                  )}
                                </div>
                                {platform.supplierQuotaGateEnabled ? (
                                  supplierQuotaScores.length > 0 ? (
                                    <div className={styles.supplierQuotaTableWrap}>
                                      <table>
                                        <thead>
                                          <tr>
                                            <th>{t('supply.supplier_quota_seller')}</th>
                                            <th>{t('supply.supplier_quota_status')}</th>
                                            <th>{t('supply.supplier_quota_score')}</th>
                                            <th>{t('supply.supplier_quota_cost_per_capacity')}</th>
                                            <th>{t('supply.supplier_quota_samples')}</th>
                                            <th>{t('supply.supplier_quota_inventory')}</th>
                                            <th>{t('supply.supplier_quota_price')}</th>
                                          </tr>
                                        </thead>
                                        <tbody>
                                          {supplierQuotaScores.map((score) => (
                                            <tr
                                              key={`${score.sellerId}:${score.channelId ?? ''}:${score.product}`}
                                            >
                                              <td>
                                                <div className={styles.supplierQuotaSeller}>
                                                  <strong>
                                                    {score.sellerName || score.sellerId}
                                                  </strong>
                                                  <small>{score.channelId || score.sellerId}</small>
                                                </div>
                                              </td>
                                              <td>
                                                <span
                                                  className={`${styles.supplierQuotaStatus} ${supplierQuotaStatusClass(score)}`}
                                                  title={t(
                                                    `supply.supplier_quota_reason_${score.reason}`,
                                                    { defaultValue: score.reason }
                                                  )}
                                                >
                                                  {t(
                                                    `supply.supplier_quota_status_${score.status}`,
                                                    {
                                                      defaultValue: score.status,
                                                    }
                                                  )}
                                                  {score.inFlightTrial
                                                    ? ` · ${t('supply.supplier_quota_in_flight')}`
                                                    : ''}
                                                </span>
                                              </td>
                                              <td>
                                                {score.sampleCount > 0
                                                  ? formatTokenM(score.scoreM)
                                                  : '-'}
                                              </td>
                                              <td>
                                                {(score.costMultiplier ?? 0) > 0
                                                  ? formatCostMultiplier(score.costMultiplier)
                                                  : t('supply.supplier_quota_cost_price_fallback')}
                                              </td>
                                              <td>
                                                <div className={styles.supplierQuotaSeller}>
                                                  <span>
                                                    {t('supply.supplier_quota_sample_summary', {
                                                      samples: score.sampleCount,
                                                      accounts: score.importedAccounts,
                                                    })}
                                                  </span>
                                                  {(score.evidenceCount ?? 0) > 0 ? (
                                                    <small>
                                                      {t(
                                                        'supply.supplier_quota_pass_rate_summary',
                                                        {
                                                          passing: score.passingSampleCount ?? 0,
                                                          total: score.evidenceCount ?? 0,
                                                          rate: formatNumber(
                                                            score.passRatePercent ?? 0,
                                                            0
                                                          ),
                                                        }
                                                      )}
                                                    </small>
                                                  ) : null}
                                                  {(score.invalidCredentialCount ?? 0) > 0 ? (
                                                    <small>
                                                      {t('supply.supplier_quota_invalid_summary', {
                                                        count: score.invalidCredentialCount,
                                                      })}
                                                    </small>
                                                  ) : null}
                                                </div>
                                              </td>
                                              <td>{formatInteger(score.available)}</td>
                                              <td>
                                                {(score.minUnitPriceFen ?? 0) > 0
                                                  ? (score.maxUnitPriceFen ?? 0) >
                                                    (score.minUnitPriceFen ?? 0)
                                                    ? `${formatMoney(score.minUnitPriceFen)}–${formatMoney(score.maxUnitPriceFen)}`
                                                    : formatMoney(
                                                        score.minUnitPriceFen ||
                                                          score.maxUnitPriceFen
                                                      )
                                                  : '-'}
                                              </td>
                                            </tr>
                                          ))}
                                        </tbody>
                                      </table>
                                    </div>
                                  ) : (
                                    <p className={styles.supplierQuotaEmpty}>
                                      {catalogState?.error
                                        ? catalogState.error
                                        : catalogState?.loading
                                          ? t('common.loading')
                                          : t('supply.supplier_quota_scores_empty')}
                                    </p>
                                  )
                                ) : null}
                              </div>
                            </>
                          ) : null}
                          <Input
                            label={t('supply.platform_priority')}
                            type="number"
                            min={1}
                            max={1000}
                            value={platform.priority ?? platformIndex + 1}
                            onChange={(event) =>
                              updateSupplyPlatform(platformIndex, {
                                priority: Number(event.target.value),
                              })
                            }
                          />
                        </div>
                        {platform.type === 'bugteam' ? (
                          <p className={styles.platformHint}>
                            {t('supply.platform_bugteam_auth_hint')}
                          </p>
                        ) : null}
                        {platform.type === 'nvtokens' ? (
                          <p
                            className={`${styles.platformHint} ${
                              sessionRefresh?.lastError ? styles.fieldError : ''
                            }`}
                          >
                            {t('supply.session_refresh_status', {
                              state: t(
                                sessionRefresh?.state === 'refreshing'
                                  ? 'supply.session_refresh_state_refreshing'
                                  : sessionRefresh?.state === 'waiting_challenge'
                                    ? 'supply.session_refresh_state_waiting_challenge'
                                    : sessionRefresh?.state === 'cooldown'
                                      ? 'supply.session_refresh_state_cooldown'
                                      : sessionRefresh?.state === 'healthy'
                                        ? 'supply.session_refresh_state_healthy'
                                        : 'supply.session_refresh_state_disabled'
                              ),
                            })}
                            {sessionRefresh?.lastError ? ` · ${sessionRefresh.lastError}` : ''}
                          </p>
                        ) : null}
                        {platform.emergencyOnly ? (
                          <p className={styles.platformHint}>
                            {t('supply.platform_emergency_only_hint')}
                          </p>
                        ) : null}
                        <div className={styles.platformQuotaGrid}>
                          {quotaPlanTypes.map((planType) => {
                            const globalPolicy = quotaPolicyForPlan(planType);
                            const policy = {
                              ...globalPolicy,
                              ...platform.quotaEstimationPolicies?.[planType],
                            };
                            return (
                              <div className={styles.platformQuotaRow} key={planType}>
                                <strong>
                                  {t(`supply.quota_plan_type_${planType}`, {
                                    defaultValue: planType.toUpperCase(),
                                  })}
                                </strong>
                                <Select
                                  value={policy.mode}
                                  options={[
                                    { value: 'auto', label: t('supply.quota_plan_mode_auto') },
                                    { value: 'fixed', label: t('supply.quota_plan_mode_fixed') },
                                  ]}
                                  onChange={(mode) =>
                                    updatePlatformQuotaEstimationPolicy(platformIndex, planType, {
                                      mode,
                                    })
                                  }
                                  ariaLabel={t('supply.quota_plan_mode')}
                                />
                                <Input
                                  aria-label={
                                    policy.mode === 'fixed'
                                      ? t('supply.quota_plan_fixed_input')
                                      : t('supply.quota_plan_fallback_input')
                                  }
                                  type="number"
                                  min={0.5}
                                  step={0.5}
                                  value={policy.mode === 'fixed' ? policy.fixedM : policy.fallbackM}
                                  onChange={(event) =>
                                    updatePlatformQuotaEstimationPolicy(platformIndex, planType, {
                                      [policy.mode === 'fixed' ? 'fixedM' : 'fallbackM']: Number(
                                        event.target.value
                                      ),
                                    })
                                  }
                                />
                              </div>
                            );
                          })}
                        </div>
                        <div className={styles.platformRuntime}>
                          <span>
                            {t('supply.supply_inventory')}: {live?.inventory?.available ?? '-'}
                          </span>
                          <span>
                            {t('supply.available_balance')}:{' '}
                            {live?.balance ? formatMoney(live.balance.availableFen) : '-'}
                          </span>
                          <span>
                            {t('supply.estimated_total')}:{' '}
                            {live?.inventory ? formatMoney(live.inventory.estimatedTotalFen) : '-'}
                          </span>
                          <span>
                            {t('supply.platform_expected_quota')}:{' '}
                            {live?.expectedQuotaM ? formatTokenM(live.expectedQuotaM, 1) : '-'}
                          </span>
                          <span>
                            {t('supply.platform_usable_quota')}:{' '}
                            {live?.usableQuotaM ? formatTokenM(live.usableQuotaM, 1) : '-'}
                          </span>
                          <span>
                            {t('supply.platform_cost_per_quota')}:{' '}
                            {live?.costMultiplier
                              ? formatCostMultiplier(live.costMultiplier)
                              : t('supply.supplier_quota_cost_price_fallback')}
                          </span>
                          {live?.lastError ? <strong>{live.lastError}</strong> : null}
                        </div>
                      </section>
                    );
                  })}
                </div>
                <div className={styles.platformGlobalFields}>
                  <Input
                    label={t('supply.revenue_multiplier')}
                    type="number"
                    min={0.000001}
                    max={100}
                    step={0.001}
                    value={draft.revenueMultiplier ?? 0.06}
                    onChange={(event) =>
                      updateDraft({ revenueMultiplier: Number(event.target.value) })
                    }
                  />
                </div>
              </article>

              <article className={styles.panel}>
                <div className={styles.panelHeader}>
                  <div>
                    <h2>{t('supply.automation_rules_title')}</h2>
                    <p>{t('supply.automation_rules_hint')}</p>
                  </div>
                </div>
                <div className={styles.formGrid}>
                  {draft.smartEnabled === false ? (
                    <Input
                      label={t('supply.target_accounts')}
                      type="number"
                      min={1}
                      max={10000}
                      value={draft.targetAvailableAccounts}
                      onChange={(event) =>
                        updateDraft({ targetAvailableAccounts: Number(event.target.value) })
                      }
                    />
                  ) : null}
                  <Input
                    label={t('supply.batch_size')}
                    type="number"
                    min={1}
                    max={100}
                    value={draft.replenishBatchSize}
                    onChange={(event) =>
                      updateDraft({ replenishBatchSize: Number(event.target.value) })
                    }
                  />
                  <Input
                    label={t('supply.max_concurrent_orders')}
                    type="number"
                    min={1}
                    max={3}
                    value={draft.maxConcurrentOrders ?? 3}
                    onChange={(event) =>
                      updateDraft({ maxConcurrentOrders: Number(event.target.value) })
                    }
                  />
                  <Input
                    label={t('supply.check_interval')}
                    type="number"
                    min={10}
                    max={3600}
                    value={draft.checkIntervalSeconds}
                    onChange={(event) =>
                      updateDraft({ checkIntervalSeconds: Number(event.target.value) })
                    }
                  />
                  <Input
                    label={t('supply.poll_interval')}
                    type="number"
                    min={1}
                    max={60}
                    value={draft.pollIntervalSeconds}
                    onChange={(event) =>
                      updateDraft({ pollIntervalSeconds: Number(event.target.value) })
                    }
                  />
                </div>
                <div className={styles.reportSectionHeader}>
                  <span>{t('supply.low_price_reserve_title')}</span>
                  <small>{t('supply.low_price_reserve_hint')}</small>
                </div>
                <div className={styles.smartToggles}>
                  <ToggleSwitch
                    checked={draft.lowPriceReserveEnabled === true}
                    onChange={(lowPriceReserveEnabled) => updateDraft({ lowPriceReserveEnabled })}
                    label={t('supply.low_price_reserve_enable')}
                  />
                </div>
                {draft.lowPriceReserveEnabled ? (
                  <>
                    <div className={styles.formGrid}>
                      <Input
                        label={t('supply.low_price_reserve_max_price')}
                        hint={t('supply.low_price_reserve_max_price_hint')}
                        type="number"
                        min={0.01}
                        max={1000000}
                        step={0.01}
                        value={(draft.lowPriceReserveMaxUnitPriceFen ?? 0) / 100}
                        onChange={(event) =>
                          updateDraft({
                            lowPriceReserveMaxUnitPriceFen: Math.max(
                              0,
                              Math.round(Number(event.target.value) * 100)
                            ),
                          })
                        }
                      />
                      <Input
                        label={t('supply.low_price_reserve_target_accounts')}
                        hint={t('supply.low_price_reserve_target_accounts_hint')}
                        type="number"
                        min={1}
                        max={10000}
                        value={draft.lowPriceReserveTargetAccounts ?? 200}
                        onChange={(event) =>
                          updateDraft({
                            lowPriceReserveTargetAccounts: Number(event.target.value),
                          })
                        }
                      />
                      <Input
                        label={t('supply.low_price_reserve_check_interval')}
                        hint={t('supply.low_price_reserve_check_interval_hint')}
                        type="number"
                        min={250}
                        max={600000}
                        step={250}
                        value={draft.lowPriceReserveCheckIntervalMilliseconds ?? 1000}
                        onChange={(event) =>
                          updateDraft({
                            lowPriceReserveCheckIntervalMilliseconds: Number(event.target.value),
                          })
                        }
                      />
                    </div>
                    <div className={styles.lowPriceRuntime}>
                      <div>
                        <span>{t('supply.low_price_reserve_pool')}</span>
                        <strong>
                          {lowPriceReserve?.reserveAccounts ?? 0}/
                          {lowPriceReserve?.targetAccounts ??
                            draft.lowPriceReserveTargetAccounts ??
                            200}
                        </strong>
                        <small>
                          {t('supply.low_price_reserve_gap', {
                            value:
                              lowPriceReserve?.gap ?? draft.lowPriceReserveTargetAccounts ?? 200,
                          })}
                        </small>
                      </div>
                      <div>
                        <span>{t('supply.low_price_reserve_ladder')}</span>
                        <strong>{lowPriceReserve?.ladder?.join(' → ') || '-'}</strong>
                        <small>
                          {t('supply.low_price_reserve_next_stage', {
                            value: lowPriceReserve?.nextStageQuantity ?? '-',
                          })}
                        </small>
                      </div>
                      <div>
                        <span>{t('supply.low_price_reserve_quote')}</span>
                        <strong>
                          {lowPriceReserve?.lastQuotedCostMultiplier
                            ? formatCostMultiplier(lowPriceReserve.lastQuotedCostMultiplier)
                            : '-'}
                        </strong>
                        <small>
                          {lowPriceReserve?.lastQuotedUnitPriceFen
                            ? `${formatMoney(lowPriceReserve.lastQuotedUnitPriceFen)} · ${lowPriceReserve.selectedPlatformId || '-'}`
                            : lowPriceReserve?.selectedPlatformId || '-'}
                        </small>
                      </div>
                      <div>
                        <span>{t('supply.low_price_reserve_next_check')}</span>
                        <strong>
                          {lowPriceReserve?.running
                            ? t('supply.automation_running')
                            : lowPriceReserve?.nextCheckAtMs
                              ? formatCountdown(lowPriceReserve.nextCheckAtMs, nowMs)
                              : '-'}
                        </strong>
                        <small>
                          {t(
                            `supply.low_price_reserve_result_${lowPriceReserve?.lastResult || 'scheduled'}`,
                            {
                              defaultValue: lowPriceReserve?.lastResult || '-',
                            }
                          )}
                        </small>
                      </div>
                    </div>
                    {lowPriceReserve?.activeTaskId ? (
                      <div className={styles.lowPriceRuntimeNote}>
                        {t('supply.low_price_reserve_active_task', {
                          value: lowPriceReserve.activeTaskId,
                        })}
                      </div>
                    ) : null}
                    {lowPriceReserve?.lastError ? (
                      <div className={styles.executionError}>
                        {localizeRuntimeError(lowPriceReserve.lastError)}
                      </div>
                    ) : null}
                  </>
                ) : null}
              </article>

              <article className={styles.panel}>
                <div className={styles.panelHeader}>
                  <div>
                    <h2>{t('supply.recovery_config_title')}</h2>
                    <p>{t('supply.recovery_config_hint')}</p>
                  </div>
                </div>
                <div className={styles.formGrid}>
                  <Input
                    label={t('supply.recovery_sync_interval')}
                    type="number"
                    min={10}
                    max={3600}
                    value={draft.recoverySyncIntervalSeconds ?? 60}
                    onChange={(event) =>
                      updateDraft({ recoverySyncIntervalSeconds: Number(event.target.value) })
                    }
                  />
                  <Input
                    label={t('supply.recovery_claim_batch_size')}
                    type="number"
                    min={1}
                    max={100}
                    value={draft.recoveryClaimBatchSize ?? 20}
                    onChange={(event) =>
                      updateDraft({ recoveryClaimBatchSize: Number(event.target.value) })
                    }
                  />
                </div>
                <div className={styles.smartToggles}>
                  <ToggleSwitch
                    checked={draft.recoverySyncEnabled !== false}
                    onChange={(recoverySyncEnabled) => updateDraft({ recoverySyncEnabled })}
                    label={t('supply.recovery_sync_enable')}
                  />
                  <ToggleSwitch
                    checked={draft.recoveryAutoClaim !== false}
                    onChange={(recoveryAutoClaim) => updateDraft({ recoveryAutoClaim })}
                    label={t('supply.recovery_auto_claim')}
                  />
                  <ToggleSwitch
                    checked={draft.recoveryDisableOriginal !== false}
                    onChange={(recoveryDisableOriginal) => updateDraft({ recoveryDisableOriginal })}
                    label={t('supply.recovery_disable_original')}
                  />
                </div>
              </article>

              <article className={`${styles.panel} ${styles.fullSpan}`}>
                <div className={styles.panelHeader}>
                  <div>
                    <h2>{t('supply.smart_config_title')}</h2>
                    <p>{t('supply.smart_config_hint')}</p>
                  </div>
                  <ToggleSwitch
                    checked={draft.smartEnabled !== false}
                    onChange={(smartEnabled) => updateDraft({ smartEnabled })}
                    label={t('supply.smart_enable')}
                  />
                </div>
                <div className={styles.strategySelector}>
                  <SegmentedTabs<SupplyStrategy>
                    items={supplyStrategyItems}
                    activeTab={draftStrategy}
                    ariaLabel={t('supply.strategy_selector_aria')}
                    onChange={selectSupplyStrategy}
                    fullWidth
                    equalWidth
                  />
                  <div className={styles.strategyDescription}>
                    <strong>{t(`supply.strategy_${draftStrategy}`)}</strong>
                    <span>{t(`supply.strategy_${draftStrategy}_description`)}</span>
                    <small>{t(`supply.strategy_${draftStrategy}_scenario`)}</small>
                  </div>
                </div>
                <div className={styles.strategyMetricGrid}>
                  <div>
                    <span>{t('supply.strategy_critical_accounts')}</span>
                    <strong>{draft.criticalAvailableAccounts ?? 0}</strong>
                  </div>
                  <div>
                    <span>{t('supply.strategy_healthy_accounts')}</span>
                    <strong>{draft.healthyAvailableAccounts ?? 0}</strong>
                  </div>
                  <div>
                    <span>{t('supply.strategy_startup_accounts')}</span>
                    <strong>{draft.startupAvailableAccounts ?? 0}</strong>
                  </div>
                  <div>
                    <span>{t('supply.strategy_emergency_min_accounts')}</span>
                    <strong>{draft.defaultEmergencyMinAccounts ?? 0}</strong>
                  </div>
                  <div>
                    <span>{t('supply.strategy_virtual_demand_ttl')}</span>
                    <strong>{draft.virtualDemandTtlMinutes ?? 0}m</strong>
                  </div>
                  <div>
                    <span>{t('supply.strategy_request_risk_limit')}</span>
                    <strong>{draft.accountMaxRequestsBefore401 ?? 0}</strong>
                  </div>
                  <div>
                    <span>{t('supply.strategy_time_risk_limit')}</span>
                    <strong>{formatSeconds(draft.accountMaxUsefulSecondsBefore401)}</strong>
                  </div>
                </div>
                <div className={styles.formGrid}>
                  <Input
                    label={t('supply.strategy_startup_accounts')}
                    hint={t('supply.strategy_startup_accounts_hint')}
                    type="number"
                    min={1}
                    max={100}
                    value={draft.startupAvailableAccounts ?? 5}
                    onChange={(event) =>
                      updateDraft({ startupAvailableAccounts: Number(event.target.value) })
                    }
                  />
                </div>
                {draftStrategy === 'custom' ? (
                  <>
                    <div className={styles.reportSectionHeader}>
                      <span>{t('supply.strategy_custom_parameters')}</span>
                      <small>{t('supply.strategy_custom_parameters_hint')}</small>
                    </div>
                    <div className={styles.formGrid}>
                      <Input
                        label={t('supply.strategy_critical_accounts')}
                        type="number"
                        min={0}
                        max={1000}
                        value={draft.criticalAvailableAccounts ?? 0}
                        onChange={(event) =>
                          updateDraft({ criticalAvailableAccounts: Number(event.target.value) })
                        }
                      />
                      <Input
                        label={t('supply.strategy_healthy_accounts')}
                        type="number"
                        min={draft.criticalAvailableAccounts ?? 0}
                        max={10000}
                        value={draft.healthyAvailableAccounts ?? 0}
                        onChange={(event) =>
                          updateDraft({ healthyAvailableAccounts: Number(event.target.value) })
                        }
                      />
                      <Input
                        label={t('supply.strategy_emergency_min_accounts')}
                        type="number"
                        min={1}
                        max={100}
                        value={draft.defaultEmergencyMinAccounts ?? 1}
                        onChange={(event) =>
                          updateDraft({ defaultEmergencyMinAccounts: Number(event.target.value) })
                        }
                      />
                      <Input
                        label={t('supply.strategy_virtual_demand_ttl')}
                        type="number"
                        min={1}
                        max={180}
                        value={draft.virtualDemandTtlMinutes ?? 60}
                        onChange={(event) =>
                          updateDraft({ virtualDemandTtlMinutes: Number(event.target.value) })
                        }
                      />
                      <Input
                        label={t('supply.strategy_request_risk_limit')}
                        type="number"
                        min={1}
                        max={100000}
                        value={draft.accountMaxRequestsBefore401 ?? 30}
                        onChange={(event) =>
                          updateDraft({ accountMaxRequestsBefore401: Number(event.target.value) })
                        }
                      />
                      <Input
                        label={t('supply.strategy_time_risk_limit')}
                        type="number"
                        min={1}
                        max={3600}
                        value={draft.accountMaxUsefulSecondsBefore401 ?? 120}
                        onChange={(event) =>
                          updateDraft({
                            accountMaxUsefulSecondsBefore401: Number(event.target.value),
                          })
                        }
                      />
                    </div>
                    <div className={styles.smartToggles}>
                      <ToggleSwitch
                        checked={draft.emergencyBypassUsageRate !== false}
                        onChange={(emergencyBypassUsageRate) =>
                          updateDraft({ emergencyBypassUsageRate })
                        }
                        label={t('supply.strategy_emergency_bypass_usage')}
                      />
                      <ToggleSwitch
                        checked={draft.recoveryTriggerOn401 !== false}
                        onChange={(recoveryTriggerOn401) => updateDraft({ recoveryTriggerOn401 })}
                        label={t('supply.strategy_recovery_trigger_401')}
                      />
                    </div>
                  </>
                ) : null}
                <div className={styles.reportSectionHeader}>
                  <span>{t('supply.quota_plan_config_title')}</span>
                  <small>{t('supply.quota_plan_config_hint')}</small>
                </div>
                <div className={styles.quotaPolicyConfigGrid}>
                  {quotaPlanTypes.map((planType) => {
                    const policy = quotaPolicyForPlan(planType);
                    return (
                      <div className={styles.quotaPolicyConfigCard} key={planType}>
                        <div className={styles.quotaPolicyConfigHeader}>
                          <strong>
                            {t(`supply.quota_plan_type_${planType}`, {
                              defaultValue: planType.toUpperCase(),
                            })}
                          </strong>
                          <span>
                            {policy.mode === 'fixed'
                              ? t('supply.quota_plan_fixed_hint')
                              : t('supply.quota_plan_auto_hint')}
                          </span>
                        </div>
                        <div className={styles.quotaPolicyFields}>
                          <div className={styles.field}>
                            <label>{t('supply.quota_plan_mode')}</label>
                            <Select
                              value={policy.mode}
                              options={[
                                { value: 'auto', label: t('supply.quota_plan_mode_auto') },
                                { value: 'fixed', label: t('supply.quota_plan_mode_fixed') },
                              ]}
                              onChange={(mode) => updateQuotaEstimationPolicy(planType, { mode })}
                              ariaLabel={t('supply.quota_plan_mode')}
                            />
                          </div>
                          {policy.mode === 'fixed' ? (
                            <Input
                              label={t('supply.quota_plan_fixed_input')}
                              type="number"
                              min={0.5}
                              step={0.5}
                              value={policy.fixedM}
                              onChange={(event) =>
                                updateQuotaEstimationPolicy(planType, {
                                  fixedM: Number(event.target.value),
                                })
                              }
                            />
                          ) : (
                            <Input
                              label={t('supply.quota_plan_fallback_input')}
                              type="number"
                              min={0.5}
                              step={0.5}
                              value={policy.fallbackM}
                              onChange={(event) =>
                                updateQuotaEstimationPolicy(planType, {
                                  fallbackM: Number(event.target.value),
                                })
                              }
                            />
                          )}
                        </div>
                      </div>
                    );
                  })}
                </div>
                <div className={styles.formGrid}>
                  <Input
                    label={t('supply.healthy_minutes_target')}
                    type="number"
                    min={10}
                    max={1440}
                    value={draft.healthyMinutesTarget}
                    onChange={(event) =>
                      updateDraft({ healthyMinutesTarget: Number(event.target.value) })
                    }
                  />
                  <Input
                    label={t('supply.warning_minutes')}
                    type="number"
                    min={5}
                    max={1440}
                    value={draft.warningMinutes}
                    onChange={(event) =>
                      updateDraft({ warningMinutes: Number(event.target.value) })
                    }
                  />
                  <Input
                    label={t('supply.critical_minutes')}
                    type="number"
                    min={1}
                    max={1440}
                    value={draft.criticalMinutes}
                    onChange={(event) =>
                      updateDraft({ criticalMinutes: Number(event.target.value) })
                    }
                  />
                  <Input
                    label={t('supply.prelock_max_quantity')}
                    type="number"
                    min={1}
                    max={100}
                    value={draft.prelockMaxQuantity}
                    onChange={(event) =>
                      updateDraft({ prelockMaxQuantity: Number(event.target.value) })
                    }
                  />
                  <Input
                    label={t('supply.critical_confirm_rounds')}
                    type="number"
                    min={1}
                    max={5}
                    value={draft.criticalTakeConfirmRounds}
                    onChange={(event) =>
                      updateDraft({ criticalTakeConfirmRounds: Number(event.target.value) })
                    }
                  />
                  <Input
                    label={t('supply.quota_snapshot_cache_ttl')}
                    type="number"
                    min={10}
                    max={600}
                    value={draft.authFilesCacheTTLSeconds}
                    onChange={(event) =>
                      updateDraft({ authFilesCacheTTLSeconds: Number(event.target.value) })
                    }
                  />
                </div>
                <div className={styles.smartToggles}>
                  <ToggleSwitch
                    checked={draft.prelockEnabled !== false}
                    onChange={(prelockEnabled) => updateDraft({ prelockEnabled })}
                    label={t('supply.prelock_enable')}
                  />
                  <Input
                    label={t('supply.balance_reserve')}
                    type="number"
                    min={0}
                    value={Math.round((draft.minBalanceReserveFen ?? 0) / 100)}
                    onChange={(event) =>
                      updateDraft({ minBalanceReserveFen: Number(event.target.value) * 100 })
                    }
                  />
                </div>
                <div className={styles.configFooter}>
                  <ToggleSwitch
                    checked={draft.defaultWebsockets}
                    onChange={(defaultWebsockets) => updateDraft({ defaultWebsockets })}
                    label={t('supply.default_websockets')}
                  />
                  <Button loading={action === 'save'} onClick={() => void save()}>
                    {t('common.save')}
                  </Button>
                </div>
              </article>
            </section>
          ) : null}

          {activeTab === 'orders' ? (
            <section className={styles.ordersGrid}>
              <article className={`${styles.panel} ${styles.manualCard}`}>
                <div className={styles.panelHeader}>
                  <div>
                    <h2>{t('supply.manual_title')}</h2>
                    <p>{t('supply.manual_hint')}</p>
                  </div>
                </div>
                <div className={styles.manualPlatformField}>
                  <label>{t('supply.manual_platform')}</label>
                  <Select
                    value={manualSupplierId}
                    options={manualPlatformOptions}
                    onChange={(supplierId) => {
                      setManualSupplierId(supplierId);
                      const selected = configuredManualPlatforms.find(
                        (platform) => platform.id === supplierId
                      );
                      setManualProduct(selected?.product ?? '');
                    }}
                    disabled={manualPlatformOptions.length === 0}
                    ariaLabel={t('supply.manual_platform')}
                  />
                  <small>
                    {t('supply.manual_platform_hint', {
                      platform: recommendedPlatform?.name || recommendedPlatform?.id || '-',
                    })}
                  </small>
                </div>
                <div className={styles.manualPlatformField}>
                  <div className={styles.fieldLabelRow}>
                    <label>{t('supply.manual_product')}</label>
                    {manualPlatformConfig?.type === 'nvtokens' ? (
                      <Button
                        type="button"
                        variant="ghost"
                        size="xs"
                        loading={manualCatalogState?.loading === true}
                        onClick={() =>
                          manualPlatformConfig
                            ? void loadPlatformCatalog(manualPlatformConfig, true)
                            : undefined
                        }
                      >
                        <IconRefreshCw size={13} />
                        {t('supply.catalog_refresh')}
                      </Button>
                    ) : null}
                  </div>
                  <Select
                    value={manualProduct}
                    options={manualProductOptions}
                    onChange={setManualProduct}
                    disabled={!manualPlatformConfig || manualProductOptions.length === 0}
                    ariaLabel={t('supply.manual_product')}
                  />
                  {manualCatalogState?.error || manualCatalogState?.catalog ? (
                    <small className={manualCatalogState?.error ? styles.fieldError : undefined}>
                      {manualCatalogState?.error ||
                        t('supply.catalog_checked_at', {
                          value: formatTime(manualCatalogState?.catalog?.checkedAtMs),
                        })}
                    </small>
                  ) : null}
                </div>
                <div className={styles.orderComposer}>
                  <Input
                    label={t('supply.quantity')}
                    type="number"
                    min={1}
                    max={10000}
                    value={manualQuantity}
                    onChange={(event) => setManualQuantity(Number(event.target.value))}
                  />
                  <div className={styles.quoteBox}>
                    <span>{t('supply.estimated_total')}</span>
                    <strong>
                      {manualQuoteLoading
                        ? t('common.loading')
                        : manualInventory
                          ? formatMoney(manualInventory.estimatedTotalFen)
                          : '-'}
                    </strong>
                    <small className={manualQuoteError ? styles.fieldError : undefined}>
                      {manualQuoteError || t('supply.quote_hint')}
                    </small>
                  </div>
                </div>
                <Button
                  fullWidth
                  loading={action === 'replenish'}
                  disabled={
                    !manualSupplierId ||
                    !manualProduct ||
                    manualQuoteLoading ||
                    Boolean(manualQuoteError) ||
                    !manualInventory ||
                    manualInventory.estimatedTotalFen <= 0 ||
                    manualQuantity < 1 ||
                    manualQuantity > 10000
                  }
                  onClick={() => void replenish()}
                >
                  {t('supply.replenish_now')}
                </Button>
              </article>

              <article className={styles.panel}>
                <div className={styles.panelHeader}>
                  <div>
                    <h2>{t('supply.current_order')}</h2>
                    <p>{t('supply.current_order_hint')}</p>
                  </div>
                  <IconTimer size={18} />
                </div>
                {activeOrders.length > 0 ? (
                  <div className={styles.orderStack}>
                    {activeOrders.map((order) => (
                      <OrderSummary
                        key={order.orderId}
                        order={order}
                        purchasePlatform={resolvePurchasePlatformLabel(order, purchasePlatforms)}
                        dismissing={action === 'dismiss'}
                        onDismissUncertain={dismissUncertain}
                      />
                    ))}
                  </div>
                ) : (
                  <div className={styles.empty}>{t('supply.no_active_order')}</div>
                )}
              </article>

              <article className={`${styles.panel} ${styles.fullSpan}`}>
                <div className={styles.panelHeader}>
                  <div>
                    <h2>{t('supply.purchase_tasks_title')}</h2>
                    <p>{t('supply.purchase_tasks_hint')}</p>
                  </div>
                  <span className={styles.statusPill}>{purchaseTasks.length}</span>
                </div>
                <div className={styles.tableWrap}>
                  <table>
                    <thead>
                      <tr>
                        <th>{t('supply.purchase_task_id')}</th>
                        <th>{t('supply.purchase_task_source')}</th>
                        <th>{t('supply.purchase_platform')}</th>
                        <th>{t('supply.purchase_task_progress')}</th>
                        <th>{t('supply.purchase_task_attempts')}</th>
                        <th>{t('supply.purchase_task_orders')}</th>
                        <th>{t('common.status')}</th>
                        <th>{t('supply.purchase_task_next_attempt')}</th>
                        <th>{t('supply.order_result_detail')}</th>
                        <th>{t('common.action')}</th>
                      </tr>
                    </thead>
                    <tbody>
                      {purchaseTasks.map((task) => {
                        const remaining = purchaseTaskRemaining(task);
                        const progress = clampPercent(
                          (task.fulfilledQuantity / Math.max(1, task.targetQuantity)) * 100
                        );
                        return (
                          <tr key={task.taskId}>
                            <td>
                              <div className={styles.accountPrimary}>
                                <strong className={styles.mono}>{shortOrderId(task.taskId)}</strong>
                                <small>{formatTime(task.createdAtMs)}</small>
                              </div>
                            </td>
                            <td>
                              {t(`supply.purchase_task_source_${task.source}`, {
                                defaultValue: task.source,
                              })}
                            </td>
                            <td>{task.supplierId || '-'}</td>
                            <td>
                              <div className={styles.purchaseTaskProgress}>
                                <div className={styles.purchaseTaskProgressLabel}>
                                  <span>
                                    {task.fulfilledQuantity}/{task.targetQuantity}
                                  </span>
                                  <small>
                                    {t('supply.purchase_task_remaining_value', {
                                      value: remaining,
                                    })}
                                  </small>
                                </div>
                                <div className={styles.progressTrack}>
                                  <span style={{ width: `${progress}%` }} />
                                </div>
                              </div>
                            </td>
                            <td>{task.attemptCount}</td>
                            <td>
                              {task.orderCount}
                              {task.activeOrderCount > 0
                                ? ` · ${t('supply.purchase_task_active_orders_value', {
                                    value: task.activeOrderCount,
                                  })}`
                                : ''}
                            </td>
                            <td>
                              <span className={`${styles.statusPill} ${orderTone(task.status)}`}>
                                {t(`supply.purchase_task_status_${task.status}`, {
                                  defaultValue: task.status,
                                })}
                              </span>
                            </td>
                            <td>
                              {task.nextAttemptAtMs && purchaseTaskActive(task)
                                ? formatCountdown(task.nextAttemptAtMs, nowMs)
                                : '-'}
                            </td>
                            <td>
                              <span className={styles.purchaseTaskError}>
                                {localizeRuntimeError(task.lastError) || '-'}
                              </span>
                            </td>
                            <td>
                              {purchaseTaskActive(task) ? (
                                <Button
                                  size="xs"
                                  variant="danger"
                                  loading={cancellingTaskId === task.taskId}
                                  disabled={Boolean(cancellingTaskId)}
                                  onClick={() => cancelPurchaseTask(task)}
                                >
                                  <IconX size={13} /> {t('supply.purchase_task_cancel_action')}
                                </Button>
                              ) : (
                                '-'
                              )}
                            </td>
                          </tr>
                        );
                      })}
                      {purchaseTasks.length === 0 ? (
                        <tr>
                          <td colSpan={10} className={styles.emptyCell}>
                            {t('supply.purchase_tasks_empty')}
                          </td>
                        </tr>
                      ) : null}
                    </tbody>
                  </table>
                </div>
              </article>
            </section>
          ) : null}

          {activeTab === 'accounts' ? (
            <section className={styles.accountsGrid}>
              <article className={`${styles.panel} ${styles.fullSpan}`}>
                <div className={styles.panelHeader}>
                  <div>
                    <h2>{t('supply.accounts_title')}</h2>
                    <p>{t('supply.accounts_hint')}</p>
                    <span className={styles.statusPill}>{t('supply.account_scope_ledger')}</span>
                  </div>
                  <div className={styles.heroSummary}>
                    <SegmentedTabs<ReportRangePreset>
                      items={reportRangeItems}
                      activeTab={reportRangePreset}
                      ariaLabel={t('supply.report_range_aria')}
                      onChange={setReportRangePreset}
                      className={styles.reportRangeTabs}
                      equalWidth
                      responsiveFullWidth={false}
                    />
                    <Select
                      value={accountStatusFilter}
                      options={accountStatusOptions}
                      onChange={(value) => setAccountStatusFilter(value as AccountStatusFilter)}
                      className={styles.accountStatusSelect}
                      ariaLabel={t('supply.account_filter_aria')}
                      fullWidth={false}
                    />
                    <span className={styles.statusPill}>{accountRangeLabel}</span>
                    {accountSummary?.cpaStatusError ? (
                      <span className={`${styles.statusPill} ${styles.warning}`}>
                        {t('supply.account_cpa_status_error')}
                      </span>
                    ) : null}
                    <Button
                      size="sm"
                      variant="secondary"
                      loading={accountsLoading}
                      onClick={() => void loadAccounts()}
                    >
                      <IconRefreshCw size={15} /> {t('supply.account_refresh')}
                    </Button>
                  </div>
                </div>
                <ReportMetricCards items={accountMetrics} />
              </article>

              <article className={`${styles.panel} ${styles.fullSpan}`}>
                <div className={styles.panelHeader}>
                  <div>
                    <h2>{t('supply.accounts_table_title')}</h2>
                    <p>{t('supply.accounts_table_hint')}</p>
                  </div>
                </div>
                <div className={styles.tableWrap}>
                  <table>
                    <thead>
                      <tr>
                        <th>{t('supply.account_file')}</th>
                        <th>{t('supply.account_source')}</th>
                        <th>{t('supply.account_status')}</th>
                        <th>{t('supply.account_cpa_status')}</th>
                        <th>{t('supply.account_usage_calls')}</th>
                        <th>{t('supply.account_auth_401')}</th>
                        <th>{t('supply.account_recovery_status')}</th>
                        <th>{t('supply.account_usage_tokens')}</th>
                        <th>{t('supply.account_usage_revenue')}</th>
                        <th>{t('supply.account_supply_cost')}</th>
                        <th>{t('supply.account_last_used_at')}</th>
                        <th>{t('supply.account_expires_at')}</th>
                        <th>{t('supply.account_warranty_expires_at')}</th>
                        <th>{t('common.status')}</th>
                      </tr>
                    </thead>
                    <tbody>
                      {(accounts?.items ?? []).map((item) => (
                        <tr key={item.id}>
                          <td>
                            <div className={styles.accountPrimary}>
                              <strong>{item.cpaAccount || item.fileName}</strong>
                              <small className={styles.mono}>{item.fileName}</small>
                            </div>
                          </td>
                          <td>
                            <div className={styles.accountMeta}>
                              <span>
                                {t(`supply.account_source_${item.source}`, {
                                  defaultValue: item.source,
                                })}
                              </span>
                              <small>{item.product || item.orderId || '-'}</small>
                            </div>
                          </td>
                          <td>
                            <div className={styles.accountStatusCell}>
                              <span
                                className={`${styles.statusPill} ${accountTone(item.accountStatus)}`}
                              >
                                {t(`supply.account_status_${item.accountStatus}`, {
                                  defaultValue: item.accountStatus,
                                })}
                              </span>
                              {item.accountStatusReason ? (
                                <small
                                  className={styles.accountReason}
                                  title={item.accountStatusReason}
                                >
                                  {t('supply.account_status_reason_value', {
                                    reason: item.accountStatusReason,
                                  })}
                                </small>
                              ) : null}
                            </div>
                          </td>
                          <td>
                            <div className={styles.accountMeta}>
                              <span>{item.cpaProvider || '-'}</span>
                              <small>{item.cpaAuthIndex || item.cpaAccountId || '-'}</small>
                            </div>
                          </td>
                          <td>{formatInteger(item.usageCalls)}</td>
                          <td>
                            {item.auth401AtMs ? (
                              <div className={styles.accountMeta}>
                                <span>{formatTime(item.auth401AtMs)}</span>
                                <small title={item.auth401Reason}>
                                  {t('supply.account_auth_401_calls_hint', {
                                    calls: formatInteger(item.auth401BeforeCalls),
                                  })}
                                  {item.autoDisabledAtMs
                                    ? ` · ${t('supply.account_auto_quarantined_short')}`
                                    : ''}
                                </small>
                                {item.auth401Reason ? (
                                  <small className={styles.accountReason}>
                                    {item.auth401Reason}
                                  </small>
                                ) : null}
                              </div>
                            ) : (
                              '-'
                            )}
                          </td>
                          <td>
                            {item.recoveryStatus ? (
                              <div className={styles.accountMeta}>
                                <span
                                  className={`${styles.statusPill} ${orderTone(item.recoveryStatus)}`}
                                >
                                  {t(`supply.recovery_status_${item.recoveryStatus}`, {
                                    defaultValue: item.recoveryStatus,
                                  })}
                                </span>
                                <small className={styles.mono}>{item.recoveryId || '-'}</small>
                              </div>
                            ) : (
                              '-'
                            )}
                          </td>
                          <td>{formatTokens(item.usageTokens)}</td>
                          <td>{formatUsd(item.usageRevenue)}</td>
                          <td>
                            {hasSupplierCost(item.supplierBasePriceFen, item.supplierChargedFen) ? (
                              <div className={styles.accountMeta}>
                                <span>{formatMoney(item.supplierChargedFen)}</span>
                                <small>
                                  {t('supply.account_supply_cost_hint', {
                                    base: formatMoney(item.supplierBasePriceFen),
                                    released: formatMoney(item.supplierReleasedFen),
                                  })}
                                </small>
                              </div>
                            ) : (
                              '-'
                            )}
                          </td>
                          <td>{formatTime(item.lastUsedAtMs)}</td>
                          <td>{formatTime(item.expiresAtMs)}</td>
                          <td>{formatTime(item.warrantyExpiresAtMs)}</td>
                          <td>
                            <span className={`${styles.statusPill} ${accountTone(item.status)}`}>
                              {t(`supply.account_status_${item.status}`, {
                                defaultValue: item.status,
                              })}
                            </span>
                          </td>
                        </tr>
                      ))}
                      {!accounts?.items?.length ? (
                        <tr>
                          <td colSpan={14} className={styles.emptyCell}>
                            {accountsLoading ? t('common.loading') : t('supply.no_accounts')}
                          </td>
                        </tr>
                      ) : null}
                    </tbody>
                  </table>
                </div>
              </article>
            </section>
          ) : null}

          {activeTab === 'recoveries' ? (
            <section className={styles.panel}>
              <div className={styles.panelHeader}>
                <div>
                  <h2>{t('supply.recoveries_title')}</h2>
                  <p>{t('supply.recoveries_hint')}</p>
                </div>
                <div className={styles.heroSummary}>
                  <span
                    className={`${styles.statusPill} ${recovery?.enabled ? styles.success : ''}`}
                  >
                    {recovery?.enabled ? t('common.enabled') : t('common.disabled')}
                  </span>
                  <Button
                    size="sm"
                    variant="secondary"
                    loading={action === 'syncRecoveries' || recovery?.running}
                    onClick={() => void syncRecoveries()}
                  >
                    <IconRefreshCw size={15} /> {t('supply.recovery_sync_now')}
                  </Button>
                </div>
              </div>
              <div className={styles.summaryList}>
                <div>
                  <span>{t('supply.recovery_next_sync')}</span>
                  <strong>{formatCountdown(recovery?.nextSyncAtMs, nowMs)}</strong>
                </div>
                <div>
                  <span>{t('supply.recovery_claimable')}</span>
                  <strong>{recovery?.claimable ?? 0}</strong>
                </div>
                <div>
                  <span>{t('supply.recovery_importing')}</span>
                  <strong>{recovery?.importing ?? 0}</strong>
                </div>
                <div>
                  <span>{t('supply.recovery_imported')}</span>
                  <strong>{recovery?.storedImported ?? recovery?.imported ?? 0}</strong>
                </div>
                <div>
                  <span>{t('supply.recovery_refunded')}</span>
                  <strong>{recovery?.storedRefunded ?? recovery?.refunded ?? 0}</strong>
                </div>
                <div>
                  <span>{t('supply.recovery_failed')}</span>
                  <strong>{recovery?.storedFailed ?? recovery?.failed ?? 0}</strong>
                </div>
                <div>
                  <span>{t('supply.recovery_other_pool')}</span>
                  <strong>{recovery?.external ?? 0}</strong>
                </div>
              </div>
              {recovery?.lastError ? (
                <div className={styles.errorBanner}>{recovery.lastError}</div>
              ) : null}
              <div className={styles.recoveryProcessNote}>
                <strong>{t('supply.recovery_process_title')}</strong>
                <span>{t('supply.recovery_process_steps')}</span>
                <small>{t('supply.recovery_process_complete_hint')}</small>
              </div>
              <div className={styles.tableWrap}>
                <table>
                  <thead>
                    <tr>
                      <th>{t('supply.recovery_id')}</th>
                      <th>{t('supply.original_account')}</th>
                      <th>{t('supply.credential_version')}</th>
                      <th>{t('supply.delivery_status')}</th>
                      <th>{t('supply.import_result')}</th>
                      <th>{t('supply.imported_files')}</th>
                      <th>{t('supply.recovery_timeline')}</th>
                      <th>{t('supply.failure_reason')}</th>
                      <th>{t('common.action')}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {recoveries.map((item) => {
                      const importItems = item.importItems ?? [];
                      const importedCount = item.importedCount ?? 0;
                      const itemCount = item.itemCount ?? 0;
                      const progress =
                        itemCount > 0 ? clampPercent((importedCount / itemCount) * 100) : 0;
                      const firstImportError = importItems.find(
                        (importItem) => importItem.lastError
                      )?.lastError;
                      const rawFailureReason = item.lastError || firstImportError;
                      const importStatus = item.importStatus || item.status || 'importing';
                      const importStageHint = t(`supply.recovery_import_${importStatus}_hint`, {
                        defaultValue: item.importMessage || '',
                      });
                      const databaseBusy =
                        /database (?:table )?is locked|sqlite_busy|locked \(517\)/i.test(
                          rawFailureReason || ''
                        );
                      const failureReason = databaseBusy
                        ? t('supply.recovery_import_database_busy')
                        : rawFailureReason || importStageHint;
                      const retryableImport =
                        Boolean(item.claimOrderId) &&
                        importStatus !== 'imported' &&
                        importStatus !== 'refunded' &&
                        (Boolean(item.importFailedCount) ||
                          importStatus === 'failed' ||
                          importStatus === 'partial' ||
                          importStatus === 'retry_scheduled');
                      return (
                        <tr key={item.recoveryId}>
                          <td>
                            <div className={styles.mono}>{item.recoveryId}</div>
                            <small>{item.product || '-'}</small>
                          </td>
                          <td>
                            <div className={styles.mono}>{item.originalFileName || '-'}</div>
                            <small>{item.originalEmail || item.originalAuthIndex || '-'}</small>
                            <div>
                              <span
                                className={`${styles.statusPill} ${
                                  item.ownership === 'local'
                                    ? styles.success
                                    : item.ownership === 'external'
                                      ? styles.warning
                                      : styles.active
                                }`}
                              >
                                {t(`supply.recovery_ownership_${item.ownership || 'unknown'}`, {
                                  defaultValue: item.ownership || 'unknown',
                                })}
                              </span>
                            </div>
                            <small title={item.sourceOrderId || ''}>
                              {t('supply.recovery_source_order')}:{' '}
                              {item.sourceOrderId ? shortOrderId(item.sourceOrderId) : '-'}
                            </small>
                          </td>
                          <td>
                            {item.credentialVersion ? (
                              <span className={`${styles.statusPill} ${styles.success}`}>
                                v{item.credentialVersion}
                              </span>
                            ) : (
                              <span className={styles.muted}>-</span>
                            )}
                          </td>
                          <td>
                            <span
                              className={`${styles.statusPill} ${accountTone(item.deliveryStatus)}`}
                            >
                              {t(`supply.recovery_delivery_${item.deliveryStatus}`, {
                                defaultValue: item.deliveryStatus || '-',
                              })}
                            </span>
                          </td>
                          <td>
                            <span className={`${styles.statusPill} ${orderTone(importStatus)}`}>
                              {t(`supply.recovery_import_${importStatus}`, {
                                defaultValue: importStatus,
                              })}
                            </span>
                            <div className={styles.recoveryImportCell}>
                              <div className={styles.recoveryProgressLabel}>
                                <small>{t('supply.import_progress')}</small>
                                <small>
                                  {itemCount > 0
                                    ? `${importedCount}/${itemCount}`
                                    : t(`supply.recovery_import_${importStatus}`, {
                                        defaultValue: t('supply.import_not_started'),
                                      })}
                                </small>
                              </div>
                              <div className={styles.progressTrack}>
                                <span style={{ width: `${progress}%` }} />
                              </div>
                              <small>
                                {item.importPendingCount
                                  ? `${t('supply.import_pending')} ${item.importPendingCount}`
                                  : ''}
                                {item.importPendingCount && item.importFailedCount ? ' · ' : ''}
                                {item.importFailedCount
                                  ? `${t('supply.failed')} ${item.importFailedCount}`
                                  : ''}
                                {!item.importPendingCount &&
                                !item.importFailedCount &&
                                itemCount > 0
                                  ? t('supply.import_complete')
                                  : ''}
                              </small>
                              {item.importNextRetryAtMs ? (
                                <small className={styles.recoveryRetryHint}>
                                  {t('supply.recovery_import_auto_retry_at', {
                                    time: formatTime(item.importNextRetryAtMs),
                                    countdown: formatCountdown(item.importNextRetryAtMs, nowMs),
                                  })}
                                </small>
                              ) : null}
                            </div>
                            {item.refundedFen ? (
                              <small>
                                {t('supply.refunded')}: {formatMoney(item.refundedFen)}
                              </small>
                            ) : null}
                          </td>
                          <td>
                            <div className={styles.importFileList}>
                              {importItems.slice(0, 4).map((importItem, index) => (
                                <div
                                  className={styles.importFileRow}
                                  key={`${importItem.fileName}-${index}`}
                                  title={importItem.lastError || importItem.fileName || ''}
                                >
                                  <span
                                    className={`${styles.statusPill} ${accountTone(importItem.status)}`}
                                  >
                                    {t(`supply.import_status_${importItem.status}`, {
                                      defaultValue: importItem.status || '-',
                                    })}
                                  </span>
                                  <div className={styles.importFileName}>
                                    {importItem.importAction ? (
                                      <span
                                        className={`${styles.statusPill} ${
                                          importItem.importAction === 'replace'
                                            ? styles.active
                                            : styles.success
                                        }`}
                                      >
                                        {t(`supply.import_action_${importItem.importAction}`, {
                                          defaultValue: importItem.importAction,
                                        })}
                                      </span>
                                    ) : null}
                                    <span className={styles.mono}>
                                      {importItem.importAction === 'replace' &&
                                      importItem.replacedFileName
                                        ? `${importItem.replacedFileName} → ${
                                            importItem.fileName || importItem.replacedFileName
                                          }`
                                        : importItem.fileName || t('supply.unnamed_import_file')}
                                    </span>
                                    {importItem.accountName ? (
                                      <small>
                                        {t('supply.import_account_name')}: {importItem.accountName}
                                      </small>
                                    ) : null}
                                  </div>
                                </div>
                              ))}
                              {importItems.length > 4 ? (
                                <small>+{importItems.length - 4}</small>
                              ) : null}
                              {!importItems.length && item.importedFileNames?.length
                                ? item.importedFileNames.slice(0, 2).map((fileName) => (
                                    <div key={fileName} className={styles.mono}>
                                      {fileName}
                                    </div>
                                  ))
                                : null}
                              {!importItems.length && !item.importedFileNames?.length ? (
                                <div className={styles.accountMeta}>
                                  <span className={styles.muted}>
                                    {t('supply.recovery_import_file_not_ready')}
                                  </span>
                                  <small>{t('supply.recovery_import_file_after_success')}</small>
                                </div>
                              ) : null}
                            </div>
                          </td>
                          <td>
                            <div>
                              <small>
                                {t('supply.discovered_at')}: {formatTime(item.lastSeenAtMs)}
                              </small>
                            </div>
                            <div>
                              <small>
                                {t('supply.claimed_at')}:{' '}
                                {item.claimedAtMs ? formatTime(item.claimedAtMs) : '-'}
                              </small>
                            </div>
                            <div>
                              <small>
                                {t('supply.imported_at')}:{' '}
                                {item.lastImportedAtMs ? formatTime(item.lastImportedAtMs) : '-'}
                              </small>
                            </div>
                          </td>
                          <td>
                            <span
                              className={styles.recoveryReason}
                              title={rawFailureReason || item.importMessage || ''}
                            >
                              {failureReason || t('supply.no_failure_reason')}
                            </span>
                          </td>
                          <td>
                            {retryableImport ? (
                              <Button
                                size="sm"
                                variant="secondary"
                                loading={action === 'retryRecoveryImport'}
                                onClick={() => void retryRecoveryImport(item.recoveryId)}
                              >
                                <IconRefreshCw size={14} /> {t('supply.recovery_import_retry_now')}
                              </Button>
                            ) : importStatus === 'not_this_pool' ? (
                              <span className={`${styles.statusPill} ${styles.warning}`}>
                                {t('supply.recovery_other_pool_no_action')}
                              </span>
                            ) : importStatus === 'claimed_without_local_payload' ? (
                              <span className={`${styles.statusPill} ${styles.warning}`}>
                                {t('supply.recovery_no_local_payload_retry')}
                              </span>
                            ) : importStatus === 'ownership_unknown' ? (
                              <span className={`${styles.statusPill} ${styles.warning}`}>
                                {t('supply.recovery_waiting_ownership')}
                              </span>
                            ) : item.status === 'claimable' ? (
                              recovery?.autoClaim !== false ? (
                                <span className={`${styles.statusPill} ${styles.active}`}>
                                  {t('supply.recovery_auto_queued')}
                                </span>
                              ) : (
                                <Button
                                  size="sm"
                                  variant="secondary"
                                  loading={action === 'claimRecovery'}
                                  onClick={() => void claimRecovery(item.recoveryId)}
                                >
                                  {t('supply.recovery_claim_now')}
                                </Button>
                              )
                            ) : importStatus === 'claimed_waiting_task' ? (
                              <span className={`${styles.statusPill} ${styles.warning}`}>
                                {t('supply.recovery_waiting_next_sync')}
                              </span>
                            ) : importStatus === 'retry_scheduled' ? (
                              <span className={`${styles.statusPill} ${styles.active}`}>
                                {t('supply.recovery_auto_retry_queued')}
                              </span>
                            ) : (
                              '-'
                            )}
                          </td>
                        </tr>
                      );
                    })}
                    {!recoveries.length ? (
                      <tr>
                        <td colSpan={9} className={styles.emptyCell}>
                          {recoveriesLoading ? t('common.loading') : t('supply.no_recoveries')}
                        </td>
                      </tr>
                    ) : null}
                  </tbody>
                </table>
              </div>
            </section>
          ) : null}

          {activeTab === 'reports' ? (
            <section className={styles.reportsGrid}>
              <article className={`${styles.panel} ${styles.fullSpan}`}>
                <div className={styles.panelHeader}>
                  <div>
                    <h2>{t('supply.reports_title')}</h2>
                    <p>{t('supply.reports_hint')}</p>
                  </div>
                  <div className={styles.heroSummary}>
                    <SegmentedTabs<ReportRangePreset>
                      items={reportRangeItems}
                      activeTab={reportRangePreset}
                      ariaLabel={t('supply.report_range_aria')}
                      onChange={setReportRangePreset}
                      className={styles.reportRangeTabs}
                      equalWidth
                      responsiveFullWidth={false}
                    />
                    <span className={styles.statusPill}>{reportRangeLabel}</span>
                    {reportRange?.truncated ? (
                      <span className={`${styles.statusPill} ${styles.warning}`}>
                        {t('supply.report_truncated')}
                      </span>
                    ) : null}
                    <Button
                      size="sm"
                      variant="secondary"
                      loading={reportLoading}
                      onClick={() => void loadReport()}
                    >
                      <IconRefreshCw size={15} /> {t('supply.report_refresh')}
                    </Button>
                  </div>
                </div>
                {!report ? (
                  <div className={styles.empty}>
                    {reportLoading ? t('common.loading') : t('supply.report_no_data')}
                  </div>
                ) : (
                  <>
                    <div className={styles.reportSectionHeader}>
                      <span>{t('supply.report_finance')}</span>
                      <small>{t('supply.report_finance_hint')}</small>
                    </div>
                    <ReportMetricCards items={reportFinanceMetrics} />
                  </>
                )}
              </article>

              {report ? (
                <>
                  <article className={styles.panel}>
                    <div className={styles.panelHeader}>
                      <div>
                        <h2>{t('supply.report_operations')}</h2>
                        <p>{t('supply.report_operations_hint')}</p>
                      </div>
                    </div>
                    <ReportMetricCards items={reportOperationsMetrics} />
                  </article>

                  <article className={styles.panel}>
                    <div className={styles.panelHeader}>
                      <div>
                        <h2>{t('supply.report_product_experience')}</h2>
                        <p>{t('supply.report_product_experience_hint')}</p>
                      </div>
                    </div>
                    <ReportMetricCards items={reportProductMetrics} />
                  </article>

                  <article className={`${styles.panel} ${styles.fullSpan}`}>
                    <div className={styles.panelHeader}>
                      <div>
                        <h2>{t('supply.report_risk')}</h2>
                        <p>{t('supply.report_risk_hint')}</p>
                      </div>
                    </div>
                    <ReportMetricCards items={reportRiskMetrics} />
                    <div className={styles.riskBucketGrid}>
                      {(reportRisk?.claimableAgeBuckets ?? []).map((bucket) => (
                        <div key={bucket.key}>
                          <span>{bucket.label}</span>
                          <strong>{formatInteger(bucket.count)}</strong>
                        </div>
                      ))}
                    </div>
                  </article>

                  <div className={`${styles.dimensionGrid} ${styles.fullSpan}`}>
                    <ReportDimensionTable
                      title={t('supply.report_products')}
                      rows={report.products}
                    />
                    <ReportDimensionTable
                      title={t('supply.report_sources')}
                      rows={report.sources}
                    />
                    <ReportDimensionTable
                      title={t('supply.report_strategies')}
                      rows={report.strategies}
                      labelKeyPrefix="supply.strategy_"
                    />
                    <ReportDimensionTable
                      title={t('supply.report_trigger_reasons')}
                      rows={report.triggerReasons}
                      labelKeyPrefix="supply.smart_reason_"
                    />
                    <ReportDimensionTable
                      title={t('supply.report_order_statuses')}
                      rows={report.orderStatuses}
                    />
                    <ReportDimensionTable
                      title={t('supply.report_recovery_statuses')}
                      rows={report.recoveryStatuses}
                    />
                    <ReportDimensionTable
                      title={t('supply.report_delivery_statuses')}
                      rows={report.deliveryStatuses}
                    />
                  </div>

                  <ReportUsageModelTable rows={report.usageModels} />
                  <ReportReconciliationSummaryPanel reconciliation={report.reconciliation} />
                  <ReportAccountLedgerTable rows={report.reconciliation?.accounts} />
                  <ReportOrderLedgerTable rows={report.reconciliation?.orders} />
                  <ReportRecoveryLedgerTable rows={report.reconciliation?.recoveries} />
                  <ReportTimelineTable rows={report.timeline} />
                </>
              ) : null}
            </section>
          ) : null}

          {activeTab === 'history' ? (
            <section className={styles.panel}>
              <div className={styles.panelHeader}>
                <div>
                  <h2>{t('supply.history_title')}</h2>
                  <p>{t('supply.history_hint')}</p>
                </div>
                <span className={styles.statusPill}>{orderCount}</span>
              </div>
              <div className={styles.tableWrap}>
                <table>
                  <thead>
                    <tr>
                      <th>{t('supply.order_id')}</th>
                      <th>{t('supply.product')}</th>
                      <th>{t('supply.purchase_platform')}</th>
                      <th>{t('supply.order_type')}</th>
                      <th>{t('supply.quantity')}</th>
                      <th>{t('supply.import_progress')}</th>
                      <th>{t('supply.charged')}</th>
                      <th>{t('common.status')}</th>
                      <th>{t('supply.order_result_detail')}</th>
                      <th>{t('supply.created_at')}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {(status?.orders ?? []).map((order) => (
                      <tr key={order.orderId}>
                        <td className={styles.mono}>{order.orderId}</td>
                        <td>{order.product}</td>
                        <td>{resolvePurchasePlatformLabel(order, purchasePlatforms)}</td>
                        <td>{order.automatic ? t('supply.automatic') : t('supply.manual')}</td>
                        <td>{order.requestedQuantity}</td>
                        <td>
                          {order.importedCount}/{order.itemCount || order.requestedQuantity}
                        </td>
                        <td>{formatMoney(order.chargedFen)}</td>
                        <td>
                          <span className={`${styles.statusPill} ${orderTone(order.status)}`}>
                            {t(`supply.status_${order.status}`, { defaultValue: order.status })}
                          </span>
                        </td>
                        <td>
                          {(order.lastError && localizeRuntimeError(order.lastError)) ||
                            t(`supply.order_result_${order.status}`, {
                              defaultValue: t('supply.order_result_unknown'),
                            })}
                        </td>
                        <td>{formatTime(order.createdAtMs)}</td>
                      </tr>
                    ))}
                    {!status?.orders?.length ? (
                      <tr>
                        <td colSpan={10} className={styles.emptyCell}>
                          {t('supply.no_history')}
                        </td>
                      </tr>
                    ) : null}
                  </tbody>
                </table>
              </div>
            </section>
          ) : null}
        </div>
      </section>
    </div>
  );
}

function OrderSummary({
  order,
  purchasePlatform,
  dismissing,
  onDismissUncertain,
}: {
  order: SupplyOrder;
  purchasePlatform: string;
  dismissing: boolean;
  onDismissUncertain: (order: SupplyOrder) => void;
}) {
  const { t } = useTranslation();
  const importing =
    order.itemCount > 0 ||
    order.status === 'importing' ||
    order.status === 'partial' ||
    order.status === 'completed_partial' ||
    order.status === 'recovery_importing' ||
    order.status === 'recovery_partial';
  const progressValue = importing
    ? (order.importedCount / Math.max(1, order.itemCount || order.requestedQuantity)) * 100
    : order.progress > 0
      ? order.progress
      : (order.readyQuantity / Math.max(1, order.requestedQuantity)) * 100;
  const progressLabel = importing
    ? `${order.importedCount}/${order.itemCount || order.requestedQuantity}`
    : `${order.readyQuantity}/${order.requestedQuantity}`;
  return (
    <div className={styles.orderSummary}>
      <div className={styles.orderTopline}>
        <span className={`${styles.statusPill} ${orderTone(order.status)}`}>
          {t(`supply.status_${order.status}`, { defaultValue: order.status })}
        </span>
        <strong>{progressLabel}</strong>
      </div>
      <div className={styles.progressTrack}>
        <span style={{ width: `${Math.min(100, Math.max(0, progressValue))}%` }} />
      </div>
      <dl>
        <div>
          <dt>{t('supply.order_id')}</dt>
          <dd>{order.orderId}</dd>
        </div>
        <div>
          <dt>{t('supply.purchase_platform')}</dt>
          <dd>{purchasePlatform}</dd>
        </div>
        <div>
          <dt>{t('supply.remote_status')}</dt>
          <dd>{order.remoteStatus || '-'}</dd>
        </div>
        <div>
          <dt>{t('supply.ready_quantity')}</dt>
          <dd>
            {order.readyQuantity}/{order.requestedQuantity}
          </dd>
        </div>
        <div>
          <dt>{t('supply.remote_progress')}</dt>
          <dd>{order.progress > 0 ? `${order.progress}%` : '-'}</dd>
        </div>
        <div>
          <dt>{t('supply.next_poll')}</dt>
          <dd>{formatTime(order.nextPollAtMs)}</dd>
        </div>
        <div>
          <dt>{t('supply.last_checked')}</dt>
          <dd>{formatTime(order.updatedAtMs)}</dd>
        </div>
      </dl>
      {order.lastError ? (
        <div className={styles.inlineError}>{localizeSupplyRuntimeError(order.lastError, t)}</div>
      ) : null}
      {order.status === 'create_uncertain' ? (
        <div className={styles.uncertainActions}>
          <p>{t('supply.create_uncertain_hint')}</p>
          <Button
            variant="danger"
            size="sm"
            loading={dismissing}
            onClick={() => onDismissUncertain(order)}
          >
            {t('supply.dismiss_uncertain_action')}
          </Button>
        </div>
      ) : null}
    </div>
  );
}

function ReportMetricCards({
  items,
}: {
  items: Array<{ label: string; value: string; detail: string }>;
}) {
  return (
    <div className={styles.reportMetricGrid}>
      {items.map((item, index) => (
        <div key={`${item.label}:${index}`}>
          <span>{item.label}</span>
          <strong>{item.value}</strong>
          <small>{item.detail}</small>
        </div>
      ))}
    </div>
  );
}

function ReportDimensionTable({
  title,
  rows,
  labelKeyPrefix,
}: {
  title: string;
  rows?: SupplyReportDimensionStat[];
  labelKeyPrefix?: string;
}) {
  const { t } = useTranslation();
  return (
    <article className={styles.panel}>
      <div className={styles.panelHeader}>
        <div>
          <h2>{title}</h2>
          <p>{t('supply.report_dimension_hint')}</p>
        </div>
      </div>
      <div className={styles.tableWrap}>
        <table>
          <thead>
            <tr>
              <th>{t('supply.report_name')}</th>
              <th>{t('supply.report_count')}</th>
              <th>{t('supply.quantity')}</th>
              <th>{t('supply.report_imported')}</th>
              <th>{t('supply.charged')}</th>
              <th>{t('supply.report_released')}</th>
              <th>{t('supply.refunded')}</th>
              <th>{t('supply.report_success_rate')}</th>
            </tr>
          </thead>
          <tbody>
            {(rows ?? []).map((row) => (
              <tr key={row.key}>
                <td className={styles.mono}>
                  {row.label ||
                    (labelKeyPrefix
                      ? t(`${labelKeyPrefix}${row.key}`, { defaultValue: row.key })
                      : row.key)}
                </td>
                <td>{formatInteger(row.count)}</td>
                <td>{formatInteger(row.quantity || row.orders || row.recoveries)}</td>
                <td>{formatInteger(row.imported)}</td>
                <td>{row.chargedFen ? formatMoney(row.chargedFen) : '-'}</td>
                <td>{row.releasedFen ? formatMoney(row.releasedFen) : '-'}</td>
                <td>{row.refundedFen ? formatMoney(row.refundedFen) : '-'}</td>
                <td>{formatPercent(row.successRate)}</td>
              </tr>
            ))}
            {!rows?.length ? (
              <tr>
                <td colSpan={8} className={styles.emptyCell}>
                  {t('supply.report_no_data')}
                </td>
              </tr>
            ) : null}
          </tbody>
        </table>
      </div>
    </article>
  );
}

function ReportUsageModelTable({ rows }: { rows?: SupplyReportUsageModelStat[] }) {
  const { t } = useTranslation();
  return (
    <article className={`${styles.panel} ${styles.fullSpan}`}>
      <div className={styles.panelHeader}>
        <div>
          <h2>{t('supply.report_usage_models')}</h2>
          <p>{t('supply.report_usage_models_hint')}</p>
        </div>
      </div>
      <div className={styles.tableWrap}>
        <table>
          <thead>
            <tr>
              <th>{t('supply.report_model')}</th>
              <th>{t('supply.report_billing_model')}</th>
              <th>{t('supply.report_service_tier')}</th>
              <th>{t('supply.report_usage_calls')}</th>
              <th>{t('supply.report_successful_calls_short')}</th>
              <th>{t('supply.report_usage_tokens')}</th>
              <th>{t('supply.report_usage_revenue')}</th>
            </tr>
          </thead>
          <tbody>
            {(rows ?? []).map((row) => (
              <tr key={`${row.model}:${row.billingModel}:${row.serviceTier || '-'}`}>
                <td className={styles.mono}>{row.model}</td>
                <td className={styles.mono}>{row.billingModel || row.model}</td>
                <td>{row.serviceTier || '-'}</td>
                <td>{formatInteger(row.calls)}</td>
                <td>{formatInteger(row.successCalls)}</td>
                <td>{formatTokens(row.tokens)}</td>
                <td>{formatUsd(row.revenue)}</td>
              </tr>
            ))}
            {!rows?.length ? (
              <tr>
                <td colSpan={7} className={styles.emptyCell}>
                  {t('supply.report_no_data')}
                </td>
              </tr>
            ) : null}
          </tbody>
        </table>
      </div>
    </article>
  );
}

function ReportReconciliationSummaryPanel({
  reconciliation,
}: {
  reconciliation?: SupplyReport['reconciliation'];
}) {
  const { t } = useTranslation();
  const summary = reconciliation?.summary;
  const allocationMethod = summary?.allocationMethod || 'order_even_split_by_visible_accounts';
  const metrics = [
    {
      label: t('supply.report_reconcile_order_charged'),
      value: formatMoney(summary?.orderChargedFen),
      detail: t('supply.report_reconcile_order_charged_hint', {
        rows: formatInteger(summary?.orderRows),
      }),
    },
    {
      label: t('supply.report_reconcile_order_net'),
      value: formatMoney(summary?.orderNetFen),
      detail: t('supply.report_reconcile_order_net_hint', {
        released: formatMoney(summary?.orderReleasedFen),
      }),
    },
    {
      label: t('supply.report_reconcile_account_allocated'),
      value: formatMoney(summary?.accountAllocatedNetFen),
      detail: t(`supply.report_allocation_method_${allocationMethod}`, {
        defaultValue: allocationMethod,
      }),
    },
    {
      label: t('supply.report_reconcile_account_revenue'),
      value: formatUsd(summary?.accountUsageRevenue),
      detail: t('supply.report_reconcile_account_revenue_hint', {
        rows: formatInteger(summary?.accountRows),
      }),
    },
    {
      label: t('supply.report_reconcile_account_calls'),
      value: formatInteger(summary?.accountUsageCalls),
      detail: t('supply.report_reconcile_account_calls_hint', {
        tokens: formatTokens(summary?.accountUsageTokens),
      }),
    },
    {
      label: t('supply.report_reconcile_refunded'),
      value: formatMoney(summary?.refundedFen),
      detail: t('supply.report_reconcile_refunded_hint', {
        rows: formatInteger(summary?.recoveryRows),
      }),
    },
  ];
  return (
    <article className={`${styles.panel} ${styles.fullSpan}`}>
      <div className={styles.panelHeader}>
        <div>
          <h2>{t('supply.report_reconciliation')}</h2>
          <p>{t('supply.report_reconciliation_hint')}</p>
        </div>
      </div>
      <ReportMetricCards items={metrics} />
    </article>
  );
}

function ReportAccountLedgerTable({ rows }: { rows?: SupplyReport['reconciliation']['accounts'] }) {
  const { t } = useTranslation();
  return (
    <article className={`${styles.panel} ${styles.fullSpan}`}>
      <div className={styles.panelHeader}>
        <div>
          <h2>{t('supply.report_account_ledger')}</h2>
          <p>{t('supply.report_account_ledger_hint')}</p>
        </div>
      </div>
      <div className={styles.tableWrap}>
        <table>
          <thead>
            <tr>
              <th>{t('supply.account_file')}</th>
              <th>{t('supply.order_id')}</th>
              <th>{t('supply.account_source')}</th>
              <th>{t('supply.account_status')}</th>
              <th>{t('supply.account_auth_401')}</th>
              <th>{t('supply.account_auto_quarantined_at')}</th>
              <th>{t('supply.report_allocated_charged')}</th>
              <th>{t('supply.report_allocated_released')}</th>
              <th>{t('supply.report_allocated_net')}</th>
              <th>{t('supply.report_usage_calls')}</th>
              <th>{t('supply.report_usage_tokens')}</th>
              <th>{t('supply.report_usage_revenue')}</th>
              <th>{t('supply.account_last_used_at')}</th>
              <th>{t('supply.account_expires_at')}</th>
              <th>{t('supply.account_warranty_expires_at')}</th>
            </tr>
          </thead>
          <tbody>
            {(rows ?? []).map((row) => (
              <tr key={`${row.orderId}:${row.fileName}`}>
                <td className={styles.mono}>{row.fileName || '-'}</td>
                <td className={styles.mono}>{row.orderId || '-'}</td>
                <td>
                  {t(`supply.account_source_${row.source}`, {
                    defaultValue: row.source || '-',
                  })}
                </td>
                <td>
                  <span className={`${styles.statusPill} ${accountTone(row.accountStatus)}`}>
                    {t(`supply.account_status_${row.accountStatus}`, {
                      defaultValue: row.accountStatus,
                    })}
                  </span>
                </td>
                <td>{formatTime(row.auth401AtMs)}</td>
                <td>{formatTime(row.autoDisabledAtMs)}</td>
                <td>{formatMoney(row.allocatedChargedFen)}</td>
                <td>{formatMoney(row.allocatedReleasedFen)}</td>
                <td>{formatMoney(row.allocatedNetFen)}</td>
                <td>{formatInteger(row.usageCalls)}</td>
                <td>{formatTokens(row.usageTokens)}</td>
                <td>{formatUsd(row.usageRevenue)}</td>
                <td>{formatTime(row.lastUsedAtMs)}</td>
                <td>{formatTime(row.expiresAtMs)}</td>
                <td>{formatTime(row.warrantyExpiresAtMs)}</td>
              </tr>
            ))}
            {!rows?.length ? (
              <tr>
                <td colSpan={15} className={styles.emptyCell}>
                  {t('supply.report_no_data')}
                </td>
              </tr>
            ) : null}
          </tbody>
        </table>
      </div>
    </article>
  );
}

function ReportOrderLedgerTable({ rows }: { rows?: SupplyReport['reconciliation']['orders'] }) {
  const { t } = useTranslation();
  return (
    <article className={`${styles.panel} ${styles.fullSpan}`}>
      <div className={styles.panelHeader}>
        <div>
          <h2>{t('supply.report_order_ledger')}</h2>
          <p>{t('supply.report_order_ledger_hint')}</p>
        </div>
      </div>
      <div className={styles.tableWrap}>
        <table>
          <thead>
            <tr>
              <th>{t('supply.order_id')}</th>
              <th>{t('supply.account_source')}</th>
              <th>{t('supply.report_strategy')}</th>
              <th>{t('supply.report_trigger_reason')}</th>
              <th>{t('supply.product')}</th>
              <th>{t('common.status')}</th>
              <th>{t('supply.quantity')}</th>
              <th>{t('supply.report_imported')}</th>
              <th>{t('supply.charged')}</th>
              <th>{t('supply.report_released')}</th>
              <th>{t('supply.report_allocated_net')}</th>
              <th>{t('supply.created_at')}</th>
              <th>{t('supply.completed_at')}</th>
            </tr>
          </thead>
          <tbody>
            {(rows ?? []).map((row) => (
              <tr key={row.orderId}>
                <td className={styles.mono}>{row.orderId}</td>
                <td>
                  {t(`supply.account_source_${row.source}`, {
                    defaultValue: row.source,
                  })}
                </td>
                <td>
                  {row.strategy
                    ? t(`supply.strategy_${row.strategy}`, { defaultValue: row.strategy })
                    : '-'}
                </td>
                <td>
                  {row.triggerReason
                    ? t(`supply.smart_reason_${row.triggerReason}`, {
                        defaultValue: row.triggerReason,
                      })
                    : '-'}
                </td>
                <td>{row.product}</td>
                <td>
                  <span className={`${styles.statusPill} ${orderTone(row.status)}`}>
                    {t(`supply.status_${row.status}`, { defaultValue: row.status })}
                  </span>
                </td>
                <td>{formatInteger(row.requestedQuantity || row.itemCount)}</td>
                <td>{formatInteger(row.importedCount)}</td>
                <td>{formatMoney(row.chargedFen)}</td>
                <td>{formatMoney(row.releasedFen)}</td>
                <td>{formatMoney(row.netFen)}</td>
                <td>{formatTime(row.createdAtMs)}</td>
                <td>{formatTime(row.completedAtMs)}</td>
              </tr>
            ))}
            {!rows?.length ? (
              <tr>
                <td colSpan={13} className={styles.emptyCell}>
                  {t('supply.report_no_data')}
                </td>
              </tr>
            ) : null}
          </tbody>
        </table>
      </div>
    </article>
  );
}

function ReportRecoveryLedgerTable({
  rows,
}: {
  rows?: SupplyReport['reconciliation']['recoveries'];
}) {
  const { t } = useTranslation();
  return (
    <article className={`${styles.panel} ${styles.fullSpan}`}>
      <div className={styles.panelHeader}>
        <div>
          <h2>{t('supply.report_recovery_ledger')}</h2>
          <p>{t('supply.report_recovery_ledger_hint')}</p>
        </div>
      </div>
      <div className={styles.tableWrap}>
        <table>
          <thead>
            <tr>
              <th>{t('supply.recovery_id')}</th>
              <th>{t('supply.product')}</th>
              <th>{t('supply.delivery_status')}</th>
              <th>{t('common.status')}</th>
              <th>{t('supply.original_account')}</th>
              <th>{t('supply.report_claim_order')}</th>
              <th>{t('supply.report_imported')}</th>
              <th>{t('supply.refunded')}</th>
              <th>{t('supply.updated_at')}</th>
            </tr>
          </thead>
          <tbody>
            {(rows ?? []).map((row) => (
              <tr key={row.recoveryId}>
                <td className={styles.mono}>{row.recoveryId}</td>
                <td>{row.product || '-'}</td>
                <td>{row.deliveryStatus || '-'}</td>
                <td>
                  <span className={`${styles.statusPill} ${orderTone(row.status)}`}>
                    {t(`supply.recovery_status_${row.status}`, {
                      defaultValue: row.status,
                    })}
                  </span>
                </td>
                <td>{row.originalFileName || '-'}</td>
                <td className={styles.mono}>{row.claimOrderId || '-'}</td>
                <td>
                  {formatInteger(row.importedCount)} / {formatInteger(row.itemCount)}
                </td>
                <td>{formatMoney(row.refundedFen)}</td>
                <td>{formatTime(row.updatedAtMs || row.lastSeenAtMs)}</td>
              </tr>
            ))}
            {!rows?.length ? (
              <tr>
                <td colSpan={9} className={styles.emptyCell}>
                  {t('supply.report_no_data')}
                </td>
              </tr>
            ) : null}
          </tbody>
        </table>
      </div>
    </article>
  );
}

function ReportTimelineTable({ rows }: { rows?: SupplyReport['timeline'] }) {
  const { t } = useTranslation();
  return (
    <article className={`${styles.panel} ${styles.fullSpan}`}>
      <div className={styles.panelHeader}>
        <div>
          <h2>{t('supply.report_timeline')}</h2>
          <p>{t('supply.report_timeline_hint')}</p>
        </div>
      </div>
      <div className={styles.tableWrap}>
        <table>
          <thead>
            <tr>
              <th>{t('supply.report_date')}</th>
              <th>{t('supply.report_orders')}</th>
              <th>{t('supply.report_requested_accounts')}</th>
              <th>{t('supply.report_imported_accounts')}</th>
              <th>{t('supply.report_supply_spend')}</th>
              <th>{t('supply.report_usage_calls')}</th>
              <th>{t('supply.report_usage_tokens')}</th>
              <th>{t('supply.report_usage_revenue')}</th>
              <th>{t('supply.report_recoveries')}</th>
              <th>{t('supply.report_import_failures')}</th>
            </tr>
          </thead>
          <tbody>
            {(rows ?? []).map((row) => (
              <tr key={row.bucketMs}>
                <td>{row.label}</td>
                <td>{formatInteger(row.orders)}</td>
                <td>{formatInteger(row.requested)}</td>
                <td>{formatInteger(row.imported)}</td>
                <td>{formatMoney(row.chargedFen)}</td>
                <td>{formatInteger(row.usageCalls)}</td>
                <td>{formatTokens(row.usageTokens)}</td>
                <td>{formatUsd(row.usageRevenue)}</td>
                <td>
                  {formatInteger(row.recoveries)} / {formatInteger(row.recoveryImported)} /{' '}
                  {formatInteger(row.recoveryRefunded)}
                </td>
                <td>{formatInteger(row.importFailures)}</td>
              </tr>
            ))}
            {!rows?.length ? (
              <tr>
                <td colSpan={10} className={styles.emptyCell}>
                  {t('supply.report_no_data')}
                </td>
              </tr>
            ) : null}
          </tbody>
        </table>
      </div>
    </article>
  );
}
