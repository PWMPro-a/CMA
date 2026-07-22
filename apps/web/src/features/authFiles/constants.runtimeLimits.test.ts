import { describe, expect, it } from 'vitest';
import {
  hasAuthFileFreezeConfig,
  hasAuthFileRateLimitConfig,
  isAuthFileRuntimeUnlimited,
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
});
