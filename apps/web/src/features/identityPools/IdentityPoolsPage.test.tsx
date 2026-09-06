import { act, create, type ReactTestRenderer } from 'react-test-renderer';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { StrictMode, type ReactNode } from 'react';

const mocks = vi.hoisted(() => ({
  auth: { apiBase: 'http://cpa.test', managementKey: 'key', connectionStatus: 'connected' },
  get: vi.fn(),
  accounts: vi.fn(),
  sessions: vi.fn(),
  stats: vi.fn(),
  rotate: vi.fn(),
  deleteSession: vi.fn(),
  patchProfile: vi.fn(),
  notify: vi.fn(),
}));

vi.mock('@/services/api', () => ({
  identityPoolsApi: mocks,
}));
vi.mock('@/stores', () => ({
  useAuthStore: (selector: (state: unknown) => unknown) => selector(mocks.auth),
  useNotificationStore: (selector: (state: unknown) => unknown) =>
    selector({ showNotification: mocks.notify }),
}));
vi.mock('@/components/ui/LoadingSpinner', () => ({
  LoadingSpinner: () => <div data-loading="true" />,
}));
vi.mock('@/components/ui/Button', () => ({
  Button: ({ children, ...props }: { children?: ReactNode; [key: string]: unknown }) => (
    <button {...props}>{children}</button>
  ),
}));
vi.mock('./IdentityPoolsPage.module.scss', () => ({
  default: new Proxy({}, { get: (_target, property) => String(property) }),
}));

vi.mock('react-i18next', () => ({
  useTranslation: () => ({
    t: (key: string) =>
      (
        ({
          'identity_pools.configuration': 'Configuration',
          'identity_pools.status.enabled': 'Enabled',
          'identity_pools.status.disabled': 'Disabled',
          'identity_pools.status.not_configured': 'Not configured',
          'identity_pools.status.unavailable': 'Unavailable',
          'identity_pools.status.disconnected': 'Disconnected',
          'identity_pools.title': 'Identity pools',
          'identity_pools.description': 'Stable account profiles and session mappings for outbound Codex requests.',
          'identity_pools.refresh': 'Refresh',
          'identity_pools.accounts': 'Accounts',
          'identity_pools.profiles': 'Profiles',
          'identity_pools.sessions': 'Sessions',
          'identity_pools.token_hit_rate': 'Token hit rate (5m window)',
          'identity_pools.request_hit_rate': 'Request hit rate (5m window)',
          'identity_pools.route_rebinds': 'Route rebinds',
          'identity_pools.prefix_heat_matches': 'Prefix heat matches',
          'identity_pools.tail_burst_fallbacks': 'Tail-burst fallbacks',
          'identity_pools.fingerprint_rejections': 'Fingerprint rejections',
          'identity_pools.observed_profiles': 'Observed profiles',
          'identity_pools.metadata_note': 'Only metadata is shown; credentials remain outside the panel.',
          'identity_pools.unknown': 'unknown',
          'identity_pools.platform_unknown': 'platform unknown',
          'identity_pools.disable': 'Disable',
          'identity_pools.enable': 'Enable',
          'identity_pools.account_bindings': 'Account bindings',
          'identity_pools.binding_note': 'Stable fingerprint, installation identity, and bound profile.',
          'identity_pools.rotate_identity': 'Rotate identity',
          'identity_pools.profile_label': 'Profile',
          'identity_pools.unassigned': 'unassigned',
          'identity_pools.loading_sessions': 'Loading sessions…',
          'identity_pools.no_active_sessions': 'No active sessions.',
          'identity_pools.clear': 'Clear',
        }) as Record<string, string>
      )[key] ?? key,
  }),
}));

import { IdentityPoolsPage } from './IdentityPoolsPage';

const deferred = <T,>() => {
  let resolve!: (value: T) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
};

const runtime = {
  enabled: true,
  pools: [{ id: 'codex', provider: 'codex', protocol: 'responses', profiles: [] }],
};
const accounts = {
  accounts: [
    {
      pool_id: 'codex',
      account_id: 'A',
      fingerprint: 'a',
      installation_id: 'ia',
      profile_id: 'p',
      identity_version: 1,
    },
    {
      pool_id: 'codex',
      account_id: 'B',
      fingerprint: 'b',
      installation_id: 'ib',
      profile_id: 'p',
      identity_version: 1,
    },
  ],
};

describe('IdentityPoolsPage request isolation', () => {
  beforeEach(() => {
    vi.resetAllMocks();
    mocks.auth = {
      apiBase: 'http://cpa.test',
      managementKey: 'key',
      connectionStatus: 'connected',
    };
    mocks.get.mockResolvedValue(runtime);
    mocks.accounts.mockResolvedValue(accounts);
    mocks.stats.mockResolvedValue({});
    mocks.sessions.mockResolvedValue({ sessions: [] });
  });

  it('does not reload the base view when the default account is selected', async () => {
    let renderer!: ReactTestRenderer;
    await act(async () => {
      renderer = create(<IdentityPoolsPage />);
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(mocks.get).toHaveBeenCalledTimes(1);
    expect(mocks.accounts).toHaveBeenCalledTimes(1);
    expect(mocks.sessions).toHaveBeenCalledTimes(1);
    await act(async () => renderer.unmount());
  });

  it('ignores a late session response from a previous account selection', async () => {
    const first = deferred<{
      sessions: Array<{
        pool_id: string;
        account_id: string;
        logical_session_id: string;
        prompt_cache_key: string;
      }>;
    }>();
    const second = deferred<{
      sessions: Array<{
        pool_id: string;
        account_id: string;
        logical_session_id: string;
        prompt_cache_key: string;
      }>;
    }>();
    mocks.sessions.mockReset();
    mocks.sessions.mockImplementation((_pool: string, account: string) =>
      account === 'A' ? first.promise : second.promise
    );
    let renderer!: ReactTestRenderer;
    await act(async () => {
      renderer = create(<IdentityPoolsPage />);
      await Promise.resolve();
    });
    await act(async () => {
      first.resolve({ sessions: [] });
      await Promise.resolve();
      await Promise.resolve();
    });
    const accountButtons = renderer.root
      .findAllByType('button')
      .filter((button) =>
        ['A', 'B'].includes(
          String(button.props.children?.[0]?.props?.children ?? button.props.children)
        )
      );
    expect(accountButtons).toHaveLength(2);
    await act(async () => {
      accountButtons[1].props.onClick();
      accountButtons[0].props.onClick();
      second.resolve({
        sessions: [
          {
            pool_id: 'codex',
            account_id: 'B',
            logical_session_id: 'late-b',
            prompt_cache_key: 'b',
          },
        ],
      });
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(
      renderer.root.findAll((node) => node.type === 'span' && node.children.includes('late-b'))
    ).toHaveLength(0);
    await act(async () => renderer.unmount());
  });
});

let view: ReactTestRenderer | undefined;
afterEach(async () => {
  if (view) {
    await act(async () => view?.unmount());
    view = undefined;
  }
});
const mountPage = async (strict = false) => {
  await act(async () => {
    view = create(
      strict ? (
        <StrictMode>
          <IdentityPoolsPage />
        </StrictMode>
      ) : (
        <IdentityPoolsPage />
      )
    );
  });
  return view!;
};
const pageText = () => JSON.stringify(view?.toJSON());
const press = async (label: string) => {
  const button = view!.root
    .findAllByType('button')
    .find(
      (node) =>
        node.props.children === label ||
        node.findAllByType('strong').some((item) => item.children.includes(label))
    );
  expect(button, 'missing button: ' + label).toBeDefined();
  await act(async () => {
    button!.props.onClick();
  });
};
const switchConnection = async (patch: Partial<typeof mocks.auth>) => {
  mocks.auth = { ...mocks.auth, ...patch };
  await act(async () => {
    view!.update(<IdentityPoolsPage />);
  });
};

describe('IdentityPoolsPage lifecycle acceptance', () => {
  beforeEach(() => {
    vi.resetAllMocks();
    mocks.auth = {
      apiBase: 'http://cpa.test',
      managementKey: 'key',
      connectionStatus: 'connected',
    };
    mocks.get.mockResolvedValue(runtime);
    mocks.accounts.mockResolvedValue(accounts);
    mocks.stats.mockResolvedValue({ token_weighted_hit_rate: 0.91 });
    mocks.sessions.mockResolvedValue({ sessions: [] });
  });

  it('loads after the StrictMode setup/cleanup/setup cycle', async () => {
    await mountPage(true);
    expect(pageText()).toContain('Account bindings');
    expect(mocks.sessions).toHaveBeenCalled();
  });

  it('loads accounts and sessions without waiting for diagnostics', async () => {
    const diagnostics = deferred<unknown>();
    mocks.stats.mockReturnValue(diagnostics.promise);
    await mountPage();
    expect(pageText()).toContain('Account bindings');
    expect(mocks.sessions).toHaveBeenCalledTimes(1);
    await press('B');
    await act(async () => {
      diagnostics.resolve({ token_weighted_hit_rate: 0.93 });
    });
    expect(pageText()).toContain('93.0%');
  });

  it('shows the account list while its first session query is pending', async () => {
    mocks.sessions.mockReturnValue(deferred<unknown>().promise);
    await mountPage();
    expect(pageText()).toContain('Account bindings');
  });

  it('clears old connection data when the replacement load fails', async () => {
    await mountPage();
    expect(pageText()).toContain('91.0%');
    mocks.get.mockRejectedValueOnce(new Error('new pool offline'));
    await switchConnection({ apiBase: 'http://other-cpa.test' });
    expect(pageText()).not.toContain('91.0%');
    expect(view!.root.findAllByType('strong').some((node) => node.children.includes('A'))).toBe(
      false
    );
    expect(pageText()).not.toContain('Ready');
  });

  it('clears stale rows on a failed refresh of the same connection', async () => {
    await mountPage();
    mocks.accounts.mockRejectedValueOnce(new Error('failed refresh'));
    await press('Refresh');
    expect(view!.root.findAllByType('strong').some((node) => node.children.includes('A'))).toBe(
      false
    );
    expect(pageText()).not.toContain('Ready');
  });

  it('discards old connection diagnostics and errors', async () => {
    const oldStats = deferred<unknown>();
    const oldBase = deferred<unknown>();
    mocks.get.mockReturnValueOnce(oldBase.promise);
    mocks.stats.mockReturnValueOnce(oldStats.promise);
    await mountPage();
    await switchConnection({ managementKey: 'replacement-key' });
    await act(async () => {
      oldStats.resolve({ token_weighted_hit_rate: 0.01 });
      oldBase.reject(new Error('old connection failed'));
    });
    expect(pageText()).toContain('91.0%');
    expect(mocks.notify).not.toHaveBeenCalled();
    expect(mocks.sessions).toHaveBeenLastCalledWith('codex', 'A', {
      apiBase: 'http://cpa.test',
      managementKey: 'replacement-key',
    });
  });

  it('isolates a late deletion from the same account and session on another connection', async () => {
    const pending = deferred<unknown>();
    const response = {
      sessions: [
        {
          pool_id: 'codex',
          account_id: 'A',
          logical_session_id: 'shared-session',
          prompt_cache_key: 'cache',
        },
      ],
    };
    mocks.sessions.mockResolvedValue(response);
    mocks.deleteSession.mockReturnValue(pending.promise);
    await mountPage();
    await press('Clear');
    await switchConnection({ apiBase: 'http://other-cpa.test' });
    await act(async () => {
      pending.resolve({ ok: true });
    });
    expect(pageText()).toContain('shared-session');
    expect(mocks.deleteSession).toHaveBeenCalledWith('codex', 'A', 'shared-session', {
      apiBase: 'http://cpa.test',
      managementKey: 'key',
    });
  });

  it('stops reading on disconnect and ignores late session results', async () => {
    const pending = deferred<unknown>();
    await mountPage();
    mocks.sessions.mockReturnValueOnce(pending.promise);
    await press('B');
    const callCount = mocks.get.mock.calls.length;
    await switchConnection({ connectionStatus: 'disconnected' });
    await act(async () => {
      pending.resolve({
        sessions: [
          { account_id: 'B', logical_session_id: 'old-session', prompt_cache_key: 'cache' },
        ],
      });
    });
    expect(pageText()).not.toContain('old-session');
    expect(mocks.get).toHaveBeenCalledTimes(callCount);
  });

  it('distinguishes disabled configuration from a missing pool', async () => {
    mocks.get.mockResolvedValue({ ...runtime, enabled: false });
    await mountPage();
    expect(pageText()).toContain('Disabled');
    expect(pageText()).not.toContain('Ready');
  });

  it('selects the Codex pool rather than the first other-provider pool', async () => {
    mocks.get.mockResolvedValue({
      enabled: true,
      pools: [{ id: 'other', profiles: [{ id: 'other-only' }] }, ...runtime.pools],
    });
    await mountPage();
    expect(pageText()).not.toContain('other-only');
  });

  it('uses a remaining account after the selected account is removed', async () => {
    await mountPage();
    await press('B');
    mocks.accounts.mockResolvedValue({ accounts: [accounts.accounts[0]] });
    await press('Refresh');
    expect(mocks.sessions).toHaveBeenLastCalledWith('codex', 'A', {
      apiBase: 'http://cpa.test',
      managementKey: 'key',
    });
  });
  it('ignores an earlier pending query after switching A to B to A', async () => {
    const earlier = deferred<unknown>();
    const newer = deferred<unknown>();
    mocks.sessions
      .mockReturnValueOnce(earlier.promise)
      .mockResolvedValueOnce({ sessions: [] })
      .mockReturnValueOnce(newer.promise);
    await mountPage();
    await press('B');
    await press('A');
    await act(async () => {
      newer.resolve({
        sessions: [
          {
            pool_id: 'codex',
            account_id: 'A',
            logical_session_id: 'new-a',
            prompt_cache_key: 'cache',
          },
        ],
      });
    });
    await act(async () => {
      earlier.resolve({
        sessions: [
          {
            pool_id: 'codex',
            account_id: 'A',
            logical_session_id: 'old-a',
            prompt_cache_key: 'cache',
          },
        ],
      });
    });
    expect(pageText()).toContain('new-a');
    expect(pageText()).not.toContain('old-a');
    expect(mocks.get).toHaveBeenCalledTimes(1);
  });

  it('discards results and errors after unmount', async () => {
    const pending = deferred<unknown>();
    mocks.sessions.mockReturnValue(pending.promise);
    await mountPage();
    await act(async () => {
      view!.unmount();
    });
    view = undefined;
    await act(async () => {
      pending.reject(new Error('late after unmount'));
    });
    expect(mocks.notify).not.toHaveBeenCalled();
  });

  it('keeps diagnostics unavailable when the endpoint fails', async () => {
    mocks.stats.mockRejectedValue(new Error('not supported'));
    await mountPage();
    expect(pageText()).toContain('Account bindings');
    expect(pageText()).not.toContain('0.0%');
    expect(mocks.notify).not.toHaveBeenCalled();
  });

  it('renders a dash for a partially unknown diagnostics field', async () => {
    mocks.stats.mockResolvedValue({ route_rebinds: 2 });
    await mountPage();
    expect(pageText()).toContain('Route rebinds');
    expect(view!.root.findAllByType('strong').some((node) => node.children.includes('—'))).toBe(
      true
    );
  });

  it('renders tail-burst and fingerprint diagnostics when measured', async () => {
    mocks.stats.mockResolvedValue({
      token_weighted_hit_rate: 0.91,
      tail_burst_fallbacks: 3,
      engine_fingerprint_rejections: 2,
    });
    await mountPage();
    expect(pageText()).toContain('Tail-burst fallbacks');
    expect(pageText()).toContain('Fingerprint rejections');
    expect(view!.root.findAllByType('strong').some((node) => node.children.includes('3'))).toBe(true);
    expect(view!.root.findAllByType('strong').some((node) => node.children.includes('2'))).toBe(true);
  });
});
