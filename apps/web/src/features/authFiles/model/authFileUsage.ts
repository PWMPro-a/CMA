import type { MonitoringAccountHistoryTarget } from '@/services/api/usageService';
import type { AuthFileItem } from '@/types';

export type AuthFileUsageSummary = {
  requests: number;
  totalTokens: number;
};

export type AuthFileUsageTarget = {
  key: string;
  request: MonitoringAccountHistoryTarget;
};

const UNKNOWN_AUTH_INDEX = '-';

const readText = (value: unknown): string => {
  if (value === undefined || value === null) return '';
  return String(value).trim();
};

const firstNonEmpty = (...values: unknown[]): string => {
  for (const value of values) {
    const text = readText(value);
    if (text) return text;
  }
  return '';
};

const looksLikeSecret = (value: string): boolean => {
  const trimmed = value.trim();
  if (!trimmed || trimmed.includes('@')) return false;
  if (/[ /\\]/.test(trimmed)) return false;
  return (
    trimmed.startsWith('sk-') ||
    trimmed.startsWith('AIza') ||
    (trimmed.length >= 32 && trimmed.length <= 512)
  );
};

const firstSafeAccount = (...values: unknown[]): string => {
  for (const value of values) {
    const text = readText(value);
    if (text && !looksLikeSecret(text)) return text;
  }
  return '';
};

export const getAuthFileUsageKey = (file: AuthFileItem): string => {
  const authIndex = firstNonEmpty(file.authIndex, file['auth_index'], file['auth-index']);
  return `${file.name}::${authIndex || UNKNOWN_AUTH_INDEX}`;
};

export const buildAuthFileUsageTarget = (file: AuthFileItem): AuthFileUsageTarget => {
  const authIndex = firstNonEmpty(file.authIndex, file['auth_index'], file['auth-index']);
  let accountSnapshot = firstSafeAccount(file.account, file.email);
  const authLabelSnapshot = firstNonEmpty(file.label, file.name, file.email, accountSnapshot);
  if (!accountSnapshot) {
    accountSnapshot = firstNonEmpty(authLabelSnapshot, file.name);
  }

  return {
    key: getAuthFileUsageKey(file),
    request: {
      row_key: getAuthFileUsageKey(file),
      account_snapshot: accountSnapshot,
      auth_label_snapshot: authLabelSnapshot,
      auth_index: authIndex,
      source: firstNonEmpty(file.source),
    },
  };
};

export const buildAuthFileUsageTargets = (files: AuthFileItem[]): AuthFileUsageTarget[] =>
  files.map(buildAuthFileUsageTarget);
