import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { sha256Hex } from '@/utils/apiKeyHash';
import { Button } from '@/components/ui/Button';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import {
  identityPoolsApi,
  type CacheAffinityStats,
  type IdentityAccount,
  type IdentityPool,
  type IdentitySession,
  type IdentityPoolsApiScope,
} from '@/services/api';
import { useAuthStore, useNotificationStore } from '@/stores';
import styles from './IdentityPoolsPage.module.scss';

const POOL_ID = 'codex';

const formatRate = (value: number | null | undefined): string =>
  value === null || value === undefined ? '—' : `${(value * 100).toFixed(1)}%`;

export function IdentityPoolsPage() {
  const apiBase = useAuthStore((state) => state.apiBase);
  const managementKey = useAuthStore((state) => state.managementKey);
  const connectionStatus = useAuthStore((state) => state.connectionStatus);
  const scope = useMemo<IdentityPoolsApiScope>(
    () => ({ apiBase, managementKey }),
    [apiBase, managementKey]
  );
  const connected = connectionStatus === 'connected' && Boolean(apiBase && managementKey);
  // Remount connection-owned state before the replacement pool is rendered.
  // The React key contains only a digest, never the management credential.
  const connectionKey = useMemo(
    () => sha256Hex(JSON.stringify([apiBase, managementKey, connected])),
    [apiBase, managementKey, connected]
  );
  return <IdentityPoolsConnection key={connectionKey} scope={scope} connected={connected} />;
}

function IdentityPoolsConnection({
  scope,
  connected,
}: {
  scope: IdentityPoolsApiScope;
  connected: boolean;
}) {
  const { t } = useTranslation();
  const showNotification = useNotificationStore((state) => state.showNotification);
  const [pool, setPool] = useState<IdentityPool | null>(null);
  const [runtimeEnabled, setRuntimeEnabled] = useState<boolean | null>(null);
  const [accounts, setAccounts] = useState<IdentityAccount[]>([]);
  const [sessions, setSessions] = useState<IdentitySession[]>([]);
  const [stats, setStats] = useState<CacheAffinityStats | null>(null);
  const [selectedAccount, setSelectedAccount] = useState('');
  const [loading, setLoading] = useState(connected);
  const [sessionsLoading, setSessionsLoading] = useState(false);
  const [loadError, setLoadError] = useState('');
  const [sessionError, setSessionError] = useState('');
  const generationRef = useRef(0);
  const sessionGenerationRef = useRef(0);
  const selectedAccountRef = useRef('');
  const mountedRef = useRef(false);

  useLayoutEffect(() => {
    generationRef.current++;
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  const isCurrent = useCallback(
    (generation: number) => mountedRef.current && generation === generationRef.current,
    []
  );

  const loadSessions = useCallback(
    async (account: string, generation: number) => {
      if (!isCurrent(generation)) return;
      const sessionGeneration = ++sessionGenerationRef.current;
      const sessionIsCurrent = () =>
        isCurrent(generation) && sessionGeneration === sessionGenerationRef.current;
      setSessions([]);
      setSessionError('');
      setSessionsLoading(Boolean(account));
      if (!account) return;
      try {
        const response = await identityPoolsApi.sessions(POOL_ID, account, scope);
        if (sessionIsCurrent()) {
          setSessions(
            (response.sessions ?? []).filter(
              (item) => item.pool_id === POOL_ID && item.account_id === account
            )
          );
        }
      } catch (error) {
        if (!sessionIsCurrent()) return;
        const message = error instanceof Error ? error.message : 'Failed to load sessions';
        setSessionError(message);
        showNotification(message, 'error');
      } finally {
        if (sessionIsCurrent()) setSessionsLoading(false);
      }
    },
    [isCurrent, scope, showNotification]
  );

  const load = useCallback(async () => {
    if (!mountedRef.current || !connected) return;
    const generation = ++generationRef.current;
    sessionGenerationRef.current++;
    setLoading(true);
    setPool(null);
    setRuntimeEnabled(null);
    setAccounts([]);
    setSessions([]);
    setSessionsLoading(false);
    setStats(null);
    setSelectedAccount('');
    setLoadError('');
    setSessionError('');
    // Attach rejection handling immediately, but never await optional diagnostics
    // or a session query before making the account list available.
    const diagnostics = identityPoolsApi.stats(scope).catch(() => null);
    try {
      const [runtime, response] = await Promise.all([
        identityPoolsApi.get(scope),
        identityPoolsApi.accounts(POOL_ID, scope),
      ]);
      if (!isCurrent(generation)) return;
      const nextPool = runtime.pools?.find((item) => item.id === POOL_ID) ?? null;
      const nextAccounts = nextPool
        ? (response.accounts ?? []).filter((item) => item.pool_id === POOL_ID)
        : [];
      const currentAccount = selectedAccountRef.current;
      const nextAccount = nextAccounts.some((item) => item.account_id === currentAccount)
        ? currentAccount
        : (nextAccounts[0]?.account_id ?? '');
      setPool(nextPool);
      setRuntimeEnabled(runtime.enabled);
      setAccounts(nextAccounts);
      setSelectedAccount(nextAccount);
      selectedAccountRef.current = nextAccount;
      void diagnostics.then((value) => {
        if (isCurrent(generation)) setStats(value);
      });
      void loadSessions(nextAccount, generation);
    } catch (error) {
      if (!isCurrent(generation)) return;
      selectedAccountRef.current = '';
      const message = error instanceof Error ? error.message : 'Failed to load identity pools';
      setLoadError(message);
      showNotification(message, 'error');
    } finally {
      if (isCurrent(generation)) setLoading(false);
    }
  }, [connected, isCurrent, loadSessions, scope, showNotification]);

  useEffect(() => {
    void load();
  }, [load]);

  const selectAccount = (account: string) => {
    if (
      !connected ||
      !mountedRef.current ||
      !accounts.some((item) => item.account_id === account) ||
      selectedAccountRef.current === account
    )
      return;
    setSelectedAccount(account);
    selectedAccountRef.current = account;
    void loadSessions(account, generationRef.current);
  };

  const rotate = async () => {
    if (!selectedAccount) return;
    const requestScope = { ...scope };
    const generation = generationRef.current;
    try {
      await identityPoolsApi.rotate(POOL_ID, selectedAccount, requestScope);
      if (!isCurrent(generation)) return;
      showNotification('Account identity rotated', 'success');
      await load();
    } catch (error) {
      if (!isCurrent(generation)) return;
      showNotification(
        error instanceof Error ? error.message : 'Failed to rotate identity',
        'error'
      );
    }
  };
  const toggleProfile = async (id: string, enabled: boolean) => {
    const requestScope = { ...scope };
    const generation = generationRef.current;
    try {
      await identityPoolsApi.patchProfile(POOL_ID, id, { enabled: !enabled }, requestScope);
      if (!isCurrent(generation)) return;
      await load();
    } catch (error) {
      if (!isCurrent(generation)) return;
      showNotification(
        error instanceof Error ? error.message : 'Failed to update profile',
        'error'
      );
    }
  };
  const removeSession = async (session: IdentitySession) => {
    const requestScope = { ...scope };
    const account = session.account_id;
    if (
      session.pool_id !== POOL_ID ||
      account !== selectedAccountRef.current ||
      !mountedRef.current
    )
      return;
    const sessionGeneration = sessionGenerationRef.current;
    const generation = generationRef.current;
    try {
      await identityPoolsApi.deleteSession(
        POOL_ID,
        account,
        session.logical_session_id,
        requestScope
      );
      if (
        isCurrent(generation) &&
        sessionGeneration === sessionGenerationRef.current &&
        account === selectedAccountRef.current
      )
        setSessions((items) =>
          items.filter(
            (item) =>
              item.account_id !== account || item.logical_session_id !== session.logical_session_id
          )
        );
    } catch (error) {
      if (!isCurrent(generation) || sessionGeneration !== sessionGenerationRef.current) return;
      showNotification(error instanceof Error ? error.message : 'Failed to clear session', 'error');
    }
  };
  const configurationStatus = !connected
    ? 'disconnected'
    : runtimeEnabled === null
      ? 'unavailable'
      : !pool
        ? 'not_configured'
        : runtimeEnabled
          ? 'enabled'
          : 'disabled';
  if (loading) return <LoadingSpinner />;
  return (
    <main className={styles.page}>
      <header className={styles.hero}>
        <div>
          <span className={styles.eyebrow}>CODEX RUNTIME</span>
          <h1>{t('identity_pools.title')}</h1>
          <p>{t('identity_pools.description')}</p>
        </div>
        <Button size="sm" variant="secondary" disabled={!connected} onClick={() => void load()}>
          {t('identity_pools.refresh')}
        </Button>
      </header>
      {loadError && <p role="alert">{loadError}</p>}
      <section className={styles.metrics}>
        <div>
          <span>{t('identity_pools.configuration')}</span>
          <strong>{t(`identity_pools.status.${configurationStatus}`)}</strong>
        </div>
        <div>
          <span>{t('identity_pools.accounts')}</span>
          <strong>{accounts.length}</strong>
        </div>
        <div>
          <span>{t('identity_pools.profiles')}</span>
          <strong>{pool?.profiles.length ?? 0}</strong>
        </div>
        <div>
          <span>{t('identity_pools.sessions')}</span>
          <strong>{sessions.length}</strong>
        </div>
        <div>
          <span>{t('identity_pools.token_hit_rate')}</span>
          <strong>{formatRate(stats?.token_weighted_hit_rate)}</strong>
        </div>
        <div>
          <span>{t('identity_pools.request_hit_rate')}</span>
          <strong>{formatRate(stats?.request_hit_rate)}</strong>
        </div>
        <div>
          <span>{t('identity_pools.route_rebinds')}</span>
          <strong>{stats?.route_rebinds ?? '—'}</strong>
        </div>
        <div>
          <span>{t('identity_pools.prefix_heat_matches')}</span>
          <strong>{stats?.prefix_heat_matches ?? '—'}</strong>
        </div>
        <div>
          <span>{t('identity_pools.tail_burst_fallbacks')}</span>
          <strong>{stats?.tail_burst_fallbacks ?? '—'}</strong>
        </div>
        <div>
          <span>{t('identity_pools.fingerprint_rejections')}</span>
          <strong>{stats?.engine_fingerprint_rejections ?? '—'}</strong>
        </div>
      </section>
      <section className={styles.panel}>
        <div className={styles.panelHeader}>
          <div>
            <h2>{t('identity_pools.observed_profiles')}</h2>
            <p>{t('identity_pools.metadata_note')}</p>
          </div>
        </div>
        <div className={styles.profileGrid}>
          {(pool?.profiles ?? []).map((profile) => (
            <article key={profile.id}>
              <strong>{profile.id}</strong>
              <span>
                {profile.version || t('identity_pools.unknown')} · {profile.platform || t('identity_pools.platform_unknown')}
              </span>
              <Button
                size="xs"
                variant={profile.enabled ? 'secondary' : 'primary'}
                onClick={() => void toggleProfile(profile.id, profile.enabled)}
              >
                {profile.enabled ? t('identity_pools.disable') : t('identity_pools.enable')}
              </Button>
            </article>
          ))}
        </div>
      </section>
      <section className={styles.workspace}>
        <div className={styles.panelHeader}>
          <div>
            <h2>{t('identity_pools.account_bindings')}</h2>
            <p>{t('identity_pools.binding_note')}</p>
          </div>
          <Button
            size="sm"
            variant="danger"
            disabled={!selectedAccount}
            onClick={() => void rotate()}
          >
            {t('identity_pools.rotate_identity')}
          </Button>
        </div>
        <div className={styles.accountList}>
          {accounts.map((account) => (
            <button
              className={account.account_id === selectedAccount ? styles.selected : ''}
              aria-pressed={account.account_id === selectedAccount}
              key={account.account_id}
              onClick={() => selectAccount(account.account_id)}
            >
              <strong>{account.account_id}</strong>
              <span>
                {t('identity_pools.profile_label')} {account.profile_id || t('identity_pools.unassigned')} · v{account.identity_version}
              </span>
            </button>
          ))}
        </div>
        {selectedAccount && (
          <div className={styles.sessions}>
            <h3>{t('identity_pools.sessions')}</h3>
            {sessionsLoading ? (
              <p>{t('identity_pools.loading_sessions')}</p>
            ) : sessionError ? (
              <p role="alert">{sessionError}</p>
            ) : sessions.length === 0 ? (
              <p>{t('identity_pools.no_active_sessions')}</p>
            ) : (
              sessions.map((session) => (
                <div
                  className={styles.session}
                  key={`${session.account_id}:${session.logical_session_id}`}
                >
                  <span>{session.logical_session_id}</span>
                  <code>{session.prompt_cache_key.slice(0, 16)}…</code>
                  <Button size="xs" variant="ghost" onClick={() => void removeSession(session)}>
                    {t('identity_pools.clear')}
                  </Button>
                </div>
              ))
            )}
          </div>
        )}
      </section>
    </main>
  );
}
