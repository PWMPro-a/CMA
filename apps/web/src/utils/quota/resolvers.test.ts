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
