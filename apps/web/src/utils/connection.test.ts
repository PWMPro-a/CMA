import { describe, expect, it } from 'vitest';
import {
  DEFAULT_DOCKER_CPA_BASE_URL,
  normalizeApiBase,
  resolveDefaultCPAConnectionBase,
} from './connection';

describe('normalizeApiBase', () => {
  it('strips a pasted management panel path and hash route', () => {
    expect(
      normalizeApiBase(
        'http://45.126.209.111:18317/management.html#/accounts?status=available'
      )
    ).toBe('http://45.126.209.111:18317');
  });

  it('preserves a reverse-proxy path before management.html', () => {
    expect(normalizeApiBase('https://panel.example/cpa/management.html?from=bookmark')).toBe(
      'https://panel.example/cpa'
    );
  });
});

describe('resolveDefaultCPAConnectionBase', () => {
  it('uses the explicit environment default first', () => {
    expect(
      resolveDefaultCPAConnectionBase({
        hostedByUsageService: true,
        currentBase: 'http://panel.local:18317',
        envDefault: 'cpa.local:8317',
      })
    ).toBe('http://cpa.local:8317');
  });

  it('uses the Docker host default when the panel is hosted by Usage Service', () => {
    expect(
      resolveDefaultCPAConnectionBase({
        hostedByUsageService: true,
        currentBase: 'http://panel.local:18317',
        envDefault: '',
      })
    ).toBe(DEFAULT_DOCKER_CPA_BASE_URL);
  });

  it('keeps the current base for regular CPA-hosted panels', () => {
    expect(
      resolveDefaultCPAConnectionBase({
        hostedByUsageService: false,
        currentBase: 'http://cpa.local:8317/',
        envDefault: '',
      })
    ).toBe('http://cpa.local:8317');
  });
});
