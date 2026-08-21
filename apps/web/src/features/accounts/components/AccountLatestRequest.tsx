import { type JSX, useLayoutEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { FailureDetailsTooltip } from '@/features/monitoring/components/FailureDetailsTooltip';
import { buildAccountLatestRequestFailureDetails } from '@/features/accounts/model/accountLatestRequest';
import type { MonitoringAccountLatestRequest } from '@/services/api/usageService';
import { normalizeRecentRequestBuckets, type RecentRequestBucket } from '@/utils/recentRequests';
import { AccountRequestTimeTooltip } from './AccountRequestTimeTooltip';
import styles from './AccountLatestRequest.module.scss';

type AccountLatestRequestProps = {
  latestRequest?: MonitoringAccountLatestRequest | null;
  recentRequests?: MonitoringAccountLatestRequest[];
  fallbackRecentRequests?: RecentRequestBucket[];
  loading?: boolean;
  unavailable?: boolean;
  locale?: string;
  className?: string;
  onCopy: (text: string) => void;
};

const STATUS_SLOT_MIN = 5;
const STATUS_SLOT_MAX = 10;
const STATUS_BAR_WIDTH = 5;
const STATUS_BAR_GAP = 4;

const resolveStatusSlotCount = (timeWidth: number): number => {
  if (!Number.isFinite(timeWidth) || timeWidth <= 0) return STATUS_SLOT_MIN;
  const availableSlots = Math.floor(
    (timeWidth + STATUS_BAR_GAP) / (STATUS_BAR_WIDTH + STATUS_BAR_GAP)
  );
  return Math.min(STATUS_SLOT_MAX, Math.max(STATUS_SLOT_MIN, availableSlots));
};

const padTimePart = (value: number) => String(value).padStart(2, '0');

const formatRequestTime = (timestamp: number, format: 'short' | 'full'): string | null => {
  if (!Number.isFinite(timestamp)) return null;
  const date = new Date(timestamp);
  if (Number.isNaN(date.getTime())) return null;

  const datePart = `${date.getFullYear()}/${padTimePart(date.getMonth() + 1)}/${padTimePart(date.getDate())}`;
  const timePart = `${padTimePart(date.getHours())}:${padTimePart(date.getMinutes())}:${padTimePart(date.getSeconds())}`;
  return format === 'full' ? `${datePart} ${timePart}` : `${datePart.slice(5)} ${timePart}`;
};

type FallbackRequestStatus = 'success' | 'failed' | 'mixed';

const getFallbackRequestStatus = (bucket: RecentRequestBucket): FallbackRequestStatus | null => {
  if (bucket.failed > 0 && bucket.success > 0) return 'mixed';
  if (bucket.failed > 0) return 'failed';
  if (bucket.success > 0) return 'success';
  return null;
};

export function AccountLatestRequest({
  latestRequest,
  recentRequests,
  fallbackRecentRequests,
  loading = false,
  unavailable = false,
  className,
  onCopy,
}: AccountLatestRequestProps): JSX.Element {
  const { t } = useTranslation();
  const timeRef = useRef<HTMLTimeElement | null>(null);
  const [statusSlotCount, setStatusSlotCount] = useState(STATUS_SLOT_MIN);
  const exactRequestHistory =
    recentRequests && recentRequests.length > 0
      ? recentRequests
      : latestRequest
        ? [latestRequest]
        : [];
  const fallbackRequestHistory = normalizeRecentRequestBuckets(fallbackRecentRequests).filter(
    (bucket) => getFallbackRequestStatus(bucket) !== null
  );
  const useFallback = exactRequestHistory.length === 0 && fallbackRequestHistory.length > 0;
  const showExactRequests = exactRequestHistory.length > 0 && !loading && !unavailable;
  const hasVisibleRequestData = showExactRequests || useFallback;
  const visibleExactRequests = showExactRequests
    ? exactRequestHistory.slice(0, statusSlotCount).reverse()
    : [];
  const visibleFallbackRequests = useFallback ? fallbackRequestHistory.slice(-statusSlotCount) : [];
  const emptySlotCount = Math.max(
    0,
    statusSlotCount - (useFallback ? visibleFallbackRequests.length : visibleExactRequests.length)
  );
  const newestRequest = exactRequestHistory[0] ?? latestRequest ?? null;
  const newestFallbackRequest = useFallback
    ? fallbackRequestHistory[fallbackRequestHistory.length - 1]
    : null;
  const statusSummary =
    unavailable && !useFallback
      ? t('accounts.latest_request_unavailable')
      : loading && !useFallback
        ? t('accounts.latest_request_loading')
        : !hasVisibleRequestData
          ? t('accounts.latest_request_empty')
          : useFallback
            ? visibleFallbackRequests
                .map((bucket) =>
                  [
                    bucket.time,
                    `${t('status_bar.success_short')} ${bucket.success}`,
                    `${t('status_bar.failure_short')} ${bucket.failed}`,
                  ]
                    .filter(Boolean)
                    .join(' · ')
                )
                .join(' · ')
            : visibleExactRequests
                .map((request) =>
                  t(request.failed ? 'monitoring.result_failed' : 'monitoring.result_success')
                )
                .join(' · ');
  const timestamp = newestRequest?.timestamp_ms;
  const shortTimestamp = useFallback
    ? newestFallbackRequest?.time || null
    : typeof timestamp === 'number'
      ? formatRequestTime(timestamp, 'short')
      : null;
  const fullTimestamp = useFallback
    ? newestFallbackRequest?.time || null
    : typeof timestamp === 'number'
      ? formatRequestTime(timestamp, 'full')
      : null;
  const timestampISO =
    !useFallback && fullTimestamp && typeof timestamp === 'number'
      ? new Date(timestamp).toISOString()
      : undefined;
  const timeFallbackLabel = t('accounts.latest_request_time_title');
  const timeTooltipValue = fullTimestamp ?? '-';
  const timeAriaLabel = `${timeFallbackLabel}: ${timeTooltipValue}`;

  useLayoutEffect(() => {
    const timeElement = timeRef.current;
    if (!timeElement) return undefined;

    const updateStatusSlotCount = () => {
      const nextCount = resolveStatusSlotCount(timeElement.getBoundingClientRect().width);
      setStatusSlotCount((currentCount) => (currentCount === nextCount ? currentCount : nextCount));
    };

    updateStatusSlotCount();

    if (typeof ResizeObserver !== 'undefined') {
      const observer = new ResizeObserver(updateStatusSlotCount);
      observer.observe(timeElement);
      return () => observer.disconnect();
    }

    if (typeof window !== 'undefined') {
      window.addEventListener('resize', updateStatusSlotCount);
      return () => window.removeEventListener('resize', updateStatusSlotCount);
    }

    return undefined;
  }, [shortTimestamp]);

  return (
    <div className={[styles.root, className].filter(Boolean).join(' ')}>
      <AccountRequestTimeTooltip
        label={timeFallbackLabel}
        value={timeTooltipValue}
        ariaLabel={timeAriaLabel}
      >
        <time
          ref={timeRef}
          className={styles.time}
          dateTime={timestampISO}
          data-account-request-time="true"
        >
          <span className={styles.timeValue}>{shortTimestamp ?? '-'}</span>
        </time>
      </AccountRequestTimeTooltip>
      <span
        className={styles.statusTrack}
        style={{ gridTemplateColumns: `repeat(${statusSlotCount}, minmax(0, 1fr))` }}
        aria-label={statusSummary}
        data-account-request-status-track="true"
        data-account-request-status-slot-count={statusSlotCount}
      >
        {Array.from({ length: emptySlotCount }, (_, index) => (
          <span key={`empty-${index}`} className={styles.statusSlot}>
            <span
              className={`${styles.statusBar} ${styles.statusBarEmpty}`}
              data-request-status="empty"
              aria-hidden="true"
            />
          </span>
        ))}
        {useFallback
          ? visibleFallbackRequests.map((bucket, index) => {
              const status = getFallbackRequestStatus(bucket);
              if (!status) return null;
              const title = [
                bucket.time,
                `${t('status_bar.success_short')} ${bucket.success}`,
                `${t('status_bar.failure_short')} ${bucket.failed}`,
              ]
                .filter(Boolean)
                .join(' · ');
              return (
                <span key={`${bucket.time || 'bucket'}-${index}`} className={styles.statusSlot}>
                  <span
                    className={`${styles.statusBar} ${
                      status === 'success'
                        ? styles.statusBarSuccess
                        : status === 'failed'
                          ? styles.statusBarFailed
                          : styles.statusBarMixed
                    }`}
                    data-request-status={status}
                    data-request-source="auth-file-bucket"
                    role="img"
                    aria-label={title}
                    title={title}
                  />
                </span>
              );
            })
          : visibleExactRequests.map((request, index) => {
              if (!request) {
                return null;
              }

              if (!request.failed) {
                return (
                  <span key={`${request.timestamp_ms}-${index}`} className={styles.statusSlot}>
                    <span
                      className={`${styles.statusBar} ${styles.statusBarSuccess}`}
                      data-request-status="success"
                      role="img"
                      aria-label={t('monitoring.result_success')}
                      title={t('monitoring.result_success')}
                    />
                  </span>
                );
              }

              const failureDetails = buildAccountLatestRequestFailureDetails(request, t);
              if (!failureDetails) {
                return (
                  <span key={`${request.timestamp_ms}-${index}`} className={styles.statusSlot}>
                    <span
                      className={`${styles.statusBar} ${styles.statusBarFailed}`}
                      data-request-status="failed"
                      role="img"
                      aria-label={t('monitoring.result_failed')}
                      title={t('monitoring.result_failed')}
                    />
                  </span>
                );
              }
              return (
                <span key={`${request.timestamp_ms}-${index}`} className={styles.statusSlot}>
                  <FailureDetailsTooltip
                    ariaLabel={failureDetails.ariaLabel}
                    statusText={failureDetails.statusText}
                    detailLines={failureDetails.detailLines}
                    copyText={failureDetails.copyText}
                    copyLabel={t('accounts.latest_request_copy_details')}
                    onCopy={onCopy}
                  >
                    <span
                      className={`${styles.statusBar} ${styles.statusBarFailed}`}
                      data-request-status="failed"
                      aria-hidden="true"
                    />
                  </FailureDetailsTooltip>
                </span>
              );
            })}
      </span>
    </div>
  );
}
