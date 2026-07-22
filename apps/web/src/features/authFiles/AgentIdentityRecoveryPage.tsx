import { useCallback, useEffect, useMemo, useState } from 'react';
import { Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import { SelectionCheckbox } from '@/components/ui/SelectionCheckbox';
import { ToggleSwitch } from '@/components/ui/ToggleSwitch';
import {
  IconChevronLeft,
  IconChevronRight,
  IconRefreshCw,
  IconSearch,
  IconSettings,
  IconTimer,
} from '@/components/ui/icons';
import {
  authFilesApi,
  type AgentIdentityRecoveryResult,
  type AgentIdentityRegistrationResult,
} from '@/services/api/authFiles';
import { useNotificationStore } from '@/stores';
import type {
  AgentIdentityRecoveryHistoryEntry,
  AgentIdentityRegistrationState,
} from '@/types/authFile';
import styles from './AgentIdentityRecoveryPage.module.scss';

type ViewMode = 'current' | 'history';
type StateFilter = 'all' | AgentIdentityRegistrationState;

const PAGE_SIZE = 50;

const STATE_ORDER: Record<AgentIdentityRegistrationState, number> = {
  registering: 0,
  queued: 1,
  retry_wait: 2,
  failed: 3,
  runtime_deleted: 4,
  credentials_pending: 5,
  ready: 6,
};

const formatTime = (value?: string) => {
  if (!value) return '-';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return '-';
  return new Intl.DateTimeFormat(undefined, {
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  }).format(date);
};

const formatDuration = (durationMs?: number) => {
  const value = Number(durationMs ?? 0);
  if (!Number.isFinite(value) || value <= 0) return '-';
  if (value < 1000) return `${Math.round(value)} ms`;
  return `${(value / 1000).toFixed(value < 10000 ? 1 : 0)} s`;
};

export function AgentIdentityRecoveryPage() {
  const { t } = useTranslation();
  const showNotification = useNotificationStore((state) => state.showNotification);
  const [data, setData] = useState<AgentIdentityRecoveryResult | null>(null);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState('');
  const [viewMode, setViewMode] = useState<ViewMode>('current');
  const [stateFilter, setStateFilter] = useState<StateFilter>('all');
  const [search, setSearch] = useState('');
  const [page, setPage] = useState(1);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [autoRefresh, setAutoRefresh] = useState(true);
  const [actionNames, setActionNames] = useState<Set<string>>(new Set());
  const [batchAction, setBatchAction] = useState(false);
  const [configSaving, setConfigSaving] = useState(false);
  const [concurrency, setConcurrency] = useState('6');
  const [configDirty, setConfigDirty] = useState(false);

  const loadRecovery = useCallback(
    async (silent = false) => {
      if (silent) setRefreshing(true);
      else setLoading(true);
      try {
        const result = await authFilesApi.getAgentIdentityRecovery(500);
        setData(result);
        setError('');
        if (!configDirty) setConcurrency(String(result.config.concurrency));
      } catch (loadError) {
        const message = loadError instanceof Error ? loadError.message : t('common.unknown_error');
        setError(message);
      } finally {
        setLoading(false);
        setRefreshing(false);
      }
    },
    [configDirty, t]
  );

  useEffect(() => {
    void loadRecovery();
  }, [loadRecovery]);

  useEffect(() => {
    if (!autoRefresh) return undefined;
    const delay = data?.summary.active ? 2000 : 5000;
    const timer = window.setInterval(() => void loadRecovery(true), delay);
    return () => window.clearInterval(timer);
  }, [autoRefresh, data?.summary.active, loadRecovery]);

  useEffect(() => {
    setPage(1);
  }, [search, stateFilter, viewMode]);

  const registrations = useMemo(() => {
    const query = search.trim().toLowerCase();
    return [...(data?.registrations ?? [])]
      .filter((item) => stateFilter === 'all' || item.registration.state === stateFilter)
      .filter((item) => !query || item.name.toLowerCase().includes(query))
      .sort((left, right) => {
        const stateDiff =
          STATE_ORDER[left.registration.state] - STATE_ORDER[right.registration.state];
        return stateDiff || left.name.localeCompare(right.name);
      });
  }, [data?.registrations, search, stateFilter]);

  const history = useMemo(() => {
    const query = search.trim().toLowerCase();
    return (data?.history ?? [])
      .filter((item) => stateFilter === 'all' || item.state === stateFilter)
      .filter((item) => !query || (item.name ?? '').toLowerCase().includes(query));
  }, [data?.history, search, stateFilter]);

  const activeRows = viewMode === 'current' ? registrations : history;
  const totalPages = Math.max(1, Math.ceil(activeRows.length / PAGE_SIZE));
  const currentPage = Math.min(page, totalPages);
  const pageRows = activeRows.slice((currentPage - 1) * PAGE_SIZE, currentPage * PAGE_SIZE);

  const runSingleAction = useCallback(
    async (name: string, rebuild: boolean) => {
      setActionNames((current) => new Set(current).add(name));
      try {
        if (rebuild) await authFilesApi.rebuildAgentIdentityRegistration(name);
        else await authFilesApi.retryAgentIdentityRegistration(name);
        showNotification(
          t(rebuild ? 'agent_recovery.rebuild_queued' : 'agent_recovery.retry_queued'),
          'success'
        );
        await loadRecovery(true);
      } catch (actionError) {
        const message =
          actionError instanceof Error ? actionError.message : t('common.unknown_error');
        showNotification(message, 'error');
      } finally {
        setActionNames((current) => {
          const next = new Set(current);
          next.delete(name);
          return next;
        });
      }
    },
    [loadRecovery, showNotification, t]
  );

  const runBatchAction = useCallback(
    async (names: string[], rebuild: boolean) => {
      if (names.length === 0) return;
      setBatchAction(true);
      try {
        const result = rebuild
          ? await authFilesApi.rebuildAgentIdentityRegistrations(names)
          : await authFilesApi.retryAgentIdentityRegistrations(names);
        showNotification(
          t('agent_recovery.batch_result', {
            queued: result.queued,
            skipped: result.skipped ?? 0,
            failed: result.failed.length,
          }),
          result.failed.length ? 'warning' : 'success'
        );
        setSelected(new Set());
        await loadRecovery(true);
      } catch (actionError) {
        const message =
          actionError instanceof Error ? actionError.message : t('common.unknown_error');
        showNotification(message, 'error');
      } finally {
        setBatchAction(false);
      }
    },
    [loadRecovery, showNotification, t]
  );

  const saveConcurrency = useCallback(async () => {
    const parsed = Number.parseInt(concurrency, 10);
    if (!Number.isFinite(parsed) || parsed < 1 || parsed > 64) {
      showNotification(t('agent_recovery.concurrency_invalid'), 'error');
      return;
    }
    setConfigSaving(true);
    try {
      const result = await authFilesApi.updateAgentIdentityRecoveryConfig({ concurrency: parsed });
      setConcurrency(String(result.config.concurrency));
      setConfigDirty(false);
      showNotification(t('agent_recovery.config_saved'), 'success');
      await loadRecovery(true);
    } catch (saveError) {
      const message = saveError instanceof Error ? saveError.message : t('common.unknown_error');
      showNotification(message, 'error');
    } finally {
      setConfigSaving(false);
    }
  }, [concurrency, loadRecovery, showNotification, t]);

  const registrationByName = new Map(
    registrations.map((item) => [item.name, item.registration] as const)
  );
  const selectedNames = Array.from(selected).filter(
    (name) => registrationByName.get(name)?.state !== 'credentials_pending'
  );
  const retryableNames = registrations
    .filter((item) => item.registration.can_retry && !item.registration.active)
    .map((item) => item.name);
  const rebuildableNames = registrations
    .filter(
      (item) => !item.registration.active && item.registration.state !== 'credentials_pending'
    )
    .map((item) => item.name);
  const visibleCurrentRows = pageRows as AgentIdentityRegistrationResult[];
  const visibleNames =
    viewMode === 'current'
      ? visibleCurrentRows
          .filter((item) => item.registration.state !== 'credentials_pending')
          .map((item) => item.name)
      : [];
  const allVisibleSelected =
    visibleNames.length > 0 && visibleNames.every((name) => selected.has(name));

  if (loading && !data) return <LoadingSpinner />;

  const summary = data?.summary;
  const coordinator = data?.coordinator;
  const stateFilters: StateFilter[] = [
    'all',
    'registering',
    'queued',
    'retry_wait',
    'failed',
    'runtime_deleted',
    'credentials_pending',
    'ready',
  ];

  return (
    <div className={styles.page}>
      <header className={styles.header}>
        <div className={styles.titleGroup}>
          <Link to="/auth-files" className={styles.backLink}>
            <IconChevronLeft size={16} />
            {t('agent_recovery.back')}
          </Link>
          <div>
            <h1>{t('agent_recovery.title')}</h1>
            <div className={styles.headerMeta}>
              <span>
                {t('agent_recovery.pool_concurrency', { count: coordinator?.concurrency ?? 6 })}
              </span>
              <span>{t('agent_recovery.queue_depth', { count: coordinator?.queued ?? 0 })}</span>
            </div>
          </div>
        </div>
        <div className={styles.headerActions}>
          <ToggleSwitch
            checked={autoRefresh}
            onChange={setAutoRefresh}
            label={t('agent_recovery.auto_refresh')}
            ariaLabel={t('agent_recovery.auto_refresh')}
          />
          <Button
            variant="secondary"
            size="sm"
            onClick={() => void loadRecovery(true)}
            disabled={refreshing}
          >
            <IconRefreshCw size={15} className={refreshing ? styles.spin : ''} />
            {t('common.refresh')}
          </Button>
        </div>
      </header>

      {error && <div className={styles.errorBanner}>{error}</div>}

      <section className={styles.metrics}>
        <div className={styles.metric}>
          <span>{t('agent_recovery.metric_total')}</span>
          <strong>{summary?.total ?? 0}</strong>
          <small>{t('agent_recovery.metric_ready', { count: summary?.ready ?? 0 })}</small>
        </div>
        <div className={`${styles.metric} ${styles.metricActive}`}>
          <span>{t('agent_recovery.metric_active')}</span>
          <strong>{summary?.active ?? 0}</strong>
          <small>{t('agent_recovery.metric_workers', { count: coordinator?.active ?? 0 })}</small>
        </div>
        <div className={`${styles.metric} ${styles.metricWarning}`}>
          <span>{t('agent_recovery.metric_waiting')}</span>
          <strong>{(summary?.queued ?? 0) + (summary?.retry_wait ?? 0)}</strong>
          <small>{t('agent_recovery.metric_queue', { count: coordinator?.queued ?? 0 })}</small>
        </div>
        <div className={`${styles.metric} ${styles.metricDanger}`}>
          <span>{t('agent_recovery.metric_failed')}</span>
          <strong>{(summary?.failed ?? 0) + (summary?.runtime_deleted ?? 0)}</strong>
          <small>
            {t('agent_recovery.metric_deleted', { count: summary?.runtime_deleted ?? 0 })}
          </small>
        </div>
        <div className={`${styles.metric} ${styles.metricPending}`}>
          <span>{t('agent_recovery.metric_credentials')}</span>
          <strong>{summary?.credentials_pending ?? 0}</strong>
          <small>
            {t('agent_recovery.metric_history', { count: coordinator?.history_count ?? 0 })}
          </small>
        </div>
      </section>

      {(summary?.credentials_pending ?? 0) > 0 && (
        <div className={styles.credentialsNotice}>
          {t('auth_files.agent_registration_credentials_pending_hint')}
        </div>
      )}

      <section className={styles.controlBand}>
        <div className={styles.viewTabs}>
          <button
            type="button"
            className={viewMode === 'current' ? styles.tabActive : ''}
            onClick={() => setViewMode('current')}
          >
            {t('agent_recovery.current_tab')}
          </button>
          <button
            type="button"
            className={viewMode === 'history' ? styles.tabActive : ''}
            onClick={() => setViewMode('history')}
          >
            {t('agent_recovery.history_tab')}
          </button>
        </div>
        <div className={styles.configControl}>
          <IconSettings size={16} />
          <label htmlFor="agent-recovery-concurrency">{t('agent_recovery.concurrency')}</label>
          <input
            id="agent-recovery-concurrency"
            type="number"
            min={1}
            max={64}
            value={concurrency}
            onChange={(event) => {
              setConcurrency(event.target.value);
              setConfigDirty(true);
            }}
          />
          <Button size="xs" onClick={() => void saveConcurrency()} loading={configSaving}>
            {t('common.save')}
          </Button>
        </div>
      </section>

      <section className={styles.filters}>
        <div className={styles.searchBox}>
          <Input
            value={search}
            onChange={(event) => setSearch(event.target.value)}
            placeholder={t('agent_recovery.search_placeholder')}
            rightElement={<IconSearch size={16} />}
            aria-label={t('agent_recovery.search_placeholder')}
          />
        </div>
        <div className={styles.stateFilters}>
          {stateFilters.map((state) => (
            <button
              key={state}
              type="button"
              className={stateFilter === state ? styles.filterActive : ''}
              onClick={() => setStateFilter(state)}
            >
              {state === 'all'
                ? t('agent_recovery.state_all')
                : t(`auth_files.agent_registration_state_${state}`)}
            </button>
          ))}
        </div>
      </section>

      {viewMode === 'current' && (
        <section className={styles.batchBar}>
          <div>
            <strong>{t('agent_recovery.selected_count', { count: selected.size })}</strong>
            <span>{t('agent_recovery.filtered_count', { count: registrations.length })}</span>
          </div>
          <div className={styles.batchActions}>
            <Button
              variant="secondary"
              size="sm"
              disabled={batchAction || selectedNames.length === 0}
              onClick={() => void runBatchAction(selectedNames, false)}
            >
              {t('agent_recovery.retry_selected')}
            </Button>
            <Button
              size="sm"
              disabled={batchAction || selectedNames.length === 0}
              onClick={() => void runBatchAction(selectedNames, true)}
            >
              {t('agent_recovery.rebuild_selected')}
            </Button>
            <Button
              variant="secondary"
              size="sm"
              disabled={batchAction || retryableNames.length === 0}
              onClick={() => void runBatchAction(retryableNames, false)}
            >
              {t('agent_recovery.retry_all_failed')}
            </Button>
            <Button
              variant="secondary"
              size="sm"
              disabled={batchAction || rebuildableNames.length === 0}
              onClick={() => void runBatchAction(rebuildableNames, true)}
            >
              {t('agent_recovery.rebuild_filtered')}
            </Button>
          </div>
        </section>
      )}

      <section className={styles.tableSection}>
        <div className={styles.tableWrap}>
          <table className={styles.table}>
            <thead>
              <tr>
                {viewMode === 'current' && (
                  <th className={styles.checkCell}>
                    <SelectionCheckbox
                      checked={allVisibleSelected}
                      onChange={() => {
                        setSelected((current) => {
                          const next = new Set(current);
                          if (allVisibleSelected) visibleNames.forEach((name) => next.delete(name));
                          else visibleNames.forEach((name) => next.add(name));
                          return next;
                        });
                      }}
                      aria-label={t('agent_recovery.select_page')}
                    />
                  </th>
                )}
                <th>{t('agent_recovery.account')}</th>
                <th>{t('common.status')}</th>
                <th>{t('agent_recovery.trigger')}</th>
                <th>{t('agent_recovery.attempts')}</th>
                <th>{t('agent_recovery.timeline')}</th>
                <th>{t('agent_recovery.duration')}</th>
                <th>{t('agent_recovery.failure')}</th>
                {viewMode === 'current' && <th>{t('agent_recovery.actions')}</th>}
              </tr>
            </thead>
            <tbody>
              {viewMode === 'current' &&
                (pageRows as AgentIdentityRegistrationResult[]).map((item) => {
                  const registration = item.registration;
                  const busy = actionNames.has(item.name) || registration.active;
                  return (
                    <tr key={item.name}>
                      <td className={styles.checkCell}>
                        <SelectionCheckbox
                          checked={selected.has(item.name)}
                          disabled={registration.state === 'credentials_pending'}
                          onChange={() =>
                            setSelected((current) => {
                              const next = new Set(current);
                              if (next.has(item.name)) next.delete(item.name);
                              else next.add(item.name);
                              return next;
                            })
                          }
                          aria-label={item.name}
                          title={
                            registration.state === 'credentials_pending'
                              ? t('auth_files.agent_registration_credentials_pending_hint')
                              : undefined
                          }
                        />
                      </td>
                      <td>
                        <span className={styles.accountName} title={item.name}>
                          {item.name}
                        </span>
                      </td>
                      <td>
                        <span
                          className={`${styles.stateBadge} ${styles[`state_${registration.state}`]}`}
                        >
                          {registration.active && (
                            <IconRefreshCw size={13} className={styles.spin} />
                          )}
                          {t(`auth_files.agent_registration_state_${registration.state}`)}
                        </span>
                      </td>
                      <td>
                        {registration.trigger
                          ? t(`agent_recovery.trigger_${registration.trigger}`)
                          : '-'}
                      </td>
                      <td>{registration.attempts ?? 0}</td>
                      <td>
                        <div className={styles.timeline}>
                          <span>{formatTime(registration.queued_at)}</span>
                          <small>
                            {formatTime(registration.next_retry_at ?? registration.finished_at)}
                          </small>
                        </div>
                      </td>
                      <td>
                        {registration.started_at && registration.finished_at
                          ? formatDuration(
                              new Date(registration.finished_at).getTime() -
                                new Date(registration.started_at).getTime()
                            )
                          : '-'}
                      </td>
                      <td>
                        <span className={styles.errorText} title={registration.error}>
                          {registration.state === 'credentials_pending'
                            ? t('auth_files.agent_registration_credentials_pending_hint')
                            : registration.error_code || registration.error || '-'}
                        </span>
                      </td>
                      <td>
                        <div className={styles.rowActions}>
                          {registration.can_retry && (
                            <Button
                              variant="secondary"
                              size="xs"
                              disabled={busy}
                              onClick={() => void runSingleAction(item.name, false)}
                            >
                              {t('common.retry')}
                            </Button>
                          )}
                          {registration.state !== 'credentials_pending' && (
                            <Button
                              size="xs"
                              disabled={busy}
                              onClick={() => void runSingleAction(item.name, true)}
                            >
                              {t('agent_recovery.rebuild')}
                            </Button>
                          )}
                        </div>
                      </td>
                    </tr>
                  );
                })}
              {viewMode === 'history' &&
                (pageRows as AgentIdentityRecoveryHistoryEntry[]).map((item) => (
                  <tr key={item.id}>
                    <td>
                      <span className={styles.accountName} title={item.name}>
                        {item.name || '-'}
                      </span>
                    </td>
                    <td>
                      <span className={`${styles.stateBadge} ${styles[`state_${item.state}`]}`}>
                        {t(`auth_files.agent_registration_state_${item.state}`)}
                      </span>
                    </td>
                    <td>{item.trigger ? t(`agent_recovery.trigger_${item.trigger}`) : '-'}</td>
                    <td>{item.attempt}</td>
                    <td>
                      <div className={styles.timeline}>
                        <span>{formatTime(item.started_at)}</span>
                        <small>{formatTime(item.finished_at)}</small>
                      </div>
                    </td>
                    <td>{formatDuration(item.duration_ms)}</td>
                    <td>
                      <span className={styles.errorText} title={item.error}>
                        {item.error_code || item.error || '-'}
                      </span>
                    </td>
                  </tr>
                ))}
              {pageRows.length === 0 && (
                <tr>
                  <td colSpan={viewMode === 'current' ? 9 : 7} className={styles.emptyCell}>
                    <IconTimer size={24} />
                    {t('agent_recovery.empty')}
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
        <footer className={styles.pagination}>
          <span>
            {t('agent_recovery.page_summary', {
              page: currentPage,
              total: totalPages,
              count: activeRows.length,
            })}
          </span>
          <div>
            <Button
              variant="secondary"
              size="xs"
              iconOnly
              aria-label={t('common.previous')}
              disabled={currentPage <= 1}
              onClick={() => setPage((value) => Math.max(1, value - 1))}
            >
              <IconChevronLeft size={16} />
            </Button>
            <Button
              variant="secondary"
              size="xs"
              iconOnly
              aria-label={t('common.next')}
              disabled={currentPage >= totalPages}
              onClick={() => setPage((value) => Math.min(totalPages, value + 1))}
            >
              <IconChevronRight size={16} />
            </Button>
          </div>
        </footer>
      </section>
    </div>
  );
}
