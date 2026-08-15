import { apiClient } from './client';

export interface AccountGroup {
  id: number;
  name: string;
  description?: string;
  color?: string;
  sort_order: number;
  member_count: number;
  api_key_count: number;
}

export interface AccountGroupInput {
  name: string;
  description?: string;
  color?: string;
  sort_order?: number;
}

export interface AccountGroupMembershipUpdate {
  name: string;
  auth_index?: string;
  group_ids: number[];
}

export interface APIKeyGroupPolicy {
  api_key_hash: string;
  allowed_group_ids: number[];
}

export interface APIKeyGroupPolicyInput {
  api_key?: string;
  api_key_hash?: string;
  allowed_group_ids: number[];
}

type AccountGroupsResponse = { groups?: AccountGroup[] };
type AccountGroupResponse = { group: AccountGroup };
type APIKeyGroupPoliciesResponse = { policies?: APIKeyGroupPolicy[] };

const normalizeIds = (value: unknown): number[] => {
  if (!Array.isArray(value)) return [];
  return Array.from(
    new Set(
      value.map((item) => Number(item)).filter((item) => Number.isSafeInteger(item) && item > 0)
    )
  ).sort((left, right) => left - right);
};

const normalizeGroup = (group: AccountGroup): AccountGroup => ({
  ...group,
  id: Number(group.id),
  sort_order: Number(group.sort_order ?? 0),
  member_count: Number(group.member_count ?? 0),
  api_key_count: Number(group.api_key_count ?? 0),
  name: String(group.name ?? '').trim(),
  description: String(group.description ?? '').trim(),
  color: String(group.color ?? '').trim(),
});

const normalizePolicy = (policy: APIKeyGroupPolicy): APIKeyGroupPolicy => ({
  api_key_hash: String(policy.api_key_hash ?? '')
    .trim()
    .toLowerCase(),
  allowed_group_ids: normalizeIds(policy.allowed_group_ids),
});

export const accountGroupsApi = {
  async list(): Promise<AccountGroup[]> {
    const response = await apiClient.get<AccountGroupsResponse>('/account-groups');
    return (response.groups ?? [])
      .map(normalizeGroup)
      .filter((group) => Number.isSafeInteger(group.id) && group.id > 0 && group.name)
      .sort(
        (left, right) =>
          left.sort_order - right.sort_order ||
          left.id - right.id ||
          left.name.localeCompare(right.name)
      );
  },

  async create(input: AccountGroupInput): Promise<AccountGroup> {
    const response = await apiClient.post<AccountGroupResponse>('/account-groups', input);
    return normalizeGroup(response.group);
  },

  async update(id: number, input: Partial<AccountGroupInput>): Promise<AccountGroup> {
    const response = await apiClient.patch<AccountGroupResponse>(
      `/account-groups/${encodeURIComponent(String(id))}`,
      input
    );
    return normalizeGroup(response.group);
  },

  delete: (id: number, force = false) =>
    apiClient.delete(`/account-groups/${encodeURIComponent(String(id))}`, {
      params: force ? { force: true } : undefined,
    }),

  updateMemberships: (updates: AccountGroupMembershipUpdate[]) =>
    apiClient.put<{ status: string; updated: number }>('/account-groups/memberships', {
      updates: updates.map((update) => ({
        name: update.name,
        auth_index: update.auth_index ?? '',
        group_ids: normalizeIds(update.group_ids),
      })),
    }),

  async listAPIKeyPolicies(): Promise<APIKeyGroupPolicy[]> {
    const response = await apiClient.get<APIKeyGroupPoliciesResponse>('/api-key-group-policies');
    return (response.policies ?? [])
      .map(normalizePolicy)
      .filter((policy) => policy.api_key_hash && policy.allowed_group_ids.length > 0);
  },

  async updateAPIKeyPolicies(items: APIKeyGroupPolicyInput[]): Promise<APIKeyGroupPolicy[]> {
    const response = await apiClient.put<APIKeyGroupPoliciesResponse>('/api-key-group-policies', {
      items: items.map((item) => ({
        api_key: item.api_key,
        api_key_hash: item.api_key_hash,
        allowed_group_ids: normalizeIds(item.allowed_group_ids),
      })),
    });
    return (response.policies ?? []).map(normalizePolicy);
  },

  deleteAPIKeyPolicy: (apiKeyHash: string) =>
    apiClient.delete('/api-key-group-policies', {
      params: { api_key_hash: apiKeyHash.trim().toLowerCase() },
    }),
};
