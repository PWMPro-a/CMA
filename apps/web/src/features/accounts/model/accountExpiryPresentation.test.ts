import { describe, expect, it } from 'vitest';
import {
  ACCOUNT_EXPIRY_CRITICAL_MS,
  ACCOUNT_EXPIRY_SOON_MS,
  ACCOUNT_EXPIRY_WARNING_MS,
  buildAccountExpiryPresentation,
} from './accountExpiryPresentation';

const NOW_MS = 1_000_000;

describe('accountExpiryPresentation', () => {
  it('marks missing timestamps as unknown', () => {
    expect(buildAccountExpiryPresentation(null, NOW_MS)).toEqual({
      tone: 'unknown',
      remainingMs: null,
      label: { kind: 'unknown' },
    });
  });

  it('marks expired timestamps separately from a critical countdown', () => {
    expect(buildAccountExpiryPresentation(NOW_MS - 1, NOW_MS)).toMatchObject({
      tone: 'expired',
      remainingMs: 0,
      label: { kind: 'expired' },
    });
    expect(buildAccountExpiryPresentation(NOW_MS + ACCOUNT_EXPIRY_CRITICAL_MS, NOW_MS)).toEqual({
      tone: 'critical',
      remainingMs: ACCOUNT_EXPIRY_CRITICAL_MS,
      label: { kind: 'seconds', minutes: 5, seconds: 0 },
    });
  });

  it('uses warning, soon, and normal tones at the configured boundaries', () => {
    expect(
      buildAccountExpiryPresentation(NOW_MS + ACCOUNT_EXPIRY_CRITICAL_MS + 1, NOW_MS).tone
    ).toBe('warning');
    expect(
      buildAccountExpiryPresentation(NOW_MS + ACCOUNT_EXPIRY_WARNING_MS + 1, NOW_MS)
    ).toMatchObject({
      tone: 'soon',
      label: { kind: 'hours', hours: 1, minutes: 0, seconds: 1 },
    });
    expect(buildAccountExpiryPresentation(NOW_MS + ACCOUNT_EXPIRY_SOON_MS, NOW_MS)).toMatchObject({
      tone: 'soon',
      label: { kind: 'hours', hours: 24, minutes: 0, seconds: 0 },
    });
    expect(
      buildAccountExpiryPresentation(NOW_MS + ACCOUNT_EXPIRY_SOON_MS + 1, NOW_MS)
    ).toMatchObject({
      tone: 'normal',
      label: { kind: 'days', days: 1, hours: 0, minutes: 0, seconds: 1 },
    });
  });

  it('rounds a partial minute up so the badge never shows zero remaining', () => {
    expect(buildAccountExpiryPresentation(NOW_MS + 1_001, NOW_MS).label).toEqual({
      kind: 'seconds',
      minutes: 0,
      seconds: 2,
    });
  });

  it('keeps seconds in minute, hour, and day countdown labels', () => {
    expect(buildAccountExpiryPresentation(NOW_MS + 34 * 60_000 + 12_001, NOW_MS).label).toEqual({
      kind: 'minutes',
      count: 34,
      seconds: 13,
    });
    expect(
      buildAccountExpiryPresentation(NOW_MS + 2 * 60 * 60_000 + 3 * 60_000 + 4_001, NOW_MS).label
    ).toEqual({ kind: 'hours', hours: 2, minutes: 3, seconds: 5 });
    expect(
      buildAccountExpiryPresentation(
        NOW_MS + 2 * 24 * 60 * 60_000 + 3 * 60 * 60_000 + 4 * 60_000 + 5_001,
        NOW_MS
      ).label
    ).toEqual({ kind: 'days', days: 2, hours: 3, minutes: 4, seconds: 6 });
  });
});
