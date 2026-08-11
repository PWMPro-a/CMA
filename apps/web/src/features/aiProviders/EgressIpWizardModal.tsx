import { useCallback, useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import { Modal } from '@/components/ui/Modal';
import { SelectionCheckbox } from '@/components/ui/SelectionCheckbox';
import { PROVIDER_KIND_LABELS, type ProviderKind } from '@/components/providers';
import {
  authFilesApi,
  containerOpsApi,
  providersApi,
  type ContainerOpsEgressIPInventory,
  type ContainerOpsSourceIPResult,
} from '@/services/api';
import type {
  ApiKeyEntry,
  GeminiKeyConfig,
  OpenAIProviderConfig,
  ProviderKeyConfig,
} from '@/types';
import type { AuthFileItem } from '@/types/authFile';
import { getAuthFilePatchTarget } from '@/features/authFiles/model/authFilesPageModel';
import { maskApiKey } from '@/utils/format';
import styles from './EgressIpWizardModal.module.scss';

type KeyProviderKind = Exclude<ProviderKind, 'openai'>;

type BindingTarget =
  | {
      id: string;
      source: 'provider';
      kind: KeyProviderKind;
      index: number;
      title: string;
      subtitle: string;
      currentSourceIp: string;
    }
  | {
      id: string;
      source: 'openai-key';
      providerIndex: number;
      entryIndex: number;
      title: string;
      subtitle: string;
      currentSourceIp: string;
    }
  | {
      id: string;
      source: 'auth-file';
      name: string;
      authIndex?: string;
      authFile: AuthFileItem;
      title: string;
      subtitle: string;
      currentSourceIp: string;
    };

interface EgressIpWizardModalProps {
  open: boolean;
  disabled?: boolean;
  geminiKeys: GeminiKeyConfig[];
  interactionsKeys: GeminiKeyConfig[];
  codexConfigs: ProviderKeyConfig[];
  xaiConfigs: ProviderKeyConfig[];
  claudeConfigs: ProviderKeyConfig[];
  vertexConfigs: ProviderKeyConfig[];
  openaiProviders: OpenAIProviderConfig[];
  onClose: () => void;
  onApplied: () => Promise<void> | void;
}

const IPV4_PATTERN = /^(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}$/;

const readAuthFileSourceIP = (item: AuthFileItem): string =>
  String(item.sourceIp ?? item.source_ip ?? '').trim();

const readAuthFileAuthIndex = (item: AuthFileItem): string => {
  const value = item.authIndex ?? item['auth_index'] ?? item['auth-index'];
  if (value === undefined || value === null) return '';
  return String(value).trim();
};

const apiKeySubtitle = (config: GeminiKeyConfig | ProviderKeyConfig) =>
  [config.prefix, config.authIndex ? `#${config.authIndex}` : '', config.baseUrl, config.proxyUrl]
    .map((value) => String(value ?? '').trim())
    .filter(Boolean)
    .join(' · ');

const openAIEntrySubtitle = (
  provider: OpenAIProviderConfig,
  entry: ApiKeyEntry,
  entryIndex: number
) =>
  [
    provider.name,
    `#${entryIndex + 1}`,
    entry.authIndex ? `auth ${entry.authIndex}` : '',
    entry.proxyUrl,
  ]
    .map((value) => String(value ?? '').trim())
    .filter(Boolean)
    .join(' · ');

export function EgressIpWizardModal({
  open,
  disabled = false,
  geminiKeys,
  interactionsKeys,
  codexConfigs,
  xaiConfigs,
  claudeConfigs,
  vertexConfigs,
  openaiProviders,
  onClose,
  onApplied,
}: EgressIpWizardModalProps) {
  const { t } = useTranslation();
  const [sourceIp, setSourceIp] = useState('');
  const [inventory, setInventory] = useState<ContainerOpsEgressIPInventory | null>(null);
  const [operationResult, setOperationResult] = useState<ContainerOpsSourceIPResult | null>(null);
  const [authFiles, setAuthFiles] = useState<AuthFileItem[]>([]);
  const [selectedTargetIds, setSelectedTargetIds] = useState<Set<string>>(new Set());
  const [selectionTouched, setSelectionTouched] = useState(false);
  const [error, setError] = useState('');
  const [busyStep, setBusyStep] = useState<
    'inventory' | 'mount' | 'bind' | 'all' | 'remove' | null
  >(null);

  const providerTargets = useMemo<BindingTarget[]>(() => {
    const targets: BindingTarget[] = [];
    const pushKeyTargets = (
      kind: KeyProviderKind,
      configs: Array<GeminiKeyConfig | ProviderKeyConfig>
    ) => {
      configs.forEach((config, index) => {
        targets.push({
          id: `provider:${kind}:${index}`,
          source: 'provider',
          kind,
          index,
          title: `${PROVIDER_KIND_LABELS[kind]} · ${maskApiKey(config.apiKey)}`,
          subtitle: apiKeySubtitle(config) || t('egress_ip.default_account_subtitle'),
          currentSourceIp: String(config.sourceIp ?? '').trim(),
        });
      });
    };

    pushKeyTargets('gemini', geminiKeys);
    pushKeyTargets('interactions', interactionsKeys);
    pushKeyTargets('codex', codexConfigs);
    pushKeyTargets('xai', xaiConfigs);
    pushKeyTargets('claude', claudeConfigs);
    pushKeyTargets('vertex', vertexConfigs);

    openaiProviders.forEach((provider, providerIndex) => {
      (provider.apiKeyEntries ?? []).forEach((entry, entryIndex) => {
        targets.push({
          id: `openai:${providerIndex}:${entryIndex}`,
          source: 'openai-key',
          providerIndex,
          entryIndex,
          title: `OpenAI · ${provider.name} · ${maskApiKey(entry.apiKey)}`,
          subtitle: openAIEntrySubtitle(provider, entry, entryIndex),
          currentSourceIp: String(entry.sourceIp ?? '').trim(),
        });
      });
    });

    return targets;
  }, [
    claudeConfigs,
    codexConfigs,
    geminiKeys,
    interactionsKeys,
    openaiProviders,
    t,
    vertexConfigs,
    xaiConfigs,
  ]);

  const authFileTargets = useMemo<BindingTarget[]>(
    () =>
      authFiles.map((file) => {
        const authIndex = readAuthFileAuthIndex(file);
        const provider = String(file.provider ?? file.type ?? 'OAuth').trim();
        return {
          id: `auth:${file.name}:${authIndex}`,
          source: 'auth-file',
          name: file.name,
          authIndex: authIndex || undefined,
          authFile: file,
          title: `${provider || 'OAuth'} · ${file.name}`,
          subtitle: authIndex
            ? t('egress_ip.auth_file_subtitle_with_index', { authIndex })
            : t('egress_ip.auth_file_subtitle'),
          currentSourceIp: readAuthFileSourceIP(file),
        };
      }),
    [authFiles, t]
  );

  const bindingTargets = useMemo(
    () => [...providerTargets, ...authFileTargets],
    [authFileTargets, providerTargets]
  );

  const selectedTargets = useMemo(
    () => bindingTargets.filter((target) => selectedTargetIds.has(target.id)),
    [bindingTargets, selectedTargetIds]
  );

  const sourceIpError =
    sourceIp.trim() && !IPV4_PATTERN.test(sourceIp.trim()) ? t('egress_ip.invalid_source_ip') : '';

  const loadInventory = useCallback(async () => {
    setBusyStep('inventory');
    setError('');
    try {
      const data = await containerOpsApi.egressIPs();
      setInventory(data);
      return data;
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      setError(message);
      throw err;
    } finally {
      setBusyStep(null);
    }
  }, []);

  const loadAuthFiles = useCallback(async () => {
    try {
      const data = await authFilesApi.list();
      setAuthFiles(data.files ?? []);
    } catch {
      setAuthFiles([]);
    }
  }, []);

  useEffect(() => {
    if (!open) return;
    setOperationResult(null);
    setError('');
    setSelectionTouched(false);
    void loadInventory().catch(() => {});
    void loadAuthFiles();
  }, [loadAuthFiles, loadInventory, open]);

  useEffect(() => {
    if (!open || bindingTargets.length === 0 || selectionTouched) return;
    setSelectedTargetIds(new Set(bindingTargets.map((target) => target.id)));
  }, [bindingTargets, open, selectionTouched]);

  const toggleTarget = (targetId: string, checked: boolean) => {
    setSelectionTouched(true);
    setSelectedTargetIds((prev) => {
      const next = new Set(prev);
      if (checked) {
        next.add(targetId);
      } else {
        next.delete(targetId);
      }
      return next;
    });
  };

  const selectAllTargets = () => {
    setSelectionTouched(true);
    setSelectedTargetIds(new Set(bindingTargets.map((target) => target.id)));
  };

  const clearSelectedTargets = () => {
    setSelectionTouched(true);
    setSelectedTargetIds(new Set());
  };

  const validateSourceIP = () => {
    const normalized = sourceIp.trim();
    if (!normalized || !IPV4_PATTERN.test(normalized)) {
      setError(t('egress_ip.invalid_source_ip'));
      return '';
    }
    setError('');
    return normalized;
  };

  const mountAndVerify = async (step: 'mount' | 'all' = 'mount') => {
    const normalized = validateSourceIP();
    if (!normalized) return null;
    setBusyStep(step);
    setError('');
    try {
      const result = await containerOpsApi.ensureSourceIP({ sourceIp: normalized });
      setOperationResult(result);
      await loadInventory().catch(() => {});
      return result;
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      setError(message);
      throw err;
    } finally {
      setBusyStep(null);
    }
  };

  const updateProviderTarget = async (target: BindingTarget, normalizedSourceIp: string) => {
    if (target.source === 'auth-file') {
      const patchTarget = getAuthFilePatchTarget(target.authFile);
      const sourceIdentities = authFiles
        .filter((file) => file.name.trim() === target.name.trim())
        .map(getAuthFilePatchTarget);
      if (target.authIndex) {
        await authFilesApi.patchFieldsForAuthIndexes(target.name, [patchTarget], sourceIdentities, {
          source_ip: normalizedSourceIp,
        });
      } else {
        await authFilesApi.patchFieldsWithPluginSourceFallback(
          patchTarget,
          { source_ip: normalizedSourceIp },
          sourceIdentities
        );
      }
      return;
    }

    if (target.source === 'openai-key') {
      const provider = openaiProviders[target.providerIndex];
      if (!provider) return;
      const nextProvider: OpenAIProviderConfig = {
        ...provider,
        apiKeyEntries: (provider.apiKeyEntries ?? []).map((entry, index) =>
          index === target.entryIndex ? { ...entry, sourceIp: normalizedSourceIp } : entry
        ),
      };
      await providersApi.updateOpenAIProvider(provider.name, target.providerIndex, nextProvider);
      return;
    }

    const updateKeyConfig = async <T extends GeminiKeyConfig | ProviderKeyConfig>(
      configs: T[],
      update: (current: T, next: T) => Promise<unknown>
    ) => {
      const current = configs[target.index];
      if (!current) return;
      await update(current, { ...current, sourceIp: normalizedSourceIp });
    };

    switch (target.kind) {
      case 'gemini':
        await updateKeyConfig(geminiKeys, providersApi.updateGeminiKey);
        return;
      case 'interactions':
        await updateKeyConfig(interactionsKeys, providersApi.updateInteractionsKey);
        return;
      case 'codex':
        await updateKeyConfig(codexConfigs, providersApi.updateCodexConfig);
        return;
      case 'xai':
        await updateKeyConfig(xaiConfigs, providersApi.updateXAIConfig);
        return;
      case 'claude':
        await updateKeyConfig(claudeConfigs, providersApi.updateClaudeConfig);
        return;
      case 'vertex':
        await updateKeyConfig(vertexConfigs, providersApi.updateVertexConfig);
        return;
      default:
        return;
    }
  };

  const bindSelectedTargets = async (step: 'bind' | 'all' = 'bind') => {
    const normalized = validateSourceIP();
    if (!normalized) return false;
    if (selectedTargets.length === 0) {
      setError(t('egress_ip.no_targets_selected'));
      return false;
    }
    setBusyStep(step);
    setError('');
    const failures: string[] = [];
    try {
      for (const target of selectedTargets) {
        try {
          await updateProviderTarget(target, normalized);
        } catch (err) {
          const message = err instanceof Error ? err.message : String(err);
          failures.push(`${target.title}: ${message}`);
        }
      }
      await onApplied();
      await loadAuthFiles();
      if (failures.length > 0) {
        setError(t('egress_ip.bind_partial_failed', { count: failures.length }));
        return false;
      }
      return true;
    } finally {
      setBusyStep(null);
    }
  };

  const runOneClick = async () => {
    const result = await mountAndVerify('all');
    if (!result || result.status !== 'completed' || !result.mounted) {
      setError(t('egress_ip.mount_before_bind_failed'));
      return;
    }
    const ok = await bindSelectedTargets('all');
    if (ok) {
      setOperationResult(result);
    }
  };

  const removeSourceIP = async () => {
    const normalized = validateSourceIP();
    if (!normalized) return;
    setBusyStep('remove');
    setError('');
    try {
      const result = await containerOpsApi.removeSourceIP({ sourceIp: normalized });
      setOperationResult(result);
      await loadInventory().catch(() => {});
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      setError(message);
    } finally {
      setBusyStep(null);
    }
  };

  const busy = busyStep !== null || disabled;
  const localAddresses = inventory?.addresses ?? [];
  const mountedSourceIPs = localAddresses.filter((item) => item.scope !== 'host');

  return (
    <Modal
      open={open}
      title={t('egress_ip.modal_title')}
      onClose={onClose}
      width={920}
      className={styles.modal}
      closeDisabled={busyStep !== null}
      footer={
        <div className={styles.footer}>
          <Button variant="secondary" onClick={onClose} disabled={busyStep !== null}>
            {t('common.close')}
          </Button>
          <Button
            variant="secondary"
            onClick={() => void mountAndVerify()}
            loading={busyStep === 'mount'}
            disabled={busy || Boolean(sourceIpError)}
          >
            {t('egress_ip.mount_and_verify')}
          </Button>
          <Button
            onClick={() => void runOneClick()}
            loading={busyStep === 'all'}
            disabled={busy || Boolean(sourceIpError) || selectedTargets.length === 0}
          >
            {t('egress_ip.one_click_finish')}
          </Button>
        </div>
      }
    >
      <div className={styles.body}>
        {error ? <div className="error-box">{error}</div> : null}

        <section className={styles.hero}>
          <div>
            <div className={styles.eyebrow}>{t('egress_ip.hero_eyebrow')}</div>
            <h3>{t('egress_ip.hero_title')}</h3>
            <p>{t('egress_ip.hero_desc')}</p>
          </div>
          <Button
            variant="secondary"
            size="sm"
            onClick={() => void loadInventory()}
            loading={busyStep === 'inventory'}
            disabled={busyStep !== null}
          >
            {t('egress_ip.detect_server_ip')}
          </Button>
        </section>

        <div className={styles.grid}>
          <section className={styles.panel}>
            <h4>{t('egress_ip.step_server')}</h4>
            {busyStep === 'inventory' && !inventory ? (
              <LoadingSpinner />
            ) : (
              <div className={styles.metricGrid}>
                <div className={styles.metric}>
                  <span>{t('egress_ip.native_ip')}</span>
                  <strong>{inventory?.nativeOutboundIp || '-'}</strong>
                </div>
                <div className={styles.metric}>
                  <span>{t('egress_ip.default_interface')}</span>
                  <strong>{inventory?.defaultInterface || '-'}</strong>
                </div>
              </div>
            )}
            <div className={styles.addressList}>
              {mountedSourceIPs.length > 0 ? (
                mountedSourceIPs.slice(0, 12).map((address) => (
                  <span key={`${address.interface}:${address.cidr}`} className={styles.addressPill}>
                    {address.interface} · {address.cidr}
                  </span>
                ))
              ) : (
                <span className={styles.muted}>{t('egress_ip.no_local_ips')}</span>
              )}
            </div>
          </section>

          <section className={styles.panel}>
            <h4>{t('egress_ip.step_source_ip')}</h4>
            <Input
              label={t('egress_ip.source_ip_label')}
              value={sourceIp}
              onChange={(event) => setSourceIp(event.target.value)}
              placeholder="144.172.106.107"
              error={sourceIpError}
              disabled={busyStep !== null}
              hint={t('egress_ip.source_ip_hint')}
            />
            <div className={styles.actionRow}>
              <Button
                variant="secondary"
                size="sm"
                onClick={() => void mountAndVerify()}
                loading={busyStep === 'mount'}
                disabled={busy || Boolean(sourceIpError)}
              >
                {t('egress_ip.mount_and_verify')}
              </Button>
              <Button
                variant="danger"
                size="sm"
                onClick={() => void removeSourceIP()}
                loading={busyStep === 'remove'}
                disabled={busy || Boolean(sourceIpError)}
              >
                {t('egress_ip.remove_source_ip')}
              </Button>
            </div>
          </section>
        </div>

        {operationResult ? (
          <section className={styles.resultPanel}>
            <div className={styles.resultHeader}>
              <strong>{t('egress_ip.latest_result')}</strong>
              <span className={styles.statusBadge}>{operationResult.status}</span>
            </div>
            <div className={styles.resultMeta}>
              <span>
                {t('egress_ip.result_mounted')}: {operationResult.mounted ? 'yes' : 'no'}
              </span>
              <span>
                {t('egress_ip.result_interface')}: {operationResult.interface || '-'}
              </span>
              <span>
                {t('egress_ip.result_outbound')}: {operationResult.outboundIp || '-'}
              </span>
            </div>
            <div className={styles.checkList}>
              {operationResult.checks.map((check) => (
                <span
                  key={`${check.code}:${check.resource ?? ''}`}
                  className={[
                    styles.checkItem,
                    check.blocking ? styles.checkBlocking : styles.checkOk,
                  ]
                    .filter(Boolean)
                    .join(' ')}
                >
                  {check.message}
                  {check.resource ? ` · ${check.resource}` : ''}
                </span>
              ))}
            </div>
          </section>
        ) : null}

        <section className={styles.panel}>
          <div className={styles.targetsHeader}>
            <div>
              <h4>{t('egress_ip.step_accounts')}</h4>
              <p>{t('egress_ip.accounts_desc')}</p>
            </div>
            <div className={styles.actionRow}>
              <Button variant="secondary" size="xs" onClick={selectAllTargets} disabled={busy}>
                {t('egress_ip.select_all')}
              </Button>
              <Button variant="secondary" size="xs" onClick={clearSelectedTargets} disabled={busy}>
                {t('egress_ip.clear_selection')}
              </Button>
              <Button
                variant="secondary"
                size="xs"
                onClick={() => void bindSelectedTargets()}
                loading={busyStep === 'bind'}
                disabled={busy || selectedTargets.length === 0 || Boolean(sourceIpError)}
              >
                {t('egress_ip.bind_selected')}
              </Button>
            </div>
          </div>

          <div className={styles.targetList}>
            {bindingTargets.length > 0 ? (
              bindingTargets.map((target) => (
                <SelectionCheckbox
                  key={target.id}
                  checked={selectedTargetIds.has(target.id)}
                  onChange={(checked) => toggleTarget(target.id, checked)}
                  disabled={busyStep !== null}
                  label={
                    <div className={styles.targetItem}>
                      <div className={styles.targetMain}>
                        <strong>{target.title}</strong>
                        <span>{target.subtitle}</span>
                      </div>
                      <span className={styles.currentIp}>
                        {target.currentSourceIp || t('egress_ip.unbound')}
                      </span>
                    </div>
                  }
                />
              ))
            ) : (
              <div className={styles.emptyTargets}>{t('egress_ip.no_accounts')}</div>
            )}
          </div>
        </section>
      </div>
    </Modal>
  );
}
