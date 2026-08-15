import { useCallback, useEffect, useMemo, useState, type CSSProperties } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import { Modal } from '@/components/ui/Modal';
import { SegmentedTabs } from '@/components/ui/SegmentedTabs';
import { Select } from '@/components/ui/Select';
import {
  IconCheck,
  IconKey,
  IconPencil,
  IconPlus,
  IconRefreshCw,
  IconSearch,
  IconTrash2,
} from '@/components/ui/icons';
import {
  accountGroupsApi,
  apiKeysApi,
  authFilesApi,
  type AccountGroup,
  type AccountGroupInput,
  type APIKeyGroupPolicy,
} from '@/services/api';
import { useAuthStore, useNotificationStore } from '@/stores';
import type { AuthFileItem } from '@/types';
import { sha256Hex } from '@/utils/apiKeyHash';
import { maskApiKey } from '@/utils/format';
import { AccountGroupBadges, AccountGroupPicker } from './AccountGroupControls';
import {
  getAuthFileGroupIds,
  isRuntimeOnlyAuthFile,
  normalizeAccountGroupColor,
} from './accountGroupModel';
import styles from './AccountGroupsPage.module.scss';

type WorkspaceTab = 'accounts' | 'api-keys';

type GroupDraft = {
  id: number | null;
  name: string;
  description: string;
  color: string;
  sortOrder: string;
};

const EMPTY_GROUP_DRAFT: GroupDraft = {
  id: null,
  name: '',
  description: '',
  color: '#14b8a6',
  sortOrder: '0',
};

const PAGE_SIZE = 40;

const getErrorMessage = (error: unknown): string =>
  error instanceof Error ? error.message : String(error);

const getAuthFileRowKey = (file: AuthFileItem): string => {
  const runtimeId = String(file.id ?? '').trim();
  const authIndex = String(file.authIndex ?? '').trim();
  return runtimeId || `${file.name}::${authIndex}`;
};

const getAuthFileSubtitle = (file: AuthFileItem): string => {
  const provider = String(file.provider ?? file.type ?? '').trim();
  const authIndex = String(file.authIndex ?? '').trim();
  return [provider, authIndex ? `auth_index ${authIndex}` : ''].filter(Boolean).join(' · ');
};

export function AccountGroupsPage() {
  const { t } = useTranslation();
  const connectionStatus = useAuthStore((state) => state.connectionStatus);
  const showNotification = useNotificationStore((state) => state.showNotification);
  const showConfirmation = useNotificationStore((state) => state.showConfirmation);

  const [groups, setGroups] = useState<AccountGroup[]>([]);
  const [files, setFiles] = useState<AuthFileItem[]>([]);
  const [apiKeys, setApiKeys] = useState<string[]>([]);
  const [policies, setPolicies] = useState<APIKeyGroupPolicy[]>([]);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [loadError, setLoadError] = useState('');
  const [activeTab, setActiveTab] = useState<WorkspaceTab>('accounts');
  const [groupModalOpen, setGroupModalOpen] = useState(false);
  const [groupDraft, setGroupDraft] = useState<GroupDraft>(EMPTY_GROUP_DRAFT);
  const [groupSaving, setGroupSaving] = useState(false);
  const [groupFormError, setGroupFormError] = useState('');
  const [accountSearch, setAccountSearch] = useState('');
  const [accountGroupFilter, setAccountGroupFilter] = useState('all');
  const [accountPage, setAccountPage] = useState(1);
  const [selectedAccountKeys, setSelectedAccountKeys] = useState<Set<string>>(() => new Set());
  const [membershipModalOpen, setMembershipModalOpen] = useState(false);
  const [membershipTargets, setMembershipTargets] = useState<AuthFileItem[]>([]);
  const [membershipIds, setMembershipIds] = useState<number[]>([]);
  const [membershipSaving, setMembershipSaving] = useState(false);
  const [keySearch, setKeySearch] = useState('');
  const [policyModalOpen, setPolicyModalOpen] = useState(false);
  const [policyApiKey, setPolicyApiKey] = useState('');
  const [policyRestricted, setPolicyRestricted] = useState(false);
  const [policyGroupIds, setPolicyGroupIds] = useState<number[]>([]);
  const [policySaving, setPolicySaving] = useState(false);
  const [policyFormError, setPolicyFormError] = useState('');

  const loadData = useCallback(async (background = false) => {
    if (background) setRefreshing(true);
    else setLoading(true);
    setLoadError('');
    try {
      const [nextGroups, authResponse, nextApiKeys, nextPolicies] = await Promise.all([
        accountGroupsApi.list(),
        authFilesApi.listForGrouping(),
        apiKeysApi.list(),
        accountGroupsApi.listAPIKeyPolicies(),
      ]);
      setGroups(nextGroups);
      setFiles(authResponse.files ?? []);
      setApiKeys(nextApiKeys);
      setPolicies(nextPolicies);
    } catch (error) {
      setLoadError(getErrorMessage(error));
    } finally {
      setLoading(false);
      setRefreshing(false);
    }
  }, []);

  useEffect(() => {
    void loadData();
  }, [loadData]);

  const policiesByHash = useMemo(
    () => new Map(policies.map((policy) => [policy.api_key_hash.toLowerCase(), policy])),
    [policies]
  );

  const groupedAccountCount = useMemo(
    () => files.filter((file) => getAuthFileGroupIds(file).length > 0).length,
    [files]
  );
  const restrictedKeyCount = useMemo(
    () => apiKeys.filter((apiKey) => policiesByHash.has(sha256Hex(apiKey).toLowerCase())).length,
    [apiKeys, policiesByHash]
  );

  const filteredFiles = useMemo(() => {
    const query = accountSearch.trim().toLowerCase();
    return files.filter((file) => {
      const ids = getAuthFileGroupIds(file);
      if (accountGroupFilter === 'ungrouped' && ids.length > 0) return false;
      if (accountGroupFilter !== 'all' && accountGroupFilter !== 'ungrouped') {
        const groupId = Number(accountGroupFilter);
        if (!ids.includes(groupId)) return false;
      }
      if (!query) return true;
      return [file.name, file.id, file.provider, file.type, file.authIndex]
        .map((value) => String(value ?? '').toLowerCase())
        .some((value) => value.includes(query));
    });
  }, [accountGroupFilter, accountSearch, files]);

  const totalAccountPages = Math.max(1, Math.ceil(filteredFiles.length / PAGE_SIZE));
  const currentAccountPage = Math.min(accountPage, totalAccountPages);
  const pageFiles = useMemo(
    () => filteredFiles.slice((currentAccountPage - 1) * PAGE_SIZE, currentAccountPage * PAGE_SIZE),
    [currentAccountPage, filteredFiles]
  );

  useEffect(() => {
    setAccountPage(1);
  }, [accountGroupFilter, accountSearch]);

  useEffect(() => {
    const validKeys = new Set(files.map(getAuthFileRowKey));
    setSelectedAccountKeys(
      (current) => new Set(Array.from(current).filter((key) => validKeys.has(key)))
    );
  }, [files]);

  const selectedAccounts = useMemo(
    () => files.filter((file) => selectedAccountKeys.has(getAuthFileRowKey(file))),
    [files, selectedAccountKeys]
  );

  const filteredApiKeys = useMemo(() => {
    const query = keySearch.trim().toLowerCase();
    if (!query) return apiKeys;
    return apiKeys.filter((apiKey) => {
      const hash = sha256Hex(apiKey).toLowerCase();
      return apiKey.toLowerCase().includes(query) || hash.includes(query);
    });
  }, [apiKeys, keySearch]);

  const openCreateGroup = () => {
    setGroupDraft(EMPTY_GROUP_DRAFT);
    setGroupFormError('');
    setGroupModalOpen(true);
  };

  const openEditGroup = (group: AccountGroup) => {
    setGroupDraft({
      id: group.id,
      name: group.name,
      description: group.description ?? '',
      color: normalizeAccountGroupColor(group.color),
      sortOrder: String(group.sort_order ?? 0),
    });
    setGroupFormError('');
    setGroupModalOpen(true);
  };

  const saveGroup = async () => {
    const name = groupDraft.name.trim();
    if (!name) {
      setGroupFormError(t('account_groups.name_required'));
      return;
    }
    const sortOrder = Number(groupDraft.sortOrder || 0);
    if (!Number.isInteger(sortOrder)) {
      setGroupFormError(t('account_groups.sort_order_invalid'));
      return;
    }
    const input: AccountGroupInput = {
      name,
      description: groupDraft.description.trim(),
      color: normalizeAccountGroupColor(groupDraft.color),
      sort_order: sortOrder,
    };
    setGroupSaving(true);
    setGroupFormError('');
    try {
      if (groupDraft.id === null) await accountGroupsApi.create(input);
      else await accountGroupsApi.update(groupDraft.id, input);
      setGroupModalOpen(false);
      showNotification(
        t(
          groupDraft.id === null ? 'account_groups.create_success' : 'account_groups.update_success'
        ),
        'success'
      );
      await loadData(true);
    } catch (error) {
      setGroupFormError(getErrorMessage(error));
    } finally {
      setGroupSaving(false);
    }
  };

  const deleteGroup = (group: AccountGroup) => {
    const force = group.member_count > 0;
    showConfirmation({
      title: t('account_groups.delete_title'),
      message: (
        <div className={styles.confirmCopy}>
          <p>{t('account_groups.delete_confirm', { name: group.name })}</p>
          {group.member_count > 0 || group.api_key_count > 0 ? (
            <p>
              {t('account_groups.delete_impact', {
                accounts: group.member_count,
                keys: group.api_key_count,
              })}
            </p>
          ) : null}
        </div>
      ),
      confirmText: t('common.delete'),
      variant: 'danger',
      onConfirm: async () => {
        try {
          await accountGroupsApi.delete(group.id, force);
          showNotification(t('account_groups.delete_success'), 'success');
          await loadData(true);
        } catch (error) {
          showNotification(getErrorMessage(error), 'error');
        }
      },
    });
  };

  const toggleAccountSelection = (file: AuthFileItem) => {
    const key = getAuthFileRowKey(file);
    setSelectedAccountKeys((current) => {
      const next = new Set(current);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  };

  const openMembershipEditor = (targets: AuthFileItem[], initialIds: number[] = []) => {
    if (targets.length === 0) return;
    setMembershipTargets(targets);
    setMembershipIds(initialIds);
    setMembershipModalOpen(true);
  };

  const saveMemberships = async () => {
    const editableTargets = membershipTargets.filter((file) => !isRuntimeOnlyAuthFile(file));
    if (editableTargets.length === 0) return;
    setMembershipSaving(true);
    try {
      await accountGroupsApi.updateMemberships(
        editableTargets.map((file) => ({
          name: String(file.id ?? '').trim() || file.name,
          auth_index: String(file.authIndex ?? '').trim(),
          group_ids: membershipIds,
        }))
      );
      setMembershipModalOpen(false);
      setSelectedAccountKeys(new Set());
      showNotification(
        t('account_groups.membership_success', { count: editableTargets.length }),
        'success'
      );
      await loadData(true);
    } catch (error) {
      showNotification(getErrorMessage(error), 'error');
    } finally {
      setMembershipSaving(false);
    }
  };

  const openPolicyEditor = (apiKey: string) => {
    const hash = sha256Hex(apiKey).toLowerCase();
    const policy = policiesByHash.get(hash);
    setPolicyApiKey(apiKey);
    setPolicyRestricted(Boolean(policy));
    setPolicyGroupIds(policy?.allowed_group_ids ?? []);
    setPolicyFormError('');
    setPolicyModalOpen(true);
  };

  const savePolicy = async () => {
    const hash = sha256Hex(policyApiKey).toLowerCase();
    if (!hash) return;
    if (policyRestricted && policyGroupIds.length === 0) {
      setPolicyFormError(t('account_groups.policy_group_required'));
      return;
    }
    setPolicySaving(true);
    setPolicyFormError('');
    try {
      if (policyRestricted) {
        const next = await accountGroupsApi.updateAPIKeyPolicies([
          { api_key_hash: hash, allowed_group_ids: policyGroupIds },
        ]);
        setPolicies(next);
      } else {
        await accountGroupsApi.deleteAPIKeyPolicy(hash);
        setPolicies((current) => current.filter((policy) => policy.api_key_hash !== hash));
      }
      setPolicyModalOpen(false);
      showNotification(t('account_groups.policy_success'), 'success');
      await loadData(true);
    } catch (error) {
      setPolicyFormError(getErrorMessage(error));
    } finally {
      setPolicySaving(false);
    }
  };

  if (loading) return <LoadingSpinner />;

  return (
    <div className={styles.page}>
      <section className={styles.hero}>
        <div>
          <span className={styles.eyebrow}>{t('account_groups.eyebrow')}</span>
          <h1>{t('account_groups.title')}</h1>
          <p>{t('account_groups.description')}</p>
        </div>
        <div className={styles.heroActions}>
          <Button
            variant="secondary"
            size="sm"
            onClick={() => void loadData(true)}
            loading={refreshing}
          >
            {!refreshing ? <IconRefreshCw size={15} /> : null}
            {t('common.refresh')}
          </Button>
          <Button size="sm" onClick={openCreateGroup} disabled={connectionStatus !== 'connected'}>
            <IconPlus size={15} />
            {t('account_groups.create')}
          </Button>
        </div>
      </section>

      {loadError ? (
        <section className={styles.errorPanel}>
          <strong>{t('account_groups.load_failed')}</strong>
          <span>{loadError}</span>
          <Button size="sm" variant="secondary" onClick={() => void loadData()}>
            {t('common.retry')}
          </Button>
        </section>
      ) : null}

      <section className={styles.metrics}>
        <div className={styles.metricCard}>
          <span>{t('account_groups.metric_groups')}</span>
          <strong>{groups.length}</strong>
          <small>{t('account_groups.metric_groups_hint')}</small>
        </div>
        <div className={styles.metricCard}>
          <span>{t('account_groups.metric_grouped_accounts')}</span>
          <strong>{groupedAccountCount}</strong>
          <small>{t('account_groups.metric_accounts_total', { count: files.length })}</small>
        </div>
        <div className={styles.metricCard}>
          <span>{t('account_groups.metric_restricted_keys')}</span>
          <strong>{restrictedKeyCount}</strong>
          <small>{t('account_groups.metric_keys_total', { count: apiKeys.length })}</small>
        </div>
        <div className={styles.metricCard}>
          <span>{t('account_groups.metric_ungrouped')}</span>
          <strong>{Math.max(0, files.length - groupedAccountCount)}</strong>
          <small>{t('account_groups.metric_ungrouped_hint')}</small>
        </div>
      </section>

      <section className={styles.panel}>
        <div className={styles.panelHeader}>
          <div>
            <h2>{t('account_groups.group_list_title')}</h2>
            <p>{t('account_groups.group_list_hint')}</p>
          </div>
          <span className={styles.countPill}>{groups.length}/64</span>
        </div>
        {groups.length === 0 ? (
          <div className={styles.emptyState}>
            <strong>{t('account_groups.empty_title')}</strong>
            <span>{t('account_groups.empty_hint')}</span>
            <Button size="sm" onClick={openCreateGroup}>
              <IconPlus size={15} />
              {t('account_groups.create')}
            </Button>
          </div>
        ) : (
          <div className={styles.groupGrid}>
            {groups.map((group) => {
              const color = normalizeAccountGroupColor(group.color);
              return (
                <article
                  key={group.id}
                  className={styles.groupCard}
                  style={{ '--group-color': color } as CSSProperties}
                >
                  <div className={styles.groupCardTopline}>
                    <span className={styles.groupColor} />
                    <span className={styles.groupOrder}>#{group.sort_order}</span>
                  </div>
                  <div className={styles.groupCopy}>
                    <h3>{group.name}</h3>
                    <p>{group.description || t('account_groups.no_description')}</p>
                  </div>
                  <div className={styles.groupStats}>
                    <span>
                      <strong>{group.member_count}</strong>
                      {t('account_groups.accounts_unit')}
                    </span>
                    <span>
                      <strong>{group.api_key_count}</strong>
                      {t('account_groups.keys_unit')}
                    </span>
                  </div>
                  <div className={styles.groupActions}>
                    <Button size="xs" variant="secondary" onClick={() => openEditGroup(group)}>
                      <IconPencil size={13} />
                      {t('common.edit')}
                    </Button>
                    <Button size="xs" variant="danger" onClick={() => deleteGroup(group)}>
                      <IconTrash2 size={13} />
                      {t('common.delete')}
                    </Button>
                  </div>
                </article>
              );
            })}
          </div>
        )}
      </section>

      <section className={styles.workspace}>
        <div className={styles.workspaceHeader}>
          <div>
            <h2>{t('account_groups.assignment_title')}</h2>
            <p>{t('account_groups.assignment_hint')}</p>
          </div>
          <SegmentedTabs
            items={[
              { id: 'accounts', label: t('account_groups.accounts_tab') },
              { id: 'api-keys', label: t('account_groups.keys_tab') },
            ]}
            activeTab={activeTab}
            onChange={setActiveTab}
            ariaLabel={t('account_groups.assignment_title')}
          />
        </div>

        {activeTab === 'accounts' ? (
          <div className={styles.workspaceBody}>
            <div className={styles.toolbar}>
              <Input
                value={accountSearch}
                onChange={(event) => setAccountSearch(event.target.value)}
                placeholder={t('account_groups.account_search')}
                rightElement={<IconSearch size={15} />}
              />
              <Select
                value={accountGroupFilter}
                onChange={setAccountGroupFilter}
                options={[
                  { value: 'all', label: t('account_groups.filter_all') },
                  { value: 'ungrouped', label: t('account_groups.ungrouped') },
                  ...groups.map((group) => ({ value: String(group.id), label: group.name })),
                ]}
                ariaLabel={t('account_groups.filter_label')}
              />
              <Button
                size="sm"
                variant="secondary"
                disabled={selectedAccounts.length === 0}
                onClick={() => openMembershipEditor(selectedAccounts, [])}
              >
                <IconCheck size={14} />
                {t('account_groups.batch_assign', { count: selectedAccounts.length })}
              </Button>
            </div>
            <div className={styles.tableWrap}>
              <table className={styles.table}>
                <thead>
                  <tr>
                    <th className={styles.checkboxCell} />
                    <th>{t('account_groups.account_column')}</th>
                    <th>{t('account_groups.groups_column')}</th>
                    <th>{t('common.action')}</th>
                  </tr>
                </thead>
                <tbody>
                  {pageFiles.map((file) => {
                    const rowKey = getAuthFileRowKey(file);
                    const groupIds = getAuthFileGroupIds(file);
                    return (
                      <tr key={rowKey}>
                        <td className={styles.checkboxCell}>
                          <input
                            type="checkbox"
                            checked={selectedAccountKeys.has(rowKey)}
                            onChange={() => toggleAccountSelection(file)}
                            disabled={isRuntimeOnlyAuthFile(file)}
                            aria-label={file.name}
                          />
                        </td>
                        <td>
                          <div className={styles.primaryCell}>
                            <strong>{file.name}</strong>
                            <span>{getAuthFileSubtitle(file) || t('common.not_set')}</span>
                          </div>
                        </td>
                        <td>
                          <AccountGroupBadges ids={groupIds} groups={groups} showEmpty />
                        </td>
                        <td>
                          <Button
                            size="xs"
                            variant="secondary"
                            disabled={isRuntimeOnlyAuthFile(file)}
                            onClick={() => openMembershipEditor([file], groupIds)}
                          >
                            {t('account_groups.edit_membership')}
                          </Button>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
              {pageFiles.length === 0 ? (
                <div className={styles.emptyRows}>{t('account_groups.no_matching_accounts')}</div>
              ) : null}
            </div>
            <div className={styles.pagination}>
              <span>
                {t('account_groups.pagination', {
                  page: currentAccountPage,
                  pages: totalAccountPages,
                  count: filteredFiles.length,
                })}
              </span>
              <div>
                <Button
                  size="xs"
                  variant="secondary"
                  disabled={currentAccountPage <= 1}
                  onClick={() => setAccountPage((page) => Math.max(1, page - 1))}
                >
                  {t('common.previous')}
                </Button>
                <Button
                  size="xs"
                  variant="secondary"
                  disabled={currentAccountPage >= totalAccountPages}
                  onClick={() => setAccountPage((page) => Math.min(totalAccountPages, page + 1))}
                >
                  {t('common.next')}
                </Button>
              </div>
            </div>
          </div>
        ) : (
          <div className={styles.workspaceBody}>
            <div className={styles.toolbar}>
              <Input
                value={keySearch}
                onChange={(event) => setKeySearch(event.target.value)}
                placeholder={t('account_groups.key_search')}
                rightElement={<IconSearch size={15} />}
              />
              <span className={styles.policyHint}>{t('account_groups.policy_semantics')}</span>
            </div>
            <div className={styles.keyList}>
              {filteredApiKeys.map((apiKey) => {
                const hash = sha256Hex(apiKey).toLowerCase();
                const policy = policiesByHash.get(hash);
                return (
                  <article key={hash} className={styles.keyRow}>
                    <span className={styles.keyIcon}>
                      <IconKey size={18} />
                    </span>
                    <div className={styles.keyCopy}>
                      <strong>{maskApiKey(apiKey)}</strong>
                      <span>{hash.slice(0, 16)}…</span>
                    </div>
                    <div className={styles.keyPolicy}>
                      {policy ? (
                        <AccountGroupBadges
                          ids={policy.allowed_group_ids}
                          groups={groups}
                          maxVisible={4}
                        />
                      ) : (
                        <span className={styles.unrestrictedBadge}>
                          {t('account_groups.unrestricted')}
                        </span>
                      )}
                    </div>
                    <Button size="xs" variant="secondary" onClick={() => openPolicyEditor(apiKey)}>
                      {t('account_groups.edit_policy')}
                    </Button>
                  </article>
                );
              })}
              {filteredApiKeys.length === 0 ? (
                <div className={styles.emptyRows}>{t('account_groups.no_matching_keys')}</div>
              ) : null}
            </div>
          </div>
        )}
      </section>

      <Modal
        open={groupModalOpen}
        onClose={() => setGroupModalOpen(false)}
        title={
          groupDraft.id === null ? t('account_groups.create_title') : t('account_groups.edit_title')
        }
        closeDisabled={groupSaving}
        footer={
          <>
            <Button
              variant="secondary"
              onClick={() => setGroupModalOpen(false)}
              disabled={groupSaving}
            >
              {t('common.cancel')}
            </Button>
            <Button onClick={() => void saveGroup()} loading={groupSaving}>
              {t('common.save')}
            </Button>
          </>
        }
      >
        <div className={styles.formGrid}>
          <Input
            label={t('account_groups.name_label')}
            value={groupDraft.name}
            maxLength={80}
            onChange={(event) => setGroupDraft((draft) => ({ ...draft, name: event.target.value }))}
          />
          <div className="form-group">
            <label htmlFor="account-group-description">
              {t('account_groups.description_label')}
            </label>
            <textarea
              id="account-group-description"
              className="input"
              rows={3}
              maxLength={240}
              value={groupDraft.description}
              onChange={(event) =>
                setGroupDraft((draft) => ({ ...draft, description: event.target.value }))
              }
            />
          </div>
          <div className={styles.formSplit}>
            <div className="form-group">
              <label htmlFor="account-group-color">{t('account_groups.color_label')}</label>
              <div className={styles.colorInputRow}>
                <input
                  id="account-group-color"
                  type="color"
                  value={normalizeAccountGroupColor(groupDraft.color)}
                  onChange={(event) =>
                    setGroupDraft((draft) => ({ ...draft, color: event.target.value }))
                  }
                />
                <input
                  className="input"
                  value={groupDraft.color}
                  onChange={(event) =>
                    setGroupDraft((draft) => ({ ...draft, color: event.target.value }))
                  }
                />
              </div>
            </div>
            <Input
              label={t('account_groups.sort_order_label')}
              type="number"
              value={groupDraft.sortOrder}
              onChange={(event) =>
                setGroupDraft((draft) => ({ ...draft, sortOrder: event.target.value }))
              }
            />
          </div>
          {groupFormError ? <div className="error-box">{groupFormError}</div> : null}
        </div>
      </Modal>

      <Modal
        open={membershipModalOpen}
        onClose={() => setMembershipModalOpen(false)}
        title={t('account_groups.membership_modal_title', { count: membershipTargets.length })}
        width={720}
        closeDisabled={membershipSaving}
        footer={
          <>
            <Button
              variant="secondary"
              onClick={() => setMembershipModalOpen(false)}
              disabled={membershipSaving}
            >
              {t('common.cancel')}
            </Button>
            <Button onClick={() => void saveMemberships()} loading={membershipSaving}>
              {t('account_groups.apply_membership')}
            </Button>
          </>
        }
      >
        <p className={styles.modalHint}>{t('account_groups.membership_replace_hint')}</p>
        <AccountGroupPicker groups={groups} value={membershipIds} onChange={setMembershipIds} />
      </Modal>

      <Modal
        open={policyModalOpen}
        onClose={() => setPolicyModalOpen(false)}
        title={t('account_groups.policy_modal_title')}
        width={720}
        closeDisabled={policySaving}
        footer={
          <>
            <Button
              variant="secondary"
              onClick={() => setPolicyModalOpen(false)}
              disabled={policySaving}
            >
              {t('common.cancel')}
            </Button>
            <Button onClick={() => void savePolicy()} loading={policySaving}>
              {t('common.save')}
            </Button>
          </>
        }
      >
        <div className={styles.policyModeGrid}>
          <button
            type="button"
            className={!policyRestricted ? styles.policyModeActive : ''}
            onClick={() => setPolicyRestricted(false)}
          >
            <strong>{t('account_groups.unrestricted')}</strong>
            <span>{t('account_groups.unrestricted_hint')}</span>
          </button>
          <button
            type="button"
            className={policyRestricted ? styles.policyModeActive : ''}
            onClick={() => setPolicyRestricted(true)}
          >
            <strong>{t('account_groups.restricted')}</strong>
            <span>{t('account_groups.restricted_hint')}</span>
          </button>
        </div>
        {policyRestricted ? (
          <AccountGroupPicker
            groups={groups}
            value={policyGroupIds}
            onChange={setPolicyGroupIds}
            disabled={policySaving}
          />
        ) : null}
        {policyFormError ? <div className="error-box">{policyFormError}</div> : null}
      </Modal>
    </div>
  );
}
