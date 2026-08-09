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
  IconRefreshCw,
  IconTimer,
  IconTrendingUp,
} from '@/components/ui/icons';
import {
  supplyApi,
  type SupplyAccountList,
  type SupplyConfig,
  type SupplyOrder,
  type SupplyReport,
  type SupplyReportDimensionStat,
  type SupplyReportUsageModelStat,
  type SupplyRecovery,
  type SupplySmartResource,
  type SupplyStatus,
  type SupplyStrategy,
} from '@/services/api';
import { useNotificationStore } from '@/stores';
import styles from './SupplyPage.module.scss';

const emptyConfig: SupplyConfig = {
  enabled: false,
  baseUrl: 'https://sogouedu.cc',
  username: '',
  password: '',
  passwordConfigured: false,
  product: 'oauth_30d',
  strategy: 'strong_supply',
  targetAvailableAccounts: 100,
  replenishBatchSize: 10,
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
  dailyMaxHoldFen: 0,
  dailyMaxReplenishQuantity: 0,
  revenueMultiplier: 0.06,
  criticalAvailableAccounts: 2,
  healthyAvailableAccounts: 10,
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
  { id: 'disabled', labelKey: 'supply.account_status_disabled' },
  { id: 'expired', labelKey: 'supply.account_status_expired' },
  { id: 'missing', labelKey: 'supply.account_status_missing' },
  { id: 'pending', labelKey: 'supply.account_status_pending' },
  { id: 'failed', labelKey: 'supply.account_status_failed' },
  { id: 'unknown', labelKey: 'supply.account_status_unknown' },
];

const SUPPLY_AUTO_REFRESH_MS = 10_000;
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

const formatRcu = (value?: number, digits = 1) =>
  typeof value === 'number' && Number.isFinite(value) ? `${value.toFixed(digits)} RCU` : '-';

const formatRcuRate = (value?: number) =>
  typeof value === 'number' && Number.isFinite(value) ? `${value.toFixed(1)} RCU/min` : '-';

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

const orderTone = (status: string) => {
  if (status === 'completed' || status === 'released') return styles.success;
  if (status === 'failed' || status === 'cancelled' || status === 'dismissed') return styles.error;
  if (status === 'partial' || status === 'recovery_partial' || status === 'create_uncertain')
    return styles.warning;
  return styles.active;
};

const accountTone = (status: string) => {
  if (status === 'active' || status === 'imported') return styles.success;
  if (status === 'disabled' || status === 'missing' || status === 'failed') return styles.error;
  if (status === 'expired' || status === 'pending' || status === 'unknown') return styles.warning;
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

export function SupplyPage() {
  const { t } = useTranslation();
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
  const [loading, setLoading] = useState(true);
  const [activeTab, setActiveTab] = useState<SupplyWorkspaceTab>('overview');
  const [reportRangePreset, setReportRangePreset] = useState<ReportRangePreset>('today');
  const [nowMs, setNowMs] = useState(() => Date.now());
  const [action, setAction] = useState<
    'save' | 'check' | 'replenish' | 'dismiss' | 'syncRecoveries' | 'claimRecovery' | null
  >(null);
  const configDirtyRef = useRef(false);
  const loadInFlightRef = useRef(false);
  const actionInFlightRef = useRef(false);
  const refreshGenerationRef = useRef(0);

  const updateDraft = useCallback((patch: Partial<SupplyConfig>) => {
    configDirtyRef.current = true;
    setDraft((current) => ({ ...current, ...patch }));
  }, []);

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
      setDraft({ ...next.config, password: '' });
    }
    setManualQuantity((current) =>
      current > 0 ? current : Math.max(1, next.config?.replenishBatchSize || 10)
    );
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
        await load(true);
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
      () => supplyApi.replenish(manualQuantity),
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

  const overview = status?.overview;
  const inventory = overview?.inventory;
  const balance = overview?.balance;
  const smart = status?.smartResource;
  const automation = status?.automation;
  const recovery = status?.recovery;
  const autoSupplyEnabled = status?.config?.enabled ?? draft.enabled ?? false;
  const smartModeEnabled = smart?.enabled ?? draft.smartEnabled !== false;
  const activeOrder = status?.activeOrder;
  const orderCount = status?.orders?.length ?? 0;
  const recoveryCount = recovery?.total ?? recoveries.length;
  const revenueMultiplier = status?.config?.revenueMultiplier ?? draft.revenueMultiplier ?? 0.06;
  const healthLevel = smart?.healthLevel || 'unknown';
  const suggestedAction = smart?.suggestedAction || 'unknown';
  const decisionReason = smart?.decisionReason || 'unknown';
  const confidence = smart?.confidence || 'low';
  const supplyPressureLevel = smart?.supplyPressureLevel || 'unknown';
  const demandTrend = smart?.demandTrend || 'unknown';
  const currentStrategy = (smart?.strategy ||
    status?.config?.strategy ||
    draft.strategy ||
    'strong_supply') as SupplyStrategy;
  const draftStrategy = (draft.strategy || 'strong_supply') as SupplyStrategy;
  const effectiveHealthTargetMinutes =
    smart?.effectiveHealthyMinutesTarget ??
    smart?.healthyMinutesTarget ??
    draft.healthyMinutesTarget;
  const configuredHealthTargetMinutes =
    smart?.configuredHealthyMinutesTarget ??
    smart?.healthyMinutesTarget ??
    draft.healthyMinutesTarget;
  const capacityPercent = smart
    ? clampPercent(
        ((smart.currentCapacityRcu ?? 0) / Math.max(1, smart.targetCapacityRcu ?? 1)) * 100
      )
    : 0;
  const snapshotLabel = smart
    ? smart.snapshotFresh
      ? t('supply.snapshot_fresh')
      : smart.snapshotRefreshInProgress
        ? t('supply.snapshot_refreshing')
        : t('supply.snapshot_stale')
    : t('supply.no_snapshot');
  const nextExecutionCountdown = !autoSupplyEnabled
    ? t('supply.automation_disabled_short')
    : automation?.running || status?.running
      ? t('supply.automation_running')
      : automation?.nextExecutionAtMs
        ? formatCountdown(automation.nextExecutionAtMs, nowMs)
        : t('supply.automation_waiting');
  const nextExecutionDetail = !autoSupplyEnabled
    ? t('supply.automation_disabled_detail')
    : automation?.nextExecutionAtMs
      ? t('supply.automation_next_execution_detail', {
          value: formatTime(automation.nextExecutionAtMs),
          seconds: automation.intervalSeconds ?? draft.checkIntervalSeconds,
        })
      : t('supply.automation_waiting_detail');
  const lastExecutionResult = automation?.lastResult || 'scheduled';
  const lastExecutionAction = automation?.lastAction || suggestedAction;
  const lastExecutionReason = automation?.lastReason || decisionReason;
  const lastExecutionDetail = automation?.lastFinishedAtMs
    ? t('supply.automation_last_execution_detail', {
        value: formatTime(automation.lastFinishedAtMs),
      })
    : t('supply.automation_no_execution');
  const lastExecutionActionLabel = t(`supply.smart_action_${lastExecutionAction}`, {
    defaultValue: lastExecutionAction,
  });
  const lastExecutionReasonLabel = t(`supply.smart_reason_${lastExecutionReason}`, {
    defaultValue: lastExecutionReason,
  });
  const lastExecutionContext = `${lastExecutionDetail} · ${lastExecutionActionLabel}`;
  const lastExecutionTooltip = `${lastExecutionContext} · ${lastExecutionReasonLabel}`;
  const activeOrderDetail = activeOrder
    ? activeOrder.nextPollAtMs && activeOrder.nextPollAtMs > nowMs
      ? t('supply.automation_order_poll_detail', {
          value: formatCountdown(activeOrder.nextPollAtMs, nowMs),
        })
      : t('supply.automation_order_processing_detail')
    : t('supply.automation_no_active_order_detail');
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
          value: formatRcu(smart?.currentCapacityRcu),
          detail: t('supply.raw_capacity_waste_detail', {
            raw: formatNumber(smart?.rawCapacityRcu ?? smart?.currentCapacityRcu),
            waste: formatNumber(smart?.expiryWasteRiskRcu ?? 0),
            minutes: smart?.accountLifetimeMinutes ?? 60,
          }),
          icon: <IconDatabaseZap size={18} />,
          tone: 'teal',
        },
        {
          label: t('supply.consume_rate'),
          value: formatRcuRate(smart?.consumeRcuPerMinute),
          detail: t('supply.consume_rate_detail', {
            rpm: formatNumber(smart?.rpm30m),
            tpm: formatNumber(smart?.tpm30m, 0),
          }),
          icon: <IconTrendingUp size={18} />,
          tone: 'orange',
        },
        {
          label: t('supply.estimated_depletion'),
          value: formatMinutes(smart?.estimatedSustainMinutes),
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
        value: overview?.cpaAvailable ?? '-',
        detail: t('supply.target_value', {
          value: overview?.cpaTarget ?? draft.targetAvailableAccounts,
        }),
        icon: <IconDatabaseZap size={18} />,
        tone: 'teal',
      },
      {
        label: t('supply.deficit'),
        value: overview?.cpaDeficit ?? '-',
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
    draft.healthyMinutesTarget,
    draft.smartEnabled,
    draft.targetAvailableAccounts,
    inventory,
    overview,
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
            {activeOrder ? <span className={styles.tabBadge}>1</span> : null}
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
    [accounts?.summary?.total, activeOrder, orderCount, recoveryCount, t]
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
          <span>{t('supply.pool_available_accounts')}</span>
          <strong>{formatInteger(smart?.availableAccounts ?? overview?.cpaAvailable)}</strong>
          <small>
            {t('supply.pool_schedulable_accounts_hint', {
              value: formatInteger(smart?.schedulableAccounts),
            })}
          </small>
        </div>
        <div className={styles.poolSummaryItem}>
          <span>{t('supply.pool_healthy_accounts')}</span>
          <strong>{formatInteger(smart?.healthyAccounts)}</strong>
          <small>
            {t('supply.pool_weak_accounts_hint', {
              value: formatInteger(smart?.weakAccounts),
            })}
          </small>
        </div>
        <div className={styles.poolSummaryItem}>
          <span>{t('supply.pool_pending_inspection')}</span>
          <strong>{formatInteger(smart?.pendingInspectionAccounts ?? 0)}</strong>
          <small>
            {t('supply.pool_account_estimate_hint', {
              current: formatInteger(smart?.projectedAvailableAccounts ?? smart?.availableAccounts),
              required: formatInteger(smart?.estimatedRequiredAccounts),
              deficit: formatInteger(smart?.accountQuantityDeficit),
            })}
          </small>
        </div>
        <div className={styles.poolSummaryItem}>
          <span>{t('supply.pool_supply_strategy')}</span>
          <strong>
            {t(`supply.strategy_${currentStrategy}`, { defaultValue: currentStrategy })}
          </strong>
          <small>
            {smart?.poolVacuumActive
              ? t('supply.pool_vacuum_duration', {
                  value: formatSeconds(smart.poolVacuumDurationSeconds),
                })
              : t('supply.pool_waterline_hint', {
                  critical: smart?.criticalAvailableAccounts ?? draft.criticalAvailableAccounts,
                  healthy: smart?.healthyAvailableAccounts ?? draft.healthyAvailableAccounts,
                })}
          </small>
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

      {overview?.lastError ? <div className={styles.errorBanner}>{overview.lastError}</div> : null}

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
                  <strong>
                    {t(`supply.smart_action_${suggestedAction}`, {
                      defaultValue: suggestedAction,
                    })}
                  </strong>
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
                      <strong>{formatRcuRate(smart?.consumeRcu1m)}</strong>
                    </div>
                    <div>
                      <span>{t('supply.demand_reference_5m')}</span>
                      <strong>{formatRcuRate(smart?.consumeRcu5m)}</strong>
                    </div>
                    <div>
                      <span>{t('supply.demand_reference_10m')}</span>
                      <strong>{formatRcuRate(smart?.consumeRcu10m)}</strong>
                    </div>
                    <div>
                      <span>{t('supply.demand_purchase_basis')}</span>
                      <strong>{formatRcuRate(smart?.demandPlanningRcuPerMinute)}</strong>
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
                      <strong>
                        {t(`supply.automation_result_${lastExecutionResult}`, {
                          defaultValue: lastExecutionResult,
                        })}
                      </strong>
                      <small title={lastExecutionTooltip}>{lastExecutionContext}</small>
                    </div>
                  </div>
                  <div className={styles.executionCell}>
                    <div className={styles.executionIcon}>
                      <IconInbox size={17} />
                    </div>
                    <div>
                      <span>{t('supply.automation_order_execution')}</span>
                      <strong>
                        {activeOrder
                          ? t(`supply.status_${activeOrder.status}`, {
                              defaultValue: activeOrder.status,
                            })
                          : t('supply.no_active_order_short')}
                      </strong>
                      <small title={activeOrderDetail}>{activeOrderDetail}</small>
                    </div>
                  </div>
                </div>
                {automation?.lastError ? (
                  <div className={styles.executionError}>
                    {t('supply.automation_last_error')}: {automation.lastError}
                  </div>
                ) : null}
                <div className={styles.decisionFooter}>
                  <div>
                    <span>{t('supply.suggested_quantity')}</span>
                    <strong>{smart?.suggestedQuantity ?? '-'}</strong>
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
                    <strong>{formatRcu(smart?.currentCapacityRcu)}</strong>
                  </div>
                  <div>
                    <span>{t('supply.target_capacity')}</span>
                    <strong>{formatRcu(smart?.targetCapacityRcu)}</strong>
                  </div>
                </div>
                <div className={styles.progressTrack}>
                  <span style={{ width: `${capacityPercent}%` }} />
                </div>
                <div className={styles.miniMetricGrid}>
                  <div>
                    <span>{t('supply.capacity_gap_label')}</span>
                    <strong>{formatRcu(smart?.capacityGapRcu)}</strong>
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
                    <strong>{formatRcuRate(smart?.consumeRcuPerMinute)}</strong>
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
              <article className={styles.panel}>
                <div className={styles.panelHeader}>
                  <div>
                    <h2>{t('supply.supply_connection_title')}</h2>
                    <p>{t('supply.supply_connection_hint')}</p>
                  </div>
                </div>
                <div className={styles.formGrid}>
                  <Input
                    label={t('supply.base_url')}
                    value={draft.baseUrl}
                    onChange={(event) => updateDraft({ baseUrl: event.target.value })}
                    placeholder="https://sogouedu.cc"
                  />
                  <Input
                    label={t('supply.username')}
                    value={draft.username}
                    onChange={(event) => updateDraft({ username: event.target.value })}
                    autoComplete="username"
                  />
                  <Input
                    label={t('supply.password')}
                    type="password"
                    value={draft.password ?? ''}
                    onChange={(event) => updateDraft({ password: event.target.value })}
                    placeholder={
                      draft.passwordConfigured
                        ? t('supply.password_saved')
                        : t('supply.password_placeholder')
                    }
                    autoComplete="new-password"
                  />
                  <div className={styles.field}>
                    <label>{t('supply.product')}</label>
                    <Select
                      value={draft.product}
                      options={[
                        { value: 'oauth_30d', label: t('supply.product_30d') },
                        { value: 'oauth_7d', label: t('supply.product_7d') },
                      ]}
                      onChange={(product) => updateDraft({ product })}
                    />
                  </div>
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
                <div className={styles.orderComposer}>
                  <Input
                    label={t('supply.quantity')}
                    type="number"
                    min={1}
                    max={100}
                    value={manualQuantity}
                    onChange={(event) => setManualQuantity(Number(event.target.value))}
                  />
                  <div className={styles.quoteBox}>
                    <span>{t('supply.estimated_total')}</span>
                    <strong>{inventory ? formatMoney(inventory.estimatedTotalFen) : '-'}</strong>
                    <small>{t('supply.quote_hint')}</small>
                  </div>
                </div>
                <Button
                  fullWidth
                  loading={action === 'replenish'}
                  disabled={Boolean(status?.activeOrder)}
                  onClick={() => void replenish()}
                >
                  {status?.activeOrder ? t('supply.order_in_progress') : t('supply.replenish_now')}
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
                {status?.activeOrder ? (
                  <OrderSummary
                    order={status.activeOrder}
                    dismissing={action === 'dismiss'}
                    onDismissUncertain={dismissUncertain}
                  />
                ) : (
                  <div className={styles.empty}>{t('supply.no_active_order')}</div>
                )}
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
                        <th>{t('supply.account_lease_expires_at')}</th>
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
                          <td>
                            {item.leaseExpiresAtMs
                              ? formatCountdown(item.leaseExpiresAtMs, nowMs)
                              : '-'}
                          </td>
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
                          <td colSpan={13} className={styles.emptyCell}>
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
              </div>
              {recovery?.lastError ? (
                <div className={styles.errorBanner}>{recovery.lastError}</div>
              ) : null}
              <div className={styles.tableWrap}>
                <table>
                  <thead>
                    <tr>
                      <th>{t('supply.recovery_id')}</th>
                      <th>{t('supply.product')}</th>
                      <th>{t('supply.original_account')}</th>
                      <th>{t('supply.delivery_status')}</th>
                      <th>{t('common.status')}</th>
                      <th>{t('supply.import_progress')}</th>
                      <th>{t('supply.refunded')}</th>
                      <th>{t('supply.updated_at')}</th>
                      <th>{t('common.action')}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {recoveries.map((item) => (
                      <tr key={item.recoveryId}>
                        <td className={styles.mono}>{item.recoveryId}</td>
                        <td>{item.product || '-'}</td>
                        <td>{item.originalFileName || item.originalEmail || '-'}</td>
                        <td>{item.deliveryStatus || '-'}</td>
                        <td>
                          <span className={`${styles.statusPill} ${orderTone(item.status)}`}>
                            {t(`supply.recovery_status_${item.status}`, {
                              defaultValue: item.status,
                            })}
                          </span>
                        </td>
                        <td>
                          {item.importedCount}/{item.itemCount || 0}
                        </td>
                        <td>{item.refundedFen ? formatMoney(item.refundedFen) : '-'}</td>
                        <td>{formatTime(item.updatedAtMs || item.lastSeenAtMs)}</td>
                        <td>
                          {item.status === 'claimable' ? (
                            <Button
                              size="sm"
                              variant="secondary"
                              loading={action === 'claimRecovery'}
                              onClick={() => void claimRecovery(item.recoveryId)}
                            >
                              {t('supply.recovery_claim_now')}
                            </Button>
                          ) : (
                            '-'
                          )}
                        </td>
                      </tr>
                    ))}
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
                      <th>{t('supply.order_type')}</th>
                      <th>{t('supply.quantity')}</th>
                      <th>{t('supply.import_progress')}</th>
                      <th>{t('supply.charged')}</th>
                      <th>{t('common.status')}</th>
                      <th>{t('supply.created_at')}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {(status?.orders ?? []).map((order) => (
                      <tr key={order.orderId}>
                        <td className={styles.mono}>{order.orderId}</td>
                        <td>{order.product}</td>
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
                        <td>{formatTime(order.createdAtMs)}</td>
                      </tr>
                    ))}
                    {!status?.orders?.length ? (
                      <tr>
                        <td colSpan={8} className={styles.emptyCell}>
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
  dismissing,
  onDismissUncertain,
}: {
  order: SupplyOrder;
  dismissing: boolean;
  onDismissUncertain: (order: SupplyOrder) => void;
}) {
  const { t } = useTranslation();
  const importing =
    order.itemCount > 0 ||
    order.status === 'importing' ||
    order.status === 'partial' ||
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
      </dl>
      {order.lastError ? <div className={styles.inlineError}>{order.lastError}</div> : null}
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
              <th>{t('supply.account_lease_expires_at')}</th>
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
                <td>{formatTime(row.leaseExpiresAtMs)}</td>
              </tr>
            ))}
            {!rows?.length ? (
              <tr>
                <td colSpan={14} className={styles.emptyCell}>
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
