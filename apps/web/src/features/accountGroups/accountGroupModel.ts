import type { AccountGroup } from '@/services/api';
import type { AuthFileItem } from '@/types';

export const DEFAULT_ACCOUNT_GROUP_COLOR = '#14b8a6';

export const normalizeAccountGroupColor = (value: unknown): string => {
  const color = String(value ?? '').trim();
  return /^#[0-9a-fA-F]{6}$/.test(color) ? color.toLowerCase() : DEFAULT_ACCOUNT_GROUP_COLOR;
};

export const normalizeAccountGroupIds = (value: unknown): number[] => {
  if (!Array.isArray(value)) return [];
  return Array.from(
    new Set(
      value.map((item) => Number(item)).filter((item) => Number.isSafeInteger(item) && item > 0)
    )
  ).sort((left, right) => left - right);
};

export const getAuthFileGroupIds = (file: AuthFileItem): number[] =>
  normalizeAccountGroupIds(file.group_ids ?? file.groupIds);

export const isRuntimeOnlyAuthFile = (file: AuthFileItem): boolean => {
  const value = file.runtime_only ?? file.runtimeOnly;
  if (typeof value === 'boolean') return value;
  return typeof value === 'string' && value.trim().toLowerCase() === 'true';
};

export const resolveAccountGroups = (
  ids: number[],
  groups: AccountGroup[]
): { groups: AccountGroup[]; missingIds: number[] } => {
  if (ids.length === 0) return { groups: [], missingIds: [] };
  const byId = new Map(groups.map((group) => [group.id, group]));
  const resolved: AccountGroup[] = [];
  const missingIds: number[] = [];
  ids.forEach((id) => {
    const group = byId.get(id);
    if (group) resolved.push(group);
    else missingIds.push(id);
  });
  return { groups: resolved, missingIds };
};

export const accountMatchesGroupFilters = (
  ids: number[],
  include: string,
  exclude: string
): boolean => {
  if (include === 'ungrouped' && ids.length > 0) return false;
  if (include !== 'all' && include !== 'ungrouped') {
    const includeId = Number(include);
    if (Number.isSafeInteger(includeId) && includeId > 0 && !ids.includes(includeId)) return false;
  }
  if (exclude !== 'none') {
    const excludeId = Number(exclude);
    if (Number.isSafeInteger(excludeId) && excludeId > 0 && ids.includes(excludeId)) return false;
  }
  return true;
};
