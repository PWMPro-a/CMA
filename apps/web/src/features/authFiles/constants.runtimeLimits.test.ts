import { describe, expect, it } from 'vitest';
import {
  classifyAuthFileOperationalState,
  hasAuthFileFreezeConfig,
  hasAuthFileRateLimitConfig,
  isAuthFileRuntimeUnlimited,
  isUsableAuthCredential,
  parseNonNegativeIntegerValue,
  readAuthFileBooleanField,
  readAuthFileIntegerField,
} from './constants';

describe('auth file runtime limits', () => {
  it('parses only non-negative integer values', () => {
    expect(parseNonNegativeIntegerValue(0)).toBe(0);
    expect(parseNonNegativeIntegerValue('12')).toBe(12);
    expect(parseNonNegativeIntegerValue('-1')).toBeUndefined();
    expect(parseNonNegativeIntegerValue('1.5')).toBeUndefined();
    expect(parseNonNegativeIntegerValue('')).toBeUndefined();
  });

  it('reads snake_case and camelCase runtime fields', () => {
    expect(readAuthFileIntegerField({ max_concurrency: '1' }, 'max_concurrency')).toBe(1);
    expect(
      readAuthFileIntegerField({ maxConcurrency: 2 }, 'max_concurrency', 'maxConcurrency')
    ).toBe(2);
    expect(
      readAuthFileBooleanField(
        { disableStickyOnNextRequest: 'true' },
        'disable_sticky_on_next_request',
        'disableStickyOnNextRequest'
      )
    ).toBe(true);
  });

  it('treats missing or zero concurrency and rate limit as unlimited', () => {
    expect(isAuthFileRuntimeUnlimited({ name: 'missing.json' })).toBe(true);
    expect(
      isAuthFileRuntimeUnlimited({
        name: 'zero.json',
        max_concurrency: 0,
        rate_limit_max_requests: 0,
      })
    ).toBe(true);
    expect(
      isAuthFileRuntimeUnlimited({
        name: 'limited.json',
        max_concurrency: 1,
      })
    ).toBe(false);
  });

  it('detects rate-limit and freeze configs independently', () => {
    expect(hasAuthFileRateLimitConfig({ name: 'rate.json', rate_limit_max_requests: 3 })).toBe(
      true
    );
    expect(hasAuthFileRateLimitConfig({ name: 'rate-zero.json', rate_limit_max_requests: 0 })).toBe(
      false
    );
    expect(
      hasAuthFileFreezeConfig({
        name: 'freeze.json',
        selection_error_freeze_seconds: 30,
      })
    ).toBe(true);
  });

  it('treats runtime health and quota states as credential-usable', () => {
    expect(isUsableAuthCredential({ name: 'new.json', success: 0, failed: 3 })).toBe(true);
    expect(
      isUsableAuthCredential({
        name: 'quota.json',
        statusMessage: 'stability_budget_exhausted · quota temporarily unavailable',
      })
    ).toBe(true);
    expect(
      isUsableAuthCredential(
        {
          name: 'observed-quota.json',
          statusMessage: '',
        },
        { isHttp401: false, needsReauth: false }
      )
    ).toBe(true);
  });

  it('rejects credentials with hard OAuth failures', () => {
    expect(isUsableAuthCredential({ name: 'disabled.json', disabled: true })).toBe(false);
    expect(isUsableAuthCredential({ name: 'gone.json', status: 'expired' })).toBe(false);
    expect(
      isUsableAuthCredential({ name: 'reauth.json', statusMessage: 'invalid_grant login_required' })
    ).toBe(false);
    expect(isUsableAuthCredential({ name: '401.json' }, { isHttp401: true })).toBe(false);
  });

  it('classifies temporary rate limits as cooldown without hiding hard failures', () => {
    expect(
      classifyAuthFileOperationalState({
        name: 'limited.json',
        statusMessage: '{"detail":"Rate limit exceeded"}',
        recent_requests: [{ time: 'now', success: 8, failed: 2 }],
      })
    ).toBe('cooldown');
    expect(
      classifyAuthFileOperationalState({
        name: '429.json',
        statusMessage: 'HTTP 429 too many requests',
      })
    ).toBe('cooldown');
    expect(
      classifyAuthFileOperationalState({
        name: 'invalid.json',
        statusMessage: 'invalid_grant login_required',
      })
    ).toBe('failed');
    expect(
      classifyAuthFileOperationalState({
        name: 'disabled-limited.json',
        disabled: true,
        statusMessage: 'HTTP 429 too many requests',
      })
    ).toBe('failed');
    expect(
      classifyAuthFileOperationalState({
        name: 'temporary-upstream.json',
        unavailable: true,
        statusMessage: 'service temporarily unavailable',
        recent_requests: [{ time: 'now', success: 1, failed: 1 }],
      })
    ).toBe('cooldown');
    expect(
      classifyAuthFileOperationalState({
        name: 'recovering.json',
        status: 'recovery_failed',
        unavailable: true,
        statusMessage: 'recovery failed; retrying: usage endpoint returned 429',
      })
    ).toBe('cooldown');
  });
});
