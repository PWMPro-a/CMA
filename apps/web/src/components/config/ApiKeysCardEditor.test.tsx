import { act, create, type ReactTestRenderer } from 'react-test-renderer';
import { beforeEach, describe, expect, it, vi } from 'vitest';

const mocks = vi.hoisted(() => ({
  listGroups: vi.fn(),
  listPolicies: vi.fn(),
  listApiKeys: vi.fn(),
  showConfirmation: vi.fn(),
  showNotification: vi.fn(),
}));

vi.mock('react-i18next', () => ({
  useTranslation: () => ({ t: (key: string) => key }),
}));

vi.mock('@/stores', () => ({
  useAuthStore: (selector: (state: unknown) => unknown) =>
    selector({ apiBase: 'http://cpa.local:8317', managementKey: 'management-key' }),
  useNotificationStore: (selector: (state: unknown) => unknown) =>
    selector({
      showConfirmation: mocks.showConfirmation,
      showNotification: mocks.showNotification,
    }),
}));

vi.mock('@/hooks/usePanelFeatureAvailability', () => ({
  usePanelFeatureAvailability: () => ({
    managerServiceAvailable: false,
    managerServiceBase: '',
  }),
}));

vi.mock('@/services/api', () => ({
  accountGroupsApi: {
    list: mocks.listGroups,
    listAPIKeyPolicies: mocks.listPolicies,
    updateAPIKeyPolicies: vi.fn(),
    deleteAPIKeyPolicy: vi.fn(),
  },
  apiKeysApi: {
    list: mocks.listApiKeys,
  },
}));

vi.mock('@/services/api/usageService', () => ({
  usageServiceApi: {
    getApiKeyAliases: vi.fn(),
  },
}));

vi.mock('@/components/ui/Modal', () => ({
  Modal: () => null,
}));

vi.mock('@/features/accountGroups/AccountGroupControls', () => ({
  AccountGroupBadges: () => null,
  AccountGroupPicker: () => null,
}));

import { ApiKeysCardEditor } from './ApiKeysCardEditor';

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

describe('ApiKeysCardEditor group policy refresh', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mocks.listGroups.mockResolvedValue([]);
    mocks.listPolicies.mockResolvedValue([]);
  });

  it('refreshes active API keys after the saved config revision changes', async () => {
    const apiKey = 'sk-newly-saved';
    mocks.listApiKeys.mockResolvedValueOnce([]).mockResolvedValueOnce([apiKey]);

    let renderer!: ReactTestRenderer;
    await act(async () => {
      renderer = create(
        <ApiKeysCardEditor value={apiKey} refreshToken={0} onChange={vi.fn()} />
      );
    });

    const findGroupButton = () =>
      renderer.root
        .findAllByType('button')
        .find((button) =>
          String(button.props.title ?? '').includes(
            'config_management.visual.api_keys.groups_'
          )
        );

    expect(findGroupButton()?.props.disabled).toBe(true);

    await act(async () => {
      renderer.update(
        <ApiKeysCardEditor value={apiKey} refreshToken={1} onChange={vi.fn()} />
      );
    });

    expect(mocks.listApiKeys).toHaveBeenCalledTimes(2);
    expect(findGroupButton()?.props.disabled).toBe(false);

    act(() => renderer.unmount());
  });
});
