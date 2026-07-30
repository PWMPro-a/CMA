import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import { Select } from '@/components/ui/Select';
import { ToggleSwitch } from '@/components/ui/ToggleSwitch';
import {
  IconDatabaseZap,
  IconDollarSign,
  IconInbox,
  IconRefreshCw,
  IconTimer,
} from '@/components/ui/icons';
import { supplyApi, type SupplyConfig, type SupplyOrder, type SupplyStatus } from '@/services/api';
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
};

const formatMoney = (fen?: number) => `¥${((fen ?? 0) / 100).toFixed(2)}`;

const formatTime = (value?: number) =>
  value && value > 0 ? new Date(value).toLocaleString() : '-';

const orderTone = (status: string) => {
  if (status === 'completed' || status === 'released') return styles.success;
  if (status === 'failed' || status === 'cancelled' || status === 'dismissed') return styles.error;
  if (status === 'partial' || status === 'create_uncertain') return styles.warning;
  return styles.active;
};

export function SupplyPage() {
  const { t } = useTranslation();
  const { showNotification, showConfirmation } = useNotificationStore();
  const [status, setStatus] = useState<SupplyStatus | null>(null);
  const [draft, setDraft] = useState<SupplyConfig>(emptyConfig);
  const [manualQuantity, setManualQuantity] = useState(10);
  const [loading, setLoading] = useState(true);
  const [action, setAction] = useState<'save' | 'check' | 'replenish' | 'dismiss' | null>(null);
  const configDirtyRef = useRef(false);

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
    async (quiet = false) => {
      if (!quiet) setLoading(true);
      try {
        applyStatus(await supplyApi.getStatus());
      } catch (error) {
        if (!quiet) {
          showNotification(
            error instanceof Error ? error.message : t('supply.load_failed'),
            'error'
          );
        }
      } finally {
        if (!quiet) setLoading(false);
      }
    },
    [applyStatus, showNotification, t]
  );

  useEffect(() => {
    void load();
    const timer = window.setInterval(() => void load(true), 10_000);
    return () => window.clearInterval(timer);
  }, [load]);

  const runAction = async (
    kind: 'save' | 'check' | 'replenish' | 'dismiss',
    operation: () => Promise<SupplyStatus>,
    successMessage: string
  ) => {
    setAction(kind);
    try {
      const result = await operation();
      if (kind === 'save') {
        configDirtyRef.current = false;
      }
      applyStatus(result);
      showNotification(successMessage, 'success');
    } catch (error) {
      showNotification(
        error instanceof Error ? error.message : t('common.unknown_error'),
        'error'
      );
    } finally {
      setAction(null);
    }
  };

  const save = () =>
    runAction('save', () => supplyApi.saveConfig(draft), t('supply.save_success'));
  const check = () =>
    runAction('check', () => supplyApi.check(), t('supply.check_success'));
  const replenish = () =>
    runAction(
      'replenish',
      () => supplyApi.replenish(manualQuantity),
      t('supply.replenish_started')
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

  const overview = status?.overview;
  const inventory = overview?.inventory;
  const balance = overview?.balance;
  const metrics = useMemo(
    () => [
      {
        label: t('supply.cpa_available'),
        value: overview?.cpaAvailable ?? '-',
        detail: t('supply.target_value', { value: overview?.cpaTarget ?? draft.targetAvailableAccounts }),
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
    ],
    [balance, draft.targetAvailableAccounts, inventory, overview, t]
  );

  if (loading && !status) {
    return <div className={styles.loading}>{t('common.loading')}</div>;
  }

  return (
    <div className={styles.page}>
      <section className={styles.hero}>
        <div>
          <div className={styles.eyebrow}>{t('supply.eyebrow')}</div>
          <h1>{t('supply.title')}</h1>
          <p>{t('supply.subtitle')}</p>
        </div>
        <div className={styles.heroActions}>
          <span className={`${styles.serviceBadge} ${draft.enabled ? styles.success : ''}`}>
            <span />
            {draft.enabled ? t('supply.auto_enabled') : t('supply.auto_disabled')}
          </span>
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

      <section className={styles.contentGrid}>
        <article className={styles.panel}>
          <div className={styles.panelHeader}>
            <div>
              <h2>{t('supply.config_title')}</h2>
              <p>{t('supply.config_hint')}</p>
            </div>
            <ToggleSwitch
              checked={Boolean(draft.enabled)}
              onChange={(enabled) => updateDraft({ enabled })}
              label={t('supply.enable_auto')}
            />
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
              placeholder={draft.passwordConfigured ? t('supply.password_saved') : t('supply.password_placeholder')}
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
              label={t('supply.target_accounts')}
              type="number"
              min={1}
              max={10000}
              value={draft.targetAvailableAccounts}
              onChange={(event) => updateDraft({ targetAvailableAccounts: Number(event.target.value) })}
            />
            <Input
              label={t('supply.batch_size')}
              type="number"
              min={1}
              max={100}
              value={draft.replenishBatchSize}
              onChange={(event) => updateDraft({ replenishBatchSize: Number(event.target.value) })}
            />
            <Input
              label={t('supply.check_interval')}
              type="number"
              min={10}
              max={3600}
              value={draft.checkIntervalSeconds}
              onChange={(event) => updateDraft({ checkIntervalSeconds: Number(event.target.value) })}
            />
            <Input
              label={t('supply.poll_interval')}
              type="number"
              min={1}
              max={60}
              value={draft.pollIntervalSeconds}
              onChange={(event) => updateDraft({ pollIntervalSeconds: Number(event.target.value) })}
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

        <aside className={styles.sideStack}>
          <article className={styles.panel}>
            <div className={styles.panelHeader}>
              <div>
                <h2>{t('supply.manual_title')}</h2>
                <p>{t('supply.manual_hint')}</p>
              </div>
            </div>
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
        </aside>
      </section>

      <section className={styles.panel}>
        <div className={styles.panelHeader}>
          <div>
            <h2>{t('supply.history_title')}</h2>
            <p>{t('supply.last_checked', { value: formatTime(overview?.checkedAtMs) })}</p>
          </div>
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
                  <td>{order.importedCount}/{order.itemCount || order.requestedQuantity}</td>
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
                <tr><td colSpan={8} className={styles.emptyCell}>{t('supply.no_history')}</td></tr>
              ) : null}
            </tbody>
          </table>
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
  const importing = order.itemCount > 0 || order.status === 'importing' || order.status === 'partial';
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
        <div><dt>{t('supply.order_id')}</dt><dd>{order.orderId}</dd></div>
        <div><dt>{t('supply.remote_status')}</dt><dd>{order.remoteStatus || '-'}</dd></div>
        <div><dt>{t('supply.ready_quantity')}</dt><dd>{order.readyQuantity}/{order.requestedQuantity}</dd></div>
        <div><dt>{t('supply.remote_progress')}</dt><dd>{order.progress > 0 ? `${order.progress}%` : '-'}</dd></div>
        <div><dt>{t('supply.next_poll')}</dt><dd>{formatTime(order.nextPollAtMs)}</dd></div>
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
