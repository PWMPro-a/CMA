import { describe, expect, it } from 'vitest';
import { resolveManagerPanelVersion } from './versionChecks';

describe('resolveManagerPanelVersion', () => {
  it('keeps a compact panel build version', () => {
    expect(resolveManagerPanelVersion('20260816-152233', '20260816-152200')).toBe(
      '20260816-152233'
    );
  });

  it('replaces an upstream tag with the Manager Server deployment version', () => {
    expect(resolveManagerPanelVersion('v1.12.0-beta2-159-ge035a9a1', '20260816-152200')).toBe(
      '20260816-152200'
    );
  });
});
