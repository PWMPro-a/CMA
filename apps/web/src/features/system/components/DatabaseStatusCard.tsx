import { useTranslation } from 'react-i18next';
import { Card } from '@/components/ui/Card';
import type { UsageServiceStatus } from '@/services/api/usageService';
import { formatFileSize } from '@/utils/format';
import styles from './DatabaseStatusCard.module.scss';

interface DatabaseStatusCardProps {
  status: UsageServiceStatus | null;
  loading: boolean;
  error: string;
}

const formatBytes = (value: number | undefined) =>
  Number.isFinite(value) && Number(value) >= 0 ? formatFileSize(Number(value)) : '-';

const formatCount = (value: number | undefined, locale: string) =>
  Number.isFinite(value) ? Number(value).toLocaleString(locale) : '-';

export function DatabaseStatusCard({ status, loading, error }: DatabaseStatusCardProps) {
  const { t, i18n } = useTranslation();
  const database = status?.database;
  const driver = database?.driver?.trim().toLowerCase() || 'sqlite';
  const isSQLite = driver === 'sqlite';
  const checkpoint = database?.checkpoint;
  const checkpointError = checkpoint?.error;
  const statusError = !database ? error : '';
  const databaseUnavailable = !loading && !database;
  const checkpointUnavailable = Boolean(isSQLite && database && !checkpoint);
  const checkpointBehind =
    Number.isFinite(checkpoint?.logFrames) &&
    Number.isFinite(checkpoint?.checkpointedFrames) &&
    Number(checkpoint?.checkpointedFrames) < Number(checkpoint?.logFrames);
  const checkpointPending = checkpoint?.mode?.trim().toLowerCase() === 'pending';
  const walOverLimit =
    Number.isFinite(database?.walBytes) &&
    Number.isFinite(database?.journalSizeLimitBytes) &&
    Number(database?.walBytes) > Number(database?.journalSizeLimitBytes);
  const checkpointWarning = Boolean(
    statusError ||
    databaseUnavailable ||
    database?.healthy === false ||
    (isSQLite &&
      (checkpointUnavailable ||
        checkpointError ||
        Number(checkpoint?.busy) > 0 ||
        checkpointBehind ||
        checkpointPending ||
        walOverLimit))
  );
  const initialLoading = loading && !database;

  const checkpointMode = (() => {
    switch (checkpoint?.mode?.trim().toLowerCase()) {
      case 'passive':
        return t('system_info.database_checkpoint_passive');
      case 'truncate':
        return t('system_info.database_checkpoint_truncate');
      case 'pending':
        return t('system_info.database_checkpoint_pending');
      case undefined:
      case '':
        return t('system_info.database_checkpoint_unavailable');
      default:
        return checkpoint?.mode || t('system_info.database_checkpoint_unavailable');
    }
  })();

  const checkpointProgress =
    Number.isFinite(checkpoint?.logFrames) && Number.isFinite(checkpoint?.checkpointedFrames)
      ? t('system_info.database_checkpoint_frames', {
          checkpointed: formatCount(checkpoint?.checkpointedFrames, i18n.language),
          log: formatCount(checkpoint?.logFrames, i18n.language),
        })
      : '-';

  const checkpointTime = !checkpoint
    ? t('system_info.database_checkpoint_unavailable')
    : checkpoint.executedAtMs && Number.isFinite(checkpoint.executedAtMs)
      ? t('system_info.database_checkpoint_time', {
          time: new Date(checkpoint.executedAtMs).toLocaleString(i18n.language),
          duration: formatCount(checkpoint.durationMs, i18n.language),
        })
      : t('system_info.database_checkpoint_pending');

  const summaryItems: Array<{ label: string; value: string; emphasis?: boolean }> = isSQLite
    ? [
        {
          label: t('system_info.database_total_size'),
          value: formatBytes(database?.totalBytes),
          emphasis: true,
        },
        { label: t('system_info.database_file'), value: formatBytes(database?.databaseBytes) },
        { label: t('system_info.database_wal_file'), value: formatBytes(database?.walBytes) },
        { label: t('system_info.database_shm_file'), value: formatBytes(database?.shmBytes) },
      ]
    : [
        { label: t('system_info.database_backend'), value: driver.toUpperCase(), emphasis: true },
        { label: t('system_info.database_total_size'), value: formatBytes(database?.sizeBytes) },
        {
          label: t('system_info.database_tables'),
          value: formatCount(database?.tables, i18n.language),
        },
        {
          label: t('system_info.database_estimated_rows'),
          value: formatCount(database?.estimatedRows, i18n.language),
        },
      ];

  const detailItems: Array<{ label: string; value: string; isStatus?: boolean }> = isSQLite
    ? [
        {
          label: t('system_info.database_journal_size_limit'),
          value: formatBytes(database?.journalSizeLimitBytes),
        },
        {
          label: t('system_info.database_checkpoint_mode'),
          value: checkpointMode,
          isStatus: true,
        },
        { label: t('system_info.database_checkpoint_progress'), value: checkpointProgress },
        {
          label: t('system_info.database_checkpoint_busy'),
          value: formatCount(checkpoint?.busy, i18n.language),
        },
        { label: t('system_info.database_checkpoint_last_run'), value: checkpointTime },
      ]
    : [
        { label: t('system_info.database_name'), value: database?.databaseName || '-' },
        { label: t('system_info.database_host'), value: database?.host || '-' },
        { label: t('system_info.database_version'), value: database?.version || '-' },
        {
          label: t('system_info.database_latency'),
          value: `${formatCount(database?.latencyMs, i18n.language)} ms`,
        },
        {
          label: t('system_info.database_connections'),
          value: `${formatCount(database?.connections?.inUseConnections, i18n.language)} / ${formatCount(database?.connections?.openConnections, i18n.language)}`,
        },
      ];

  const statusTone = initialLoading
    ? styles.statusPending
    : checkpointWarning
      ? styles.statusWarn
      : styles.statusOk;

  return (
    <Card
      title={t('system_info.database_status_title')}
      extra={
        <span className={`${styles.statusBadge} ${statusTone}`}>
          <span className={styles.statusDot} />
          {initialLoading
            ? t('common.loading')
            : isSQLite
              ? checkpointMode
              : database?.healthy
                ? t('system_info.database_healthy')
                : t('system_info.database_unhealthy')}
        </span>
      }
      className={styles.card}
    >
      <div className={styles.summaryGrid}>
        {summaryItems.map((item) => (
          <div
            key={item.label}
            className={`${styles.summaryItem} ${item.emphasis ? styles.summaryEmphasis : ''}`}
          >
            <span className={styles.label}>{item.label}</span>
            <strong className={styles.summaryValue}>{initialLoading ? '...' : item.value}</strong>
          </div>
        ))}
      </div>

      <div className={styles.detailsGrid}>
        {detailItems.map((item) => (
          <div key={item.label} className={styles.detailItem}>
            <span className={styles.label}>{item.label}</span>
            <span className={`${styles.value} ${item.isStatus ? statusTone : ''}`}>
              {initialLoading ? '...' : item.value}
              {item.isStatus && database ? <span className={styles.statusDot} /> : null}
            </span>
          </div>
        ))}
      </div>

      {(isSQLite && checkpointError) || statusError || database?.error ? (
        <div className={styles.errorStack}>
          {checkpointError ? (
            <div className={styles.errorLine}>
              <span>{t('system_info.database_checkpoint_error')}</span>
              <strong>{checkpointError}</strong>
            </div>
          ) : null}
          {statusError ? (
            <div className={styles.errorLine}>
              <span>{t('system_info.database_status_error')}</span>
              <strong>{statusError}</strong>
            </div>
          ) : null}
          {database?.error ? (
            <div className={styles.errorLine}>
              <span>{t('system_info.database_status_error')}</span>
              <strong>{database.error}</strong>
            </div>
          ) : null}
        </div>
      ) : null}
    </Card>
  );
}
