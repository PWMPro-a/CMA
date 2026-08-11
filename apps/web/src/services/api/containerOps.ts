import { apiClient } from './client';

export interface ContainerOpsAgentInfo {
  configured: boolean;
  reachable: boolean;
  baseUrl?: string;
  service?: string;
  version?: string;
  mode?: string;
  dockerHost?: string;
  readOnly?: boolean;
  error?: string;
}

export interface ContainerOpsStandardResource {
  composeProject: string;
  network: string;
  cpaService: string;
  cpampService: string;
  agentService: string;
  stackRoot: string;
  backupRoot: string;
}

export interface ContainerOpsNewApiInfo {
  recommendedBaseUrl: string;
}

export interface ContainerOpsLifecycleState {
  operationId?: string;
  operation?: string;
  phase?: string;
  status: string;
  active: boolean;
  startedAt?: number;
  finishedAt?: number;
  message?: string;
}

export interface ContainerOpsAuditEntry {
  id: number;
  operationId: string;
  operation: string;
  phase?: string;
  status: string;
  backupId?: string;
  agentBaseUrl?: string;
  message?: string;
  error?: string;
  request?: Record<string, unknown>;
  result?: Record<string, unknown>;
  startedAtMs: number;
  finishedAtMs?: number;
  durationMs?: number;
  createdAtMs: number;
  updatedAtMs: number;
}

export interface ContainerOpsAuditResponse {
  items: ContainerOpsAuditEntry[];
}

export interface ContainerOpsUpgradeTask {
  id: number;
  taskId: string;
  operationId?: string;
  status: string;
  phase?: string;
  cpaImage?: string;
  cpampImage?: string;
  rollbackBackupId?: string;
  agentBaseUrl?: string;
  message?: string;
  error?: string;
  nextAction?: string;
  request?: Record<string, unknown>;
  result?: Record<string, unknown>;
  startedAtMs: number;
  finishedAtMs?: number;
  createdAtMs: number;
  updatedAtMs: number;
}

export interface ContainerOpsUpgradeTaskResponse {
  items: ContainerOpsUpgradeTask[];
}

export interface ContainerOpsInfo {
  enabled: boolean;
  mode: string;
  agent: ContainerOpsAgentInfo;
  standardResources: ContainerOpsStandardResource;
  newApi: ContainerOpsNewApiInfo;
  managedStack: string;
  supportedScopes: string[];
  unsupportedScopes: string[];
  destructiveActions: boolean;
  lifecycle: ContainerOpsLifecycleState;
}

export interface ContainerOpsEgressAddress {
  interface: string;
  address: string;
  cidr: string;
  scope?: string;
}

export interface ContainerOpsEgressCheck {
  severity: 'info' | 'warning' | 'error' | string;
  code: string;
  message: string;
  resource?: string;
  blocking: boolean;
}

export interface ContainerOpsEgressAction {
  order: number;
  code: string;
  target?: string;
  status: string;
  message?: string;
  output?: string;
}

export interface ContainerOpsEgressIPInventory {
  agent?: ContainerOpsAgentInfo;
  defaultInterface?: string;
  route?: string;
  nativeOutboundIp?: string;
  addresses: ContainerOpsEgressAddress[];
  checks?: ContainerOpsEgressCheck[];
}

export interface ContainerOpsSourceIPRequest {
  sourceIp: string;
  interface?: string;
  verifyUrl?: string;
}

export interface ContainerOpsSourceIPResult {
  agent?: ContainerOpsAgentInfo;
  sourceIp: string;
  interface?: string;
  status: string;
  mounted: boolean;
  alreadyPresent: boolean;
  removed?: boolean;
  outboundIp?: string;
  nativeOutboundIp?: string;
  checks: ContainerOpsEgressCheck[];
  actions?: ContainerOpsEgressAction[];
  lifecycle?: ContainerOpsLifecycleState;
}

export interface ContainerOpsDockerSummary {
  containerCount: number;
  runningCount: number;
  networkCount: number;
  imageCount: number;
  cpaCount: number;
  cpampCount: number;
  newApiCount: number;
  managedCount: number;
}

export interface ContainerOpsDockerPort {
  privatePort: number;
  publicPort?: number;
  type?: string;
  ip?: string;
}

export interface ContainerOpsDockerMount {
  type: string;
  name?: string;
  source?: string;
  destination: string;
  mode?: string;
  rw: boolean;
}

export interface ContainerOpsDockerAttachment {
  name: string;
  networkId?: string;
  ipAddress?: string;
  gateway?: string;
}

export interface ContainerOpsDockerContainer {
  id: string;
  name: string;
  image: string;
  imageId?: string;
  state: string;
  status: string;
  role?: string;
  managed: boolean;
  labels?: Record<string, string>;
  ports?: ContainerOpsDockerPort[];
  mounts?: ContainerOpsDockerMount[];
  networks?: ContainerOpsDockerAttachment[];
}

export interface ContainerOpsDockerNetwork {
  id: string;
  name: string;
  driver: string;
  scope: string;
  internal: boolean;
  attachable: boolean;
  managed: boolean;
  labels?: Record<string, string>;
  containers: number;
}

export interface ContainerOpsDockerImage {
  id: string;
  repoTags: string[];
  size: number;
  created: number;
  labels?: Record<string, string>;
}

export interface ContainerOpsDockerOverview {
  summary: ContainerOpsDockerSummary;
  containers: ContainerOpsDockerContainer[];
  networks: ContainerOpsDockerNetwork[];
  images: ContainerOpsDockerImage[];
}

export interface ContainerOpsDiscovery {
  agent: ContainerOpsAgentInfo;
  docker: ContainerOpsDockerOverview;
  newApi: ContainerOpsNewApiInfo;
  recommendedAction: string;
}

export interface ContainerOpsImportSummary {
  ready: boolean;
  cpaFound: boolean;
  cpampFound: boolean;
  agentFound: boolean;
  newApiFound: boolean;
  riskCount: number;
  blockingRiskCount: number;
}

export interface ContainerOpsImportCandidate {
  role: string;
  containerId?: string;
  name: string;
  image: string;
  state: string;
  managed: boolean;
  targetService: string;
  includeInCompose: boolean;
  networks?: string[];
  ports?: ContainerOpsDockerPort[];
  mounts?: ContainerOpsDockerMount[];
  reasons?: string[];
}

export interface ContainerOpsManifestService {
  role: string;
  service: string;
  sourceContainer?: string;
  image?: string;
  state?: string;
  managed: boolean;
  includeInCompose: boolean;
  internalBaseUrl?: string;
  networks?: string[];
  mounts?: string[];
  ports?: string[];
}

export interface ContainerOpsManifestVolume {
  name: string;
  type: string;
  source?: string;
  destination: string;
  external: boolean;
}

export interface ContainerOpsStackManifest {
  stack: string;
  composeProject: string;
  network: string;
  stackRoot: string;
  backupRoot: string;
  newApiBaseUrl: string;
  services: ContainerOpsManifestService[];
  volumes?: ContainerOpsManifestVolume[];
}

export interface ContainerOpsComposeDraft {
  fileName: string;
  projectName: string;
  networkName: string;
  services: string[];
  content: string;
}

export interface ContainerOpsImportRisk {
  severity: 'info' | 'warning' | 'error' | string;
  code: string;
  message: string;
  resource?: string;
  blocking: boolean;
}

export interface ContainerOpsImportPlan {
  agent: ContainerOpsAgentInfo;
  summary: ContainerOpsImportSummary;
  manifest: ContainerOpsStackManifest;
  compose: ContainerOpsComposeDraft;
  candidates: ContainerOpsImportCandidate[];
  risks: ContainerOpsImportRisk[];
  nextActions: string[];
  newApi: ContainerOpsNewApiInfo;
  readOnly: boolean;
}

export interface ContainerOpsDeployRequest {
  apply: boolean;
  action?: string;
}

export interface ContainerOpsDeployCheck {
  severity: 'info' | 'warning' | 'error' | string;
  code: string;
  message: string;
  resource?: string;
  blocking: boolean;
}

export interface ContainerOpsDeployStep {
  order: number;
  code: string;
  title: string;
  target?: string;
  destructive: boolean;
}

export interface ContainerOpsDeployFile {
  path: string;
  kind: string;
  size: number;
}

export interface ContainerOpsImagePull {
  image: string;
  status: string;
  message?: string;
}

export interface ContainerOpsDeployAction {
  order: number;
  code: string;
  target?: string;
  status: string;
  message?: string;
}

export interface ContainerOpsDeployPlan {
  agent?: ContainerOpsAgentInfo;
  status: string;
  manifest: ContainerOpsStackManifest;
  compose: ContainerOpsComposeDraft;
  checks: ContainerOpsDeployCheck[];
  steps: ContainerOpsDeployStep[];
  files?: ContainerOpsDeployFile[];
  imagePulls?: ContainerOpsImagePull[];
  actions?: ContainerOpsDeployAction[];
  applied: boolean;
  destructive: boolean;
  readOnly: boolean;
  overview?: ContainerOpsDockerOverview;
  lifecycle?: ContainerOpsLifecycleState;
}

export interface ContainerOpsBackupArchive {
  role: string;
  service: string;
  container: string;
  path: string;
  fileName: string;
  size: number;
}

export interface ContainerOpsBackupWarning {
  code: string;
  message: string;
  resource?: string;
}

export interface ContainerOpsBackupResult {
  agent?: ContainerOpsAgentInfo;
  backupId: string;
  status: string;
  backupRoot: string;
  createdAt: number;
  archives: ContainerOpsBackupArchive[];
  warnings?: ContainerOpsBackupWarning[];
  readOnly: boolean;
  lifecycle?: ContainerOpsLifecycleState;
}

export interface ContainerOpsRestoreRequest {
  backupId: string;
  apply?: boolean;
}

export interface ContainerOpsRollbackRequest {
  backupId: string;
}

export interface ContainerOpsRestoreCheck {
  severity: 'info' | 'warning' | 'error' | string;
  code: string;
  message: string;
  resource?: string;
  blocking: boolean;
}

export interface ContainerOpsRestoreStep {
  order: number;
  code: string;
  title: string;
  target?: string;
  destructive: boolean;
}

export interface ContainerOpsRestoreAction {
  order: number;
  code: string;
  target?: string;
  status: string;
  message?: string;
}

export interface ContainerOpsRestorePlan {
  agent?: ContainerOpsAgentInfo;
  backupId: string;
  status: string;
  backupRoot: string;
  createdAt: number;
  archives: ContainerOpsBackupArchive[];
  checks: ContainerOpsRestoreCheck[];
  steps: ContainerOpsRestoreStep[];
  actions?: ContainerOpsRestoreAction[];
  rollbackBackup?: ContainerOpsBackupResult;
  applied: boolean;
  destructive: boolean;
  readOnly: boolean;
  overview?: ContainerOpsDockerOverview;
  lifecycle?: ContainerOpsLifecycleState;
}

export interface ContainerOpsNetworkStandardizeRequest {
  backupId: string;
  apply: boolean;
}

export interface ContainerOpsNetworkCheck {
  severity: 'info' | 'warning' | 'error' | string;
  code: string;
  message: string;
  resource?: string;
  blocking: boolean;
}

export interface ContainerOpsNetworkAction {
  order: number;
  code: string;
  target?: string;
  status: string;
  message?: string;
}

export interface ContainerOpsNetworkStandardizeResult {
  agent?: ContainerOpsAgentInfo;
  backupId: string;
  status: string;
  network: string;
  checks: ContainerOpsNetworkCheck[];
  actions: ContainerOpsNetworkAction[];
  applied: boolean;
  destructive: boolean;
  overview?: ContainerOpsDockerOverview;
  lifecycle?: ContainerOpsLifecycleState;
}

export interface ContainerOpsUpgradeRequest {
  cpaImage?: string;
  cpampImage?: string;
  apply?: boolean;
}

export interface ContainerOpsUpgradeCheck {
  severity: 'info' | 'warning' | 'error' | string;
  code: string;
  message: string;
  resource?: string;
  blocking: boolean;
}

export interface ContainerOpsUpgradeStep {
  order: number;
  code: string;
  title: string;
  target?: string;
  destructive: boolean;
}

export interface ContainerOpsUpgradeAction {
  order: number;
  code: string;
  target?: string;
  status: string;
  message?: string;
}

export interface ContainerOpsUpgradePlan {
  agent?: ContainerOpsAgentInfo;
  status: string;
  cpaImage: string;
  cpampImage: string;
  checks: ContainerOpsUpgradeCheck[];
  steps: ContainerOpsUpgradeStep[];
  actions?: ContainerOpsUpgradeAction[];
  imagePulls?: ContainerOpsImagePull[];
  rollbackBackup?: ContainerOpsBackupResult;
  task?: ContainerOpsUpgradeTask;
  applied: boolean;
  destructive: boolean;
  readOnly: boolean;
  overview?: ContainerOpsDockerOverview;
  lifecycle?: ContainerOpsLifecycleState;
}

const basePath = '/container-ops';

export const containerOpsApi = {
  info: () => apiClient.get<ContainerOpsInfo>(`${basePath}/info`),
  audits: (limit = 20) =>
    apiClient.get<ContainerOpsAuditResponse>(`${basePath}/audits`, { params: { limit } }),
  upgradeTasks: (limit = 20) =>
    apiClient.get<ContainerOpsUpgradeTaskResponse>(`${basePath}/upgrade-tasks`, {
      params: { limit },
    }),
  startUpgradeTask: (taskId: string) =>
    apiClient.post<ContainerOpsUpgradeTask>(`${basePath}/upgrade-tasks/start`, { taskId }),
  agent: () => apiClient.get<ContainerOpsAgentInfo>(`${basePath}/agent`),
  discover: () => apiClient.get<ContainerOpsDiscovery>(`${basePath}/discover`),
  importPlan: () => apiClient.post<ContainerOpsImportPlan>(`${basePath}/import`),
  deployPlan: (request: ContainerOpsDeployRequest = { apply: false }) =>
    apiClient.post<ContainerOpsDeployPlan>(`${basePath}/deploy`, request),
  backup: () => apiClient.post<ContainerOpsBackupResult>(`${basePath}/backup`),
  restorePlan: (request: ContainerOpsRestoreRequest) =>
    apiClient.post<ContainerOpsRestorePlan>(`${basePath}/restore`, request),
  rollback: (request: ContainerOpsRollbackRequest) =>
    apiClient.post<ContainerOpsRestorePlan>(`${basePath}/rollback`, request),
  standardizeNetwork: (request: ContainerOpsNetworkStandardizeRequest) =>
    apiClient.post<ContainerOpsNetworkStandardizeResult>(
      `${basePath}/network-standardize`,
      request
    ),
  egressIPs: () => apiClient.get<ContainerOpsEgressIPInventory>(`${basePath}/egress-ips`),
  ensureSourceIP: (request: ContainerOpsSourceIPRequest) =>
    apiClient.post<ContainerOpsSourceIPResult>(`${basePath}/source-ip/ensure`, request),
  checkSourceIP: (request: ContainerOpsSourceIPRequest) =>
    apiClient.post<ContainerOpsSourceIPResult>(`${basePath}/source-ip/check`, request),
  removeSourceIP: (request: ContainerOpsSourceIPRequest) =>
    apiClient.post<ContainerOpsSourceIPResult>(`${basePath}/source-ip/remove`, request),
  upgrade: (request: ContainerOpsUpgradeRequest = { apply: false }) =>
    apiClient.post<ContainerOpsUpgradePlan>(`${basePath}/upgrade`, request),
};
