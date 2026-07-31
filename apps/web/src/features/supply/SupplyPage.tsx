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
  type SupplyConfig,
  type SupplyOrder,
  type SupplySmartResource,
  type SupplyStatus,
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
};

type SupplyWorkspaceTab = 'overview' | 'automation' | 'orders' | 'history';

const SUPPLY_AUTO_REFRESH_MS = 10_000;

const formatMoney = (fen?: number) => `¥${((fen ?? 0) / 100).toFixed(2)}`;

const formatNumber = (value?: number, digits = 1) =>
  typeof value === 'number' && Number.isFinite(value) ? value.toFixed(digits) : '-';

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

const clampPercent = (value: number) => Math.min(100, Math.max(0, value));

const shortOrderId = (value?: string) => {
  if (!value) return '-';
  return value.length > 10 ? `…${value.slice(-8)}` : value;
};

const orderTone = (status: string) => {
  if (status === 'completed' || status === 'released') return styles.success;
  if (status === 'failed' || status === 'cancelled' || status === 'dismissed') return styles.error;
  if (status === 'partial' || status === 'create_uncertain') return styles.warning;
  return styles.active;
};

const canCancelOrder = (status: string) =>
  ['created', 'waiting_inventory', 'ready', 'taking'].includes(status);

const smartTone = (resource?: SupplySmartResource) => {
  if (!resource?.enabled) return styles.warning;
  if (!resource.snapshotFresh) return styles.warning;
  if (resource.healthLevel === 'healthy') return styles.success;
  if (resource.healthLevel === 'critical') return styles.error;
  if (resource.healthLevel === 'warning') return styles.warning;
  return styles.active;
};

const smartPanelTone = (resource?: SupplySmartResource) => {
  if (!resource?.enabled || !resource.snapshotFresh) return styles.smartPanelWarning;
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
  const [manualQuantity, setManualQuantity] = useState(10);
  const [loading, setLoading] = useState(true);
  const [activeTab, setActiveTab] = useState<SupplyWorkspaceTab>('overview');
  const [action, setAction] = useState<
    'save' | 'check' | 'replenish' | 'dismiss' | 'cancel' | null
  >(null);
  const configDirtyRef = useRef(false);
  const loadInFlightRef = useRef(false);
  const actionInFlightRef = useRef(false);
  const refreshGenerationRef = useRef(0);

  const updateDraft = useCallback((patch: Partial<SupplyConfig>) => {
    configDirtyRef.current = true;
    setDraft((current) => ({ ...current, ...patch }));
  }, []);

  const applyStatus = useCallback((next: SupplyStatus) => {
    setStatus(next);
    if (!configDirtyRef.current) {
      setDraft({ ...next.config, password: '' });
    }
    setManualQuantity((current) =>
      current > 0 ? current : Math.max(1, next.config.replenishBatchSize || 10)
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

  const runAction = async (
    kind: 'save' | 'check' | 'replenish' | 'dismiss' | 'cancel',
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

  const cancelOrder = (order: SupplyOrder) => {
    showConfirmation({
      title: t('supply.cancel_order_title'),
      message: t('supply.cancel_order_confirm', { orderId: order.orderId }),
      variant: 'danger',
      confirmText: t('supply.cancel_order_action'),
      onConfirm: () =>
        runAction(
          'cancel',
          () => supplyApi.cancelOrder(order.orderId),
          t('supply.cancel_order_success')
        ),
    });
  };

  const overview = status?.overview;
  const inventory = overview?.inventory;
  const balance = overview?.balance;
  const smart = status?.smartResource;
  const autoSupplyEnabled = status?.config.enabled ?? draft.enabled ?? false;
  const smartModeEnabled = smart?.enabled ?? draft.smartEnabled !== false;
  const activeOrder = status?.activeOrder;
  const orderCount = status?.orders?.length ?? 0;
  const healthLevel = smart?.healthLevel || 'unknown';
  const suggestedAction = smart?.suggestedAction || 'unknown';
  const decisionReason = smart?.decisionReason || 'unknown';
  const confidence = smart?.confidence || 'low';
  const supplyPressureLevel = smart?.supplyPressureLevel || 'unknown';
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
      : t('supply.snapshot_stale')
    : t('supply.no_snapshot');

  const metrics = useMemo(
    () => {
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
    },
    [
      balance,
      draft.healthyMinutesTarget,
      draft.smartEnabled,
      draft.targetAvailableAccounts,
      inventory,
      overview,
      smart,
      t,
    ]
  );

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
        id: 'history',
        label: (
          <span className={styles.tabLabel}>
            {t('supply.tabs_history')}
            {orderCount > 0 ? <span className={styles.tabBadge}>{orderCount}</span> : null}
          </span>
        ),
      },
    ],
    [activeOrder, orderCount, t]
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
                    {t('supply.decision_reason')}: {' '}
                    {t(`supply.smart_reason_${decisionReason}`, {
                      defaultValue: decisionReason,
                    })}
                  </p>
                </div>
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
                    <p>{smartModeEnabled ? t('supply.smart_enabled') : t('supply.smart_disabled')}</p>
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
                    <p>{t('supply.usage_sample')}: {smart?.usageSampleMinutes ?? 0}m</p>
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
                    onChange={(event) => updateDraft({ warningMinutes: Number(event.target.value) })}
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
                    cancelling={action === 'cancel'}
                    onDismissUncertain={dismissUncertain}
                    onCancelOrder={cancelOrder}
                  />
                ) : (
                  <div className={styles.empty}>{t('supply.no_active_order')}</div>
                )}
              </article>
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
  cancelling,
  onDismissUncertain,
  onCancelOrder,
}: {
  order: SupplyOrder;
  dismissing: boolean;
  cancelling: boolean;
  onDismissUncertain: (order: SupplyOrder) => void;
  onCancelOrder: (order: SupplyOrder) => void;
}) {
  const { t } = useTranslation();
  const importing =
    order.itemCount > 0 || order.status === 'importing' || order.status === 'partial';
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
      {canCancelOrder(order.status) ? (
        <div className={styles.orderActions}>
          <Button
            variant="danger"
            size="sm"
            loading={cancelling}
            onClick={() => onCancelOrder(order)}
          >
            {t('supply.cancel_order_action')}
          </Button>
        </div>
      ) : null}
    </div>
  );
}
