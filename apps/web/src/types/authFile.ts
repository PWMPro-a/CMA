/**
 * 认证文件相关类型
 * 基于原项目 src/modules/auth-files.js
 */

import type { RecentRequestBucket } from '@/utils/recentRequests';

export type AuthFileType =
  | 'qwen'
  | 'kimi'
  | 'gemini'
  | 'aistudio'
  | 'claude'
  | 'codex'
  | 'antigravity'
  | 'xai'
  | 'iflow'
  | 'vertex'
  | 'empty'
  | 'unknown';

export type AgentIdentityRegistrationState =
  | 'ready'
  | 'credentials_pending'
  | 'queued'
  | 'registering'
  | 'retry_wait'
  | 'runtime_deleted'
  | 'failed';

export interface AgentIdentityRegistrationStatus {
  state: AgentIdentityRegistrationState;
  attempts: number;
  queued_at?: string;
  started_at?: string;
  finished_at?: string;
  next_retry_at?: string;
  error_code?: string;
  error?: string;
  trigger?: string;
  active: boolean;
  can_retry: boolean;
}

export interface AgentIdentityRecoveryHistoryEntry {
  id: number;
  name?: string;
  state: AgentIdentityRegistrationState;
  trigger?: string;
  attempt: number;
  started_at?: string;
  finished_at: string;
  duration_ms: number;
  error_code?: string;
  error?: string;
  success: boolean;
}

export interface AgentIdentityRecoveryCoordinator {
  concurrency: number;
  active: number;
  queued: number;
  queue_capacity: number;
  history_count: number;
  history_limit: number;
}

export interface AgentIdentityRecoverySummary {
  total: number;
  active: number;
  ready: number;
  credentials_pending: number;
  queued: number;
  registering: number;
  retry_wait: number;
  runtime_deleted: number;
  failed: number;
}

export interface AgentIdentityRecoveryConfig {
  concurrency: number;
  history_limit: number;
}

// CodexQuotaSnapshot is an in-memory usage sample supplied by the CPA runtime.
// It is deliberately independent from the browser-side "refresh quota" request:
// the runtime collects it asynchronously and includes it in the normal auth-file
// list payload.
export interface CodexQuotaSnapshot {
  used_ratio?: number;
  remaining_ratio?: number;
  window?: string;
  sampled_at?: string | number;
  expires_at?: string | number;
  generation?: number;
}

export type AuthFileImportSource = 'manual' | 'supply' | string;

export type AuthFileImportMethod =
  | 'file_upload'
  | 'json_paste'
  | 'automatic_supply'
  | 'manual_supply'
  | string;

export interface AuthFileImportMetadata {
  version: number;
  source: AuthFileImportSource;
  method: AuthFileImportMethod;
  platform_id: string;
  platform_name: string;
  imported_by: string;
  imported_at: string;
}

export interface AuthFileItem {
  id?: string;
  name: string;
  type?: AuthFileType | string;
  provider?: string;
  size?: number;
  authIndex?: string | number | null;
  runtimeOnly?: boolean | string;
  config_backed?: boolean;
  disabled?: boolean;
  unavailable?: boolean;
  status?: string;
  statusMessage?: string;
  lastRefresh?: string | number;
  modified?: number;
  success?: unknown;
  failed?: unknown;
  project_id?: string;
  projectId?: string;
  source_ip?: string;
  sourceIp?: string;
  'source-ip'?: string;
  websockets?: boolean;
  websocket?: boolean;
  gemini_virtual_project?: string;
  geminiVirtualProject?: string;
  recent_requests?: RecentRequestBucket[];
  recentRequests?: RecentRequestBucket[];
  /** Number of requests currently in flight for this runtime credential. */
  runtime_current_concurrency?: number;
  runtimeCurrentConcurrency?: number;
  codex_quota_snapshots?: Record<string, CodexQuotaSnapshot>;
  codexQuotaSnapshots?: Record<string, CodexQuotaSnapshot>;
  codex_identity_fingerprint?: string;
  codexIdentityFingerprint?: string;
  agent_identity_registration?: AgentIdentityRegistrationStatus;
  agentIdentityRegistration?: AgentIdentityRegistrationStatus;
  group_ids?: number[];
  groupIds?: number[];
  cpamp_import?: AuthFileImportMetadata;
  cpampImport?: AuthFileImportMetadata;
  [key: string]: unknown;
}

export interface AuthFilesResponse {
  files: AuthFileItem[];
  total?: number;
}
