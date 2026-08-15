import { describe, expect, it } from 'vitest';
import type { AccountGroup } from '@/services/api';
import {
  accountMatchesGroupFilters,
  isRuntimeOnlyAuthFile,
  normalizeAccountGroupColor,
  normalizeAccountGroupIds,
  resolveAccountGroups,
} from './accountGroupModel';

const groups: AccountGroup[] = [
  {
    id: 1,
    name: 'Production',
    color: '#14b8a6',
    sort_order: 1,
    member_count: 10,
    api_key_count: 2,
  },
  {
    id: 2,
    name: 'Canary',
    color: '#0ea5e9',
    sort_order: 2,
    member_count: 2,
    api_key_count: 1,
  },
];

describe('account group model', () => {
  it('normalizes membership IDs and colors', () => {
    expect(normalizeAccountGroupIds([2, 1, 2, 0, -1, '3'])).toEqual([1, 2, 3]);
    expect(normalizeAccountGroupColor('#0EA5E9')).toBe('#0ea5e9');
    expect(normalizeAccountGroupColor('invalid')).toBe('#14b8a6');
  });

  it('resolves known groups while preserving missing IDs', () => {
    expect(resolveAccountGroups([2, 99, 1], groups)).toEqual({
      groups: [groups[1], groups[0]],
      missingIds: [99],
    });
  });

  it('supports ungrouped, include, and exclude filters', () => {
    expect(accountMatchesGroupFilters([], 'ungrouped', 'none')).toBe(true);
    expect(accountMatchesGroupFilters([1], 'ungrouped', 'none')).toBe(false);
    expect(accountMatchesGroupFilters([1, 2], '1', 'none')).toBe(true);
    expect(accountMatchesGroupFilters([2], '1', 'none')).toBe(false);
    expect(accountMatchesGroupFilters([1, 2], 'all', '2')).toBe(false);
    expect(accountMatchesGroupFilters([1], 'all', '2')).toBe(true);
  });

  it('normalizes boolean and string runtime-only markers', () => {
    expect(isRuntimeOnlyAuthFile({ name: 'runtime.json', runtimeOnly: true })).toBe(true);
    expect(isRuntimeOnlyAuthFile({ name: 'runtime.json', runtime_only: ' TRUE ' })).toBe(true);
    expect(isRuntimeOnlyAuthFile({ name: 'file.json', runtimeOnly: 'false' })).toBe(false);
    expect(isRuntimeOnlyAuthFile({ name: 'file.json' })).toBe(false);
  });
});
