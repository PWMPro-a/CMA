export type PayloadParamValueType = 'string' | 'number' | 'boolean' | 'json';
export type DisableImageGenerationMode = 'false' | 'true' | 'chat' | 'passthrough';
export type RemoteManagementSecretKeyAction = 'unchanged' | 'replace' | 'clear';
export type PluginStoreAuthType = 'none' | 'bearer' | 'basic' | 'header' | 'github-token';
export type PluginStoreAuthApplyTo = 'registry' | 'metadata' | 'artifact';
export type CodexFingerprintSignalType = 'header_exact' | 'header_prefix' | 'body_path';
export type PayloadParamValidationErrorCode =
  | 'payload_invalid_number'
  | 'payload_invalid_boolean'
  | 'payload_invalid_json';

export type VisualConfigFieldPath =
  | 'port'
  | 'errorLogsMaxFiles'
  | 'logsMaxTotalSizeMb'
  | 'redisUsageQueueRetentionSeconds'
  | 'transientErrorCooldownSeconds'
  | 'requestRetry'
  | 'maxRetryCredentials'
  | 'maxRetryInterval'
  | 'authAutoRefreshWorkers'
  | 'codexTailBurstTriggerRemainingPercent'
  | 'codexTailBurstExpiryWindow'
  | 'codexTailBurstMaxConcurrency'
  | 'codexTailBurstCollectorMaxConcurrency'
  | 'codexCacheAffinityMaxConcurrency'
  | 'codexCacheAffinityMaxEntries'
  | 'codexCacheAffinityMaxRetryCredentials'
  | 'codexCacheAffinityWebsocketPoolSlots'
  | 'codexCacheAffinityMaxSessionRequests'
  | 'codexCacheAffinityMaxSessionDuration'
  | 'codexCacheAffinityMaxSharePercent'
  | 'codexCacheAffinityQuotaPreemptPercent'
  | 'codexCacheAffinityQuotaHardStopPercent'
  | 'codexClientMinVersion'
  | 'codexClientMaxVersion'
  | 'streaming.keepaliveSeconds'
  | 'streaming.bootstrapRetries'
  | 'streaming.nonstreamKeepaliveInterval';

export type VisualConfigValidationErrorCode =
  | 'port_range'
  | 'non_negative_integer'
  | 'integer'
  | 'retention_seconds_range'
  | 'codex_version'
  | 'codex_version_range'
  | 'positive_integer'
  | 'cache_affinity_websocket_slots_range'
  | 'cache_affinity_share_percent_range'
  | 'positive_duration'
  | 'cache_affinity_preempt_percent_range'
  | 'cache_affinity_hard_stop_percent_range'
  | 'tail_burst_trigger_percent_range'
  | 'tail_burst_collector_concurrency_range';

export type CodexClientRestrictionEntry = {
  id: string;
  originator: string;
  uaContains: string[];
  skipEngineFingerprint: boolean;
};

export type CodexEngineFingerprintSignal = {
  id: string;
  type: CodexFingerprintSignalType;
  match: string[];
  required: boolean;
};

export type VisualConfigValidationErrors = Partial<
  Record<VisualConfigFieldPath, VisualConfigValidationErrorCode>
>;

export type PayloadParamEntry = {
  id: string;
  path: string;
  valueType: PayloadParamValueType;
  value: string;
};

export type PayloadHeaderEntry = {
  id: string;
  name: string;
  value: string;
};

export type PayloadModelEntry = {
  id: string;
  name: string;
  protocol?: string;
  fromProtocol?: string;
  headers?: PayloadHeaderEntry[];
  match?: PayloadParamEntry[];
  notMatch?: PayloadParamEntry[];
  exist?: string[];
  notExist?: string[];
};

export type PayloadRule = {
  id: string;
  models: PayloadModelEntry[];
  params: PayloadParamEntry[];
};

export type PayloadFilterRule = {
  id: string;
  models: PayloadModelEntry[];
  params: string[];
};

export interface StreamingConfig {
  keepaliveSeconds: string;
  bootstrapRetries: string;
  nonstreamKeepaliveInterval: string;
}

export type PluginStoreAuthRule = {
  id: string;
  match: string;
  applyTo: PluginStoreAuthApplyTo[];
  type: PluginStoreAuthType;
  tokenEnv: string;
  usernameEnv: string;
  passwordEnv: string;
  headerName: string;
  headerValueEnv: string;
  allowInsecure: boolean;
};

export type VisualConfigValues = {
  host: string;
  port: string;
  tlsEnable: boolean;
  tlsCert: string;
  tlsKey: string;
  rmAllowRemote: boolean;
  rmSecretKey: string;
  rmSecretKeyAction: RemoteManagementSecretKeyAction;
  rmSecretKeyConfigured: boolean;
  rmDisableControlPanel: boolean;
  rmDisableAutoUpdatePanel: boolean;
  rmPanelRepo: string;
  authDir: string;
  apiKeysText: string;
  pluginsEnabled: boolean;
  pluginsDir: string;
  pluginStoreSourcesText: string;
  pluginStoreAuth: PluginStoreAuthRule[];
  debug: boolean;
  pprofEnable: boolean;
  pprofAddr: string;
  commercialMode: boolean;
  usageStatisticsEnabled: boolean;
  loggingToFile: boolean;
  logsMaxTotalSizeMb: string;
  errorLogsMaxFiles: string;
  redisUsageQueueRetentionSeconds: string;
  proxyUrl: string;
  forceModelPrefix: boolean;
  passthroughHeaders: boolean;
  requestRetry: string;
  maxRetryCredentials: string;
  maxRetryInterval: string;
  disableCooling: boolean;
  saveCooldownStatus: boolean;
  transientErrorCooldownSeconds: string;
  disableClaudeCloakMode: boolean;
  disableImageGeneration: DisableImageGenerationMode;
  gptImage2BaseModel: string;
  videoResultAuthCacheTtl: string;
  authAutoRefreshWorkers: string;
  quotaSwitchProject: boolean;
  quotaSwitchPreviewModel: boolean;
  quotaAntigravityCredits: boolean;
  routingStrategy: 'concurrency-balanced' | 'round-robin' | 'fill-first';
  routingSessionAffinity: boolean;
  routingHighCacheMode: boolean;
  routingSessionAffinityTTL: string;
  wsAuth: boolean;
  antigravitySignatureCacheEnabled: boolean;
  antigravitySignatureBypassStrict: boolean;
  claudeHeaderUserAgent: string;
  claudeHeaderPackageVersion: string;
  claudeHeaderRuntimeVersion: string;
  claudeHeaderOs: string;
  claudeHeaderArch: string;
  claudeHeaderTimeout: string;
  claudeHeaderStabilizeDeviceProfile: boolean;
  codexHeaderUserAgent: string;
  codexHeaderBetaFeatures: string;
  codexIdentityConfuse: boolean;
  codexClientForceAllow: boolean;
  codexClientMinVersion: string;
  codexClientMaxVersion: string;
  codexClientAllowAppServer: boolean;
  codexClientWhitelist: CodexClientRestrictionEntry[];
  codexClientBlacklist: CodexClientRestrictionEntry[];
  codexClientFingerprintSignals: CodexEngineFingerprintSignal[];
  codexCacheAffinityEnabled: boolean;
  codexCacheAffinityShadow: boolean;
  codexCacheAffinityMaxConcurrency: string;
  codexCacheAffinityMaxEntries: string;
  codexCacheAffinityMaxRetryCredentials: string;
  codexCacheAffinityWebsocketPoolSlots: string;
  codexCacheAffinityMaxSessionRequests: string;
  codexCacheAffinityMaxSessionDuration: string;
  codexCacheAffinityMaxSharePercent: string;
  codexCacheAffinityQuotaPreemptPercent: string;
  codexCacheAffinityQuotaHardStopPercent: string;
  codexTailBurstEnabled: boolean;
  codexTailBurstTriggerRemainingPercent: string;
  codexTailBurstSnapshotTtl: string;
  codexTailBurstExpiryWindow: string;
  codexTailBurstMaxConcurrency: string;
  codexTailBurstCollectorInterval: string;
  codexTailBurstCollectorMaxConcurrency: string;
  codexTailBurstCollectorTimeout: string;
  codexTailBurstToolInjectionEnabled: boolean;
  payloadDefaultRules: PayloadRule[];
  payloadDefaultRawRules: PayloadRule[];
  payloadOverrideRules: PayloadRule[];
  payloadOverrideRawRules: PayloadRule[];
  payloadFilterRules: PayloadFilterRule[];
  streaming: StreamingConfig;
};

export const makeClientId = () => {
  if (typeof globalThis.crypto?.randomUUID === 'function') return globalThis.crypto.randomUUID();
  return `${Date.now().toString(36)}_${Math.random().toString(36).slice(2, 10)}`;
};

export const DEFAULT_VISUAL_VALUES: VisualConfigValues = {
  host: '',
  port: '',
  tlsEnable: false,
  tlsCert: '',
  tlsKey: '',
  rmAllowRemote: false,
  rmSecretKey: '',
  rmSecretKeyAction: 'unchanged',
  rmSecretKeyConfigured: false,
  rmDisableControlPanel: false,
  rmDisableAutoUpdatePanel: true,
  rmPanelRepo: '',
  authDir: '',
  apiKeysText: '',
  pluginsEnabled: false,
  pluginsDir: '',
  pluginStoreSourcesText: '',
  pluginStoreAuth: [],
  debug: false,
  pprofEnable: false,
  pprofAddr: '127.0.0.1:8316',
  commercialMode: false,
  usageStatisticsEnabled: false,
  loggingToFile: false,
  logsMaxTotalSizeMb: '',
  errorLogsMaxFiles: '',
  redisUsageQueueRetentionSeconds: '',
  proxyUrl: '',
  forceModelPrefix: false,
  passthroughHeaders: false,
  requestRetry: '',
  maxRetryCredentials: '',
  maxRetryInterval: '',
  disableCooling: false,
  saveCooldownStatus: false,
  transientErrorCooldownSeconds: '',
  disableClaudeCloakMode: false,
  disableImageGeneration: 'false',
  gptImage2BaseModel: '',
  videoResultAuthCacheTtl: '',
  authAutoRefreshWorkers: '',
  quotaSwitchProject: false,
  quotaSwitchPreviewModel: false,
  quotaAntigravityCredits: false,
  routingStrategy: 'concurrency-balanced',
  routingSessionAffinity: false,
  routingHighCacheMode: false,
  routingSessionAffinityTTL: '',
  wsAuth: true,
  antigravitySignatureCacheEnabled: true,
  antigravitySignatureBypassStrict: false,
  claudeHeaderUserAgent: '',
  claudeHeaderPackageVersion: '',
  claudeHeaderRuntimeVersion: '',
  claudeHeaderOs: '',
  claudeHeaderArch: '',
  claudeHeaderTimeout: '',
  claudeHeaderStabilizeDeviceProfile: false,
  codexHeaderUserAgent: '',
  codexHeaderBetaFeatures: '',
  codexIdentityConfuse: false,
  codexClientForceAllow: false,
  codexClientMinVersion: '',
  codexClientMaxVersion: '',
  codexClientAllowAppServer: false,
  codexClientWhitelist: [],
  codexClientBlacklist: [],
  codexClientFingerprintSignals: [
    {
      id: 'codex-fingerprint-x-codex',
      type: 'header_prefix',
      match: ['x-codex-'],
      required: true,
    },
    {
      id: 'codex-fingerprint-session',
      type: 'header_exact',
      match: ['session-id', 'session_id'],
      required: true,
    },
    {
      id: 'codex-fingerprint-thread',
      type: 'header_exact',
      match: ['thread-id', 'thread_id'],
      required: true,
    },
    {
      id: 'codex-fingerprint-body',
      type: 'body_path',
      match: ['client_metadata.x-codex-window-id', 'client_metadata.x-codex-installation-id'],
      required: true,
    },
  ],
  codexCacheAffinityEnabled: false,
  codexCacheAffinityShadow: false,
  codexCacheAffinityMaxConcurrency: '8',
  codexCacheAffinityMaxEntries: '65536',
  codexCacheAffinityMaxRetryCredentials: '2',
  codexCacheAffinityWebsocketPoolSlots: '8',
  codexCacheAffinityMaxSessionRequests: '50',
  codexCacheAffinityMaxSessionDuration: '5m',
  codexCacheAffinityMaxSharePercent: '0',
  codexCacheAffinityQuotaPreemptPercent: '97',
  codexCacheAffinityQuotaHardStopPercent: '99',
  codexTailBurstEnabled: false,
  codexTailBurstTriggerRemainingPercent: '2',
  codexTailBurstSnapshotTtl: '90s',
  codexTailBurstExpiryWindow: '10m',
  codexTailBurstMaxConcurrency: '32',
  codexTailBurstCollectorInterval: '45s',
  codexTailBurstCollectorMaxConcurrency: '4',
  codexTailBurstCollectorTimeout: '8s',
  codexTailBurstToolInjectionEnabled: false,
  payloadDefaultRules: [],
  payloadDefaultRawRules: [],
  payloadOverrideRules: [],
  payloadOverrideRawRules: [],
  payloadFilterRules: [],
  streaming: {
    keepaliveSeconds: '',
    bootstrapRetries: '',
    nonstreamKeepaliveInterval: '',
  },
};
