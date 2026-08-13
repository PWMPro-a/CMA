/**
 * Resolver functions for extracting data from auth files.
 */

import type { AuthFileItem } from '@/types';
import { normalizeStringValue, normalizePlanType, parseIdTokenPayload } from './parsers';

const resolveAccountIdCandidate = (value: unknown): string | null => {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return null;
  const record = value as Record<string, unknown>;
  return normalizeStringValue(
    record.chatgpt_account_id ?? record.chatgptAccountId ?? record.account_id ?? record.accountId
  );
};

export function extractCodexChatgptAccountId(value: unknown): string | null {
  const direct = resolveAccountIdCandidate(value);
  if (direct) return direct;

  const payload = parseIdTokenPayload(value);
  if (!payload) return null;
  return normalizeStringValue(
    payload.chatgpt_account_id ??
      payload.chatgptAccountId ??
      payload.account_id ??
      payload.accountId
  );
}

export function resolveCodexChatgptAccountId(file: AuthFileItem): string | null {
  const metadata =
    file && typeof file.metadata === 'object' && file.metadata !== null
      ? (file.metadata as Record<string, unknown>)
      : null;
  const attributes =
    file && typeof file.attributes === 'object' && file.attributes !== null
      ? (file.attributes as Record<string, unknown>)
      : null;

  const directCandidates = [
    file.chatgpt_account_id,
    file.chatgptAccountId,
    file.account_id,
    file.accountId,
    metadata?.chatgpt_account_id,
    metadata?.chatgptAccountId,
    metadata?.account_id,
    metadata?.accountId,
    attributes?.chatgpt_account_id,
    attributes?.chatgptAccountId,
    attributes?.account_id,
    attributes?.accountId,
  ];

  for (const candidate of directCandidates) {
    const id = normalizeStringValue(candidate) ?? resolveAccountIdCandidate(candidate);
    if (id) return id;
  }

  const tokenCandidates = [file.id_token, metadata?.id_token, attributes?.id_token];

  for (const candidate of tokenCandidates) {
    const id = extractCodexChatgptAccountId(candidate);
    if (id) return id;
  }

  // A Team export may only expose the selected workspace. The Codex usage
  // endpoint accepts that workspace identifier through Chatgpt-Account-Id,
  // so use it only after the canonical account-id fields are exhausted.
  const workspaceCandidates = [
    file.workspace_id,
    file.workspaceId,
    file.chatgpt_workspace_id,
    file.chatgptWorkspaceId,
    metadata?.workspace_id,
    metadata?.workspaceId,
    metadata?.chatgpt_workspace_id,
    metadata?.chatgptWorkspaceId,
    attributes?.workspace_id,
    attributes?.workspaceId,
    attributes?.chatgpt_workspace_id,
    attributes?.chatgptWorkspaceId,
  ];
  for (const candidate of workspaceCandidates) {
    const id = normalizeStringValue(candidate);
    if (id) return id;
  }

  return null;
}

export function resolveCodexPlanType(file: AuthFileItem): string | null {
  const metadata =
    file && typeof file.metadata === 'object' && file.metadata !== null
      ? (file.metadata as Record<string, unknown>)
      : null;
  const attributes =
    file && typeof file.attributes === 'object' && file.attributes !== null
      ? (file.attributes as Record<string, unknown>)
      : null;
  const idToken =
    file && typeof file.id_token === 'object' && file.id_token !== null
      ? (file.id_token as Record<string, unknown>)
      : null;
  const metadataIdToken =
    metadata && typeof metadata.id_token === 'object' && metadata.id_token !== null
      ? (metadata.id_token as Record<string, unknown>)
      : null;
  const resolveIdTokenPlanCandidate = (value: unknown): string | null => {
    const payload = parseIdTokenPayload(value);
    if (!payload) return null;
    return normalizePlanType(payload.plan_type ?? payload.planType);
  };
  const candidates = [
    file.chatgpt_plan_type,
    file.chatgptPlanType,
    file.plan_type,
    file.planType,
    file['plan_type'],
    file['planType'],
    resolveIdTokenPlanCandidate(file.id_token),
    idToken?.plan_type,
    idToken?.planType,
    metadata?.chatgpt_plan_type,
    metadata?.chatgptPlanType,
    metadata?.plan_type,
    metadata?.planType,
    resolveIdTokenPlanCandidate(metadata?.id_token),
    metadataIdToken?.plan_type,
    metadataIdToken?.planType,
    attributes?.chatgpt_plan_type,
    attributes?.chatgptPlanType,
    attributes?.plan_type,
    attributes?.planType,
    resolveIdTokenPlanCandidate(attributes?.id_token),
  ];

  for (const candidate of candidates) {
    const planType = normalizePlanType(candidate);
    if (planType) return planType;
  }

  return null;
}

const readBoolean = (value: unknown): boolean | null => {
  if (typeof value === 'boolean') return value;
  if (typeof value === 'number' && Number.isFinite(value)) return value !== 0;
  if (typeof value === 'string') {
    const normalized = value.trim().toLowerCase();
    if (['true', '1', 'yes', 'on'].includes(normalized)) return true;
    if (['false', '0', 'no', 'off'].includes(normalized)) return false;
  }
  return null;
};

export function isCodexPlanTypePinned(file: AuthFileItem): boolean {
  const metadata =
    file && typeof file.metadata === 'object' && file.metadata !== null
      ? (file.metadata as Record<string, unknown>)
      : null;
  const attributes =
    file && typeof file.attributes === 'object' && file.attributes !== null
      ? (file.attributes as Record<string, unknown>)
      : null;
  for (const record of [file as Record<string, unknown>, metadata, attributes]) {
    if (!record) continue;
    for (const key of ['codex_plan_type_pinned', 'codexPlanTypePinned']) {
      if (!(key in record)) continue;
      return readBoolean(record[key]) === true;
    }
  }
  const planType = normalizePlanType(resolveCodexPlanType(file));
  if (!planType || planType === 'free') return false;
  if (
    [file as Record<string, unknown>, metadata, attributes].some((record) => {
      if (!record) return false;
      const format = normalizeStringValue(record.import_format ?? record.importFormat);
      return format?.toLowerCase() === 'sub2api';
    })
  ) {
    return true;
  }
  const tokenCandidates = [file.id_token, metadata?.id_token, attributes?.id_token];
  return tokenCandidates.some((candidate) => {
    const payload = parseIdTokenPayload(candidate);
    const tokenPlan = normalizePlanType(
      payload?.chatgpt_plan_type ??
        payload?.chatgptPlanType ??
        payload?.plan_type ??
        payload?.planType
    );
    return tokenPlan === 'free';
  });
}

export function resolveEffectiveCodexPlanType(
  file: AuthFileItem,
  observedPlanType: unknown
): string | null {
  const filePlanType = normalizePlanType(resolveCodexPlanType(file));
  if (filePlanType && filePlanType !== 'free' && isCodexPlanTypePinned(file)) {
    return filePlanType;
  }
  return normalizePlanType(observedPlanType) ?? filePlanType;
}
