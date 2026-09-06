import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Drawer } from '@/components/ui/Drawer';
import { Input } from '@/components/ui/Input';
import { SegmentedTabs, type SegmentedTabItem } from '@/components/ui/SegmentedTabs';
import { Select } from '@/components/ui/Select';
import {
  IconCheck,
  IconChevronRight,
  IconCopy,
  IconRefreshCw,
  IconSearch,
  IconTrash2,
} from '@/components/ui/icons';
import {
  identityPoolsApi,
  type IdentityAccount,
  type IdentityCatalogValidation,
  type IdentityPool,
  type IdentityProfile,
  type IdentitySession,
  type IdentityPoolsApiScope,
} from '@/services/api';
import { useAuthStore, useNotificationStore } from '@/stores';
import { sha256Hex } from '@/utils/apiKeyHash';
import styles from './IdentityPoolsPage.module.scss';

const POOL_ID = 'codex';
type ViewTab = 'accounts' | 'environments' | 'validation';

type EnvironmentRow = IdentityProfile & {
  accounts: IdentityAccount[];
  accountCount: number;
  sessionCount: number;
};

export function IdentityPoolsPage() {
  const apiBase = useAuthStore((state) => state.apiBase);
  const managementKey = useAuthStore((state) => state.managementKey);
  const connectionStatus = useAuthStore((state) => state.connectionStatus);
  const scope = useMemo<IdentityPoolsApiScope>(() => ({ apiBase, managementKey }), [apiBase, managementKey]);
  const connected = connectionStatus === 'connected' && Boolean(apiBase && managementKey);
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
  const notify = useNotificationStore((state) => state.showNotification);
  const translateRef = useRef(t);
  translateRef.current = t;
  const [pool, setPool] = useState<IdentityPool | null>(null);
  const [runtimeEnabled, setRuntimeEnabled] = useState<boolean | null>(null);
  const [accounts, setAccounts] = useState<IdentityAccount[]>([]);
  const [sessions, setSessions] = useState<IdentitySession[]>([]);
  const [selectedAccount, setSelectedAccount] = useState<IdentityAccount | null>(null);
  const [tab, setTab] = useState<ViewTab>('accounts');
  const [query, setQuery] = useState('');
  const [environmentFilter, setEnvironmentFilter] = useState('all');
  const [sessionFilter, setSessionFilter] = useState('all');
  const [loading, setLoading] = useState(connected);
  const [sessionsLoading, setSessionsLoading] = useState(false);
  const [loadError, setLoadError] = useState('');
  const [sessionError, setSessionError] = useState('');
  const [catalog, setCatalog] = useState<IdentityProfile[]>([]);
  const [catalogValidation, setCatalogValidation] = useState<IdentityCatalogValidation | null>(null);
  const [validation, setValidation] = useState<{ total?: number; valid?: number; invalid?: number; exact_match?: number; dynamic_fields_valid?: number; proxy_exposure?: number; by_mode?: Record<string, number>; by_profile?: Record<string, number>; by_issue?: Record<string, number>; records?: Array<{ id: number; score: number; valid: boolean; exact_match?: boolean; dynamic_fields_valid?: boolean; proxy_exposure?: boolean; issue_codes?: string[]; mode: string; profile_id?: string; version?: string; platform?: string; architecture?: string; evidence_status?: string; request_hash?: string; header_names?: string[]; body_keys?: string[] }> }>({});
  const [copiedIdentity, setCopiedIdentity] = useState('');
  const generationRef = useRef(0);
  const sessionGenerationRef = useRef(0);
  const mountedRef = useRef(false);

  useLayoutEffect(() => {
    mountedRef.current = true;
    generationRef.current += 1;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  const isCurrent = useCallback(
    (generation: number) => mountedRef.current && generation === generationRef.current,
    []
  );

  const loadSessions = useCallback(
    async (account: IdentityAccount | null, generation: number) => {
      const sessionGeneration = ++sessionGenerationRef.current;
      setSessions([]);
      setSessionError('');
      if (!account) return;
      setSessionsLoading(true);
      try {
        const response = await identityPoolsApi.sessions(POOL_ID, account.account_id, scope);
        if (isCurrent(generation) && sessionGeneration === sessionGenerationRef.current) {
          setSessions(
            (response.sessions ?? []).filter(
              (item) => item.pool_id === POOL_ID && item.account_id === account.account_id
            )
          );
        }
      } catch (error) {
        if (isCurrent(generation) && sessionGeneration === sessionGenerationRef.current) {
          const message =
            error instanceof Error ? error.message : translateRef.current('identity_pools.load_failed');
          setSessionError(message);
          notify(message, 'error');
        }
      } finally {
        if (isCurrent(generation) && sessionGeneration === sessionGenerationRef.current) {
          setSessionsLoading(false);
        }
      }
    },
    [isCurrent, notify, scope]
  );

  const load = useCallback(async () => {
    if (!connected || !mountedRef.current) return;
    const generation = ++generationRef.current;
    ++sessionGenerationRef.current;
    setLoading(true);
    setPool(null);
    setRuntimeEnabled(null);
    setAccounts([]);
    setSessions([]);
    setSelectedAccount(null);
    setCatalog([]);
    setCatalogValidation(null);
    setValidation({});
    setLoadError('');
    try {
      const catalogPromise =
        typeof identityPoolsApi.catalog === 'function'
          ? identityPoolsApi.catalog(scope)
          : Promise.resolve({ items: [] });
      const validationPromise =
        typeof identityPoolsApi.validation === 'function'
          ? identityPoolsApi.validation(30, scope)
          : Promise.resolve({});
      const catalogValidationPromise =
        typeof identityPoolsApi.catalogValidation === 'function'
          ? identityPoolsApi.catalogValidation(scope)
          : Promise.resolve(null);
      const [runtime, response, catalogResponse, validationResponse, catalogValidationResponse] = await Promise.all([
        identityPoolsApi.get(scope),
        identityPoolsApi.accounts(POOL_ID, scope),
        catalogPromise,
        validationPromise,
        catalogValidationPromise,
      ]);
      if (!isCurrent(generation)) return;
      const nextPool = runtime.pools?.find((item) => item.id === POOL_ID) ?? null;
      const profiles = nextPool?.profiles ?? [];
      const nextAccounts = nextPool
        ? (response.accounts ?? [])
            .filter((item) => item.pool_id === POOL_ID)
            .map((item) => ({
              ...item,
              environment:
                item.environment ?? profiles.find((profile) => profile.id === item.profile_id),
            }))
        : [];
      setPool(nextPool);
      setRuntimeEnabled(runtime.enabled);
      setAccounts(nextAccounts);
      setCatalog(catalogResponse.items ?? []);
      setCatalogValidation(catalogValidationResponse);
      setValidation(validationResponse);
    } catch (error) {
      if (!isCurrent(generation)) return;
      const message =
        error instanceof Error ? error.message : translateRef.current('identity_pools.load_failed');
      setLoadError(message);
      notify(message, 'error');
    } finally {
      if (isCurrent(generation)) setLoading(false);
    }
  }, [connected, isCurrent, notify, scope]);

  useEffect(() => {
    void load();
  }, [load]);

  const openAccount = (account: IdentityAccount) => {
    setSelectedAccount(account);
    void loadSessions(account, generationRef.current);
  };

  const rotate = async (account = selectedAccount) => {
    if (!account) return;
    if (typeof window !== 'undefined' && !window.confirm(t('identity_pools.rotate_confirm'))) return;
    try {
      await identityPoolsApi.rotate(POOL_ID, account.account_id, scope);
      notify(t('identity_pools.rotate_success'), 'success');
      setSelectedAccount(null);
      await load();
    } catch (error) {
      notify(error instanceof Error ? error.message : t('identity_pools.rotate_failed'), 'error');
    }
  };

  const removeSession = async (session: IdentitySession) => {
    if (!selectedAccount) return;
    try {
      await identityPoolsApi.deleteSession(
        POOL_ID,
        selectedAccount.account_id,
        session.logical_session_id,
        scope
      );
      setSessions((items) =>
        items.filter((item) => item.logical_session_id !== session.logical_session_id)
      );
      setAccounts((items) =>
        items.map((item) =>
          item.account_id === selectedAccount.account_id
            ? { ...item, session_count: Math.max(0, (item.session_count ?? 0) - 1) }
            : item
        )
      );
      setSelectedAccount((item) =>
        item ? { ...item, session_count: Math.max(0, (item.session_count ?? 0) - 1) } : item
      );
    } catch (error) {
      notify(error instanceof Error ? error.message : t('identity_pools.clear_failed'), 'error');
    }
  };

  const copyIdentity = async (account: IdentityAccount) => {
    const identity = account.identity_id || account.installation_id || '';
    if (!identity || typeof navigator === 'undefined' || !navigator.clipboard) return;
    try {
      await navigator.clipboard.writeText(identity);
      setCopiedIdentity(account.account_id);
      notify(t('identity_pools.identity_copied'), 'success');
      window.setTimeout(() => setCopiedIdentity(''), 1600);
    } catch {
      notify(t('identity_pools.identity_copy_failed'), 'error');
    }
  };

  const assignEnvironment = async (profileID: string) => {
    if (!selectedAccount || !profileID || profileID === selectedAccount.profile_id) return;
    try {
      await identityPoolsApi.patchAccountProfile(
        POOL_ID,
        selectedAccount.account_id,
        profileID,
        scope
      );
      const environment = pool?.profiles.find((profile) => profile.id === profileID);
      const update = (account: IdentityAccount): IdentityAccount =>
        account.account_id === selectedAccount.account_id
          ? { ...account, profile_id: profileID, environment }
          : account;
      setAccounts((items) => items.map(update));
      setSelectedAccount(update(selectedAccount));
      notify(t('identity_pools.environment_assigned'), 'success');
    } catch (error) {
      notify(
        error instanceof Error ? error.message : t('identity_pools.environment_assign_failed'),
        'error'
      );
    }
  };

  const environments = useMemo<EnvironmentRow[]>(() => {
    const profiles = pool?.profiles ?? [];
    return profiles.map((profile) => {
      const boundAccounts = accounts.filter((account) => account.profile_id === profile.id);
      return {
        ...profile,
        accounts: boundAccounts,
        accountCount: boundAccounts.length,
        sessionCount: boundAccounts.reduce((sum, account) => sum + (account.session_count ?? 0), 0),
      };
    });
  }, [accounts, pool]);

  const catalogObserved = catalog.filter((item) => item.evidence_status === 'request_observed' || item.source === 'real_request').length;
  const catalogArtifacts = catalog.filter((item) => item.evidence_status === 'artifact_verified' || item.source === 'npm_release').length;
  const catalogEligible = catalog.filter((item) => item.eligible).length;
  const catalogRoutable = catalog.filter((item) => item.routable).length;

  const filteredAccounts = useMemo(() => {
    const needle = query.trim().toLowerCase();
    return accounts.filter((account) => {
      const haystack = [
        account.email,
        account.account_label,
        account.auth_file,
        account.auth_index,
        account.account_id,
        account.fingerprint,
        account.identity_id,
        account.installation_id,
        account.environment?.id,
        account.environment?.version,
      ]
        .join(' ')
        .toLowerCase();
      const matchesQuery = !needle || haystack.includes(needle);
      const matchesEnvironment =
        environmentFilter === 'all' || account.profile_id === environmentFilter;
      const matchesSessions =
        sessionFilter === 'all' ||
        (sessionFilter === 'active'
          ? (account.session_count ?? 0) > 0
          : (account.session_count ?? 0) === 0);
      return matchesQuery && matchesEnvironment && matchesSessions;
    });
  }, [accounts, environmentFilter, query, sessionFilter]);

  const totalSessions = accounts.reduce((sum, account) => sum + (account.session_count ?? 0), 0);
  const boundAccounts = accounts.filter((account) => account.identity_bound).length;
  const status = !connected
    ? 'disconnected'
    : runtimeEnabled === null
      ? 'unavailable'
      : !pool
        ? 'not_configured'
        : runtimeEnabled
          ? 'enabled'
          : 'disabled';
  const tabs: ReadonlyArray<SegmentedTabItem<ViewTab>> = [
    { id: 'accounts', label: t('identity_pools.account_identities') },
    { id: 'environments', label: t('identity_pools.environment_templates') },
    { id: 'validation', label: 'Request validation' },
  ];

  if (loading) {
    return (
      <div className={styles.loading}>
        <span className="loading-spinner" />
        {t('identity_pools.loading')}
      </div>
    );
  }

  return (
    <main className={styles.page}>
      <header className={styles.header}>
        <div>
          <span className={styles.eyebrow}>CODEX RUNTIME</span>
          <h1>{t('identity_pools.title')}</h1>
          <p>{t('identity_pools.management_description')}</p>
        </div>
        <div className={styles.headerActions}>
          <span className={`${styles.status} ${styles[`status_${status}`]}`}>
            {t(`identity_pools.status.${status}`)}
          </span>
          <Button size="sm" variant="secondary" disabled={!connected} onClick={() => void load()}>
            <IconRefreshCw size={15} />
            {t('identity_pools.refresh')}
          </Button>
        </div>
      </header>

      {loadError && (
        <p role="alert" className={styles.error}>
          {loadError}
        </p>
      )}

      <section className={styles.operationsBar} aria-label={t('identity_pools.summary')}>
        <strong>{t('identity_pools.operations_title')}</strong>
        <span>{t('identity_pools.accounts_total', { count: accounts.length })}</span>
        <span>{t('identity_pools.accounts_bound', { count: boundAccounts })}</span>
        <span>{t('identity_pools.sessions_total', { count: totalSessions })}</span>
        <span>{t('identity_pools.environments_total', { count: environments.length })}</span>
        <span>{`Catalog ${catalog.length} · request-observed ${catalogObserved} · artifacts ${catalogArtifacts} · evidence-ready ${catalogEligible} · active ${catalogRoutable}`}</span>
        <span>{`Catalog structure ${catalogValidation?.valid ? 'valid' : 'needs attention'} · request evidence ${catalogValidation ? `${Math.round(catalogValidation.evidence_coverage * 100)}%` : '—'} · latest ${(catalogValidation?.latest_versions ?? []).slice(0, 5).join(', ') || '—'}`}</span>
        <span>{`Validation ${validation.valid ?? 0}/${validation.total ?? 0}`}</span>
      </section>

      <SegmentedTabs items={tabs} activeTab={tab} onChange={setTab} ariaLabel={t('identity_pools.view')} />

      {tab === 'accounts' ? (
        <section className={styles.workspace}>
          <div className={styles.sectionHeading}>
            <div>
              <h2>{t('identity_pools.account_list_title')}</h2>
              <p>{t('identity_pools.account_list_note')}</p>
            </div>
            <span className={styles.resultCount}>
              {t('identity_pools.result_count', { count: filteredAccounts.length })}
            </span>
          </div>
          <div className={styles.toolbar}>
            <Input
              aria-label={t('identity_pools.search')}
              placeholder={t('identity_pools.search_placeholder')}
              value={query}
              onChange={(event) => setQuery(event.target.value)}
              rightElement={<IconSearch size={16} />}
            />
            <Select
              ariaLabel={t('identity_pools.environment_filter')}
              value={environmentFilter}
              onChange={setEnvironmentFilter}
              options={[
                { value: 'all', label: t('identity_pools.all_environments') },
                ...environments.map((item) => ({
                  value: item.id,
                  label: `${item.id}${item.version ? ` · ${item.version}` : ''}`,
                })),
              ]}
            />
            <Select
              ariaLabel={t('identity_pools.session_filter')}
              value={sessionFilter}
              onChange={setSessionFilter}
              options={[
                { value: 'all', label: t('identity_pools.all_sessions') },
                { value: 'active', label: t('identity_pools.with_sessions') },
                { value: 'empty', label: t('identity_pools.without_sessions') },
              ]}
            />
          </div>
          <div className={styles.tableWrap}>
            <table className={styles.table}>
              <thead>
                <tr>
                  <th>{t('identity_pools.account')}</th>
                  <th>{t('identity_pools.identity')}</th>
                  <th>{t('identity_pools.environment')}</th>
                  <th>{t('identity_pools.session_count')}</th>
                  <th>{t('identity_pools.account_status')}</th>
                  <th>{t('identity_pools.actions')}</th>
                </tr>
              </thead>
              <tbody>
                {filteredAccounts.map((account) => {
                  const identity = account.identity_id || account.installation_id || '';
                  const isBound = Boolean(account.identity_bound);
                  return (
                    <tr
                      key={account.account_id}
                      onClick={() => openAccount(account)}
                      tabIndex={0}
                      onKeyDown={(event) => {
                        if (event.key === 'Enter') openAccount(account);
                      }}
                    >
                      <td>
                        <strong>{account.email || account.account_label || t('identity_pools.unnamed_account')}</strong>
                        <small>{account.auth_file || account.auth_index || account.account_id}</small>
                      </td>
                      <td>
                        <div className={styles.identityCell} title={identity || t('identity_pools.identity_not_created')}>
                          <code>{identity ? `${identity.slice(0, 8)}…${identity.slice(-6)}` : '—'}</code>
                          {identity && (
                            <button
                              type="button"
                              className={styles.iconButton}
                              aria-label={t('identity_pools.copy_identity')}
                              onClick={(event) => {
                                event.stopPropagation();
                                void copyIdentity(account);
                              }}
                            >
                              {copiedIdentity === account.account_id ? <IconCheck size={14} /> : <IconCopy size={14} />}
                            </button>
                          )}
                        </div>
                        <small>
                          {isBound
                            ? `${t('identity_pools.identity_version')} v${account.identity_version}`
                            : t('identity_pools.identity_not_created')}
                        </small>
                      </td>
                      <td>
                        <strong>{account.environment?.id || account.profile_id || t('identity_pools.unassigned')}</strong>
                        <small>
                          {[
                            account.environment?.version,
                            account.environment?.platform,
                            account.environment?.architecture,
                          ]
                            .filter(Boolean)
                            .join(' · ') || t('identity_pools.environment_unknown')}
                        </small>
                      </td>
                      <td>
                        <span className={styles.sessionCount}>{account.session_count ?? 0}</span>
                      </td>
                      <td>
                        <span className={`${styles.accountStatus} ${account.disabled ? styles.accountStatusDisabled : styles.accountStatusActive}`}>
                          {account.disabled
                            ? t('identity_pools.disabled')
                            : isBound
                              ? account.runtime_status || t('identity_pools.active')
                              : t('identity_pools.not_started')}
                        </span>
                      </td>
                      <td>
                        <div className={styles.rowActions}>
                          <Button
                            size="xs"
                            variant="secondary"
                            onClick={(event) => {
                              event.stopPropagation();
                              openAccount(account);
                            }}
                          >
                            {t('identity_pools.view_details')}
                          </Button>
                          <button
                            type="button"
                            className={styles.rowChevron}
                            aria-label={t('identity_pools.view_details')}
                            onClick={(event) => {
                              event.stopPropagation();
                              openAccount(account);
                            }}
                          >
                            <IconChevronRight size={16} />
                          </button>
                        </div>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
            {filteredAccounts.length === 0 && <div className={styles.empty}>{t('identity_pools.no_accounts')}</div>}
          </div>
        </section>
      ) : tab === 'environments' ? (
        <section className={styles.workspace}>
          <div className={styles.sectionHeading}>
            <div>
              <h2>{t('identity_pools.environment_management_title')}</h2>
              <p>{t('identity_pools.environment_management_note')}</p>
            </div>
          </div>
          <div className={styles.environmentList}>
            {catalogValidation && (
              <article className={styles.environmentRow}>
                <div className={styles.environmentMain}>
                  <div className={styles.environmentTitleLine}>
                    <strong>Environment catalog integrity</strong>
                    <span className={catalogValidation.valid ? styles.accountStatusActive : styles.accountStatusDisabled}>
                      {catalogValidation.evidence_complete
                        ? '100% request evidence'
                        : `${Math.round(catalogValidation.evidence_coverage * 100)}% request evidence`}
                    </span>
                  </div>
                  <p>{`100-entry catalog · ${catalogValidation.observed} request-observed · ${catalogValidation.artifact_verified} artifact-only · evidence gap ${catalogValidation.evidence_gap} · ${catalogValidation.routable} active`}</p>
                  <small>{`Latest client versions: ${(catalogValidation.latest_versions ?? []).slice(0, 8).join(', ') || '—'}`}</small>
                </div>
              </article>
            )}
            {environments.map((environment) => (
              <article key={environment.id} className={styles.environmentRow}>
                <div className={styles.environmentMain}>
                  <div className={styles.environmentTitleLine}>
                    <strong>{environment.id}</strong>
                    <span className={environment.routable ? styles.accountStatusActive : styles.accountStatusDisabled}>
                      {environment.routable
                        ? t('identity_pools.enabled')
                        : environment.evidence_status === 'artifact_verified'
                          ? 'Artifact verified · request capture pending'
                          : environment.observed
                            ? t('identity_pools.disabled')
                            : 'Pending capture'}
                    </span>
                  </div>
                  <p>
                    {[environment.version, environment.platform, environment.architecture, environment.terminal, environment.client_mode, environment.source, environment.evidence_status]
                      .filter(Boolean)
                      .join(' · ') || t('identity_pools.environment_unknown')}
                  </p>
                  <small>{environment.user_agent || t('identity_pools.user_agent_unknown')}</small>
                </div>
                <div className={styles.environmentCounts}>
                  <strong>{environment.accountCount}</strong>
                  <span>{t('identity_pools.bound_accounts')}</span>
                  <strong>{environment.sessionCount}</strong>
                  <span>{t('identity_pools.sessions')}</span>
                </div>
                <div className={styles.environmentAccounts}>
                  {environment.accounts.length === 0 ? (
                    <span className={styles.muted}>{t('identity_pools.no_bound_accounts')}</span>
                  ) : (
                    environment.accounts.slice(0, 4).map((account) => (
                      <button key={account.account_id} type="button" onClick={() => openAccount(account)}>
                        {account.email || account.account_label || account.auth_file || account.account_id}
                      </button>
                    ))
                  )}
                  {environment.accounts.length > 4 && (
                    <span className={styles.muted}>
                      {t('identity_pools.more_accounts', { count: environment.accounts.length - 4 })}
                    </span>
                  )}
                </div>
                <div className={styles.environmentActions}>
                  <Button
                    size="xs"
                    variant="secondary"
                    onClick={() => {
                      setTab('accounts');
                      setEnvironmentFilter(environment.id);
                    }}
                  >
                    {t('identity_pools.view_accounts')}
                  </Button>
                  <Button
                    size="xs"
                    variant={environment.enabled ? 'secondary' : 'primary'}
                    disabled={!environment.eligible}
                    onClick={() =>
                      void (async () => {
                        try {
                          await identityPoolsApi.patchProfile(
                            POOL_ID,
                            environment.id,
                            { enabled: !environment.enabled },
                            scope
                          );
                          await load();
                        } catch (error) {
                          notify(
                            error instanceof Error
                              ? error.message
                              : t('identity_pools.profile_update_failed'),
                            'error'
                          );
                        }
                      })()
                    }
                  >
                    {environment.enabled ? t('identity_pools.disable') : t('identity_pools.enable')}
                  </Button>
                </div>
              </article>
            ))}
          </div>
          {environments.length === 0 && <div className={styles.empty}>{t('identity_pools.no_environments')}</div>}
        </section>
      ) : (
        <section className={styles.workspace}>
          <div className={styles.sectionHeading}>
            <div>
              <h2>Request validation</h2>
              <p>Outbound summaries only; credentials and request bodies are never shown.</p>
            </div>
            <span className={styles.resultCount}>{`${validation.valid ?? 0}/${validation.total ?? 0} valid · ${validation.exact_match ?? 0} exact`}</span>
          </div>
          <div className={styles.tableWrap}>
            <table className={styles.table}>
              <thead><tr><th>ID</th><th>Mode</th><th>Environment</th><th>Evidence</th><th>Score</th><th>Status</th><th>Issues</th></tr></thead>
              <tbody>
                {(validation.records ?? []).map((record) => (
                  <tr key={record.id}>
                    <td>{record.id}</td>
                    <td>{record.mode}</td>
                    <td>
                      <strong>{record.profile_id || '—'}</strong>
                      <small>{[record.version, record.platform, record.architecture].filter(Boolean).join(' · ') || '—'}</small>
                    </td>
                    <td>
                      <strong>{record.evidence_status || 'unknown'}</strong>
                      <small title={record.request_hash}>{record.request_hash ? `${record.request_hash.slice(0, 19)}…` : '—'}</small>
                    </td>
                    <td>{record.score}</td>
                    <td className={record.valid ? styles.accountStatusActive : styles.accountStatusDisabled}>
                      {record.valid && record.exact_match ? 'exact' : record.valid ? 'valid' : '异常'}
                    </td>
                    <td>{(record.issue_codes ?? []).join(', ') || '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
            {(validation.records ?? []).length === 0 && <div className={styles.empty}>No validation records</div>}
          </div>
          <p className={styles.muted}>{`Dynamic fields valid: ${validation.dynamic_fields_valid ?? 0}/${validation.total ?? 0} · Proxy exposure detections: ${validation.proxy_exposure ?? 0}`}</p>
        </section>
      )}

      <Drawer
        open={Boolean(selectedAccount)}
        onClose={() => setSelectedAccount(null)}
        title={
          selectedAccount
            ? selectedAccount.email || selectedAccount.account_label || t('identity_pools.account_details')
            : undefined
        }
        width={620}
      >
        {selectedAccount && (
          <div className={styles.drawerBody}>
            <div className={styles.drawerActions}>
              <span className={`${styles.accountStatus} ${selectedAccount.identity_bound ? styles.accountStatusActive : styles.accountStatusDisabled}`}>
                {selectedAccount.identity_bound ? t('identity_pools.identity_bound') : t('identity_pools.identity_not_created')}
              </span>
              <Button
                size="sm"
                variant="danger"
                disabled={!selectedAccount.identity_bound}
                title={!selectedAccount.identity_bound ? t('identity_pools.rotate_requires_bound') : undefined}
                onClick={() => void rotate()}
              >
                {t('identity_pools.rotate_identity')}
              </Button>
            </div>
            <div className={styles.detailGrid}>
              <div className={styles.detailWide}>
                <span>{t('identity_pools.identity')}</span>
                <code>{selectedAccount.identity_id || selectedAccount.installation_id || '—'}</code>
              </div>
              <div>
                <span>{t('identity_pools.environment')}</span>
                <Select
                  ariaLabel={t('identity_pools.environment')}
                  value={selectedAccount.profile_id || ''}
                  onChange={(value) => void assignEnvironment(value)}
                  options={[
                    { value: '', label: t('identity_pools.unassigned') },
                    ...(pool?.profiles ?? [])
                      .filter((profile) => profile.enabled && profile.eligible)
                      .map((profile) => ({
                        value: profile.id,
                        label: `${profile.id}${profile.version ? ` · ${profile.version}` : ''}`,
                      })),
                  ]}
                />
                <small>
                  {[selectedAccount.environment?.version, selectedAccount.environment?.platform, selectedAccount.environment?.architecture]
                    .filter(Boolean)
                    .join(' · ') || t('identity_pools.environment_unknown')}
                </small>
              </div>
              <div>
                <span>{t('identity_pools.auth_file')}</span>
                <strong>{selectedAccount.auth_file || selectedAccount.auth_index || '—'}</strong>
              </div>
              <div>
                <span>{t('identity_pools.identity_version')}</span>
                <strong>{selectedAccount.identity_bound ? `v${selectedAccount.identity_version}` : '—'}</strong>
              </div>
              <div>
                <span>{t('identity_pools.status_label')}</span>
                <strong>
                  {selectedAccount.disabled
                    ? t('identity_pools.disabled')
                    : selectedAccount.runtime_status || t('identity_pools.not_started')}
                </strong>
              </div>
            </div>
            <div className={styles.drawerSection}>
              <div className={styles.sectionHeading}>
                <div>
                  <h2>{t('identity_pools.sessions')}</h2>
                  <p>{t('identity_pools.session_mapping_note')}</p>
                </div>
                <span className={styles.sessionSummary}>{t('identity_pools.session_count_value', { count: selectedAccount.session_count ?? 0 })}</span>
              </div>
              {sessionsLoading ? (
                <p>{t('identity_pools.loading_sessions')}</p>
              ) : sessionError ? (
                <p role="alert" className={styles.error}>{sessionError}</p>
              ) : sessions.length === 0 ? (
                <p>{t('identity_pools.no_active_sessions')}</p>
              ) : (
                <div className={styles.sessionList}>
                  {sessions.map((session) => (
                    <div className={styles.sessionRow} key={`${session.account_id}:${session.logical_session_id}`}>
                      <div>
                        <strong>{session.logical_session_id}</strong>
                        <small>{[session.thread_id, session.window_id].filter(Boolean).join(' · ')}</small>
                        <code>{session.prompt_cache_key || t('identity_pools.no_cache_key')}</code>
                      </div>
                      <Button
                        size="xs"
                        variant="ghost"
                        iconOnly
                        aria-label={t('identity_pools.clear')}
                        onClick={() => void removeSession(session)}
                      >
                        <IconTrash2 size={15} />
                      </Button>
                    </div>
                  ))}
                </div>
              )}
            </div>
          </div>
        )}
      </Drawer>
    </main>
  );
}
