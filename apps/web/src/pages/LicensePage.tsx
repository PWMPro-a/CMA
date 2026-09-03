import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import { Modal } from '@/components/ui/Modal';
import { IconKey, IconRefreshCw, IconShield, IconShieldCheck } from '@/components/ui/icons';
import { licenseApi, type LicenseStatus } from '@/services/api/license';
import { useNotificationStore } from '@/stores';
import styles from './LicensePage.module.scss';

type CallbackMessage = {
  type?: string;
  state?: string;
  code?: string;
  error?: string;
};

const popupFeatures = 'popup=yes,width=920,height=760,resizable=yes,scrollbars=yes';
const EXPIRY_WARNING_SECONDS = 72 * 60 * 60;

export function LicensePage() {
  const { t, i18n } = useTranslation();
  const { showNotification } = useNotificationStore();
  const [status, setStatus] = useState<LicenseStatus | null>(null);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [authorizing, setAuthorizing] = useState(false);
  const [activateOpen, setActivateOpen] = useState(false);
  const [activationCode, setActivationCode] = useState('');
  const [activating, setActivating] = useState(false);
  const [clock, setClock] = useState(() => Date.now());
  const pendingStateRef = useRef('');
  const popupRef = useRef<Window | null>(null);

  const loadStatus = useCallback(async () => {
    try {
      setStatus(await licenseApi.status());
    } catch (error) {
      showNotification(error instanceof Error ? error.message : String(error), 'error');
    } finally {
      setLoading(false);
    }
  }, [showNotification]);

  useEffect(() => {
    void loadStatus();
    return () => popupRef.current?.close();
  }, [loadStatus]);

  useEffect(() => {
    if (!status?.grace_until && !status?.expires_at && !status?.expiry_grace_until) return;
    const timer = window.setInterval(() => setClock(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [status?.grace_until]);

  const errorText = useCallback(
    (code: string) =>
      t(`license.errors.${code}`, {
        defaultValue: t('license.errors.provider_error'),
      }),
    [t]
  );

  useEffect(() => {
    const handleMessage = (event: MessageEvent<CallbackMessage>) => {
      if (
        event.source !== popupRef.current ||
        event.origin !== window.location.origin ||
        event.data?.type !== 'cpa-license-callback'
      ) {
        return;
      }
      if (!event.data.state || event.data.state !== pendingStateRef.current) {
        return;
      }
      const state = pendingStateRef.current;
      pendingStateRef.current = '';
      popupRef.current?.close();
      popupRef.current = null;
      if (event.data.error || !event.data.code) {
        setAuthorizing(false);
        showNotification(errorText(event.data.error || 'authorization_invalid'), 'error');
        return;
      }
      void licenseApi
        .exchangeShopCode(state, event.data.code)
        .then((result) => {
          setStatus(result.license);
          showNotification(t('license.authorization_success'), 'success');
        })
        .catch((error) => {
          showNotification(errorText(error instanceof Error ? error.message : 'provider_error'), 'error');
        })
        .finally(() => setAuthorizing(false));
    };
    window.addEventListener('message', handleMessage);
    return () => window.removeEventListener('message', handleMessage);
  }, [errorText, showNotification, t]);

  const handleShopAuthorization = async () => {
    const popup = window.open('about:blank', 'cpa-shop-license', popupFeatures);
    if (!popup) {
      showNotification(t('license.popup_blocked'), 'warning');
      return;
    }
    popup.document.title = t('license.shop_authorization');
    popup.document.body.textContent = t('license.opening_shop');
    popupRef.current = popup;
    setAuthorizing(true);
    try {
      const callbackUrl = new URL('/license/shop/callback', window.location.origin).toString();
      const authorization = await licenseApi.startShopAuthorization(
        callbackUrl,
        window.location.origin
      );
      pendingStateRef.current = authorization.state;
      popup.location.replace(authorization.url);
      popup.focus();
    } catch (error) {
      popup.close();
      popupRef.current = null;
      setAuthorizing(false);
      showNotification(errorText(error instanceof Error ? error.message : 'provider_error'), 'error');
    }
  };

  const handleRefresh = async () => {
    setRefreshing(true);
    try {
      const result = await licenseApi.refresh();
      setStatus(result.license);
      showNotification(t('license.refresh_success'), 'success');
    } catch (error) {
      showNotification(errorText(error instanceof Error ? error.message : 'provider_error'), 'error');
    } finally {
      setRefreshing(false);
    }
  };

  const handleActivate = async () => {
    const code = activationCode.trim();
    if (!code) return;
    setActivating(true);
    try {
      const result = await licenseApi.activate(code);
      setStatus(result.license);
      setActivationCode('');
      setActivateOpen(false);
      showNotification(t('license.activation_success'), 'success');
    } catch (error) {
      showNotification(errorText(error instanceof Error ? error.message : 'provider_error'), 'error');
    } finally {
      setActivating(false);
    }
  };

  const formatTime = (value?: number) => {
    if (!value) return t('common.not_available', { defaultValue: '-' });
    return new Intl.DateTimeFormat(i18n.language, {
      dateStyle: 'medium',
      timeStyle: 'medium',
    }).format(new Date(value * 1000));
  };

  const graceRemainingSeconds = status?.grace_until
    ? Math.max(0, Math.ceil(status.grace_until - clock / 1000))
    : Math.max(0, status?.grace_remaining_seconds || 0);
  const formatCountdown = (seconds: number) => {
    const total = Math.max(0, Math.floor(seconds));
    const days = Math.floor(total / 86400);
    const hours = Math.floor((total % 86400) / 3600);
    const minutes = Math.floor((total % 3600) / 60);
    const secs = total % 60;
    if (days > 0) return t('license.grace_countdown_days', { days, hours });
    if (hours > 0) return t('license.grace_countdown_hours', { hours, minutes });
    return t('license.grace_countdown_minutes', { minutes, seconds: secs });
  };

  const statusKey = useMemo(() => {
    if (!status?.enabled) return 'disabled';
    if (status.valid && status.expiry_grace) return 'expiry_grace';
    if (status.valid && status.in_grace) return 'grace';
    if (status.valid) return 'active';
    return status?.reason || 'not_activated';
  }, [status]);

  if (loading && !status) {
    return <LoadingSpinner />;
  }

  const active = Boolean(status?.valid);
  const graceActive = Boolean(status?.in_grace && graceRemainingSeconds > 0);
  const expiryGraceRemainingSeconds = status?.expiry_grace_until
    ? Math.max(0, Math.ceil(status.expiry_grace_until - clock / 1000))
    : Math.max(0, status?.expiry_grace_remaining_seconds || 0);
  const expiryGraceActive = Boolean(status?.expiry_grace && expiryGraceRemainingSeconds > 0);
  const expiryRemainingSeconds = status?.expires_at
    ? Math.max(0, Math.ceil(status.expires_at - clock / 1000))
    : 0;
  const expiryWarningActive = Boolean(
    status?.valid && !status?.in_grace && !expiryGraceActive && expiryRemainingSeconds > 0 && expiryRemainingSeconds <= EXPIRY_WARNING_SECONDS
  );
  const graceEnded = Boolean(status?.grace_until && !graceActive && status?.reason === 'not_activated');
  return (
    <div className={styles.page}>
      <header className={styles.toolbar}>
        <div className={styles.titleGroup}>
          <h1>{t('license.title')}</h1>
          <p>{t('license.subtitle')}</p>
        </div>
        <div className={styles.actions}>
          <Button
            variant="secondary"
            onClick={() => void handleRefresh()}
            loading={refreshing}
            disabled={!status?.enabled}
          >
            <IconRefreshCw size={16} />
            {t('license.refresh')}
          </Button>
          <Button
            onClick={() => void handleShopAuthorization()}
            loading={authorizing}
            disabled={!status?.enabled}
          >
            <IconShieldCheck size={16} />
            {t('license.shop_authorization')}
          </Button>
        </div>
      </header>

      <section className={styles.statusPanel}>
        <div className={styles.statusLead}>
          <span className={`${styles.statusIcon} ${active ? styles.active : styles.inactive}`}>
            {active ? <IconShieldCheck size={24} /> : <IconShield size={24} />}
          </span>
          <div>
            <span className={styles.label}>{t('license.current_status')}</span>
            <strong>{t(`license.status.${statusKey}`, { defaultValue: statusKey })}</strong>
          </div>
        </div>
        {!status?.enabled ? <p className={styles.notice}>{t('license.disabled_notice')}</p> : null}
        {expiryWarningActive ? (
          <div className={`${styles.graceNotice} ${styles.expiryWarning}`}>
            <strong>{t('license.expiry_warning_title')}</strong>
            <span>{formatCountdown(expiryRemainingSeconds)}</span>
            <p>{t('license.expiry_warning_notice', { countdown: formatCountdown(expiryRemainingSeconds), grace: formatCountdown(status?.grace_period_seconds || 0) })}</p>
          </div>
        ) : null}
        {graceActive ? (
          <div className={styles.graceNotice}>
            <strong>{t('license.grace_title')}</strong>
            <span>{formatCountdown(graceRemainingSeconds)}</span>
            <p>{t('license.grace_notice', { until: formatTime(status?.grace_until) })}</p>
          </div>
        ) : null}
        {expiryGraceActive ? (
          <div className={`${styles.graceNotice} ${styles.expiryGrace}`}>
            <strong>{t('license.expiry_grace_title')}</strong>
            <span>{formatCountdown(expiryGraceRemainingSeconds)}</span>
            <p>{t('license.expiry_grace_notice', { expiredAt: formatTime(status?.expires_at), until: formatTime(status?.expiry_grace_until), countdown: formatCountdown(expiryGraceRemainingSeconds) })}</p>
          </div>
        ) : null}
        {status?.expiry_grace_until && !expiryGraceActive && status?.reason === 'license_expired' ? (
          <div className={`${styles.graceNotice} ${styles.graceExpired}`}>
            <strong>{t('license.expiry_grace_expired_title')}</strong>
            <p>{t('license.expiry_grace_expired_notice')}</p>
          </div>
        ) : null}
        {graceEnded ? <div className={`${styles.graceNotice} ${styles.graceExpired}`}><strong>{t('license.grace_expired_title')}</strong><p>{t('license.grace_expired_notice')}</p></div> : null}
      </section>

      <section className={styles.detailsPanel}>
        <h2>{t('license.details')}</h2>
        <div className={styles.detailsGrid}>
          <div><span>{t('license.product')}</span><strong>{status?.product_name || status?.product_code || '-'}</strong></div>
          <div><span>{t('license.license_id')}</span><strong>{status?.license_id || '-'}</strong></div>
          <div><span>{t('license.expires_at')}</span><strong>{formatTime(status?.expires_at)}</strong></div>
          <div><span>{t('license.instance_binding')}</span><strong>{status?.instance_bound ? t('license.bound') : t('license.unbound')}</strong></div>
          <div><span>{t('license.instance')}</span><strong>{status?.instance_id || '-'}</strong></div>
          <div><span>{t('license.last_verified')}</span><strong>{formatTime(status?.last_verified_at)}</strong></div>
          <div><span>{t('license.grace_period')}</span><strong>{status?.grace_period_seconds ? formatCountdown(status.grace_period_seconds) : t('common.not_available', { defaultValue: '-' })}</strong></div>
        </div>
      </section>

      <section className={styles.backupPanel}>
        <div>
          <h2>{t('license.activation_code')}</h2>
          <p>{t('license.activation_code_hint')}</p>
        </div>
        <Button variant="secondary" onClick={() => setActivateOpen(true)} disabled={!status?.enabled}>
          <IconKey size={16} />
          {t('license.enter_activation_code')}
        </Button>
      </section>

      <Modal
        open={activateOpen}
        title={t('license.enter_activation_code')}
        onClose={() => !activating && setActivateOpen(false)}
        closeDisabled={activating}
        footer={
          <>
            <Button variant="secondary" onClick={() => setActivateOpen(false)} disabled={activating}>
              {t('common.cancel')}
            </Button>
            <Button onClick={() => void handleActivate()} loading={activating} disabled={!activationCode.trim()}>
              {t('license.activate')}
            </Button>
          </>
        }
      >
        <Input
          label={t('license.activation_code')}
          value={activationCode}
          onChange={(event) => setActivationCode(event.target.value)}
          autoComplete="off"
          spellCheck={false}
          placeholder={t('license.activation_code_placeholder')}
        />
      </Modal>
    </div>
  );
}
