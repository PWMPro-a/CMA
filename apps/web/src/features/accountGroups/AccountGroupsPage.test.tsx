import { act, create, type ReactTestInstance, type ReactTestRenderer } from 'react-test-renderer';
import type { ReactNode } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { sha256Hex } from '@/utils/apiKeyHash';

const { mocks } = vi.hoisted(() => ({
  mocks: {
    listGroups: vi.fn(),
    createGroup: vi.fn(),
    updateGroup: vi.fn(),
    deleteGroup: vi.fn(),
    updateMemberships: vi.fn(),
    listPolicies: vi.fn(),
    updatePolicies: vi.fn(),
    deletePolicy: vi.fn(),
    listAuthFiles: vi.fn(),
    listApiKeys: vi.fn(),
    showConfirmation: vi.fn(),
    showNotification: vi.fn(),
  },
}));

vi.mock('react-i18next', () => ({
  useTranslation: () => ({
    t: (key: string, options?: Record<string, unknown>) =>
      options ? `${key}:${JSON.stringify(options)}` : key,
  }),
}));

vi.mock('@/stores', () => ({
  useAuthStore: (selector: (state: unknown) => unknown) =>
    selector({ connectionStatus: 'connected' }),
  useNotificationStore: (selector: (state: unknown) => unknown) =>
    selector({
      showConfirmation: mocks.showConfirmation,
      showNotification: mocks.showNotification,
    }),
}));

vi.mock('@/services/api', () => ({
  accountGroupsApi: {
    list: mocks.listGroups,
    create: mocks.createGroup,
    update: mocks.updateGroup,
    delete: mocks.deleteGroup,
    updateMemberships: mocks.updateMemberships,
    listAPIKeyPolicies: mocks.listPolicies,
    updateAPIKeyPolicies: mocks.updatePolicies,
    deleteAPIKeyPolicy: mocks.deletePolicy,
  },
  apiKeysApi: { list: mocks.listApiKeys },
  authFilesApi: { listForGrouping: mocks.listAuthFiles },
}));

vi.mock('@/components/ui/Button', () => ({
  Button: ({
    children,
    loading,
    ...props
  }: {
    children?: ReactNode;
    loading?: boolean;
    [key: string]: unknown;
  }) => (
    <button {...props} disabled={Boolean(props.disabled) || loading}>
      {children}
    </button>
  ),
}));

vi.mock('@/components/ui/Input', () => ({
  Input: ({ label, ...props }: { label?: string; [key: string]: unknown }) => (
    <label>
      {label}
      <input {...props} />
    </label>
  ),
}));

vi.mock('@/components/ui/LoadingSpinner', () => ({
  LoadingSpinner: () => <div data-loading="true" />,
}));

vi.mock('@/components/ui/Modal', () => ({
  Modal: ({
    open,
    title,
    children,
    footer,
  }: {
    open: boolean;
    title?: ReactNode;
    children?: ReactNode;
    footer?: ReactNode;
  }) =>
    open ? (
      <section data-modal-title={String(title)}>
        <h2>{title}</h2>
        {children}
        <footer>{footer}</footer>
      </section>
    ) : null,
}));

vi.mock('@/components/ui/SegmentedTabs', () => ({
  SegmentedTabs: ({
    items,
    activeTab,
    onChange,
  }: {
    items: Array<{ id: string; label: ReactNode }>;
    activeTab: string;
    onChange?: (id: string) => void;
  }) => (
    <div role="tablist">
      {items.map((item) => (
        <button
          key={item.id}
          role="tab"
          aria-selected={item.id === activeTab}
          onClick={() => onChange?.(item.id)}
        >
          {item.label}
        </button>
      ))}
    </div>
  ),
}));

vi.mock('@/components/ui/Select', () => ({
  Select: ({
    value,
    options,
    onChange,
    ariaLabel,
  }: {
    value: string;
    options: Array<{ value: string; label: string }>;
    onChange: (value: string) => void;
    ariaLabel?: string;
  }) => (
    <select aria-label={ariaLabel} value={value} onChange={(event) => onChange(event.target.value)}>
      {options.map((option) => (
        <option key={option.value} value={option.value}>
          {option.label}
        </option>
      ))}
    </select>
  ),
}));

vi.mock('@/components/ui/icons', () => {
  const Icon = () => null;
  return {
    IconCheck: Icon,
    IconKey: Icon,
    IconPencil: Icon,
    IconPlus: Icon,
    IconRefreshCw: Icon,
    IconSearch: Icon,
    IconTrash2: Icon,
  };
});

vi.mock('./AccountGroupsPage.module.scss', () => ({
  default: new Proxy({}, { get: (_target, property) => String(property) }),
}));

vi.mock('./AccountGroupControls', () => ({
  AccountGroupBadges: ({ ids }: { ids: number[] }) => <span data-group-badges={ids.join(',')} />,
  AccountGroupPicker: ({
    groups,
    value,
    onChange,
    disabled,
  }: {
    groups: Array<{ id: number; name: string }>;
    value: number[];
    onChange: (ids: number[]) => void;
    disabled?: boolean;
  }) => (
    <div data-group-picker="true">
      {groups.map((group) => {
        const selected = value.includes(group.id);
        return (
          <button
            key={group.id}
            type="button"
            aria-label={`group-${group.id}`}
            disabled={disabled}
            onClick={() =>
              onChange(
                selected
                  ? value.filter((id) => id !== group.id)
                  : [...value, group.id].sort((left, right) => left - right)
              )
            }
          >
            {group.name}
          </button>
        );
      })}
    </div>
  ),
}));

import { AccountGroupsPage } from './AccountGroupsPage';

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const groups = [
  {
    id: 1,
    name: 'Production',
    description: 'Stable accounts',
    color: '#14b8a6',
    sort_order: 1,
    member_count: 1,
    api_key_count: 0,
  },
  {
    id: 2,
    name: 'Canary',
    description: 'Canary accounts',
    color: '#0ea5e9',
    sort_order: 2,
    member_count: 1,
    api_key_count: 0,
  },
];

const authFiles = {
  files: [
    {
      id: 'runtime-a',
      name: 'account-a.json',
      provider: 'codex',
      authIndex: 0,
      group_ids: [1],
    },
    {
      id: 'runtime-b',
      name: 'account-b.json',
      provider: 'codex',
      authIndex: 1,
      group_ids: [],
    },
  ],
};

const flushPromises = async () => {
  await Promise.resolve();
  await Promise.resolve();
};

const getText = (node: ReactTestInstance): string =>
  node.children
    .map((child) =>
      typeof child === 'string' || typeof child === 'number' ? String(child) : getText(child)
    )
    .join('');

const findButton = (renderer: ReactTestRenderer, text: string, occurrence = 0) => {
  const matches = renderer.root
    .findAllByType('button')
    .filter((button) => getText(button).includes(text));
  const button = matches[occurrence];
  if (!button) throw new Error(`Button not found: ${text} (${occurrence})`);
  return button;
};

const renderPage = async () => {
  let renderer!: ReactTestRenderer;
  await act(async () => {
    renderer = create(<AccountGroupsPage />);
    await flushPromises();
  });
  return renderer;
};

describe('AccountGroupsPage', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mocks.listGroups.mockResolvedValue(groups);
    mocks.createGroup.mockResolvedValue(groups[0]);
    mocks.updateGroup.mockResolvedValue(groups[0]);
    mocks.deleteGroup.mockResolvedValue({ status: 'ok' });
    mocks.updateMemberships.mockResolvedValue({ status: 'ok', updated: 1 });
    mocks.listPolicies.mockResolvedValue([]);
    mocks.updatePolicies.mockResolvedValue([]);
    mocks.deletePolicy.mockResolvedValue({ status: 'ok' });
    mocks.listAuthFiles.mockResolvedValue(authFiles);
    mocks.listApiKeys.mockResolvedValue(['sk-test-key']);
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('loads groups, accounts, and keys into the independent workspace', async () => {
    const renderer = await renderPage();
    const text = getText(renderer.root);

    expect(mocks.listGroups).toHaveBeenCalledTimes(1);
    expect(mocks.listAuthFiles).toHaveBeenCalledTimes(1);
    expect(mocks.listApiKeys).toHaveBeenCalledTimes(1);
    expect(mocks.listPolicies).toHaveBeenCalledTimes(1);
    expect(text).toContain('Production');
    expect(text).toContain('account-a.json');
    expect(text).toContain('account_groups.metric_groups');

    act(() => renderer.unmount());
  });

  it('saves single and batch account membership updates', async () => {
    const renderer = await renderPage();
    const accountButtons = renderer.root
      .findAllByType('button')
      .filter((button) => getText(button).includes('account_groups.edit_membership'));

    act(() => accountButtons[0]?.props.onClick());
    act(() => renderer.root.findByProps({ 'aria-label': 'group-1' }).props.onClick());
    act(() => renderer.root.findByProps({ 'aria-label': 'group-2' }).props.onClick());
    await act(async () => {
      findButton(renderer, 'account_groups.apply_membership').props.onClick();
      await flushPromises();
    });

    expect(mocks.updateMemberships).toHaveBeenNthCalledWith(1, [
      { name: 'runtime-a', auth_index: '0', group_ids: [2] },
    ]);

    const checkboxes = renderer.root
      .findAllByType('input')
      .filter((input) => input.props.type === 'checkbox');
    await act(async () => {
      checkboxes[0]?.props.onChange();
      checkboxes[1]?.props.onChange();
      await flushPromises();
    });
    act(() => findButton(renderer, 'account_groups.batch_assign').props.onClick());
    act(() => renderer.root.findByProps({ 'aria-label': 'group-1' }).props.onClick());
    await act(async () => {
      findButton(renderer, 'account_groups.apply_membership').props.onClick();
      await flushPromises();
    });

    expect(mocks.updateMemberships).toHaveBeenNthCalledWith(2, [
      { name: 'runtime-a', auth_index: '0', group_ids: [1] },
      { name: 'runtime-b', auth_index: '1', group_ids: [1] },
    ]);

    act(() => renderer.unmount());
  });

  it('saves a restricted API key policy for the selected groups', async () => {
    const renderer = await renderPage();
    const keyTab = renderer.root
      .findAllByProps({ role: 'tab' })
      .find((tab) => getText(tab).includes('account_groups.keys_tab'));
    act(() => keyTab?.props.onClick());
    act(() => findButton(renderer, 'account_groups.edit_policy').props.onClick());
    act(() => findButton(renderer, 'account_groups.restricted').props.onClick());
    act(() => renderer.root.findByProps({ 'aria-label': 'group-2' }).props.onClick());

    await act(async () => {
      findButton(renderer, 'common.save').props.onClick();
      await flushPromises();
    });

    expect(mocks.updatePolicies).toHaveBeenCalledWith([
      {
        api_key_hash: sha256Hex('sk-test-key').toLowerCase(),
        allowed_group_ids: [2],
      },
    ]);

    act(() => renderer.unmount());
  });

  it('uses force deletion when the group has members', async () => {
    const renderer = await renderPage();
    act(() => findButton(renderer, 'common.delete').props.onClick());

    const confirmation = mocks.showConfirmation.mock.calls[0]?.[0] as {
      onConfirm: () => Promise<void>;
    };
    expect(confirmation).toBeDefined();

    await act(async () => {
      await confirmation.onConfirm();
      await flushPromises();
    });
    expect(mocks.deleteGroup).toHaveBeenCalledWith(1, true);

    act(() => renderer.unmount());
  });

  it('renders the API error instead of leaving the page in a loading state', async () => {
    mocks.listGroups.mockRejectedValueOnce(new Error('group service unavailable'));
    const renderer = await renderPage();

    expect(getText(renderer.root)).toContain('group service unavailable');
    expect(getText(renderer.root)).toContain('account_groups.load_failed');

    act(() => renderer.unmount());
  });
});
