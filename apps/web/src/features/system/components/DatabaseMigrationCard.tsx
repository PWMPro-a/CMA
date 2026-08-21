import { useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Card } from '@/components/ui/Card';
import { Input } from '@/components/ui/Input';
import { Select } from '@/components/ui/Select';
import {
  usageServiceApi,
  type DatabaseConnectionConfig,
  type DatabaseManagementStatus,
  type DatabaseMigrationJob,
  type DatabaseMigrationPlan,
  type DatabaseProbeResult,
  type DatabaseSwitchResult,
} from '@/services/api/usageService';
import { useNotificationStore } from '@/stores';
import styles from './DatabaseMigrationCard.module.scss';

interface DatabaseMigrationCardProps {
  base: string;
  managementKey: string;
}

const TERMINAL_STATUSES = new Set(['completed', 'failed', 'cancelled']);

const connectionLabel = (connection?: { driver?: string; path?: string; dsnMasked?: string }) => {
  if (!connection) return '-';
  return connection.driver === 'mysql'
    ? connection.dsnMasked || 'MySQL'
    : connection.path || 'SQLite';
};

export function DatabaseMigrationCard({ base, managementKey }: DatabaseMigrationCardProps) {
  const { t, i18n } = useTranslation();
  const { showConfirmation } = useNotificationStore();
  const [status, setStatus] = useState<DatabaseManagementStatus | null>(null);
  const [driver, setDriver] = useState<'sqlite' | 'mysql'>('mysql');
  const [path, setPath] = useState('');
  const [dsn, setDsn] = useState('');
  const [probe, setProbe] = useState<DatabaseProbeResult | null>(null);
  const [plan, setPlan] = useState<DatabaseMigrationPlan | null>(null);
  const [job, setJob] = useState<DatabaseMigrationJob | null>(null);
  const [switchResult, setSwitchResult] = useState<DatabaseSwitchResult | null>(null);
  const [busy, setBusy] = useState<
    'status' | 'probe' | 'plan' | 'migrate' | 'switch' | 'cancel' | ''
  >('status');
  const [error, setError] = useState('');

  const target = useMemo<DatabaseConnectionConfig>(
    () => (driver === 'mysql' ? { driver, dsn: dsn.trim() } : { driver, path: path.trim() }),
    [driver, dsn, path]
  );
  const targetReady = driver === 'mysql' ? Boolean(dsn.trim()) : Boolean(path.trim());
  const progress = job?.totalRows
    ? Math.min(100, Math.round((job.copiedRows / job.totalRows) * 100))
    : job?.totalTables
      ? Math.min(100, Math.round((job.completedTables / job.totalTables) * 100))
      : 0;

  const refreshStatus = async () => {
    setBusy('status');
    setError('');
    try {
      const next = await usageServiceApi.getDatabaseManagementStatus(base, managementKey);
      setStatus(next);
      if (next.latestMigration) {
        setJob(next.latestMigration);
        if (next.latestMigration.target.driver === 'sqlite') {
          setDriver('sqlite');
          setPath(next.latestMigration.target.path || '');
        } else {
          setDriver('mysql');
        }
      } else if (next.connection.driver === 'mysql') {
        setDriver('sqlite');
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy('');
    }
  };

  useEffect(() => {
    void refreshStatus();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [base, managementKey]);

  useEffect(() => {
    if (!job || TERMINAL_STATUSES.has(job.status)) return;
    const timer = window.setInterval(async () => {
      try {
        const next = await usageServiceApi.getDatabaseMigration(base, managementKey, job.id);
        setJob(next);
      } catch {
        // Keep the last visible progress; the next poll can recover.
      }
    }, 1000);
    return () => window.clearInterval(timer);
  }, [base, job, managementKey]);

  const handleProbe = async () => {
    setBusy('probe');
    setError('');
    setProbe(null);
    setPlan(null);
    setSwitchResult(null);
    try {
      setProbe(await usageServiceApi.testDatabaseConnection(base, managementKey, target));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy('');
    }
  };

  const handlePlan = async () => {
    setBusy('plan');
    setError('');
    setPlan(null);
    setSwitchResult(null);
    try {
      setPlan(await usageServiceApi.planDatabaseMigration(base, managementKey, target));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy('');
    }
  };

  const handleStart = async () => {
    setBusy('migrate');
    setError('');
    setSwitchResult(null);
    try {
      setJob(await usageServiceApi.startDatabaseMigration(base, managementKey, target));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy('');
    }
  };

  const handleCancel = async () => {
    if (!job) return;
    setBusy('cancel');
    setError('');
    try {
      setJob(await usageServiceApi.cancelDatabaseMigration(base, managementKey, job.id));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy('');
    }
  };

  const handleSwitch = async () => {
    if (!job) return;
    setBusy('switch');
    setError('');
    try {
      setSwitchResult(
        await usageServiceApi.prepareDatabaseSwitch(base, managementKey, job.id, target)
      );
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy('');
    }
  };

  const confirmStart = () => {
    showConfirmation({
      title: t('system_info.database_start_confirm_title'),
      message: t('system_info.database_start_confirm_message'),
      confirmText: t('system_info.database_start_migration'),
      onConfirm: () => void handleStart(),
    });
  };

  const confirmSwitch = () => {
    showConfirmation({
      title: t('system_info.database_switch_confirm_title'),
      message: t('system_info.database_switch_confirm_message'),
      confirmText: t('system_info.database_prepare_switch'),
      onConfirm: () => void handleSwitch(),
    });
  };

  return (
    <Card
      title={t('system_info.database_migration_title')}
      extra={
        <Button variant="ghost" size="xs" loading={busy === 'status'} onClick={refreshStatus}>
          {t('common.refresh')}
        </Button>
      }
      className={styles.card}
    >
      <p className={styles.description}>{t('system_info.database_migration_desc')}</p>

      <div className={styles.currentGrid}>
        <div className={styles.metric}>
          <span>{t('system_info.database_current_backend')}</span>
          <strong>{status?.current.driver?.toUpperCase() || '-'}</strong>
        </div>
        <div className={styles.metric}>
          <span>{t('system_info.database_current_endpoint')}</span>
          <strong title={connectionLabel(status?.connection)}>
            {connectionLabel(status?.connection)}
          </strong>
        </div>
        <div className={styles.metric}>
          <span>{t('system_info.database_config_source')}</span>
          <strong>{status?.configuration.source || '-'}</strong>
        </div>
        <div className={styles.metric}>
          <span>{t('system_info.database_tables')}</span>
          <strong>{status?.current.tables?.toLocaleString(i18n.language) ?? '-'}</strong>
        </div>
      </div>

      <div className={styles.formGrid}>
        <div className={styles.selectField}>
          <label htmlFor="database-target-driver">{t('system_info.database_target_driver')}</label>
          <Select
            id="database-target-driver"
            value={driver}
            options={[
              { value: 'mysql', label: 'MySQL' },
              { value: 'sqlite', label: 'SQLite' },
            ]}
            onChange={(value) => {
              setDriver(value as 'sqlite' | 'mysql');
              setProbe(null);
              setPlan(null);
              setSwitchResult(null);
            }}
          />
        </div>
        {driver === 'mysql' ? (
          <Input
            label={t('system_info.database_mysql_dsn')}
            type="password"
            autoComplete="off"
            value={dsn}
            placeholder="USER:PASSWORD@tcp(HOST:3306)/DATABASE"
            onChange={(event) => setDsn(event.target.value)}
            hint={t('system_info.database_mysql_dsn_hint')}
          />
        ) : (
          <Input
            label={t('system_info.database_sqlite_path')}
            value={path}
            placeholder="/data/usage.sqlite"
            onChange={(event) => setPath(event.target.value)}
            hint={t('system_info.database_sqlite_path_hint')}
          />
        )}
      </div>

      <div className={styles.actions}>
        <Button
          variant="secondary"
          loading={busy === 'probe'}
          disabled={!targetReady}
          onClick={handleProbe}
        >
          {t('system_info.database_test_connection')}
        </Button>
        <Button
          variant="secondary"
          loading={busy === 'plan'}
          disabled={!targetReady}
          onClick={handlePlan}
        >
          {t('system_info.database_plan_migration')}
        </Button>
        <Button
          loading={busy === 'migrate'}
          disabled={!plan?.targetEmpty || Boolean(job && !TERMINAL_STATUSES.has(job.status))}
          onClick={confirmStart}
        >
          {t('system_info.database_start_migration')}
        </Button>
      </div>

      {probe ? (
        <div className={`${styles.notice} ${probe.healthy ? styles.success : styles.warning}`}>
          <strong>
            {probe.healthy
              ? t('system_info.database_connection_ok')
              : t('system_info.database_connection_failed')}
          </strong>
          <span>
            {connectionLabel(probe.connection)} · {probe.latencyMs} ms · {probe.tables}{' '}
            {t('system_info.database_tables').toLowerCase()}
          </span>
          {probe.error ? <span>{probe.error}</span> : null}
        </div>
      ) : null}

      {plan ? (
        <div className={`${styles.notice} ${plan.targetEmpty ? styles.success : styles.warning}`}>
          <strong>
            {plan.targetEmpty
              ? t('system_info.database_plan_ready')
              : t('system_info.database_target_not_empty')}
          </strong>
          <span>
            {plan.sourceTables} → {plan.targetTables}{' '}
            {t('system_info.database_tables').toLowerCase()} ·{' '}
            {plan.estimatedSourceRows?.toLocaleString(i18n.language) ?? '-'}{' '}
            {t('system_info.database_rows')}
          </span>
          {plan.warnings?.map((warning) => (
            <span key={warning}>
              {t(`system_info.database_warning_${warning}`, { defaultValue: warning })}
            </span>
          ))}
        </div>
      ) : null}

      {job ? (
        <div className={styles.jobPanel}>
          <div className={styles.jobHeader}>
            <div>
              <span className={styles.jobLabel}>{t('system_info.database_migration_job')}</span>
              <strong>{job.status}</strong>
            </div>
            <span>{progress}%</span>
          </div>
          <div className={styles.progressTrack}>
            <span style={{ width: `${progress}%` }} />
          </div>
          <div className={styles.jobStats}>
            <span>
              {job.completedTables}/{job.totalTables}{' '}
              {t('system_info.database_tables').toLowerCase()}
            </span>
            <span>
              {job.copiedRows.toLocaleString(i18n.language)}/
              {job.totalRows.toLocaleString(i18n.language)} {t('system_info.database_rows')}
            </span>
            <span>{job.currentTable || '-'}</span>
          </div>
          {job.error ? <div className={styles.errorBox}>{job.error}</div> : null}
          <div className={styles.actions}>
            {!TERMINAL_STATUSES.has(job.status) ? (
              <Button variant="danger" loading={busy === 'cancel'} onClick={handleCancel}>
                {t('system_info.database_cancel_migration')}
              </Button>
            ) : null}
            {job.status === 'completed' && job.verified ? (
              <Button loading={busy === 'switch'} disabled={!targetReady} onClick={confirmSwitch}>
                {t('system_info.database_prepare_switch')}
              </Button>
            ) : null}
          </div>
        </div>
      ) : null}

      {switchResult ? (
        <div className={`${styles.notice} ${styles.success}`}>
          <strong>{t('system_info.database_switch_ready')}</strong>
          <span>{switchResult.message}</span>
          {switchResult.configPath ? <code>{switchResult.configPath}</code> : null}
          {switchResult.pendingFile ? <code>{switchResult.pendingFile}</code> : null}
          {switchResult.environment
            ? Object.entries(switchResult.environment).map(([key, value]) => (
                <code key={key}>
                  {key}={value}
                </code>
              ))
            : null}
        </div>
      ) : null}

      {status?.configuration.environmentLock ? (
        <div className={`${styles.notice} ${styles.warning}`}>
          {t('system_info.database_env_locked')}
        </div>
      ) : null}
      {error ? <div className={styles.errorBox}>{error}</div> : null}
    </Card>
  );
}
