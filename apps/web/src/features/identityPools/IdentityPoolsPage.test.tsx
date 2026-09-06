import { act, create, type ReactTestRenderer } from 'react-test-renderer';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { type ReactNode } from 'react';

const mocks = vi.hoisted(() => ({
  auth: { apiBase: 'http://cpa.test', managementKey: 'key', connectionStatus: 'connected' },
  get: vi.fn(), accounts: vi.fn(), sessions: vi.fn(), rotate: vi.fn(), deleteSession: vi.fn(), patchProfile: vi.fn(), notify: vi.fn(),
}));
vi.mock('@/services/api', () => ({ identityPoolsApi: mocks }));
vi.mock('@/stores', () => ({
  useAuthStore: (selector: (state: unknown) => unknown) => selector(mocks.auth),
  useNotificationStore: (selector: (state: unknown) => unknown) => selector({ showNotification: mocks.notify }),
}));
vi.mock('@/components/ui/LoadingSpinner', () => ({ LoadingSpinner: () => <div data-loading="true" /> }));
vi.mock('@/components/ui/Button', () => ({ Button: ({ children, ...props }: { children?: ReactNode; [key: string]: unknown }) => <button {...props}>{children}</button> }));
vi.mock('@/components/ui/Drawer', () => ({ Drawer: ({ children }: { children?: ReactNode }) => <aside>{children}</aside> }));
vi.mock('@/components/ui/SegmentedTabs.module.scss', () => ({ default: new Proxy({}, { get: (_target, property) => String(property) }) }));
vi.mock('@/components/ui/Select.module.scss', () => ({ default: new Proxy({}, { get: (_target, property) => String(property) }) }));
vi.mock('@/components/ui/Drawer.module.scss', () => ({ default: new Proxy({}, { get: (_target, property) => String(property) }) }));
vi.mock('./IdentityPoolsPage.module.scss', () => ({ default: new Proxy({}, { get: (_target, property) => String(property) }) }));
vi.mock('react-i18next', () => ({
  useTranslation: () => ({ t: (key: string) => {
    const labels: Record<string, string> = {
      'identity_pools.title':'Identity pools','identity_pools.description':'Stable identities','identity_pools.refresh':'Refresh','identity_pools.status.enabled':'Enabled','identity_pools.status.disconnected':'Disconnected','identity_pools.status.unavailable':'Unavailable','identity_pools.status.not_configured':'Not configured','identity_pools.status.disabled':'Disabled','identity_pools.accounts':'Accounts','identity_pools.identities':'Identities','identity_pools.sessions':'Sessions','identity_pools.enabled_environments':'Enabled environments','identity_pools.account_identities':'Account identities','identity_pools.environment_templates':'Environment templates','identity_pools.summary':'Summary','identity_pools.view':'View','identity_pools.search':'Search','identity_pools.search_placeholder':'Search','identity_pools.environment_filter':'Environment','identity_pools.session_filter':'Sessions','identity_pools.all_environments':'All environments','identity_pools.all_sessions':'All sessions','identity_pools.with_sessions':'With sessions','identity_pools.without_sessions':'Without sessions','identity_pools.account':'Account','identity_pools.identity':'Identity','identity_pools.environment':'Environment','identity_pools.session_count':'Sessions','identity_pools.account_status':'Status','identity_pools.unnamed_account':'Unnamed','identity_pools.identity_version':'Identity version','identity_pools.environment_unknown':'Unknown environment','identity_pools.no_accounts':'No accounts','identity_pools.environment_note':'Environment metadata','identity_pools.user_agent_unknown':'Unknown user agent','identity_pools.enabled':'Enabled','identity_pools.disabled':'Disabled','identity_pools.disable':'Disable','identity_pools.enable':'Enable','identity_pools.no_environments':'No environments','identity_pools.profile_update_failed':'Profile update failed','identity_pools.account_details':'Account details','identity_pools.auth_file':'Auth file','identity_pools.status_label':'Status','identity_pools.session_mapping_note':'Session mapping','identity_pools.no_cache_key':'No key','identity_pools.rotate_confirm':'Rotate?','identity_pools.rotate_success':'Rotated','identity_pools.rotate_failed':'Rotate failed','identity_pools.clear_failed':'Clear failed','identity_pools.load_failed':'Load failed','identity_pools.loading':'Loading','identity_pools.loading_sessions':'Loading sessions','identity_pools.no_active_sessions':'No sessions','identity_pools.rotate_identity':'Rotate identity','identity_pools.clear':'Clear',
    };
    return labels[key] ?? key;
  } }),
}));
vi.mock('@/components/ui/icons', () => ({ IconChevronRight: () => <span>→</span>, IconRefreshCw: () => <span>↻</span>, IconSearch: () => <span>⌕</span>, IconTrash2: () => <span>×</span>, IconChevronDown: () => <span>⌄</span>, IconX: () => <span>×</span>, IconCopy: () => <span>copy</span>, IconCheck: () => <span>check</span> }));

import { IdentityPoolsPage } from './IdentityPoolsPage';

const runtime = { enabled: true, pools: [{ id: 'codex', provider: 'codex', protocol: 'responses', profiles: [{ id: 'cli', version: '0.147.0', platform: 'macOS', architecture: 'arm64', user_agent: 'codex-tui', originator: 'codex-tui', enabled: true, observed: true }] }] };
const accounts = { accounts: [
  { pool_id: 'codex', account_id: 'A', email: 'alice@example.com', auth_file: 'alice.json', auth_index: 'auth-a', installation_id: 'install-a', fingerprint: 'finger-a', profile_id: 'cli', identity_version: 2, session_count: 2, runtime_status: 'active' },
  { pool_id: 'codex', account_id: 'B', account_label: 'Bob', auth_file: 'bob.json', installation_id: 'install-b', fingerprint: 'finger-b', profile_id: 'cli', identity_version: 1, session_count: 0, runtime_status: 'disabled', disabled: true },
] };
let view: ReactTestRenderer | undefined;
const mount = async () => { await act(async () => { view = create(<IdentityPoolsPage />); await Promise.resolve(); await Promise.resolve(); }); return view!; };
afterEach(async () => { if (view) { await act(async () => view?.unmount()); view = undefined; } });
beforeEach(() => { vi.resetAllMocks(); mocks.auth = { apiBase: 'http://cpa.test', managementKey: 'key', connectionStatus: 'connected' }; mocks.get.mockResolvedValue(runtime); mocks.accounts.mockResolvedValue(accounts); mocks.sessions.mockResolvedValue({ sessions: [] }); mocks.rotate.mockResolvedValue({}); mocks.deleteSession.mockResolvedValue({}); mocks.patchProfile.mockResolvedValue({}); });

describe('IdentityPoolsPage operations workspace', () => {
  it('renders account identity, environment and server-provided session counts', async () => {
    await mount();
    const text = JSON.stringify(view?.toJSON());
    expect(text).toContain('alice@example.com');
    expect(text).toContain('alice.json');
    expect(text).toContain('install-a');
    expect(text).toContain('cli');
    expect(text).toContain('2');
    expect(text).toContain('macOS');
    expect(mocks.get).toHaveBeenCalledTimes(1);
    expect(mocks.accounts).toHaveBeenCalledTimes(1);
  });

  it('does not request sessions until an account is opened', async () => {
    await mount();
    expect(mocks.sessions).not.toHaveBeenCalled();
    const row = view!.root.findAllByType('tr').find((node) => node.props.tabIndex === 0);
    expect(row).toBeDefined();
    await act(async () => { row!.props.onClick(); await Promise.resolve(); });
    expect(mocks.sessions).toHaveBeenCalledWith('codex', 'A', expect.anything());
  });

  it('keeps connection-owned data isolated after switching endpoint', async () => {
    await mount();
    expect(JSON.stringify(view?.toJSON())).toContain('alice@example.com');
    mocks.auth = { ...mocks.auth, apiBase: 'http://other.test' };
    mocks.get.mockRejectedValueOnce(new Error('offline'));
    await act(async () => { view!.update(<IdentityPoolsPage />); await Promise.resolve(); await Promise.resolve(); });
    expect(JSON.stringify(view?.toJSON())).not.toContain('alice@example.com');
  });

  it('renders environment templates separately and keeps profile toggle wired', async () => {
    await mount();
    const tabs = view!.root.findAllByType('button').filter((node) => node.props.role === 'tab');
    expect(tabs).toHaveLength(2);
    await act(async () => { tabs[1].props.onClick({ preventDefault() {} }); await Promise.resolve(); });
    expect(JSON.stringify(view?.toJSON())).toContain('macOS');
    const toggle = view!.root.findAllByType('button').find((node) => node.props.children === 'Disable');
    expect(toggle).toBeDefined();
    await act(async () => { toggle!.props.onClick(); await Promise.resolve(); });
    expect(mocks.patchProfile).toHaveBeenCalledWith('codex', 'cli', { enabled: false }, expect.anything());
  });
});
