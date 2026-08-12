import type { AxiosRequestConfig } from 'axios';
import type { TFunction } from 'i18next';
import type { AuthFileItem, CodexUsagePayload } from '@/types';
import { CODEX_USAGE_URL } from '@/utils/quota/constants';
import { buildCodexUsageRequestHeaders } from '@/utils/quota/codexRequestHeaders';
import { normalizeAuthIndex, parseCodexUsagePayload } from '@/utils/quota/parsers';
import { fetchCodexQuota, type CodexQuotaData } from '@/utils/quota/providerRequests';
import { resolveCodexChatgptAccountId } from '@/utils/quota/resolvers';
import { apiCallApi, getApiCallErrorMessage, type ApiCallResult } from './apiCall';
import { useAuthStore } from '@/stores/useAuthStore';
import { useUsageServiceStore } from '@/stores/useUsageServiceStore';
import { usageServiceApi, type CodexQuotaResetOperation } from './usageService';

export type CodexUsageRequestParams = {
  authIndex: string;
  accountId?: string | null;
  userAgent?: string;
  requestConfig?: AxiosRequestConfig;
};

export type CodexUsageRawResult = {
  result: ApiCallResult;
  payload: CodexUsagePayload | null;
};

export { buildCodexUsageRequestHeaders };

export const requestCodexUsageRaw = async ({
  authIndex,
  accountId,
  userAgent,
  requestConfig,
}: CodexUsageRequestParams): Promise<CodexUsageRawResult> => {
  const result = await apiCallApi.request(
    {
      authIndex,
      method: 'GET',
      url: CODEX_USAGE_URL,
      header: buildCodexUsageRequestHeaders(accountId, { userAgent }),
    },
    requestConfig
  );

  return {
    result,
    payload: parseCodexUsagePayload(result.body ?? result.bodyText),
  };
};

export const requestCodexUsagePayload = async (
  params: CodexUsageRequestParams,
  options: { emptyMessage?: string } = {}
): Promise<CodexUsagePayload> => {
  const { result, payload } = await requestCodexUsageRaw(params);
  if (result.statusCode < 200 || result.statusCode >= 300) {
    throw new Error(getApiCallErrorMessage(result));
  }
  if (!payload) {
    throw new Error(options.emptyMessage || 'No Codex quota data available');
  }
  return payload;
};

export const createCodexRedeemRequestId = () => {
  if (typeof globalThis.crypto?.randomUUID === 'function') {
    return globalThis.crypto.randomUUID();
  }
  const bytes = new Uint8Array(16);
  if (typeof globalThis.crypto?.getRandomValues === 'function') {
    globalThis.crypto.getRandomValues(bytes);
  } else {
    for (let index = 0; index < bytes.length; index += 1) {
      bytes[index] = Math.floor(Math.random() * 256);
    }
  }
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = Array.from(bytes, (value) => value.toString(16).padStart(2, '0')).join('');
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
};

const resetOperationIds = new Map<string, string>();

const resetOperationKey = (authIndex: string, accountId?: string | null) =>
  `${authIndex}\u0000${String(accountId ?? '').trim().toLowerCase()}`;

const isAmbiguousCodexQuotaResetError = (error: unknown): boolean => {
  if (!error || typeof error !== 'object') return false;
  const candidate = error as { status?: unknown; code?: unknown };
  if (typeof candidate.status === 'number') return false;
  const code = typeof candidate.code === 'string' ? candidate.code.toUpperCase() : '';
  return (
    code === '' ||
    code === 'ECONNABORTED' ||
    code === 'ETIMEDOUT' ||
    code === 'ECONNRESET' ||
    code === 'ERR_NETWORK'
  );
};

export const requestCodexQuotaReset = async (
  file: AuthFileItem,
  t?: TFunction
): Promise<CodexQuotaResetOperation> => {
  const rawAuthIndex = file['auth_index'] ?? file.authIndex;
  const authIndex = normalizeAuthIndex(rawAuthIndex);
  if (!authIndex) {
    throw new Error(t?.('codex_quota.missing_auth_index') ?? 'Auth file missing auth_index');
  }

  const accountId = resolveCodexChatgptAccountId(file);
  const usageServiceState = useUsageServiceStore.getState();
  const authState = useAuthStore.getState();
  const operationKey = `${usageServiceState.serviceBase || authState.apiBase}\u0000${resetOperationKey(authIndex, accountId)}`;
  const operationId = resetOperationIds.get(operationKey) ?? createCodexRedeemRequestId();
  const serviceBase = usageServiceState.enabled
    ? usageServiceState.serviceBase
    : authState.sessionMode === 'manager_embedded'
      ? authState.apiBase
      : '';
  if (!serviceBase || !authState.managementKey) {
    throw new Error('Codex quota controller is not connected');
  }
  resetOperationIds.set(operationKey, operationId);

  let terminal = false;
  try {
    let operation: CodexQuotaResetOperation | null = null;
    let lastError: unknown;
    for (let attempt = 0; attempt < 2; attempt += 1) {
      try {
        operation = await usageServiceApi.resetCodexQuota(
          serviceBase,
          authState.managementKey,
          authIndex,
          operationId
        );
        break;
      } catch (error) {
        lastError = error;
        if (!isAmbiguousCodexQuotaResetError(error)) break;
      }
    }
    if (!operation) throw lastError;
    terminal = operation.state === 'completed' || operation.state === 'failed';
    if (operation.state !== 'completed') {
      throw new Error(
        operation.last_error ||
          `Codex quota reset operation ended with state ${operation.state}`
      );
    }
    return operation;
  } finally {
    if (terminal) resetOperationIds.delete(operationKey);
  }
};

export const resetCodexQuota = async (
  file: AuthFileItem,
  t: TFunction
): Promise<CodexQuotaData> => {
  await requestCodexQuotaReset(file, t);
  return fetchCodexQuota(file, t);
};
