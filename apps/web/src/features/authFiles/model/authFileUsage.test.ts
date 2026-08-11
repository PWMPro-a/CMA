import { describe, expect, it } from 'vitest';
import { buildAuthFileUsageTarget, getAuthFileUsageKey } from './authFileUsage';

describe('auth file usage targets', () => {
  it('matches the Manager collector account identity fields', () => {
    const target = buildAuthFileUsageTarget({
      name: 'alice.json',
      type: 'codex',
      auth_index: 'auth-1',
      account: 'alice@example.com',
      email: 'fallback@example.com',
      label: 'Alice',
      source: 'team',
    });

    expect(target).toEqual({
      key: 'alice.json::auth-1',
      request: {
        row_key: 'alice.json::auth-1',
        account_snapshot: 'alice@example.com',
        auth_label_snapshot: 'Alice',
        auth_index: 'auth-1',
        source: 'team',
      },
    });
  });

  it('skips secret-looking account values and uses the email identity', () => {
    const target = buildAuthFileUsageTarget({
      name: 'secret-account.json',
      authIndex: 7,
      account: 'sk-abcdefghijklmnopqrstuvwxyz1234567890',
      email: 'safe@example.com',
    });

    expect(target.request.account_snapshot).toBe('safe@example.com');
    expect(target.request.auth_index).toBe('7');
  });

  it('falls back to the label or file name exactly like snapshot enrichment', () => {
    expect(
      buildAuthFileUsageTarget({ name: 'label.json', label: 'Shared account' }).request
        .account_snapshot
    ).toBe('Shared account');
    expect(getAuthFileUsageKey({ name: 'no-index.json' })).toBe('no-index.json::-');
  });
});
