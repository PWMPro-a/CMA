import { describe, expect, it } from 'vitest';

import { buildSourceIpSelectOptions, collectSourceIpUsageCounts } from './sourceIp';

const interpolate = (value: string, options?: Record<string, unknown>): string =>
  value.replace(/{{\s*(\w+)\s*}}/g, (_, key: string) => String(options?.[key] ?? ''));

const t = (key: string, options?: Record<string, unknown>): string =>
  interpolate(
    ({
      'auth_files.source_ip_bound_accounts': '已绑定 {{count}} 个账号',
      'auth_files.source_ip_current_option': '当前已选',
      'common.not_set': '未设置',
    })[key] ?? key,
    options
  );

describe('sourceIp utils', () => {
  it('counts configured source IP usage after trimming blank values', () => {
    expect(
      collectSourceIpUsageCounts([
        '144.172.117.178',
        ' 144.172.117.178 ',
        '',
        null,
        '144.172.106.107',
      ])
    ).toEqual({
      '144.172.117.178': 2,
      '144.172.106.107': 1,
    });
  });

  it('builds dropdown options from known egress IPs with account counts', () => {
    const options = buildSourceIpSelectOptions({
      inventory: {
        nativeOutboundIp: '144.172.117.178',
        addresses: [
          {
            address: '144.172.102.2',
            cidr: '144.172.102.2/32',
            interface: 'eth0',
            scope: 'global',
          },
          { address: '127.0.0.1', cidr: '127.0.0.1/8', interface: 'lo', scope: 'host' },
          {
            address: '144.172.117.178',
            cidr: '144.172.117.178/32',
            interface: 'eth0',
            scope: 'global',
          },
          {
            address: '10.0.1.11',
            cidr: '10.0.1.11/32',
            interface: 'eth0',
            scope: 'global',
          },
          {
            address: '172.17.0.1',
            cidr: '172.17.0.1/16',
            interface: 'docker0',
            scope: 'global',
          },
        ],
      },
      usageCounts: {
        '144.172.117.178': 2,
        '144.172.102.2': 3,
      },
      t,
    });

    expect(options).toEqual([
      { value: '', label: '未设置' },
      { value: '144.172.117.178', label: '144.172.117.178 · 已绑定 2 个账号' },
      { value: '144.172.102.2', label: '144.172.102.2 · 已绑定 3 个账号' },
    ]);
  });

  it('keeps current value selectable without adding it to usage counts', () => {
    const options = buildSourceIpSelectOptions({
      inventory: {
        nativeOutboundIp: '',
        addresses: [
          {
            address: '144.172.102.2',
            cidr: '144.172.102.2/32',
            interface: 'eth0',
            scope: 'global',
          },
        ],
      },
      usageCounts: { '144.172.102.2': 1 },
      fallbackValues: ['144.172.106.107'],
      t,
    });

    expect(options).toEqual([
      { value: '', label: '未设置' },
      { value: '144.172.102.2', label: '144.172.102.2 · 已绑定 1 个账号' },
      { value: '144.172.106.107', label: '144.172.106.107 · 当前已选' },
    ]);
  });
});
