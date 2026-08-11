import { describe, expect, it } from 'vitest';
import { resolveCodexChatgptAccountId, resolveCodexPlanType } from './resolvers';

describe('Sub2API Team metadata resolvers', () => {
  it('uses a Team workspace as the quota request identity when account id is absent', () => {
    expect(
      resolveCodexChatgptAccountId({
        name: 'sub2-team.json',
        type: 'codex',
        metadata: {
          workspaceId: 'team-workspace',
        },
      })
    ).toBe('team-workspace');
  });

  it('does not let a generic free fallback mask the Team plan', () => {
    expect(
      resolveCodexPlanType({
        name: 'sub2-team.json',
        type: 'codex',
        plan_type: 'free',
        chatgpt_plan_type: 'team',
      })
    ).toBe('team');
  });
});

describe('resolveCodexChatgptAccountId', () => {
  it.each([
    { field: 'chatgpt_account_id', value: 'chatgpt-account' },
    { field: 'chatgptAccountId', value: 'chatgpt-account-camel' },
    { field: 'account_id', value: 'account' },
    { field: 'accountId', value: 'account-camel' },
  ])('reads a direct $field string', ({ field, value }) => {
    expect(resolveCodexChatgptAccountId({ name: 'codex.json', [field]: ` ${value} ` })).toBe(value);
  });

  it('still extracts an account ID from an id_token payload object', () => {
    expect(
      resolveCodexChatgptAccountId({
        name: 'codex.json',
        id_token: { account_id: 'token-account' },
      })
    ).toBe('token-account');
  });
});
