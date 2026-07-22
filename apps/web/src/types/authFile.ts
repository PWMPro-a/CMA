/**
 * 认证文件相关类型
 * 基于原项目 src/modules/auth-files.js
 */

import type { RecentRequestBucket } from '@/utils/recentRequests';

export type AuthFileType =
  | 'qwen'
  | 'kimi'
  | 'gemini'
  | 'gemini-cli'
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

export interface AuthFileItem {
  name: string;
  type?: AuthFileType | string;
  provider?: string;
  size?: number;
  authIndex?: string | number | null;
  runtimeOnly?: boolean | string;
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
  gemini_virtual_project?: string;
  geminiVirtualProject?: string;
  recent_requests?: RecentRequestBucket[];
  recentRequests?: RecentRequestBucket[];
  agent_identity_registration?: AgentIdentityRegistrationStatus;
  agentIdentityRegistration?: AgentIdentityRegistrationStatus;
  [key: string]: unknown;
}

export interface AuthFilesResponse {
  files: AuthFileItem[];
  total?: number;
}
