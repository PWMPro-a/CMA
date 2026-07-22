import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import type { TFunction } from 'i18next';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import {
  IconCheck,
  IconCopy,
  IconDownload,
  IconFileText,
  IconPlay,
  IconRefreshCw,
  IconX,
} from '@/components/ui/icons';
import {
  containerOpsApi,
  type ContainerOpsAgentInfo,
  type ContainerOpsAuditEntry,
  type ContainerOpsBackupResult,
  type ContainerOpsComposeDraft,
  type ContainerOpsDeployCheck,
  type ContainerOpsDeployPlan,
  type ContainerOpsDeployStep,
  type ContainerOpsDiscovery,
  type ContainerOpsDockerContainer,
  type ContainerOpsDockerImage,
  type ContainerOpsDockerNetwork,
  type ContainerOpsInfo,
  type ContainerOpsImportCandidate,
  type ContainerOpsImportPlan,
  type ContainerOpsImportRisk,
  type ContainerOpsLifecycleState,
  type ContainerOpsManifestService,
  type ContainerOpsNetworkCheck,
  type ContainerOpsNetworkStandardizeResult,
  type ContainerOpsRestoreCheck,
  type ContainerOpsRestorePlan,
  type ContainerOpsStandardResource,
  type ContainerOpsUpgradeAction,
  type ContainerOpsUpgradeCheck,
  type ContainerOpsUpgradePlan,
  type ContainerOpsUpgradeStep,
  type ContainerOpsUpgradeTask,
} from '@/services/api/containerOps';
import { useNotificationStore } from '@/stores';
import styles from './ContainerOpsPage.module.scss';

type StatusTone = 'success' | 'warning' | 'error' | 'muted';

const roleOrder: Record<string, number> = {
  cpa: 1,
  cpamp: 2,
  agent: 3,
  newapi: 4,
};

const formatBytes = (value: number) => {
  if (!Number.isFinite(value) || value <= 0) return '-';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let next = value;
  let unitIndex = 0;
  while (next >= 1024 && unitIndex < units.length - 1) {
    next /= 1024;
    unitIndex += 1;
  }
  return `${next >= 10 || unitIndex === 0 ? next.toFixed(0) : next.toFixed(1)} ${units[unitIndex]}`;
};

const joinPorts = (container: ContainerOpsDockerContainer) => {
  const ports = container.ports ?? [];
  if (!ports.length) return '-';
  return ports
    .map((port) => {
      const target = [port.privatePort, port.type].filter(Boolean).join('/');
      return port.publicPort ? `${port.publicPort}->${target}` : target;
    })
    .join(', ');
};

const joinNetworks = (container: ContainerOpsDockerContainer) => {
  const networks = container.networks ?? [];
  if (!networks.length) return '-';
  return networks
    .map((network) => (network.ipAddress ? `${network.name} ${network.ipAddress}` : network.name))
    .join(', ');
};

const primaryMounts = (container: ContainerOpsDockerContainer) => {
  const mounts = container.mounts ?? [];
  if (!mounts.length) return '-';
  return mounts
    .slice(0, 2)
    .map((mount) => mount.source || mount.name || mount.destination)
    .join(', ');
};

const joinValues = (items?: string[]) => (items?.length ? items.join(', ') : '-');

function StatusPill({ tone, children }: { tone: StatusTone; children: ReactNode }) {
  return <span className={`${styles.statusPill} ${styles[tone]}`}>{children}</span>;
}

function CopyButton({ value }: { value: string }) {
  const { t } = useTranslation();
  const { showNotification } = useNotificationStore();

  const handleCopy = async () => {
    await navigator.clipboard.writeText(value);
    showNotification(t('container_ops.copied'), 'success');
  };

  return (
    <button
      type="button"
      className={styles.copyButton}
      onClick={() => void handleCopy()}
      title={t('common.copy')}
      aria-label={t('common.copy')}
    >
      <IconCopy size={14} />
    </button>
  );
}

function KeyValueGrid({
  items,
}: {
  items: Array<{ label: string; value: string | number | boolean | undefined; copy?: boolean }>;
}) {
  return (
    <div className={styles.keyValueGrid}>
      {items.map((item) => {
        const display =
          item.value === true ? 'true' : item.value === false ? 'false' : item.value || '-';
        return (
          <div className={styles.keyValueItem} key={item.label}>
            <span className={styles.keyLabel}>{item.label}</span>
            <span className={styles.keyValue}>
              {display}
              {item.copy && typeof display === 'string' && display !== '-' ? (
                <CopyButton value={display} />
              ) : null}
            </span>
          </div>
        );
      })}
    </div>
  );
}

function buildResourceItems(t: TFunction, resources?: ContainerOpsStandardResource) {
  return [
    { label: t('container_ops.compose_project'), value: resources?.composeProject },
    { label: t('container_ops.network'), value: resources?.network },
    { label: t('container_ops.cpa_service'), value: resources?.cpaService },
    { label: t('container_ops.cpamp_service'), value: resources?.cpampService },
    { label: t('container_ops.agent_service'), value: resources?.agentService },
    { label: t('container_ops.stack_root'), value: resources?.stackRoot },
    { label: t('container_ops.backup_root'), value: resources?.backupRoot },
  ];
}

function agentTone(agent?: ContainerOpsAgentInfo): StatusTone {
  if (!agent?.configured) return 'muted';
  return agent.reachable ? 'success' : 'error';
}

function agentLabel(t: TFunction, agent?: ContainerOpsAgentInfo) {
  if (!agent?.configured) return t('container_ops.agent_not_configured');
  if (!agent.reachable) return t('container_ops.agent_unreachable');
  return agent.readOnly ? t('container_ops.agent_read_only') : t('container_ops.agent_ready');
}

function riskTone(risk: ContainerOpsImportRisk): StatusTone {
  if (risk.severity === 'error' || risk.blocking) return 'error';
  if (risk.severity === 'warning') return 'warning';
  return 'muted';
}

function restoreCheckTone(check: ContainerOpsRestoreCheck): StatusTone {
  if (check.severity === 'error' || check.blocking) return 'error';
  if (check.severity === 'warning') return 'warning';
  return 'muted';
}

function deployCheckTone(check: ContainerOpsDeployCheck): StatusTone {
  if (check.severity === 'error' || check.blocking) return 'error';
  if (check.severity === 'warning') return 'warning';
  return 'muted';
}

function deployActionTone(status: string): StatusTone {
  if (status === 'failed') return 'error';
  if (status === 'applied') return 'success';
  if (status === 'skipped') return 'muted';
  return 'muted';
}

function networkCheckTone(check: ContainerOpsNetworkCheck): StatusTone {
  if (check.severity === 'error' || check.blocking) return 'error';
  if (check.severity === 'warning') return 'warning';
  return 'muted';
}

function upgradeCheckTone(check: ContainerOpsUpgradeCheck): StatusTone {
  if (check.severity === 'error' || check.blocking) return 'error';
  if (check.severity === 'warning') return 'warning';
  return 'muted';
}

function lifecycleTone(lifecycle?: ContainerOpsLifecycleState | null): StatusTone {
  if (!lifecycle?.active && (!lifecycle || lifecycle.status === 'idle')) return 'muted';
  if (lifecycle.status === 'failed' || lifecycle.status === 'blocked') return 'error';
  if (lifecycle.status === 'in_progress') return 'warning';
  return 'success';
}

function auditTone(status: string): StatusTone {
  if (status === 'failed' || status === 'blocked') return 'error';
  if (status === 'in_progress') return 'warning';
  if (status === 'completed') return 'success';
  return 'muted';
}

function upgradeTaskTone(status: string): StatusTone {
  if (status === 'failed' || status === 'blocked') return 'error';
  if (status === 'preparing' || status === 'prepare_review' || status === 'running') return 'warning';
  if (status === 'prepared' || status === 'completed') return 'success';
  return 'muted';
}

function upgradeTaskStartable(task: ContainerOpsUpgradeTask): boolean {
  return task.status === 'prepared' && task.nextAction === 'start_async_recreate';
}

function formatTimestampMs(value?: number) {
  if (!value || value <= 0) return '-';
  return new Date(value).toLocaleString();
}

function formatDurationMs(value?: number) {
  if (!value || value <= 0) return '-';
  if (value < 1000) return `${value} ms`;
  return `${(value / 1000).toFixed(value < 10_000 ? 1 : 0)} s`;
}

export function ContainerOpsPage() {
  const { t } = useTranslation();
  const { showNotification } = useNotificationStore();
  const [info, setInfo] = useState<ContainerOpsInfo | null>(null);
  const [discovery, setDiscovery] = useState<ContainerOpsDiscovery | null>(null);
  const [importPlan, setImportPlan] = useState<ContainerOpsImportPlan | null>(null);
  const [deployPlan, setDeployPlan] = useState<ContainerOpsDeployPlan | null>(null);
  const [backupResult, setBackupResult] = useState<ContainerOpsBackupResult | null>(null);
  const [restorePlan, setRestorePlan] = useState<ContainerOpsRestorePlan | null>(null);
  const [rollbackResult, setRollbackResult] = useState<ContainerOpsRestorePlan | null>(null);
  const [networkResult, setNetworkResult] = useState<ContainerOpsNetworkStandardizeResult | null>(null);
  const [upgradePlan, setUpgradePlan] = useState<ContainerOpsUpgradePlan | null>(null);
  const [lifecycle, setLifecycle] = useState<ContainerOpsLifecycleState | null>(null);
  const [auditEntries, setAuditEntries] = useState<ContainerOpsAuditEntry[]>([]);
  const [upgradeTasks, setUpgradeTasks] = useState<ContainerOpsUpgradeTask[]>([]);
  const [restoreBackupId, setRestoreBackupId] = useState('');
  const [rollbackBackupId, setRollbackBackupId] = useState('');
  const [networkBackupId, setNetworkBackupId] = useState('');
  const [upgradeCpaImage, setUpgradeCpaImage] = useState('');
  const [upgradeCpampImage, setUpgradeCpampImage] = useState('');
  const [loadingInfo, setLoadingInfo] = useState(false);
  const [discovering, setDiscovering] = useState(false);
  const [importing, setImporting] = useState(false);
  const [deploying, setDeploying] = useState(false);
  const [backingUp, setBackingUp] = useState(false);
  const [restoring, setRestoring] = useState(false);
  const [rollingBack, setRollingBack] = useState(false);
  const [standardizingNetwork, setStandardizingNetwork] = useState(false);
  const [upgrading, setUpgrading] = useState(false);
  const [startingUpgradeTaskId, setStartingUpgradeTaskId] = useState('');
  const [loadingAudits, setLoadingAudits] = useState(false);
  const [loadingUpgradeTasks, setLoadingUpgradeTasks] = useState(false);
  const [error, setError] = useState('');
  const [auditError, setAuditError] = useState('');
  const [upgradeTaskError, setUpgradeTaskError] = useState('');
  const [discoverError, setDiscoverError] = useState('');
  const [importError, setImportError] = useState('');
  const [deployError, setDeployError] = useState('');
  const [backupError, setBackupError] = useState('');
  const [restoreError, setRestoreError] = useState('');
  const [rollbackError, setRollbackError] = useState('');
  const [networkError, setNetworkError] = useState('');
  const [upgradeError, setUpgradeError] = useState('');

  const agent = discovery?.agent ?? info?.agent;
  const summary = discovery?.docker.summary;

  const containers = useMemo(() => {
    const items = discovery?.docker.containers ?? [];
    return [...items].sort((a, b) => {
      const roleDelta = (roleOrder[a.role || ''] ?? 99) - (roleOrder[b.role || ''] ?? 99);
      return roleDelta || a.name.localeCompare(b.name);
    });
  }, [discovery?.docker.containers]);

  const networks = useMemo(() => discovery?.docker.networks ?? [], [discovery?.docker.networks]);
  const images = useMemo(() => discovery?.docker.images ?? [], [discovery?.docker.images]);

  const loadInfo = useCallback(async () => {
    setLoadingInfo(true);
    setError('');
    try {
      const next = await containerOpsApi.info();
      setInfo(next);
      setLifecycle(next.lifecycle);
      return next;
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : String(err || t('common.unknown_error'));
      setError(message);
      throw err;
    } finally {
      setLoadingInfo(false);
    }
  }, [t]);

  const loadAudits = useCallback(async () => {
    setLoadingAudits(true);
    setAuditError('');
    try {
      const next = await containerOpsApi.audits(20);
      setAuditEntries(Array.isArray(next.items) ? next.items : []);
      return next.items;
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : String(err || t('common.unknown_error'));
      setAuditError(message);
      return [];
    } finally {
      setLoadingAudits(false);
    }
  }, [t]);

  const loadUpgradeTasks = useCallback(async () => {
    setLoadingUpgradeTasks(true);
    setUpgradeTaskError('');
    try {
      const next = await containerOpsApi.upgradeTasks(20);
      setUpgradeTasks(Array.isArray(next.items) ? next.items : []);
      return next.items;
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : String(err || t('common.unknown_error'));
      setUpgradeTaskError(message);
      return [];
    } finally {
      setLoadingUpgradeTasks(false);
    }
  }, [t]);

  const discover = useCallback(async () => {
    setDiscovering(true);
    setDiscoverError('');
    try {
      const next = await containerOpsApi.discover();
      setDiscovery(next);
      setImportPlan(null);
      setDeployPlan(null);
      setUpgradePlan(null);
      setImportError('');
      setDeployError('');
      setUpgradeError('');
      showNotification(t('container_ops.discover_success'), 'success');
      return next;
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : String(err || t('common.unknown_error'));
      setDiscoverError(message);
      return null;
    } finally {
      setDiscovering(false);
    }
  }, [showNotification, t]);

  const generateImportPlan = useCallback(async () => {
    setImporting(true);
    setImportError('');
    try {
      const next = await containerOpsApi.importPlan();
      setImportPlan(next);
      showNotification(t('container_ops.import_plan_success'), 'success');
      return next;
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : String(err || t('common.unknown_error'));
      setImportError(message);
      return null;
    } finally {
      setImporting(false);
    }
  }, [showNotification, t]);

  const generateDeployPlan = useCallback(async () => {
    setDeploying(true);
    setDeployError('');
    try {
      const next = await containerOpsApi.deployPlan({ apply: false });
      setDeployPlan(next);
      if (next.overview) {
        setDiscovery({
          agent: next.agent ?? agent ?? {
            configured: false,
            reachable: false,
          },
          docker: next.overview,
          newApi: next.manifest
            ? { recommendedBaseUrl: next.manifest.newApiBaseUrl }
            : { recommendedBaseUrl: '' },
          recommendedAction: 'deploy_cpa_stack',
        });
      }
      showNotification(t('container_ops.deploy_plan_success'), 'success');
      return next;
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : String(err || t('common.unknown_error'));
      setDeployError(message);
      return null;
    } finally {
      setDeploying(false);
    }
  }, [agent, showNotification, t]);

  const renderDeployFiles = useCallback(async () => {
    setDeploying(true);
    setDeployError('');
    try {
      const next = await containerOpsApi.deployPlan({ apply: true });
      setDeployPlan(next);
      if (next.lifecycle) setLifecycle(next.lifecycle);
      if (next.overview) {
        setDiscovery({
          agent: next.agent ?? agent ?? {
            configured: false,
            reachable: false,
          },
          docker: next.overview,
          newApi: { recommendedBaseUrl: next.manifest.newApiBaseUrl },
          recommendedAction: 'deploy_cpa_stack',
        });
      }
      void loadAudits();
      showNotification(t('container_ops.deploy_render_success'), 'success');
      return next;
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : String(err || t('common.unknown_error'));
      setDeployError(message);
      return null;
    } finally {
      setDeploying(false);
    }
  }, [agent, loadAudits, showNotification, t]);

  const pullDeployImages = useCallback(async () => {
    setDeploying(true);
    setDeployError('');
    try {
      const next = await containerOpsApi.deployPlan({ apply: true, action: 'pull_images' });
      setDeployPlan((current) => ({
        ...next,
        files: next.files ?? current?.files,
      }));
      if (next.lifecycle) setLifecycle(next.lifecycle);
      if (next.overview) {
        setDiscovery({
          agent: next.agent ?? agent ?? {
            configured: false,
            reachable: false,
          },
          docker: next.overview,
          newApi: { recommendedBaseUrl: next.manifest.newApiBaseUrl },
          recommendedAction: 'deploy_cpa_stack',
        });
      }
      void loadAudits();
      showNotification(t('container_ops.deploy_pull_success'), 'success');
      return next;
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : String(err || t('common.unknown_error'));
      setDeployError(message);
      return null;
    } finally {
      setDeploying(false);
    }
  }, [agent, loadAudits, showNotification, t]);

  const startDeployServices = useCallback(async () => {
    setDeploying(true);
    setDeployError('');
    try {
      const next = await containerOpsApi.deployPlan({ apply: true, action: 'start_services' });
      setDeployPlan((current) => ({
        ...next,
        files: next.files ?? current?.files,
        imagePulls: next.imagePulls ?? current?.imagePulls,
      }));
      if (next.lifecycle) setLifecycle(next.lifecycle);
      if (next.overview) {
        setDiscovery({
          agent: next.agent ?? agent ?? {
            configured: false,
            reachable: false,
          },
          docker: next.overview,
          newApi: { recommendedBaseUrl: next.manifest.newApiBaseUrl },
          recommendedAction: 'deploy_cpa_stack',
        });
      }
      void loadAudits();
      showNotification(
        t(
          next.status === 'started'
            ? 'container_ops.deploy_start_success'
            : 'container_ops.deploy_start_blocked',
        ),
        next.status === 'started' ? 'success' : 'warning',
      );
      return next;
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : String(err || t('common.unknown_error'));
      setDeployError(message);
      return null;
    } finally {
      setDeploying(false);
    }
  }, [agent, loadAudits, showNotification, t]);

  const createBackup = useCallback(async () => {
    setBackingUp(true);
    setBackupError('');
    try {
      const next = await containerOpsApi.backup();
      setBackupResult(next);
      if (next.lifecycle) setLifecycle(next.lifecycle);
      setRestoreBackupId(next.backupId);
      setNetworkBackupId(next.backupId);
      setRestorePlan(null);
      setRollbackResult(null);
      setNetworkResult(null);
      setUpgradePlan(null);
      setRestoreError('');
      setRollbackError('');
      setNetworkError('');
      setUpgradeError('');
      void loadAudits();
      showNotification(t('container_ops.backup_success'), 'success');
      return next;
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : String(err || t('common.unknown_error'));
      setBackupError(message);
      return null;
    } finally {
      setBackingUp(false);
    }
  }, [loadAudits, showNotification, t]);

  const generateRestorePlan = useCallback(async () => {
    const backupId = restoreBackupId.trim();
    setRestoreError('');
    if (!backupId) {
      setRestoreError(t('container_ops.restore_backup_id_required'));
      return null;
    }
    setRestoring(true);
    try {
      const next = await containerOpsApi.restorePlan({ backupId });
      setRestorePlan(next);
      showNotification(t('container_ops.restore_plan_success'), 'success');
      return next;
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : String(err || t('common.unknown_error'));
      setRestoreError(message);
      return null;
    } finally {
      setRestoring(false);
    }
  }, [restoreBackupId, showNotification, t]);

  const applyRestore = useCallback(async () => {
    const backupId = restoreBackupId.trim();
    setRestoreError('');
    if (!backupId) {
      setRestoreError(t('container_ops.restore_backup_id_required'));
      return null;
    }
    setRestoring(true);
    try {
      const next = await containerOpsApi.restorePlan({ backupId, apply: true });
      setRestorePlan(next);
      if (next.lifecycle) setLifecycle(next.lifecycle);
      if (next.rollbackBackup?.backupId) {
        setRollbackBackupId(next.rollbackBackup.backupId);
        setRollbackResult(null);
      }
      if (next.overview) {
        setDiscovery({
          agent: next.agent ?? agent ?? {
            configured: false,
            reachable: false,
          },
          docker: next.overview,
          newApi: info?.newApi ?? { recommendedBaseUrl: 'http://cli-proxy-api:8317/v1' },
          recommendedAction: 'verify_newapi_internal_route',
        });
      }
      void loadAudits();
      showNotification(
        t(next.status === 'restored' ? 'container_ops.restore_apply_success' : 'container_ops.restore_apply_blocked'),
        next.status === 'restored' ? 'success' : 'warning',
      );
      return next;
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : String(err || t('common.unknown_error'));
      setRestoreError(message);
      return null;
    } finally {
      setRestoring(false);
    }
  }, [agent, info?.newApi, loadAudits, restoreBackupId, showNotification, t]);

  const applyRollback = useCallback(async () => {
    const backupId = rollbackBackupId.trim();
    setRollbackError('');
    if (!backupId) {
      setRollbackError(t('container_ops.rollback_backup_id_required'));
      return null;
    }
    setRollingBack(true);
    try {
      const next = await containerOpsApi.rollback({ backupId });
      setRollbackResult(next);
      if (next.lifecycle) setLifecycle(next.lifecycle);
      if (next.overview) {
        setDiscovery({
          agent: next.agent ?? agent ?? {
            configured: false,
            reachable: false,
          },
          docker: next.overview,
          newApi: info?.newApi ?? { recommendedBaseUrl: 'http://cli-proxy-api:8317/v1' },
          recommendedAction: 'verify_newapi_internal_route',
        });
      }
      void loadAudits();
      showNotification(
        t(next.status === 'rolled_back' ? 'container_ops.rollback_apply_success' : 'container_ops.rollback_apply_blocked'),
        next.status === 'rolled_back' ? 'success' : 'warning',
      );
      return next;
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : String(err || t('common.unknown_error'));
      setRollbackError(message);
      return null;
    } finally {
      setRollingBack(false);
    }
  }, [agent, info?.newApi, loadAudits, rollbackBackupId, showNotification, t]);

  const standardizeNetwork = useCallback(
    async (apply: boolean) => {
      const backupId = networkBackupId.trim();
      setNetworkError('');
      if (!backupId) {
        setNetworkError(t('container_ops.network_backup_id_required'));
        return null;
      }
      setStandardizingNetwork(true);
      try {
        const next = await containerOpsApi.standardizeNetwork({ backupId, apply });
        setNetworkResult(next);
        if (next.lifecycle) setLifecycle(next.lifecycle);
        if (next.overview && discovery) {
          setDiscovery({ ...discovery, docker: next.overview });
        }
        if (apply) void loadAudits();
        showNotification(
          t(apply ? 'container_ops.network_apply_success' : 'container_ops.network_plan_success'),
          'success'
        );
        return next;
      } catch (err: unknown) {
        const message = err instanceof Error ? err.message : String(err || t('common.unknown_error'));
        setNetworkError(message);
        return null;
      } finally {
        setStandardizingNetwork(false);
      }
    },
    [discovery, loadAudits, networkBackupId, showNotification, t]
  );

  const runUpgrade = useCallback(
    async (apply: boolean) => {
      setUpgradeError('');
      setUpgrading(true);
      try {
        const next = await containerOpsApi.upgrade({
          cpaImage: upgradeCpaImage.trim() || undefined,
          cpampImage: upgradeCpampImage.trim() || undefined,
          apply,
        });
        setUpgradePlan(next);
        if (next.lifecycle) setLifecycle(next.lifecycle);
        if (next.overview) {
          setDiscovery({
            agent: next.agent ?? agent ?? {
              configured: false,
              reachable: false,
            },
            docker: next.overview,
            newApi: info?.newApi ?? { recommendedBaseUrl: 'http://cli-proxy-api:8317/v1' },
            recommendedAction: 'verify_newapi_internal_route',
          });
        }
        if (next.rollbackBackup?.backupId) {
          setRollbackBackupId(next.rollbackBackup.backupId);
        }
        if (apply) {
          void loadAudits();
          void loadUpgradeTasks();
        }
        showNotification(
          t(
            apply
              ? next.status === 'prepared'
                ? 'container_ops.upgrade_prepare_success'
                : 'container_ops.upgrade_prepare_blocked'
              : 'container_ops.upgrade_plan_success',
          ),
          apply && next.status !== 'prepared' ? 'warning' : 'success',
        );
        return next;
      } catch (err: unknown) {
        const message = err instanceof Error ? err.message : String(err || t('common.unknown_error'));
        setUpgradeError(message);
        return null;
      } finally {
        setUpgrading(false);
      }
    },
    [
      agent,
      info?.newApi,
      loadAudits,
      loadUpgradeTasks,
      showNotification,
      t,
      upgradeCpaImage,
      upgradeCpampImage,
    ],
  );

  const startUpgradeTask = useCallback(
    async (taskId: string) => {
      const nextTaskId = taskId.trim();
      if (!nextTaskId) return null;
      setUpgradeTaskError('');
      setStartingUpgradeTaskId(nextTaskId);
      try {
        const task = await containerOpsApi.startUpgradeTask(nextTaskId);
        setUpgradeTasks((current) =>
          current.map((item) => (item.taskId === task.taskId ? task : item)),
        );
        await loadInfo();
        await loadAudits();
        await loadUpgradeTasks();
        showNotification(t('container_ops.upgrade_task_start_success'), 'success');
        return task;
      } catch (err: unknown) {
        const message = err instanceof Error ? err.message : String(err || t('common.unknown_error'));
        setUpgradeTaskError(message);
        return null;
      } finally {
        setStartingUpgradeTaskId('');
      }
    },
    [loadAudits, loadInfo, loadUpgradeTasks, showNotification, t],
  );

  useEffect(() => {
    let alive = true;
    const run = async () => {
      try {
        const next = await loadInfo();
        if (!alive) return;
        if (next.agent.configured && next.agent.reachable) {
          await discover();
        }
        await loadAudits();
        await loadUpgradeTasks();
      } catch {
        // The inline error state is enough for this page.
      }
    };
    void run();
    return () => {
      alive = false;
    };
  }, [discover, loadAudits, loadInfo, loadUpgradeTasks]);

  const handleRefresh = async () => {
    const next = await loadInfo();
    if (next.agent.configured && next.agent.reachable) {
      await discover();
    } else {
      setDiscovery(null);
      setImportPlan(null);
      setDeployPlan(null);
      setBackupResult(null);
      setRestorePlan(null);
      setRollbackResult(null);
      setNetworkResult(null);
      setUpgradePlan(null);
    }
    await loadAudits();
    await loadUpgradeTasks();
  };

  const modeLabel =
    info?.mode === 'agent'
      ? t('container_ops.mode_agent')
      : info?.mode === 'read_only'
        ? t('container_ops.mode_read_only')
        : '-';

  const recommendedAction = discovery?.recommendedAction
    ? t(`container_ops.action.${discovery.recommendedAction}`, {
        defaultValue: discovery.recommendedAction,
      })
    : t('container_ops.awaiting_discovery');
  const currentLifecycle = lifecycle ?? info?.lifecycle ?? null;
  const lifecycleBusy = Boolean(currentLifecycle?.active);

  return (
    <div className={styles.page}>
      <section className={styles.toolbar}>
        <div className={styles.titleGroup}>
          <h1>{t('container_ops.title')}</h1>
          <p>{t('container_ops.subtitle')}</p>
        </div>
        <div className={styles.actions}>
          <Button
            variant="secondary"
            size="sm"
            onClick={() => void handleRefresh()}
            loading={loadingInfo || discovering}
          >
            <IconRefreshCw size={15} />
            {t('common.refresh')}
          </Button>
          <Button
            size="sm"
            onClick={() => void discover()}
            loading={discovering}
            disabled={!agent?.configured}
          >
            {t('container_ops.discover')}
          </Button>
          <Button
            variant="secondary"
            size="sm"
            onClick={() => void generateImportPlan()}
            loading={importing}
            disabled={!agent?.configured || !agent?.reachable}
          >
            <IconFileText size={15} />
            {t('container_ops.import_plan')}
          </Button>
          <Button
            variant="secondary"
            size="sm"
            onClick={() => void generateDeployPlan()}
            loading={deploying}
          >
            <IconFileText size={15} />
            {t('container_ops.deploy_plan')}
          </Button>
          <Button
            variant="secondary"
            size="sm"
            onClick={() => void createBackup()}
            loading={backingUp}
            disabled={!agent?.configured || !agent?.reachable || lifecycleBusy}
          >
            <IconDownload size={15} />
            {t('container_ops.create_backup')}
          </Button>
        </div>
      </section>

      {error ? <div className="error-box">{error}</div> : null}
      {auditError ? <div className="error-box">{auditError}</div> : null}
      {discoverError ? <div className="error-box">{discoverError}</div> : null}
      {importError ? <div className="error-box">{importError}</div> : null}
      {deployError ? <div className="error-box">{deployError}</div> : null}
      {backupError ? <div className="error-box">{backupError}</div> : null}
      {restoreError ? <div className="error-box">{restoreError}</div> : null}
      {rollbackError ? <div className="error-box">{rollbackError}</div> : null}
      {networkError ? <div className="error-box">{networkError}</div> : null}
      {upgradeError ? <div className="error-box">{upgradeError}</div> : null}
      {upgradeTaskError ? <div className="error-box">{upgradeTaskError}</div> : null}

      <section className={styles.statusGrid}>
        <div className={styles.statusPanel}>
          <div className={styles.panelHeader}>
            <span>{t('container_ops.manager_status')}</span>
            <StatusPill
              tone={
                currentLifecycle && currentLifecycle.status !== 'idle'
                  ? lifecycleTone(currentLifecycle)
                  : info?.enabled
                    ? 'success'
                    : 'warning'
              }
            >
              {currentLifecycle && currentLifecycle.status !== 'idle'
                ? t(`container_ops.lifecycle_status_value.${currentLifecycle.status}`, {
                    defaultValue: currentLifecycle.status,
                  })
                : info?.enabled
                  ? t('common.enabled')
                  : t('common.disabled')}
            </StatusPill>
          </div>
          <KeyValueGrid
            items={[
              { label: t('container_ops.mode'), value: modeLabel },
              { label: t('container_ops.managed_stack'), value: info?.managedStack },
              {
                label: t('container_ops.destructive_actions'),
                value: info?.destructiveActions ? t('common.enabled') : t('common.disabled'),
              },
              {
                label: t('container_ops.lifecycle_status'),
                value: currentLifecycle
                  ? t(`container_ops.lifecycle_status_value.${currentLifecycle.status}`, {
                      defaultValue: currentLifecycle.status,
                    })
                  : '-',
              },
              {
                label: t('container_ops.lifecycle_operation'),
                value: currentLifecycle?.operation
                  ? t(`container_ops.lifecycle_operation_value.${currentLifecycle.operation}`, {
                      defaultValue: currentLifecycle.operation,
                    })
                  : '-',
              },
              {
                label: t('container_ops.newapi_base_url'),
                value: info?.newApi?.recommendedBaseUrl,
                copy: true,
              },
            ]}
          />
        </div>

        <div className={styles.statusPanel}>
          <div className={styles.panelHeader}>
            <span>{t('container_ops.agent_status')}</span>
            <StatusPill tone={agentTone(agent)}>{agentLabel(t, agent)}</StatusPill>
          </div>
          <KeyValueGrid
            items={[
              { label: t('container_ops.agent_base_url'), value: agent?.baseUrl },
              { label: t('container_ops.agent_service_label'), value: agent?.service },
              { label: t('container_ops.version'), value: agent?.version },
              { label: t('container_ops.docker_host'), value: agent?.dockerHost },
              { label: t('container_ops.error'), value: agent?.error },
            ]}
          />
        </div>
      </section>

      <section className={styles.summaryGrid}>
        {[
          { label: t('container_ops.containers'), value: summary?.containerCount ?? 0 },
          { label: t('container_ops.running'), value: summary?.runningCount ?? 0 },
          { label: 'CPA', value: summary?.cpaCount ?? 0 },
          { label: 'CPAMP', value: summary?.cpampCount ?? 0 },
          { label: 'NewAPI', value: summary?.newApiCount ?? 0 },
          { label: t('container_ops.managed'), value: summary?.managedCount ?? 0 },
          { label: t('container_ops.networks'), value: summary?.networkCount ?? 0 },
          { label: t('container_ops.images'), value: summary?.imageCount ?? 0 },
        ].map((item) => (
          <div className={styles.metric} key={item.label}>
            <span>{item.label}</span>
            <strong>{item.value}</strong>
          </div>
        ))}
      </section>

      <section className={styles.recommendationPanel}>
        <div>
          <span className={styles.eyebrow}>{t('container_ops.recommended_action')}</span>
          <strong>{recommendedAction}</strong>
        </div>
        <p>{t('container_ops.readonly_notice')}</p>
      </section>

      <AuditPanel entries={auditEntries} loading={loadingAudits} />

      {importPlan ? <ImportPlanPanel plan={importPlan} /> : null}
      {deployPlan ? (
        <DeployPlanPanel
          disabled={!agent?.configured || !agent?.reachable || lifecycleBusy}
          loading={deploying}
          plan={deployPlan}
          onRender={() => void renderDeployFiles()}
          onPullImages={() => void pullDeployImages()}
          onStartServices={() => void startDeployServices()}
        />
      ) : null}
      {backupResult ? <BackupResultPanel result={backupResult} /> : null}
      <RestorePlanPanel
        backupId={restoreBackupId}
        disabled={!agent?.configured || !agent?.reachable}
        writeDisabled={lifecycleBusy}
        loading={restoring}
        plan={restorePlan}
        onBackupIdChange={setRestoreBackupId}
        onGenerate={() => void generateRestorePlan()}
        onApply={() => void applyRestore()}
      />
      <RollbackPanel
        backupId={rollbackBackupId}
        disabled={!agent?.configured || !agent?.reachable || lifecycleBusy}
        loading={rollingBack}
        result={rollbackResult}
        onBackupIdChange={setRollbackBackupId}
        onApply={() => void applyRollback()}
      />
      <NetworkStandardizePanel
        backupId={networkBackupId}
        disabled={!agent?.configured || !agent?.reachable}
        writeDisabled={lifecycleBusy}
        loading={standardizingNetwork}
        result={networkResult}
        onBackupIdChange={setNetworkBackupId}
        onPlan={() => void standardizeNetwork(false)}
        onApply={() => void standardizeNetwork(true)}
      />
      <UpgradePreparePanel
        cpaImage={upgradeCpaImage}
        cpampImage={upgradeCpampImage}
        disabled={!agent?.configured || !agent?.reachable}
        writeDisabled={lifecycleBusy}
        loading={upgrading}
        plan={upgradePlan}
        onCpaImageChange={setUpgradeCpaImage}
        onCpampImageChange={setUpgradeCpampImage}
        onPlan={() => void runUpgrade(false)}
        onPrepare={() => void runUpgrade(true)}
      />
      <UpgradeTaskPanel
        tasks={upgradeTasks}
        loading={loadingUpgradeTasks}
        disabled={!agent?.configured || !agent?.reachable || lifecycleBusy}
        startingTaskId={startingUpgradeTaskId}
        onStart={startUpgradeTask}
      />

      <section className={styles.resourcePanel}>
        <div className={styles.sectionHeader}>
          <h2>{t('container_ops.standard_resources')}</h2>
        </div>
        <KeyValueGrid items={buildResourceItems(t, info?.standardResources)} />
      </section>

      <section className={styles.tablePanel}>
        <div className={styles.sectionHeader}>
          <h2>{t('container_ops.container_inventory')}</h2>
          <span>{t('container_ops.item_count', { count: containers.length })}</span>
        </div>
        {containers.length ? (
          <div className={styles.tableWrap}>
            <table className={styles.inventoryTable}>
              <thead>
                <tr>
                  <th>{t('container_ops.name')}</th>
                  <th>{t('container_ops.role')}</th>
                  <th>{t('container_ops.state')}</th>
                  <th>{t('container_ops.image')}</th>
                  <th>{t('container_ops.ports')}</th>
                  <th>{t('container_ops.networks')}</th>
                  <th>{t('container_ops.mounts')}</th>
                </tr>
              </thead>
              <tbody>
                {containers.map((container) => (
                  <ContainerRow key={container.id || container.name} container={container} />
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <EmptyState
            text={
              agent?.reachable ? t('container_ops.no_containers') : t('container_ops.discover_hint')
            }
          />
        )}
      </section>

      <section className={styles.lowerGrid}>
        <ResourceList
          title={t('container_ops.network_inventory')}
          count={networks.length}
          emptyText={t('container_ops.no_networks')}
          items={networks}
          renderItem={(network) => (
            <NetworkItem key={network.id || network.name} network={network} />
          )}
        />
        <ResourceList
          title={t('container_ops.image_inventory')}
          count={images.length}
          emptyText={t('container_ops.no_images')}
          items={images}
          renderItem={(image) => (
            <ImageItem key={image.id || image.repoTags.join(',')} image={image} />
          )}
        />
      </section>
    </div>
  );
}

function AuditPanel({
  entries,
  loading,
}: {
  entries: ContainerOpsAuditEntry[];
  loading: boolean;
}) {
  const { t } = useTranslation();
  return (
    <section className={styles.resourcePanel}>
      <div className={styles.sectionHeader}>
        <h2>{t('container_ops.audit_history')}</h2>
        <span>
          {loading
            ? t('container_ops.audit_history_loading')
            : t('container_ops.item_count', { count: entries.length })}
        </span>
      </div>
      {entries.length ? (
        <div className={styles.tableWrap}>
          <table className={styles.planTable}>
            <thead>
              <tr>
                <th>{t('container_ops.lifecycle_operation')}</th>
                <th>{t('container_ops.status')}</th>
                <th>{t('container_ops.phase')}</th>
                <th>{t('container_ops.backup_id')}</th>
                <th>{t('container_ops.started_at')}</th>
                <th>{t('container_ops.duration')}</th>
                <th>{t('container_ops.message')}</th>
              </tr>
            </thead>
            <tbody>
              {entries.map((entry) => (
                <tr key={entry.id || entry.operationId}>
                  <td>
                    {t(`container_ops.lifecycle_operation_value.${entry.operation}`, {
                      defaultValue: entry.operation,
                    })}
                  </td>
                  <td>
                    <StatusPill tone={auditTone(entry.status)}>
                      {t(`container_ops.lifecycle_status_value.${entry.status}`, {
                        defaultValue: entry.status,
                      })}
                    </StatusPill>
                  </td>
                  <td>{entry.phase || '-'}</td>
                  <td className={styles.monoCell}>{entry.backupId || '-'}</td>
                  <td>{formatTimestampMs(entry.startedAtMs)}</td>
                  <td>{formatDurationMs(entry.durationMs)}</td>
                  <td>{entry.error || entry.message || '-'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <EmptyState text={loading ? t('container_ops.audit_history_loading') : t('container_ops.audit_empty')} />
      )}
    </section>
  );
}

function UpgradeTaskPanel({
  tasks,
  loading,
  disabled,
  startingTaskId,
  onStart,
}: {
  tasks: ContainerOpsUpgradeTask[];
  loading: boolean;
  disabled: boolean;
  startingTaskId: string;
  onStart: (taskId: string) => Promise<ContainerOpsUpgradeTask | null>;
}) {
  const { t } = useTranslation();
  return (
    <section className={styles.resourcePanel}>
      <div className={styles.sectionHeader}>
        <h2>{t('container_ops.upgrade_task_history')}</h2>
        <span>
          {loading
            ? t('container_ops.upgrade_task_loading')
            : t('container_ops.item_count', { count: tasks.length })}
        </span>
      </div>
      {tasks.length ? (
        <div className={styles.tableWrap}>
          <table className={styles.planTable}>
            <thead>
              <tr>
                <th>{t('container_ops.task_id')}</th>
                <th>{t('container_ops.status')}</th>
                <th>{t('container_ops.phase')}</th>
                <th>{t('container_ops.upgrade_cpa_image')}</th>
                <th>{t('container_ops.upgrade_cpamp_image')}</th>
                <th>{t('container_ops.rollback_backup')}</th>
                <th>{t('container_ops.next_action_label')}</th>
                <th>{t('container_ops.started_at')}</th>
                <th>{t('common.action')}</th>
              </tr>
            </thead>
            <tbody>
              {tasks.map((task) => {
                const canStart = upgradeTaskStartable(task);
                const starting = startingTaskId === task.taskId;
                return (
                  <tr key={task.id || task.taskId}>
                    <td className={styles.monoCell}>{task.taskId}</td>
                    <td>
                      <StatusPill tone={upgradeTaskTone(task.status)}>
                        {t(`container_ops.upgrade_task_status.${task.status}`, {
                          defaultValue: task.status,
                        })}
                      </StatusPill>
                    </td>
                    <td>
                      {t(`container_ops.upgrade_task_phase.${task.phase}`, {
                        defaultValue: task.phase || '-',
                      })}
                    </td>
                    <td className={styles.monoCell}>{task.cpaImage || '-'}</td>
                    <td className={styles.monoCell}>{task.cpampImage || '-'}</td>
                    <td className={styles.monoCell}>{task.rollbackBackupId || '-'}</td>
                    <td>
                      {t(`container_ops.upgrade_next_action.${task.nextAction}`, {
                        defaultValue: task.nextAction || '-',
                      })}
                    </td>
                    <td>{formatTimestampMs(task.startedAtMs)}</td>
                    <td>
                      {canStart ? (
                        <Button
                          variant="secondary"
                          size="xs"
                          loading={starting}
                          disabled={disabled}
                          onClick={() => void onStart(task.taskId)}
                        >
                          <IconPlay size={13} />
                          {t('container_ops.start_upgrade_task')}
                        </Button>
                      ) : (
                        <span className={styles.mutedText}>-</span>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      ) : (
        <EmptyState text={loading ? t('container_ops.upgrade_task_loading') : t('container_ops.upgrade_task_empty')} />
      )}
    </section>
  );
}

function BackupResultPanel({ result }: { result: ContainerOpsBackupResult }) {
  const { t } = useTranslation();
  const warningCount = result.warnings?.length ?? 0;
  const statusTone: StatusTone = warningCount > 0 ? 'warning' : 'success';
  const createdAt =
    result.createdAt > 0 ? new Date(result.createdAt * 1000).toLocaleString() : '-';

  return (
    <section className={styles.backupPanel}>
      <div className={styles.sectionHeader}>
        <h2>{t('container_ops.backup_result')}</h2>
        <StatusPill tone={statusTone}>
          {t(`container_ops.backup_status.${result.status}`, { defaultValue: result.status })}
        </StatusPill>
      </div>
      <KeyValueGrid
        items={[
          { label: t('container_ops.backup_id'), value: result.backupId, copy: true },
          { label: t('container_ops.backup_root'), value: result.backupRoot, copy: true },
          { label: t('container_ops.created_at'), value: createdAt },
          { label: t('container_ops.archives'), value: result.archives.length },
          { label: t('container_ops.warnings'), value: warningCount },
        ]}
      />

      <section className={styles.planSection}>
        <div className={styles.subHeader}>
          <h3>{t('container_ops.backup_archives')}</h3>
          <span>{t('container_ops.item_count', { count: result.archives.length })}</span>
        </div>
        <div className={styles.backupArchiveList}>
          {result.archives.map((archive) => (
            <div className={styles.backupArchiveItem} key={`${archive.role}-${archive.fileName}`}>
              <div>
                <strong>{archive.fileName}</strong>
                <span>
                  {archive.container} {archive.path}
                </span>
              </div>
              <div className={styles.candidateMeta}>
                <span className={styles.roleBadge}>
                  {t(`container_ops.role_value.${archive.role}`, { defaultValue: archive.role })}
                </span>
                <span>{formatBytes(archive.size)}</span>
              </div>
            </div>
          ))}
        </div>
      </section>

      {warningCount ? (
        <section className={styles.planSection}>
          <div className={styles.subHeader}>
            <h3>{t('container_ops.backup_warnings')}</h3>
            <span>{t('container_ops.item_count', { count: warningCount })}</span>
          </div>
          <div className={styles.riskList}>
            {result.warnings?.map((warning) => (
              <div className={styles.riskItem} key={`${warning.code}-${warning.resource || 'global'}`}>
                <StatusPill tone="warning">{t('common.warning')}</StatusPill>
                <div>
                  <strong>
                    {t(`container_ops.backup_warning.${warning.code}`, {
                      defaultValue: warning.message,
                    })}
                  </strong>
                  <span>{warning.resource || warning.code}</span>
                </div>
              </div>
            ))}
          </div>
        </section>
      ) : null}
    </section>
  );
}

function RestorePlanPanel({
  backupId,
  disabled,
  writeDisabled,
  loading,
  plan,
  onBackupIdChange,
  onGenerate,
  onApply,
}: {
  backupId: string;
  disabled: boolean;
  writeDisabled: boolean;
  loading: boolean;
  plan: ContainerOpsRestorePlan | null;
  onBackupIdChange: (value: string) => void;
  onGenerate: () => void;
  onApply: () => void;
}) {
  const { t } = useTranslation();
  const blockingCount = plan?.checks.filter((check) => check.blocking).length ?? 0;
  const warningCount =
    plan?.checks.filter((check) => check.severity === 'warning' && !check.blocking).length ?? 0;
  const statusTone: StatusTone = plan
    ? plan.status === 'blocked' || plan.status === 'restore_failed'
      ? 'error'
      : plan.status === 'ready_with_warnings'
        ? 'warning'
        : 'success'
    : 'muted';
  const statusLabel = plan
    ? t(`container_ops.restore_status.${plan.status}`, { defaultValue: plan.status })
    : t('container_ops.restore_plan_waiting');
  const createdAt =
    plan && plan.createdAt > 0 ? new Date(plan.createdAt * 1000).toLocaleString() : '-';
  const canApply = Boolean(plan && plan.status !== 'blocked' && plan.status !== 'restored');

  return (
    <section className={styles.restorePanel}>
      <div className={styles.sectionHeader}>
        <h2>{t('container_ops.restore_plan_title')}</h2>
        <StatusPill tone={statusTone}>{statusLabel}</StatusPill>
      </div>

      <div className={styles.restoreForm}>
        <Input
          label={t('container_ops.restore_backup_id')}
          value={backupId}
          placeholder="cpa-20260610T010203Z"
          disabled={disabled || loading}
          onChange={(event) => onBackupIdChange(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === 'Enter') onGenerate();
          }}
        />
        <Button
          variant="secondary"
          size="sm"
          onClick={onGenerate}
          loading={loading}
          disabled={disabled || loading}
        >
          <IconFileText size={15} />
          {t('container_ops.generate_restore_plan')}
        </Button>
        <Button
          variant="secondary"
          size="sm"
          onClick={onApply}
          loading={loading && canApply}
          disabled={disabled || writeDisabled || loading || !canApply}
        >
          <IconCheck size={15} />
          {t('container_ops.apply_restore')}
        </Button>
      </div>

      {plan ? (
        <>
          <KeyValueGrid
            items={[
              { label: t('container_ops.backup_id'), value: plan.backupId, copy: true },
              { label: t('container_ops.backup_root'), value: plan.backupRoot, copy: true },
              { label: t('container_ops.created_at'), value: createdAt },
              { label: t('container_ops.archives'), value: plan.archives.length },
              { label: t('container_ops.blocking_checks'), value: blockingCount },
              { label: t('container_ops.warnings'), value: warningCount },
              { label: t('container_ops.rollback_backup'), value: plan.rollbackBackup?.backupId, copy: true },
              { label: t('container_ops.destructive'), value: plan.destructive ? t('common.yes') : t('common.no') },
            ]}
          />

          <div className={styles.planGrid}>
            <RestoreChecks checks={plan.checks} />
            <RestoreSteps steps={plan.steps} />
          </div>
          {plan.actions?.length ? <RestoreActions actions={plan.actions} /> : null}
        </>
      ) : null}
    </section>
  );
}

function RollbackPanel({
  backupId,
  disabled,
  loading,
  result,
  onBackupIdChange,
  onApply,
}: {
  backupId: string;
  disabled: boolean;
  loading: boolean;
  result: ContainerOpsRestorePlan | null;
  onBackupIdChange: (value: string) => void;
  onApply: () => void;
}) {
  const { t } = useTranslation();
  const blockingCount = result?.checks.filter((check) => check.blocking).length ?? 0;
  const warningCount =
    result?.checks.filter((check) => check.severity === 'warning' && !check.blocking).length ?? 0;
  const statusTone: StatusTone = result
    ? result.status === 'blocked' || result.status === 'rollback_failed'
      ? 'error'
      : result.status === 'ready_with_warnings'
        ? 'warning'
        : 'success'
    : 'muted';
  const statusLabel = result
    ? t(`container_ops.restore_status.${result.status}`, { defaultValue: result.status })
    : t('container_ops.rollback_waiting');
  const createdAt =
    result && result.createdAt > 0 ? new Date(result.createdAt * 1000).toLocaleString() : '-';

  return (
    <section className={styles.restorePanel}>
      <div className={styles.sectionHeader}>
        <h2>{t('container_ops.rollback_title')}</h2>
        <StatusPill tone={statusTone}>{statusLabel}</StatusPill>
      </div>

      <div className={styles.restoreForm}>
        <Input
          label={t('container_ops.rollback_backup_id')}
          value={backupId}
          placeholder="rollback-cpa-20260610T010203Z"
          disabled={disabled || loading}
          onChange={(event) => onBackupIdChange(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === 'Enter') onApply();
          }}
        />
        <Button
          variant="secondary"
          size="sm"
          onClick={onApply}
          loading={loading}
          disabled={disabled || loading || !backupId.trim()}
        >
          <IconRefreshCw size={15} />
          {t('container_ops.apply_rollback')}
        </Button>
      </div>

      {result ? (
        <>
          <KeyValueGrid
            items={[
              { label: t('container_ops.backup_id'), value: result.backupId, copy: true },
              { label: t('container_ops.backup_root'), value: result.backupRoot, copy: true },
              { label: t('container_ops.created_at'), value: createdAt },
              { label: t('container_ops.archives'), value: result.archives.length },
              { label: t('container_ops.blocking_checks'), value: blockingCount },
              { label: t('container_ops.warnings'), value: warningCount },
              { label: t('container_ops.safety_backup'), value: result.rollbackBackup?.backupId, copy: true },
              { label: t('container_ops.destructive'), value: result.destructive ? t('common.yes') : t('common.no') },
            ]}
          />

          <div className={styles.planGrid}>
            <RestoreChecks checks={result.checks} titleKey="container_ops.rollback_checks" />
            <RestoreSteps steps={result.steps} titleKey="container_ops.rollback_steps" />
          </div>
          {result.actions?.length ? (
            <RestoreActions actions={result.actions} titleKey="container_ops.rollback_actions" />
          ) : null}
        </>
      ) : null}
    </section>
  );
}

function RestoreChecks({
  checks,
  titleKey = 'container_ops.restore_checks',
}: {
  checks: ContainerOpsRestoreCheck[];
  titleKey?: string;
}) {
  const { t } = useTranslation();
  return (
    <section className={styles.planSection}>
      <div className={styles.subHeader}>
        <h3>{t(titleKey)}</h3>
        <span>{t('container_ops.item_count', { count: checks.length })}</span>
      </div>
      <div className={styles.riskList}>
        {checks.map((check) => (
          <div className={styles.riskItem} key={`${check.code}-${check.resource || 'global'}`}>
            <StatusPill tone={restoreCheckTone(check)}>
              {t(`container_ops.severity.${check.severity}`, { defaultValue: check.severity })}
            </StatusPill>
            <div>
              <strong>
                {t(`container_ops.restore_check.${check.code}`, {
                  defaultValue: check.message,
                })}
              </strong>
              <span>{check.resource || check.code}</span>
            </div>
          </div>
        ))}
      </div>
    </section>
  );
}

function RestoreSteps({
  steps,
  titleKey = 'container_ops.restore_steps',
}: {
  steps: ContainerOpsRestorePlan['steps'];
  titleKey?: string;
}) {
  const { t } = useTranslation();
  return (
    <section className={styles.planSection}>
      <div className={styles.subHeader}>
        <h3>{t(titleKey)}</h3>
        <span>{t('container_ops.item_count', { count: steps.length })}</span>
      </div>
      <div className={styles.tableWrap}>
        <table className={styles.planTable}>
          <thead>
            <tr>
              <th>{t('container_ops.order')}</th>
              <th>{t('container_ops.step')}</th>
              <th>{t('container_ops.target')}</th>
              <th>{t('container_ops.destructive')}</th>
            </tr>
          </thead>
          <tbody>
            {steps.map((step) => (
              <tr key={`${step.order}-${step.code}`}>
                <td>{step.order}</td>
                <td>
                  {t(`container_ops.restore_step.${step.code}`, {
                    defaultValue: step.title,
                  })}
                </td>
                <td className={styles.monoCell}>{step.target || '-'}</td>
                <td>{step.destructive ? t('common.yes') : t('common.no')}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}

function RestoreActions({
  actions,
  titleKey = 'container_ops.restore_actions',
}: {
  actions: NonNullable<ContainerOpsRestorePlan['actions']>;
  titleKey?: string;
}) {
  const { t } = useTranslation();
  return (
    <section className={styles.planSection}>
      <div className={styles.subHeader}>
        <h3>{t(titleKey)}</h3>
        <span>{t('container_ops.item_count', { count: actions.length })}</span>
      </div>
      <div className={styles.tableWrap}>
        <table className={styles.planTable}>
          <thead>
            <tr>
              <th>{t('container_ops.order')}</th>
              <th>{t('container_ops.action_label')}</th>
              <th>{t('container_ops.target')}</th>
              <th>{t('container_ops.status')}</th>
            </tr>
          </thead>
          <tbody>
            {actions.map((action) => (
              <tr key={`${action.order}-${action.code}-${action.target || 'global'}`}>
                <td>{action.order}</td>
                <td>
                  {t(`container_ops.restore_action.${action.code}`, {
                    defaultValue: action.message || action.code,
                  })}
                </td>
                <td className={styles.monoCell}>{action.target || '-'}</td>
                <td>
                  <StatusPill tone={deployActionTone(action.status)}>
                    {t(`container_ops.restore_action_status.${action.status}`, {
                      defaultValue: action.status,
                    })}
                  </StatusPill>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}

function NetworkStandardizePanel({
  backupId,
  disabled,
  writeDisabled,
  loading,
  result,
  onBackupIdChange,
  onPlan,
  onApply,
}: {
  backupId: string;
  disabled: boolean;
  writeDisabled: boolean;
  loading: boolean;
  result: ContainerOpsNetworkStandardizeResult | null;
  onBackupIdChange: (value: string) => void;
  onPlan: () => void;
  onApply: () => void;
}) {
  const { t } = useTranslation();
  const blockingCount = result?.checks.filter((check) => check.blocking).length ?? 0;
  const plannedCount = result?.actions.filter((action) => action.status === 'planned').length ?? 0;
  const appliedCount = result?.actions.filter((action) => action.status === 'applied').length ?? 0;
  const statusTone: StatusTone = result
    ? result.status === 'blocked'
      ? 'error'
      : result.status.includes('warning')
        ? 'warning'
        : result.applied
          ? 'success'
          : 'muted'
    : 'muted';
  const statusLabel = result
    ? t(`container_ops.network_status.${result.status}`, { defaultValue: result.status })
    : t('container_ops.network_plan_waiting');

  return (
    <section className={styles.networkPanel}>
      <div className={styles.sectionHeader}>
        <h2>{t('container_ops.network_standardize_title')}</h2>
        <StatusPill tone={statusTone}>{statusLabel}</StatusPill>
      </div>

      <div className={styles.restoreForm}>
        <Input
          label={t('container_ops.network_backup_id')}
          value={backupId}
          placeholder="cpa-20260610T010203Z"
          disabled={disabled || loading}
          onChange={(event) => onBackupIdChange(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === 'Enter') onPlan();
          }}
        />
        <div className={styles.inlineActions}>
          <Button
            variant="secondary"
            size="sm"
            onClick={onPlan}
            loading={loading}
            disabled={disabled || loading}
          >
            <IconFileText size={15} />
            {t('container_ops.generate_network_plan')}
          </Button>
          <Button
            size="sm"
            onClick={onApply}
            loading={loading}
            disabled={disabled || writeDisabled || loading}
          >
            <IconCheck size={15} />
            {t('container_ops.apply_network_standardize')}
          </Button>
        </div>
      </div>

      {result ? (
        <>
          <KeyValueGrid
            items={[
              { label: t('container_ops.backup_id'), value: result.backupId, copy: true },
              { label: t('container_ops.network'), value: result.network, copy: true },
              { label: t('container_ops.blocking_checks'), value: blockingCount },
              { label: t('container_ops.planned_actions'), value: plannedCount },
              { label: t('container_ops.applied_actions'), value: appliedCount },
              { label: t('container_ops.destructive'), value: result.destructive ? t('common.yes') : t('common.no') },
            ]}
          />

          <div className={styles.planGrid}>
            <NetworkChecks checks={result.checks} />
            <NetworkActions actions={result.actions} />
          </div>
        </>
      ) : null}
    </section>
  );
}

function NetworkChecks({ checks }: { checks: ContainerOpsNetworkCheck[] }) {
  const { t } = useTranslation();
  return (
    <section className={styles.planSection}>
      <div className={styles.subHeader}>
        <h3>{t('container_ops.network_checks')}</h3>
        <span>{t('container_ops.item_count', { count: checks.length })}</span>
      </div>
      <div className={styles.riskList}>
        {checks.map((check) => (
          <div className={styles.riskItem} key={`${check.code}-${check.resource || 'global'}`}>
            <StatusPill tone={networkCheckTone(check)}>
              {t(`container_ops.severity.${check.severity}`, { defaultValue: check.severity })}
            </StatusPill>
            <div>
              <strong>
                {t(`container_ops.network_check.${check.code}`, {
                  defaultValue: check.message,
                })}
              </strong>
              <span>{check.resource || check.code}</span>
            </div>
          </div>
        ))}
      </div>
    </section>
  );
}

function NetworkActions({ actions }: { actions: ContainerOpsNetworkStandardizeResult['actions'] }) {
  const { t } = useTranslation();
  return (
    <section className={styles.planSection}>
      <div className={styles.subHeader}>
        <h3>{t('container_ops.network_actions')}</h3>
        <span>{t('container_ops.item_count', { count: actions.length })}</span>
      </div>
      <div className={styles.tableWrap}>
        <table className={styles.planTable}>
          <thead>
            <tr>
              <th>{t('container_ops.order')}</th>
              <th>{t('container_ops.action_label')}</th>
              <th>{t('container_ops.target')}</th>
              <th>{t('container_ops.status')}</th>
            </tr>
          </thead>
          <tbody>
            {actions.map((action) => (
              <tr key={`${action.order}-${action.code}-${action.target || 'global'}`}>
                <td>{action.order}</td>
                <td>
                  {t(`container_ops.network_action.${action.code}`, {
                    defaultValue: action.message || action.code,
                  })}
                </td>
                <td className={styles.monoCell}>{action.target || '-'}</td>
                <td>
                  {t(`container_ops.network_action_status.${action.status}`, {
                    defaultValue: action.status,
                  })}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}

function UpgradePreparePanel({
  cpaImage,
  cpampImage,
  disabled,
  writeDisabled,
  loading,
  plan,
  onCpaImageChange,
  onCpampImageChange,
  onPlan,
  onPrepare,
}: {
  cpaImage: string;
  cpampImage: string;
  disabled: boolean;
  writeDisabled: boolean;
  loading: boolean;
  plan: ContainerOpsUpgradePlan | null;
  onCpaImageChange: (value: string) => void;
  onCpampImageChange: (value: string) => void;
  onPlan: () => void;
  onPrepare: () => void;
}) {
  const { t } = useTranslation();
  const blockingCount = plan?.checks.filter((check) => check.blocking).length ?? 0;
  const warningCount =
    plan?.checks.filter((check) => check.severity === 'warning' && !check.blocking).length ?? 0;
  const statusTone: StatusTone = plan
    ? plan.status === 'blocked' || plan.status.includes('failed')
      ? 'error'
      : plan.status.includes('warning')
        ? 'warning'
        : plan.applied
          ? 'success'
          : 'muted'
    : 'muted';
  const statusLabel = plan
    ? t(`container_ops.upgrade_status.${plan.status}`, { defaultValue: plan.status })
    : t('container_ops.upgrade_plan_waiting');
  const canPrepare = Boolean(plan && plan.status !== 'blocked' && plan.status !== 'prepared');

  return (
    <section className={styles.upgradePanel}>
      <div className={styles.sectionHeader}>
        <h2>{t('container_ops.upgrade_prepare_title')}</h2>
        <StatusPill tone={statusTone}>{statusLabel}</StatusPill>
      </div>

      <div className={styles.upgradeForm}>
        <Input
          label={t('container_ops.upgrade_cpa_image')}
          value={cpaImage}
          placeholder="seakee/cli-proxy-api:latest"
          disabled={disabled || loading}
          onChange={(event) => onCpaImageChange(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === 'Enter') onPlan();
          }}
        />
        <Input
          label={t('container_ops.upgrade_cpamp_image')}
          value={cpampImage}
          placeholder="seakee/cpa-manager-plus:latest"
          disabled={disabled || loading}
          onChange={(event) => onCpampImageChange(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === 'Enter') onPlan();
          }}
        />
        <div className={styles.inlineActions}>
          <Button
            variant="secondary"
            size="sm"
            onClick={onPlan}
            loading={loading}
            disabled={disabled || loading}
          >
            <IconFileText size={15} />
            {t('container_ops.generate_upgrade_plan')}
          </Button>
          <Button
            size="sm"
            onClick={onPrepare}
            loading={loading && canPrepare}
            disabled={disabled || writeDisabled || loading || !canPrepare}
          >
            <IconDownload size={15} />
            {t('container_ops.prepare_upgrade')}
          </Button>
        </div>
      </div>

      {plan ? (
        <>
          <KeyValueGrid
            items={[
              { label: t('container_ops.upgrade_cpa_image'), value: plan.cpaImage, copy: true },
              { label: t('container_ops.upgrade_cpamp_image'), value: plan.cpampImage, copy: true },
              { label: t('container_ops.blocking_checks'), value: blockingCount },
              { label: t('container_ops.warnings'), value: warningCount },
              { label: t('container_ops.rollback_backup'), value: plan.rollbackBackup?.backupId, copy: true },
              { label: t('container_ops.image_pulls'), value: plan.imagePulls?.length ?? 0 },
              { label: t('container_ops.destructive'), value: plan.destructive ? t('common.yes') : t('common.no') },
            ]}
          />

          <div className={styles.planGrid}>
            <UpgradeChecks checks={plan.checks} />
            <UpgradeSteps steps={plan.steps} />
          </div>
          {plan.imagePulls?.length ? <UpgradeImagePulls imagePulls={plan.imagePulls} /> : null}
          {plan.actions?.length ? <UpgradeActions actions={plan.actions} /> : null}
        </>
      ) : null}
    </section>
  );
}

function UpgradeChecks({ checks }: { checks: ContainerOpsUpgradeCheck[] }) {
  const { t } = useTranslation();
  return (
    <section className={styles.planSection}>
      <div className={styles.subHeader}>
        <h3>{t('container_ops.upgrade_checks')}</h3>
        <span>{t('container_ops.item_count', { count: checks.length })}</span>
      </div>
      <div className={styles.riskList}>
        {checks.map((check) => (
          <div className={styles.riskItem} key={`${check.code}-${check.resource || 'global'}`}>
            <StatusPill tone={upgradeCheckTone(check)}>
              {t(`container_ops.severity.${check.severity}`, { defaultValue: check.severity })}
            </StatusPill>
            <div>
              <strong>
                {t(`container_ops.upgrade_check.${check.code}`, {
                  defaultValue: check.message,
                })}
              </strong>
              <span>{check.resource || check.code}</span>
            </div>
          </div>
        ))}
      </div>
    </section>
  );
}

function UpgradeSteps({ steps }: { steps: ContainerOpsUpgradeStep[] }) {
  const { t } = useTranslation();
  return (
    <section className={styles.planSection}>
      <div className={styles.subHeader}>
        <h3>{t('container_ops.upgrade_steps')}</h3>
        <span>{t('container_ops.item_count', { count: steps.length })}</span>
      </div>
      <div className={styles.tableWrap}>
        <table className={styles.planTable}>
          <thead>
            <tr>
              <th>{t('container_ops.order')}</th>
              <th>{t('container_ops.step')}</th>
              <th>{t('container_ops.target')}</th>
              <th>{t('container_ops.destructive')}</th>
            </tr>
          </thead>
          <tbody>
            {steps.map((step) => (
              <tr key={`${step.order}-${step.code}`}>
                <td>{step.order}</td>
                <td>
                  {t(`container_ops.upgrade_step.${step.code}`, {
                    defaultValue: step.title,
                  })}
                </td>
                <td className={styles.monoCell}>{step.target || '-'}</td>
                <td>{step.destructive ? t('common.yes') : t('common.no')}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}

function UpgradeImagePulls({
  imagePulls,
}: {
  imagePulls: NonNullable<ContainerOpsUpgradePlan['imagePulls']>;
}) {
  const { t } = useTranslation();
  return (
    <section className={styles.planSection}>
      <div className={styles.subHeader}>
        <h3>{t('container_ops.upgrade_image_pulls')}</h3>
        <span>{t('container_ops.item_count', { count: imagePulls.length })}</span>
      </div>
      <div className={styles.backupArchiveList}>
        {imagePulls.map((pull) => (
          <div className={styles.backupArchiveItem} key={`${pull.image}-${pull.status}`}>
            <div>
              <strong>{pull.image}</strong>
              <span>{pull.message || t(`container_ops.deploy_image_pull_status.${pull.status}`, { defaultValue: pull.status })}</span>
            </div>
            <StatusPill tone={pull.status === 'pulled' ? 'success' : 'muted'}>
              {t(`container_ops.deploy_image_pull_status.${pull.status}`, { defaultValue: pull.status })}
            </StatusPill>
          </div>
        ))}
      </div>
    </section>
  );
}

function UpgradeActions({ actions }: { actions: ContainerOpsUpgradeAction[] }) {
  const { t } = useTranslation();
  return (
    <section className={styles.planSection}>
      <div className={styles.subHeader}>
        <h3>{t('container_ops.upgrade_actions')}</h3>
        <span>{t('container_ops.item_count', { count: actions.length })}</span>
      </div>
      <div className={styles.tableWrap}>
        <table className={styles.planTable}>
          <thead>
            <tr>
              <th>{t('container_ops.order')}</th>
              <th>{t('container_ops.action_label')}</th>
              <th>{t('container_ops.target')}</th>
              <th>{t('container_ops.status')}</th>
            </tr>
          </thead>
          <tbody>
            {actions.map((action) => (
              <tr key={`${action.order}-${action.code}-${action.target || 'global'}`}>
                <td>{action.order}</td>
                <td>
                  {t(`container_ops.upgrade_action.${action.code}`, {
                    defaultValue: action.message || action.code,
                  })}
                </td>
                <td className={styles.monoCell}>{action.target || '-'}</td>
                <td>
                  <StatusPill tone={deployActionTone(action.status)}>
                    {t(`container_ops.upgrade_action_status.${action.status}`, {
                      defaultValue: action.status,
                    })}
                  </StatusPill>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}

function DeployPlanPanel({
  disabled,
  loading,
  plan,
  onRender,
  onPullImages,
  onStartServices,
}: {
  disabled: boolean;
  loading: boolean;
  plan: ContainerOpsDeployPlan;
  onRender: () => void;
  onPullImages: () => void;
  onStartServices: () => void;
}) {
  const { t } = useTranslation();
  const blockingCount = plan.checks.filter((check) => check.blocking).length;
  const warningCount = plan.checks.filter((check) => check.severity === 'warning').length;
  const statusTone: StatusTone =
    plan.status === 'blocked' || plan.status === 'start_failed'
      ? 'error'
      : plan.status === 'ready_with_warnings'
        ? 'warning'
        : 'success';
  const renderDisabled = disabled || loading || plan.status === 'blocked' || Boolean(plan.files?.length);
  const canPullImages = plan.status === 'rendered' || Boolean(plan.files?.length);
  const pullImagesDisabled =
    disabled || loading || plan.status === 'blocked' || !canPullImages || plan.status === 'images_pulled';
  const canStartServices = plan.status === 'images_pulled' || Boolean(plan.imagePulls?.length);
  const startServicesDisabled =
    disabled ||
    loading ||
    plan.status === 'blocked' ||
    plan.status === 'started' ||
    !canStartServices;

  return (
    <section className={styles.deployPanel}>
      <div className={styles.sectionHeader}>
        <h2>{t('container_ops.deploy_plan_title')}</h2>
        <div className={styles.inlineActions}>
          <StatusPill tone={statusTone}>
            {t(`container_ops.deploy_status.${plan.status}`, { defaultValue: plan.status })}
          </StatusPill>
          <Button
            size="sm"
            variant="secondary"
            loading={loading}
            disabled={renderDisabled}
            onClick={onRender}
          >
            <IconCheck size={15} />
            {t('container_ops.render_deploy_files')}
          </Button>
          <Button
            size="sm"
            variant="secondary"
            loading={loading && canPullImages}
            disabled={pullImagesDisabled}
            onClick={onPullImages}
          >
            <IconDownload size={15} />
            {t('container_ops.pull_deploy_images')}
          </Button>
          <Button
            size="sm"
            variant="secondary"
            loading={loading && canStartServices}
            disabled={startServicesDisabled}
            onClick={onStartServices}
          >
            <IconPlay size={15} />
            {t('container_ops.start_deploy_services')}
          </Button>
        </div>
      </div>

      <KeyValueGrid
        items={[
          { label: t('container_ops.compose_project'), value: plan.manifest.composeProject },
          { label: t('container_ops.network'), value: plan.manifest.network, copy: true },
          { label: t('container_ops.stack_root'), value: plan.manifest.stackRoot, copy: true },
          { label: t('container_ops.backup_root'), value: plan.manifest.backupRoot, copy: true },
          { label: t('container_ops.newapi_base_url'), value: plan.manifest.newApiBaseUrl, copy: true },
          { label: t('container_ops.blocking_checks'), value: blockingCount },
          { label: t('container_ops.warnings'), value: warningCount },
          { label: t('container_ops.destructive'), value: plan.destructive ? t('common.yes') : t('common.no') },
        ]}
      />

      <div className={styles.planGrid}>
        <DeployChecks checks={plan.checks} />
        <DeploySteps steps={plan.steps} />
      </div>

      {plan.files?.length ? <DeployFiles files={plan.files} /> : null}
      {plan.imagePulls?.length ? <DeployImagePulls imagePulls={plan.imagePulls} /> : null}
      {plan.actions?.length ? <DeployActions actions={plan.actions} /> : null}

      <ComposeDraftBlock compose={plan.compose} />
    </section>
  );
}

function DeployChecks({ checks }: { checks: ContainerOpsDeployCheck[] }) {
  const { t } = useTranslation();
  return (
    <section className={styles.planSection}>
      <div className={styles.subHeader}>
        <h3>{t('container_ops.deploy_checks')}</h3>
        <span>{t('container_ops.item_count', { count: checks.length })}</span>
      </div>
      <div className={styles.riskList}>
        {checks.map((check) => (
          <div className={styles.riskItem} key={`${check.code}-${check.resource || 'global'}`}>
            <StatusPill tone={deployCheckTone(check)}>
              {t(`container_ops.severity.${check.severity}`, { defaultValue: check.severity })}
            </StatusPill>
            <div>
              <strong>
                {t(`container_ops.deploy_check.${check.code}`, {
                  defaultValue: check.message,
                })}
              </strong>
              <span>{check.resource || check.code}</span>
            </div>
          </div>
        ))}
      </div>
    </section>
  );
}

function DeployFiles({ files }: { files: NonNullable<ContainerOpsDeployPlan['files']> }) {
  const { t } = useTranslation();
  return (
    <section className={styles.planSection}>
      <div className={styles.subHeader}>
        <h3>{t('container_ops.deploy_files')}</h3>
        <span>{t('container_ops.item_count', { count: files.length })}</span>
      </div>
      <div className={styles.backupArchiveList}>
        {files.map((file) => (
          <div className={styles.backupArchiveItem} key={`${file.kind}-${file.path}`}>
            <div>
              <strong>{file.path}</strong>
              <span>{t(`container_ops.deploy_file_kind.${file.kind}`, { defaultValue: file.kind })}</span>
            </div>
            <div className={styles.candidateMeta}>
              <span>{formatBytes(file.size)}</span>
            </div>
          </div>
        ))}
      </div>
    </section>
  );
}

function DeployImagePulls({ imagePulls }: { imagePulls: NonNullable<ContainerOpsDeployPlan['imagePulls']> }) {
  const { t } = useTranslation();
  return (
    <section className={styles.planSection}>
      <div className={styles.subHeader}>
        <h3>{t('container_ops.deploy_image_pulls')}</h3>
        <span>{t('container_ops.item_count', { count: imagePulls.length })}</span>
      </div>
      <div className={styles.backupArchiveList}>
        {imagePulls.map((pull) => (
          <div className={styles.backupArchiveItem} key={`${pull.image}-${pull.status}`}>
            <div>
              <strong>{pull.image}</strong>
              <span>{pull.message || t(`container_ops.deploy_image_pull_status.${pull.status}`, { defaultValue: pull.status })}</span>
            </div>
            <StatusPill tone={pull.status === 'pulled' ? 'success' : 'muted'}>
              {t(`container_ops.deploy_image_pull_status.${pull.status}`, { defaultValue: pull.status })}
            </StatusPill>
          </div>
        ))}
      </div>
    </section>
  );
}

function DeployActions({ actions }: { actions: NonNullable<ContainerOpsDeployPlan['actions']> }) {
  const { t } = useTranslation();
  return (
    <section className={styles.planSection}>
      <div className={styles.subHeader}>
        <h3>{t('container_ops.deploy_actions')}</h3>
        <span>{t('container_ops.item_count', { count: actions.length })}</span>
      </div>
      <div className={styles.tableWrap}>
        <table className={styles.planTable}>
          <thead>
            <tr>
              <th>{t('container_ops.order')}</th>
              <th>{t('container_ops.action_label')}</th>
              <th>{t('container_ops.target')}</th>
              <th>{t('container_ops.status')}</th>
            </tr>
          </thead>
          <tbody>
            {actions.map((action) => (
              <tr key={`${action.order}-${action.code}-${action.target || 'global'}`}>
                <td>{action.order}</td>
                <td>
                  {t(`container_ops.deploy_action.${action.code}`, {
                    defaultValue: action.message || action.code,
                  })}
                </td>
                <td className={styles.monoCell}>{action.target || '-'}</td>
                <td>
                  <StatusPill tone={deployActionTone(action.status)}>
                    {t(`container_ops.deploy_action_status.${action.status}`, {
                      defaultValue: action.status,
                    })}
                  </StatusPill>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}

function DeploySteps({ steps }: { steps: ContainerOpsDeployStep[] }) {
  const { t } = useTranslation();
  return (
    <section className={styles.planSection}>
      <div className={styles.subHeader}>
        <h3>{t('container_ops.deploy_steps')}</h3>
        <span>{t('container_ops.item_count', { count: steps.length })}</span>
      </div>
      <div className={styles.tableWrap}>
        <table className={styles.planTable}>
          <thead>
            <tr>
              <th>{t('container_ops.order')}</th>
              <th>{t('container_ops.step')}</th>
              <th>{t('container_ops.target')}</th>
              <th>{t('container_ops.destructive')}</th>
            </tr>
          </thead>
          <tbody>
            {steps.map((step) => (
              <tr key={`${step.order}-${step.code}`}>
                <td>{step.order}</td>
                <td>
                  {t(`container_ops.deploy_step.${step.code}`, {
                    defaultValue: step.title,
                  })}
                </td>
                <td className={styles.monoCell}>{step.target || '-'}</td>
                <td>{step.destructive ? t('common.yes') : t('common.no')}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}

function ComposeDraftBlock({ compose }: { compose: ContainerOpsComposeDraft }) {
  const { t } = useTranslation();
  return (
    <div className={styles.composeDraft}>
      <div className={styles.subHeader}>
        <h3>{t('container_ops.compose_draft')}</h3>
        <span className={styles.composeFile}>
          {compose.fileName}
          <CopyButton value={compose.content} />
        </span>
      </div>
      <pre>
        <code>{compose.content}</code>
      </pre>
    </div>
  );
}

function ImportPlanPanel({ plan }: { plan: ContainerOpsImportPlan }) {
  const { t } = useTranslation();
  const statusTone: StatusTone = plan.summary.ready ? 'success' : 'warning';
  const statusLabel = plan.summary.ready
    ? t('container_ops.import_plan_ready')
    : t('container_ops.import_plan_blocked');

  return (
    <section className={styles.importPlanPanel}>
      <div className={styles.sectionHeader}>
        <h2>{t('container_ops.import_plan_title')}</h2>
        <StatusPill tone={statusTone}>{statusLabel}</StatusPill>
      </div>

      <div className={styles.importSummaryGrid}>
        <ImportMetric label="CPA" active={plan.summary.cpaFound} />
        <ImportMetric label="CPAMP" active={plan.summary.cpampFound} />
        <ImportMetric label={t('container_ops.agent_service')} active={plan.summary.agentFound} />
        <ImportMetric label="NewAPI" active={plan.summary.newApiFound} />
        <ImportMetric
          label={t('container_ops.risks')}
          value={plan.summary.riskCount}
          active={plan.summary.blockingRiskCount === 0}
        />
        <ImportMetric
          label={t('container_ops.blocking_risks')}
          value={plan.summary.blockingRiskCount}
          active={plan.summary.blockingRiskCount === 0}
        />
      </div>

      <KeyValueGrid
        items={[
          { label: t('container_ops.compose_project'), value: plan.manifest.composeProject },
          { label: t('container_ops.network'), value: plan.manifest.network },
          { label: t('container_ops.stack_root'), value: plan.manifest.stackRoot },
          { label: t('container_ops.backup_root'), value: plan.manifest.backupRoot },
          { label: t('container_ops.newapi_base_url'), value: plan.manifest.newApiBaseUrl, copy: true },
        ]}
      />

      <div className={styles.planGrid}>
        <PlanServices services={plan.manifest.services} />
        <PlanCandidates candidates={plan.candidates} />
      </div>

      <RiskList risks={plan.risks} />
      <NextActionList actions={plan.nextActions} />

      <ComposeDraftBlock compose={plan.compose} />
    </section>
  );
}

function ImportMetric({
  label,
  active,
  value,
}: {
  label: string;
  active: boolean;
  value?: string | number;
}) {
  const { t } = useTranslation();
  return (
    <div className={styles.importMetric}>
      <span>{label}</span>
      <strong>{value ?? (active ? t('common.yes') : t('common.no'))}</strong>
    </div>
  );
}

function PlanServices({ services }: { services: ContainerOpsManifestService[] }) {
  const { t } = useTranslation();
  return (
    <section className={styles.planSection}>
      <div className={styles.subHeader}>
        <h3>{t('container_ops.services_to_manage')}</h3>
        <span>{t('container_ops.item_count', { count: services.length })}</span>
      </div>
      <div className={styles.tableWrap}>
        <table className={styles.planTable}>
          <thead>
            <tr>
              <th>{t('container_ops.role')}</th>
              <th>{t('container_ops.target_service')}</th>
              <th>{t('container_ops.source_container')}</th>
              <th>{t('container_ops.state')}</th>
              <th>{t('container_ops.image')}</th>
              <th>{t('container_ops.include_compose')}</th>
            </tr>
          </thead>
          <tbody>
            {services.map((service) => (
              <tr key={`${service.role}-${service.service}`}>
                <td>
                  <span className={styles.roleBadge}>
                    {t(`container_ops.role_value.${service.role}`, { defaultValue: service.role })}
                  </span>
                </td>
                <td className={styles.monoCell}>{service.service}</td>
                <td className={styles.monoCell}>{service.sourceContainer || '-'}</td>
                <td>{service.state || '-'}</td>
                <td className={styles.monoCell}>{service.image || '-'}</td>
                <td>{service.includeInCompose ? t('common.yes') : t('common.no')}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}

function PlanCandidates({ candidates }: { candidates: ContainerOpsImportCandidate[] }) {
  const { t } = useTranslation();
  return (
    <section className={styles.planSection}>
      <div className={styles.subHeader}>
        <h3>{t('container_ops.import_candidates')}</h3>
        <span>{t('container_ops.item_count', { count: candidates.length })}</span>
      </div>
      {candidates.length ? (
        <div className={styles.candidateList}>
          {candidates.map((candidate) => (
            <div className={styles.candidateItem} key={`${candidate.role}-${candidate.name}`}>
              <div>
                <strong>{candidate.name}</strong>
                <span>{candidate.image || '-'}</span>
              </div>
              <div className={styles.candidateMeta}>
                <span className={styles.roleBadge}>
                  {t(`container_ops.role_value.${candidate.role}`, { defaultValue: candidate.role })}
                </span>
                <span>{candidate.managed ? t('container_ops.managed') : t('container_ops.unmanaged')}</span>
                <span>{joinValues(candidate.networks)}</span>
              </div>
            </div>
          ))}
        </div>
      ) : (
        <EmptyState text={t('container_ops.no_import_candidates')} />
      )}
    </section>
  );
}

function RiskList({ risks }: { risks: ContainerOpsImportRisk[] }) {
  const { t } = useTranslation();
  return (
    <section className={styles.planSection}>
      <div className={styles.subHeader}>
        <h3>{t('container_ops.risk_inventory')}</h3>
        <span>{t('container_ops.item_count', { count: risks.length })}</span>
      </div>
      <div className={styles.riskList}>
        {risks.map((risk) => (
          <div className={styles.riskItem} key={`${risk.code}-${risk.resource || 'global'}`}>
            <StatusPill tone={riskTone(risk)}>
              {t(`container_ops.severity.${risk.severity}`, { defaultValue: risk.severity })}
            </StatusPill>
            <div>
              <strong>
                {t(`container_ops.risk.${risk.code}`, { defaultValue: risk.message })}
              </strong>
              <span>{risk.resource || risk.code}</span>
            </div>
          </div>
        ))}
      </div>
    </section>
  );
}

function NextActionList({ actions }: { actions: string[] }) {
  const { t } = useTranslation();
  if (!actions.length) return null;
  return (
    <section className={styles.planSection}>
      <div className={styles.subHeader}>
        <h3>{t('container_ops.next_actions')}</h3>
      </div>
      <div className={styles.actionChips}>
        {actions.map((action) => (
          <span key={action}>
            {t(`container_ops.next_action.${action}`, { defaultValue: action })}
          </span>
        ))}
      </div>
    </section>
  );
}

function ContainerRow({ container }: { container: ContainerOpsDockerContainer }) {
  const { t } = useTranslation();
  const stateTone: StatusTone =
    container.state === 'running' ? 'success' : container.state === 'exited' ? 'muted' : 'warning';
  const role = container.role
    ? t(`container_ops.role_value.${container.role}`, { defaultValue: container.role })
    : '-';

  return (
    <tr>
      <td>
        <div className={styles.primaryCell}>
          <strong>{container.name || '-'}</strong>
          <span>{container.id}</span>
        </div>
      </td>
      <td>
        <span className={styles.roleBadge}>{role}</span>
      </td>
      <td>
        <StatusPill tone={stateTone}>{container.state || '-'}</StatusPill>
      </td>
      <td className={styles.monoCell}>{container.image || '-'}</td>
      <td className={styles.monoCell}>{joinPorts(container)}</td>
      <td className={styles.monoCell}>{joinNetworks(container)}</td>
      <td className={styles.monoCell}>{primaryMounts(container)}</td>
    </tr>
  );
}

function EmptyState({ text }: { text: string }) {
  return <div className={styles.emptyState}>{text}</div>;
}

function ResourceList<T>({
  title,
  count,
  items,
  emptyText,
  renderItem,
}: {
  title: string;
  count: number;
  items: T[];
  emptyText: string;
  renderItem: (item: T) => ReactNode;
}) {
  const { t } = useTranslation();
  return (
    <section className={styles.listPanel}>
      <div className={styles.sectionHeader}>
        <h2>{title}</h2>
        <span>{t('container_ops.item_count', { count })}</span>
      </div>
      {items.length ? (
        <div className={styles.resourceList}>{items.map(renderItem)}</div>
      ) : (
        <EmptyState text={emptyText} />
      )}
    </section>
  );
}

function NetworkItem({ network }: { network: ContainerOpsDockerNetwork }) {
  const { t } = useTranslation();
  return (
    <div className={styles.resourceItem}>
      <div>
        <strong>{network.name}</strong>
        <span>
          {network.driver || '-'} / {network.scope || '-'}
        </span>
      </div>
      <div className={styles.resourceMeta}>
        {network.managed ? <IconCheck size={14} /> : <IconX size={14} />}
        <span>{network.managed ? t('container_ops.managed') : t('container_ops.unmanaged')}</span>
        <span>{t('container_ops.attached_containers', { count: network.containers })}</span>
      </div>
    </div>
  );
}

function ImageItem({ image }: { image: ContainerOpsDockerImage }) {
  return (
    <div className={styles.resourceItem}>
      <div>
        <strong>{image.repoTags?.[0] || image.id || '-'}</strong>
        <span>{image.repoTags?.slice(1).join(', ') || image.id}</span>
      </div>
      <div className={styles.resourceMeta}>
        <span>{formatBytes(image.size)}</span>
      </div>
    </div>
  );
}
