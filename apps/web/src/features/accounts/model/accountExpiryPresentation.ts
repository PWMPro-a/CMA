export const ACCOUNT_EXPIRY_CRITICAL_MS = 5 * 60 * 1000;
export const ACCOUNT_EXPIRY_WARNING_MS = 60 * 60 * 1000;
export const ACCOUNT_EXPIRY_SOON_MS = 24 * 60 * 60 * 1000;

const MINUTE_MS = 60 * 1000;
const HOUR_MS = 60 * MINUTE_MS;
const DAY_MS = 24 * HOUR_MS;

export type AccountExpiryTone = 'expired' | 'critical' | 'warning' | 'soon' | 'normal' | 'unknown';

export type AccountExpiryLabel =
  | { kind: 'expired' }
  | { kind: 'unknown' }
  | { kind: 'seconds'; minutes: number; seconds: number }
  | { kind: 'minutes'; count: number }
  | { kind: 'hours'; hours: number; minutes: number }
  | { kind: 'days'; days: number; hours: number };

export interface AccountExpiryPresentation {
  tone: AccountExpiryTone;
  remainingMs: number | null;
  label: AccountExpiryLabel;
}

const isFiniteTimestamp = (value: number | null | undefined): value is number =>
  typeof value === 'number' && Number.isFinite(value) && value > 0;

export const buildAccountExpiryPresentation = (
  expiresAtMs: number | null | undefined,
  nowMs: number = Date.now()
): AccountExpiryPresentation => {
  if (!isFiniteTimestamp(expiresAtMs)) {
    return { tone: 'unknown', remainingMs: null, label: { kind: 'unknown' } };
  }

  const currentMs = Number.isFinite(nowMs) ? nowMs : Date.now();
  const remainingMs = expiresAtMs - currentMs;
  if (remainingMs <= 0) {
    return { tone: 'expired', remainingMs: 0, label: { kind: 'expired' } };
  }

  const tone: Exclude<AccountExpiryTone, 'expired' | 'unknown'> =
    remainingMs <= ACCOUNT_EXPIRY_CRITICAL_MS
      ? 'critical'
      : remainingMs <= ACCOUNT_EXPIRY_WARNING_MS
        ? 'warning'
        : remainingMs <= ACCOUNT_EXPIRY_SOON_MS
          ? 'soon'
          : 'normal';

  if (remainingMs <= ACCOUNT_EXPIRY_CRITICAL_MS) {
    const totalSeconds = Math.max(1, Math.ceil(remainingMs / 1000));
    return {
      tone,
      remainingMs,
      label: {
        kind: 'seconds',
        minutes: Math.floor(totalSeconds / 60),
        seconds: totalSeconds % 60,
      },
    };
  }

  const totalMinutes = Math.max(1, Math.ceil(remainingMs / MINUTE_MS));

  if (remainingMs <= HOUR_MS) {
    return {
      tone,
      remainingMs,
      label: { kind: 'minutes', count: totalMinutes },
    };
  }

  if (remainingMs <= DAY_MS) {
    return {
      tone,
      remainingMs,
      label: {
        kind: 'hours',
        hours: Math.floor(totalMinutes / 60),
        minutes: totalMinutes % 60,
      },
    };
  }

  return {
    tone,
    remainingMs,
    label: {
      kind: 'days',
      days: Math.floor(totalMinutes / (24 * 60)),
      hours: Math.floor((totalMinutes % (24 * 60)) / 60),
    },
  };
};
