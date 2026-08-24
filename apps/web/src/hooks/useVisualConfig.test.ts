import { act, createElement, createRef, useImperativeHandle, type Ref } from 'react';
import { create, type ReactTestRenderer } from 'react-test-renderer';
import { describe, expect, it } from 'vitest';
import { parse as parseYaml } from 'yaml';
import { getVisualConfigValidationErrors, useVisualConfig } from './useVisualConfig';
import { DEFAULT_VISUAL_VALUES } from '@/types/visualConfig';

type UseVisualConfigResult = ReturnType<typeof useVisualConfig>;

type UseVisualConfigHarness = {
  getCurrent: () => UseVisualConfigResult;
  unmount: () => void;
};

function HookHarness({ hookRef }: { hookRef: Ref<UseVisualConfigResult> }) {
  const hook = useVisualConfig();
  useImperativeHandle(hookRef, () => hook, [hook]);
  return null;
}

const mountUseVisualConfig = (): UseVisualConfigHarness => {
  const hookRef = createRef<UseVisualConfigResult>();
  let renderer: ReactTestRenderer | null = null;

  act(() => {
    renderer = create(createElement(HookHarness, { hookRef }));
  });

  return {
    getCurrent: () => {
      if (!hookRef.current) {
        throw new Error('Failed to mount useVisualConfig test harness');
      }
      return hookRef.current;
    },
    unmount: () => {
      if (!renderer) return;
      act(() => {
        renderer?.unmount();
      });
    },
  };
};

describe('useVisualConfig', () => {
  it('disables management panel auto updates by default', () => {
    expect(DEFAULT_VISUAL_VALUES.rmDisableAutoUpdatePanel).toBe(true);
  });

  it('keeps panel auto updates disabled when legacy YAML omits the setting', () => {
    const harness = mountUseVisualConfig();

    act(() => {
      const result = harness
        .getCurrent()
        .loadVisualValuesFromYaml(
          ['remote-management:', '  disable-control-panel: false', ''].join('\n')
        );
      expect(result.ok).toBe(true);
    });

    expect(harness.getCurrent().visualValues.rmDisableAutoUpdatePanel).toBe(true);
    harness.unmount();
  });

  it('preserves an explicit panel auto-update opt-in', () => {
    const harness = mountUseVisualConfig();

    act(() => {
      const result = harness
        .getCurrent()
        .loadVisualValuesFromYaml(
          ['remote-management:', '  disable-auto-update-panel: false', ''].join('\n')
        );
      expect(result.ok).toBe(true);
    });

    expect(harness.getCurrent().visualValues.rmDisableAutoUpdatePanel).toBe(false);
    harness.unmount();
  });

  it('uses the strict Codex quota-protection fingerprint defaults', () => {
    expect(DEFAULT_VISUAL_VALUES.codexClientFingerprintSignals).toHaveLength(4);
    expect(
      DEFAULT_VISUAL_VALUES.codexClientFingerprintSignals.map((signal) => signal.required)
    ).toEqual([true, true, true, true]);
  });

  it('validates Codex version syntax and range before writing YAML', () => {
    expect(
      getVisualConfigValidationErrors({
        ...DEFAULT_VISUAL_VALUES,
        codexClientMinVersion: 'latest',
        codexClientMaxVersion: '0.200.0',
      })
    ).toMatchObject({
      codexClientMinVersion: 'codex_version',
    });

    expect(
      getVisualConfigValidationErrors({
        ...DEFAULT_VISUAL_VALUES,
        codexClientMinVersion: '0.200.0',
        codexClientMaxVersion: '0.142.0',
      })
    ).toMatchObject({
      codexClientMaxVersion: 'codex_version_range',
    });
  });

  it('loads plugin system state from plugins.enabled', () => {
    const harness = mountUseVisualConfig();
    const yaml = ['plugins:', '  enabled: true', ''].join('\n');

    act(() => {
      const result = harness.getCurrent().loadVisualValuesFromYaml(yaml);
      expect(result.ok).toBe(true);
    });

    expect(harness.getCurrent().visualValues.pluginsEnabled).toBe(true);
    harness.unmount();
  });

  it('loads plugin directory and store sources from plugins config', () => {
    const harness = mountUseVisualConfig();
    const yaml = [
      'plugins:',
      '  enabled: true',
      '  dir: /data/cpa/plugins',
      '  store-sources:',
      '    - https://plugins.example.com/official.json',
      '    - https://plugins.example.com/private.json',
      '',
    ].join('\n');

    act(() => {
      const result = harness.getCurrent().loadVisualValuesFromYaml(yaml);
      expect(result.ok).toBe(true);
    });

    expect(harness.getCurrent().visualValues.pluginsEnabled).toBe(true);
    expect(harness.getCurrent().visualValues.pluginsDir).toBe('/data/cpa/plugins');
    expect(harness.getCurrent().visualValues.pluginStoreSourcesText).toBe(
      [
        'https://plugins.example.com/official.json',
        'https://plugins.example.com/private.json',
      ].join('\n')
    );

    harness.unmount();
  });

  it('loads plugin store auth rules from plugins config', () => {
    const harness = mountUseVisualConfig();
    const yaml = [
      'plugins:',
      '  store-auth:',
      '    - match: https://api.github.com/repos/acme/private/releases/',
      '      apply-to:',
      '        - metadata',
      '        - artifact',
      '      type: github-token',
      '      token-env: GITHUB_TOKEN',
      '      allow-insecure: true',
      '',
    ].join('\n');

    act(() => {
      const result = harness.getCurrent().loadVisualValuesFromYaml(yaml);
      expect(result.ok).toBe(true);
    });

    expect(harness.getCurrent().visualValues.pluginStoreAuth).toEqual([
      expect.objectContaining({
        match: 'https://api.github.com/repos/acme/private/releases/',
        applyTo: ['metadata', 'artifact'],
        type: 'github-token',
        tokenEnv: 'GITHUB_TOKEN',
        allowInsecure: true,
      }),
    ]);

    harness.unmount();
  });

  it('writes plugins.enabled when enabling plugin system from visual editor', () => {
    const harness = mountUseVisualConfig();
    const yaml = ['host: 127.0.0.1', ''].join('\n');

    act(() => {
      const result = harness.getCurrent().loadVisualValuesFromYaml(yaml);
      expect(result.ok).toBe(true);
    });

    act(() => {
      harness.getCurrent().setVisualValues({ pluginsEnabled: true });
    });

    const savedYaml = harness.getCurrent().applyVisualChangesToYaml(yaml);
    expect(savedYaml).toContain('plugins:');
    expect(savedYaml).toContain('enabled: true');

    harness.unmount();
  });

  it('writes plugin directory and store sources while preserving plugin configs', () => {
    const harness = mountUseVisualConfig();
    const yaml = ['plugins:', '  configs:', '    demo:', '      enabled: true', ''].join('\n');

    act(() => {
      const result = harness.getCurrent().loadVisualValuesFromYaml(yaml);
      expect(result.ok).toBe(true);
    });

    act(() => {
      harness.getCurrent().setVisualValues({
        pluginsDir: '/opt/cpa/plugins',
        pluginStoreSourcesText: [
          'https://plugins.example.com/official.json',
          '',
          ' https://plugins.example.com/private.json ',
        ].join('\n'),
      });
    });

    const savedYaml = harness.getCurrent().applyVisualChangesToYaml(yaml);
    const parsed = parseYaml(savedYaml) as {
      plugins?: {
        dir?: string;
        'store-sources'?: string[];
        configs?: { demo?: { enabled?: boolean } };
      };
    };

    expect(parsed.plugins?.dir).toBe('/opt/cpa/plugins');
    expect(parsed.plugins?.['store-sources']).toEqual([
      'https://plugins.example.com/official.json',
      'https://plugins.example.com/private.json',
    ]);
    expect(parsed.plugins?.configs?.demo?.enabled).toBe(true);

    harness.unmount();
  });

  it('writes plugin store auth rules only after editing the auth field', () => {
    const harness = mountUseVisualConfig();
    const yaml = ['plugins:', '  configs:', '    demo:', '      enabled: true', ''].join('\n');

    act(() => {
      const result = harness.getCurrent().loadVisualValuesFromYaml(yaml);
      expect(result.ok).toBe(true);
    });

    const unchangedYaml = harness.getCurrent().applyVisualChangesToYaml(yaml);
    expect(parseYaml(unchangedYaml) as { plugins?: { 'store-auth'?: unknown } }).toEqual(
      expect.objectContaining({
        plugins: expect.not.objectContaining({ 'store-auth': expect.anything() }),
      })
    );

    act(() => {
      harness.getCurrent().setVisualValues({
        pluginStoreAuth: [
          {
            id: 'rule-1',
            match: 'https://downloads.example.com/private/',
            applyTo: ['artifact'],
            type: 'bearer',
            tokenEnv: 'PLUGIN_TOKEN',
            usernameEnv: '',
            passwordEnv: '',
            headerName: '',
            headerValueEnv: '',
            allowInsecure: false,
          },
        ],
      });
    });

    const savedYaml = harness.getCurrent().applyVisualChangesToYaml(yaml);
    const parsed = parseYaml(savedYaml) as {
      plugins?: {
        'store-auth'?: Array<Record<string, unknown>>;
        configs?: { demo?: { enabled?: boolean } };
      };
    };

    expect(parsed.plugins?.['store-auth']).toEqual([
      {
        match: 'https://downloads.example.com/private/',
        type: 'bearer',
        'apply-to': ['artifact'],
        'token-env': 'PLUGIN_TOKEN',
      },
    ]);
    expect(parsed.plugins?.configs?.demo?.enabled).toBe(true);

    harness.unmount();
  });

  it('clears plugin directory and store sources without removing plugin configs', () => {
    const harness = mountUseVisualConfig();
    const yaml = [
      'plugins:',
      '  dir: /opt/cpa/plugins',
      '  store-sources:',
      '    - https://plugins.example.com/official.json',
      '  configs:',
      '    demo:',
      '      enabled: true',
      '',
    ].join('\n');

    act(() => {
      const result = harness.getCurrent().loadVisualValuesFromYaml(yaml);
      expect(result.ok).toBe(true);
    });

    act(() => {
      harness.getCurrent().setVisualValues({
        pluginsDir: '',
        pluginStoreSourcesText: '',
      });
    });

    const savedYaml = harness.getCurrent().applyVisualChangesToYaml(yaml);
    const parsed = parseYaml(savedYaml) as {
      plugins?: {
        dir?: string;
        'store-sources'?: string[];
        configs?: { demo?: { enabled?: boolean } };
      };
    };

    expect(parsed.plugins?.dir).toBeUndefined();
    expect(parsed.plugins?.['store-sources']).toBeUndefined();
    expect(parsed.plugins?.configs?.demo?.enabled).toBe(true);

    harness.unmount();
  });

  it('clears camelCase codex identityConfuse when disabling from visual editor', () => {
    const harness = mountUseVisualConfig();
    const yaml = [
      'host: 127.0.0.1',
      'codex:',
      '  identityConfuse: true',
      '  other-setting: kept',
      '',
    ].join('\n');

    act(() => {
      const result = harness.getCurrent().loadVisualValuesFromYaml(yaml);
      expect(result.ok).toBe(true);
    });
    expect(harness.getCurrent().visualValues.codexIdentityConfuse).toBe(true);

    act(() => {
      harness.getCurrent().setVisualValues({ codexIdentityConfuse: false });
    });

    const savedYaml = harness.getCurrent().applyVisualChangesToYaml(yaml);
    expect(savedYaml).not.toContain('identityConfuse: true');
    expect(savedYaml).not.toContain('identityConfuse:');
    expect(savedYaml).toContain('identity-confuse: false');
    expect(savedYaml).toContain('other-setting: kept');

    harness.unmount();
  });

  it('migrates legacy Codex client restriction fields without dropping untouched values', () => {
    const harness = mountUseVisualConfig();
    const yaml = [
      'codex:',
      '  clientRestriction:',
      '    forceCodexCli: true',
      '    minCodexVersion: 0.142.0',
      '    whitelist:',
      '      - originator: opencode',
      '        uaContains:',
      '          - opencode/',
      '        skipEngineFingerprint: true',
      '    future-policy:',
      '      enabled: true',
      '',
    ].join('\n');

    act(() => {
      const result = harness.getCurrent().loadVisualValuesFromYaml(yaml);
      expect(result.ok).toBe(true);
    });
    act(() => {
      harness.getCurrent().setVisualValues({ codexClientMaxVersion: '0.200.0' });
    });

    const savedYaml = harness.getCurrent().applyVisualChangesToYaml(yaml);
    const parsed = parseYaml(savedYaml) as {
      codex?: {
        clientRestriction?: unknown;
        'client-restriction'?: {
          'force-codex-cli'?: boolean;
          'min-codex-version'?: string;
          'max-codex-version'?: string;
          whitelist?: Array<Record<string, unknown>>;
          'future-policy'?: { enabled?: boolean };
        };
      };
    };
    const restriction = parsed.codex?.['client-restriction'];
    expect(parsed.codex?.clientRestriction).toBeUndefined();
    expect(restriction?.['force-codex-cli']).toBe(true);
    expect(restriction?.['min-codex-version']).toBe('0.142.0');
    expect(restriction?.['max-codex-version']).toBe('0.200.0');
    expect(restriction?.whitelist).toEqual([
      {
        originator: 'opencode',
        'ua-contains': ['opencode/'],
        'skip-engine-fingerprint': true,
      },
    ]);
    expect(restriction?.['future-policy']?.enabled).toBe(true);

    harness.unmount();
  });

  it('round-trips disable-image-generation passthrough without rewriting it', () => {
    const harness = mountUseVisualConfig();
    const yaml = ['disable-image-generation: passthrough', 'debug: false', ''].join('\n');

    act(() => {
      expect(harness.getCurrent().loadVisualValuesFromYaml(yaml).ok).toBe(true);
    });
    expect(harness.getCurrent().visualValues.disableImageGeneration).toBe('passthrough');

    act(() => {
      harness.getCurrent().setVisualValues({ debug: true });
    });
    const parsed = parseYaml(harness.getCurrent().applyVisualChangesToYaml(yaml)) as Record<
      string,
      unknown
    >;
    expect(parsed['disable-image-generation']).toBe('passthrough');
    expect(parsed.debug).toBe(true);

    harness.unmount();
  });

  it('applies only dirty visual fields to the latest server YAML', () => {
    const harness = mountUseVisualConfig();
    const originalYaml = ['debug: false', 'proxy-url: http://old-proxy.example', ''].join('\n');

    act(() => {
      expect(harness.getCurrent().loadVisualValuesFromYaml(originalYaml).ok).toBe(true);
      harness.getCurrent().setVisualValues({ proxyUrl: 'http://localhost:8080' });
    });

    const latestYaml = ['debug: true', 'proxy-url: http://old-proxy.example', ''].join('\n');
    const parsed = parseYaml(harness.getCurrent().applyVisualChangesToYaml(latestYaml)) as Record<
      string,
      unknown
    >;

    expect(parsed).toEqual({
      debug: true,
      'proxy-url': 'http://localhost:8080',
    });
    harness.unmount();
  });

  it('uses CPA defaults for absent quota and WebSocket auth fields', () => {
    const harness = mountUseVisualConfig();
    const yaml = ['host: 127.0.0.1', ''].join('\n');

    act(() => {
      expect(harness.getCurrent().loadVisualValuesFromYaml(yaml).ok).toBe(true);
    });

    expect(harness.getCurrent().visualValues.quotaSwitchProject).toBe(false);
    expect(harness.getCurrent().visualValues.quotaSwitchPreviewModel).toBe(false);
    expect(harness.getCurrent().visualValues.codexTailBurstEnabled).toBe(false);
    expect(harness.getCurrent().visualValues.codexTailBurstToolInjectionEnabled).toBe(false);
    expect(harness.getCurrent().visualValues.wsAuth).toBe(true);

    const parsed = parseYaml(harness.getCurrent().applyVisualChangesToYaml(yaml)) as Record<
      string,
      unknown
    >;
    expect(parsed['quota-exceeded']).toBeUndefined();
    expect(parsed.codex).toBeUndefined();
    expect(parsed['ws-auth']).toBeUndefined();

    harness.unmount();
  });

  it('writes only the quota option explicitly changed from an absent quota block', () => {
    const harness = mountUseVisualConfig();
    const yaml = ['host: 127.0.0.1', ''].join('\n');

    act(() => {
      expect(harness.getCurrent().loadVisualValuesFromYaml(yaml).ok).toBe(true);
      harness.getCurrent().setVisualValues({ quotaSwitchProject: true });
    });

    const parsed = parseYaml(harness.getCurrent().applyVisualChangesToYaml(yaml)) as {
      'quota-exceeded'?: Record<string, unknown>;
    };
    expect(parsed['quota-exceeded']).toEqual({ 'switch-project': true });

    harness.unmount();
  });

  it('writes ws-auth false when the user explicitly disables the CPA default', () => {
    const harness = mountUseVisualConfig();
    const yaml = ['host: 127.0.0.1', ''].join('\n');

    act(() => {
      expect(harness.getCurrent().loadVisualValuesFromYaml(yaml).ok).toBe(true);
      harness.getCurrent().setVisualValues({ wsAuth: false });
    });

    const parsed = parseYaml(harness.getCurrent().applyVisualChangesToYaml(yaml)) as Record<
      string,
      unknown
    >;
    expect(parsed['ws-auth']).toBe(false);

    harness.unmount();
  });

  it('rejects zero Redis usage retention because CPA normalizes it to 60', () => {
    const harness = mountUseVisualConfig();

    act(() => {
      harness.getCurrent().setVisualValues({ redisUsageQueueRetentionSeconds: '0' });
    });

    expect(harness.getCurrent().visualValidationErrors.redisUsageQueueRetentionSeconds).toBe(
      'retention_seconds_range'
    );
    harness.unmount();
  });

  it('keeps an existing management key unchanged during unrelated visual edits', () => {
    const harness = mountUseVisualConfig();
    const hash = '$2a$10$01234567890123456789012345678901234567890123456789012';
    const yaml = [
      'remote-management:',
      `  secret-key: '${hash}'`,
      '  allow-remote: false',
      '',
    ].join('\n');

    act(() => {
      expect(harness.getCurrent().loadVisualValuesFromYaml(yaml).ok).toBe(true);
    });
    expect(harness.getCurrent().visualValues.rmSecretKey).toBe('');
    expect(harness.getCurrent().visualValues.rmSecretKeyAction).toBe('unchanged');
    expect(harness.getCurrent().visualValues.rmSecretKeyConfigured).toBe(true);

    act(() => {
      harness.getCurrent().setVisualValues({ rmAllowRemote: true });
    });
    const parsed = parseYaml(harness.getCurrent().applyVisualChangesToYaml(yaml)) as {
      'remote-management'?: Record<string, unknown>;
    };
    expect(parsed['remote-management']?.['secret-key']).toBe(hash);
    expect(parsed['remote-management']?.['allow-remote']).toBe(true);

    harness.unmount();
  });

  it('replaces a management key without trimming its bytes', () => {
    const harness = mountUseVisualConfig();
    const yaml = ['remote-management:', "  secret-key: '$2a$10$existing'", ''].join('\n');

    act(() => {
      expect(harness.getCurrent().loadVisualValuesFromYaml(yaml).ok).toBe(true);
      harness.getCurrent().setVisualValues({
        rmSecretKey: '  exact key  ',
        rmSecretKeyAction: 'replace',
      });
    });

    const parsed = parseYaml(harness.getCurrent().applyVisualChangesToYaml(yaml)) as {
      'remote-management'?: Record<string, unknown>;
    };
    expect(parsed['remote-management']?.['secret-key']).toBe('  exact key  ');

    harness.unmount();
  });

  it('does not clear an existing management key through an empty replacement', () => {
    const harness = mountUseVisualConfig();
    const hash = '$2a$10$existing';
    const yaml = ['remote-management:', `  secret-key: '${hash}'`, ''].join('\n');

    act(() => {
      expect(harness.getCurrent().loadVisualValuesFromYaml(yaml).ok).toBe(true);
      harness.getCurrent().setVisualValues({
        rmSecretKey: '',
        rmSecretKeyAction: 'replace',
      });
    });

    const parsed = parseYaml(harness.getCurrent().applyVisualChangesToYaml(yaml)) as {
      'remote-management'?: Record<string, unknown>;
    };
    expect(parsed['remote-management']?.['secret-key']).toBe(hash);

    harness.unmount();
  });

  it('explicitly clears the management key to disable the Management API', () => {
    const harness = mountUseVisualConfig();
    const yaml = ['remote-management:', "  secret-key: '$2a$10$existing'", ''].join('\n');

    act(() => {
      expect(harness.getCurrent().loadVisualValuesFromYaml(yaml).ok).toBe(true);
      harness.getCurrent().setVisualValues({ rmSecretKey: '', rmSecretKeyAction: 'clear' });
    });

    const parsed = parseYaml(harness.getCurrent().applyVisualChangesToYaml(yaml)) as {
      'remote-management'?: Record<string, unknown>;
    };
    expect(parsed['remote-management']?.['secret-key']).toBe('');

    harness.unmount();
  });

  it('loads and saves the added CPA runtime settings', () => {
    const harness = mountUseVisualConfig();
    const yaml = [
      'pprof:',
      '  enable: true',
      '  addr: 127.0.0.1:9316',
      'save-cooldown-status: true',
      'transient-error-cooldown-seconds: -1',
      'disable-claude-cloak-mode: true',
      'gpt-image-2-base-model: gpt-5.4',
      'video-result-auth-cache-ttl: 45m',
      '',
    ].join('\n');

    act(() => {
      expect(harness.getCurrent().loadVisualValuesFromYaml(yaml).ok).toBe(true);
    });
    expect(harness.getCurrent().visualValues).toEqual(
      expect.objectContaining({
        pprofEnable: true,
        pprofAddr: '127.0.0.1:9316',
        saveCooldownStatus: true,
        transientErrorCooldownSeconds: '-1',
        disableClaudeCloakMode: true,
        gptImage2BaseModel: 'gpt-5.4',
        videoResultAuthCacheTtl: '45m',
      })
    );

    act(() => {
      harness.getCurrent().setVisualValues({
        pprofEnable: false,
        pprofAddr: '127.0.0.1:8316',
        saveCooldownStatus: false,
        transientErrorCooldownSeconds: '15',
        disableClaudeCloakMode: false,
        gptImage2BaseModel: 'gpt-5.4-mini',
        videoResultAuthCacheTtl: '3h',
      });
    });

    const parsed = parseYaml(harness.getCurrent().applyVisualChangesToYaml(yaml)) as Record<
      string,
      unknown
    >;
    expect(parsed.pprof).toEqual({ enable: false, addr: '127.0.0.1:8316' });
    expect(parsed['save-cooldown-status']).toBe(false);
    expect(parsed['transient-error-cooldown-seconds']).toBe(15);
    expect(parsed['disable-claude-cloak-mode']).toBe(false);
    expect(parsed['gpt-image-2-base-model']).toBe('gpt-5.4-mini');
    expect(parsed['video-result-auth-cache-ttl']).toBe('3h');

    harness.unmount();
  });

  it('reads and writes Codex tail-burst controls while removing the obsolete normal limit', () => {
    const harness = mountUseVisualConfig();
    const yaml = [
      'codex:',
      '  identity-confuse: true',
      '  future-option: preserve-me',
      '  tail-burst:',
      '    enabled: true',
      '    trigger-used-ratio: 0.98',
      '    snapshot-ttl: 90s',
      '    normal-max-concurrency: 7',
      '    quota-collector:',
      '      interval: 45s',
      '      max-concurrency: 4',
      '      timeout: 8s',
      '      future-collector-option: preserve-me',
      '    tool-injection:',
      '      enabled: true',
      '',
    ].join('\n');

    act(() => {
      expect(harness.getCurrent().loadVisualValuesFromYaml(yaml).ok).toBe(true);
    });

    expect(harness.getCurrent().visualValues).toEqual(
      expect.objectContaining({
        codexIdentityConfuse: true,
        codexTailBurstEnabled: true,
        codexTailBurstTriggerRemainingPercent: '2',
        codexTailBurstSnapshotTtl: '90s',
        codexTailBurstExpiryWindow: '10m',
        codexTailBurstMaxConcurrency: '32',
        codexTailBurstCollectorInterval: '45s',
        codexTailBurstCollectorMaxConcurrency: '4',
        codexTailBurstCollectorTimeout: '8s',
        codexTailBurstToolInjectionEnabled: true,
      })
    );

    act(() => {
      harness.getCurrent().setVisualValues({
        codexTailBurstTriggerRemainingPercent: '1.5',
        codexTailBurstSnapshotTtl: '2m',
        codexTailBurstExpiryWindow: '12m',
        codexTailBurstMaxConcurrency: '48',
        codexTailBurstCollectorInterval: '30s',
        codexTailBurstCollectorMaxConcurrency: '6',
        codexTailBurstCollectorTimeout: '10s',
        codexTailBurstToolInjectionEnabled: false,
      });
    });

    const parsed = parseYaml(harness.getCurrent().applyVisualChangesToYaml(yaml)) as {
      codex?: {
        'identity-confuse'?: boolean;
        'future-option'?: string;
        'tail-burst'?: {
          'trigger-used-ratio'?: number;
          'trigger-remaining-ratio'?: number;
          'snapshot-ttl'?: string;
          'expiry-window'?: string;
          'normal-max-concurrency'?: number;
          'max-concurrency'?: number;
          'quota-collector'?: {
            interval?: string;
            'max-concurrency'?: number;
            timeout?: string;
            'future-collector-option'?: string;
          };
          'tool-injection'?: { enabled?: boolean };
        };
      };
    };

    expect(parsed.codex?.['identity-confuse']).toBe(true);
    expect(parsed.codex?.['future-option']).toBe('preserve-me');
    expect(parsed.codex?.['tail-burst']).toEqual(
      expect.objectContaining({
        enabled: true,
        'trigger-remaining-ratio': 0.015,
        'snapshot-ttl': '2m',
        'expiry-window': '12m',
        'max-concurrency': 48,
        'tool-injection': { enabled: false },
      })
    );
    expect(parsed.codex?.['tail-burst']?.['normal-max-concurrency']).toBeUndefined();
    expect(parsed.codex?.['tail-burst']?.['trigger-used-ratio']).toBeUndefined();
    expect(parsed.codex?.['tail-burst']?.['quota-collector']).toEqual({
      interval: '30s',
      'max-concurrency': 6,
      timeout: '10s',
      'future-collector-option': 'preserve-me',
    });

    harness.unmount();
  });

  it('reads camelCase Codex cache affinity and writes canonical YAML while preserving unknown keys', () => {
    const harness = mountUseVisualConfig();
    const yaml = [
      'codex:',
      '  identity-confuse: true',
      '  future-option: preserve-me',
      '  cacheAffinity:',
      '    enabled: true',
      '    shadow: true',
      '    maxConcurrency: 6',
      '    maxEntries: 4096',
      '    maxRetryCredentials: 3',
      '    websocketPoolSlots: 12',
      '    maxSessionRequests: 80',
      '    maxSessionDuration: 8m',
      '    maxShareRatio: 0.72',
      '    quotaPreemptUsedRatio: 0.96',
      '    quotaHardStopUsedRatio: 1',
      '    future-affinity-option: preserve-affinity',
      '',
    ].join('\n');

    act(() => {
      expect(harness.getCurrent().loadVisualValuesFromYaml(yaml).ok).toBe(true);
    });

    expect(harness.getCurrent().visualValues).toEqual(
      expect.objectContaining({
        codexCacheAffinityEnabled: true,
        codexCacheAffinityShadow: true,
        codexCacheAffinityMaxConcurrency: '6',
        codexCacheAffinityMaxEntries: '4096',
        codexCacheAffinityMaxRetryCredentials: '3',
        codexCacheAffinityWebsocketPoolSlots: '12',
        codexCacheAffinityMaxSessionRequests: '80',
        codexCacheAffinityMaxSessionDuration: '8m',
        codexCacheAffinityMaxSharePercent: '72',
        codexCacheAffinityQuotaPreemptPercent: '96',
        codexCacheAffinityQuotaHardStopPercent: '100',
      })
    );

    act(() => {
      harness.getCurrent().setVisualValues({
        codexCacheAffinityShadow: false,
        codexCacheAffinityMaxConcurrency: '4',
        codexCacheAffinityMaxEntries: '65536',
        codexCacheAffinityMaxRetryCredentials: '2',
        codexCacheAffinityWebsocketPoolSlots: '8',
        codexCacheAffinityMaxSessionRequests: '50',
        codexCacheAffinityMaxSessionDuration: '5m',
        codexCacheAffinityMaxSharePercent: '70',
        codexCacheAffinityQuotaPreemptPercent: '97',
        codexCacheAffinityQuotaHardStopPercent: '100',
      });
    });

    const parsed = parseYaml(harness.getCurrent().applyVisualChangesToYaml(yaml)) as {
      codex?: {
        cacheAffinity?: unknown;
        'future-option'?: string;
        'cache-affinity'?: Record<string, unknown>;
      };
    };
    expect(parsed.codex?.cacheAffinity).toBeUndefined();
    expect(parsed.codex?.['future-option']).toBe('preserve-me');
    expect(parsed.codex?.['cache-affinity']).toEqual({
      enabled: true,
      shadow: false,
      'max-concurrency': 4,
      'max-entries': 65536,
      'max-retry-credentials': 2,
      'websocket-pool-slots': 8,
      'max-session-requests': 50,
      'max-session-duration': '5m',
      'max-share-ratio': 0.7,
      'quota-preempt-used-ratio': 0.97,
      'quota-hard-stop-used-ratio': 1,
      'future-affinity-option': 'preserve-affinity',
    });

    harness.unmount();
  });

  it('validates Codex cache-affinity bounds and quota threshold ordering', () => {
    const errors = getVisualConfigValidationErrors({
      ...DEFAULT_VISUAL_VALUES,
      codexCacheAffinityEnabled: true,
      codexCacheAffinityMaxConcurrency: '0',
      codexCacheAffinityMaxEntries: '0',
      codexCacheAffinityMaxRetryCredentials: '-1',
      codexCacheAffinityWebsocketPoolSlots: '31',
      codexCacheAffinityMaxSessionRequests: '0',
      codexCacheAffinityMaxSessionDuration: 'five minutes',
      codexCacheAffinityMaxSharePercent: '101',
      codexCacheAffinityQuotaPreemptPercent: '97',
      codexCacheAffinityQuotaHardStopPercent: '97',
    });

    expect(errors).toMatchObject({
      codexCacheAffinityMaxConcurrency: 'positive_integer',
      codexCacheAffinityMaxEntries: 'positive_integer',
      codexCacheAffinityMaxRetryCredentials: 'positive_integer',
      codexCacheAffinityWebsocketPoolSlots: 'cache_affinity_websocket_slots_range',
      codexCacheAffinityMaxSessionRequests: 'positive_integer',
      codexCacheAffinityMaxSessionDuration: 'positive_duration',
      codexCacheAffinityMaxSharePercent: 'cache_affinity_share_percent_range',
      codexCacheAffinityQuotaHardStopPercent: 'cache_affinity_hard_stop_percent_range',
    });
  });

  it('validates Codex tail-burst trigger percentage and collector concurrency', () => {
    const harness = mountUseVisualConfig();

    act(() => {
      harness.getCurrent().setVisualValues({
        codexTailBurstEnabled: true,
        codexTailBurstTriggerRemainingPercent: '100',
        codexTailBurstExpiryWindow: 'soon',
        codexTailBurstMaxConcurrency: '0',
        codexTailBurstCollectorMaxConcurrency: '17',
      });
    });

    expect(harness.getCurrent().visualValidationErrors).toMatchObject({
      codexTailBurstTriggerRemainingPercent: 'tail_burst_trigger_percent_range',
      codexTailBurstExpiryWindow: 'positive_duration',
      codexTailBurstMaxConcurrency: 'positive_integer',
      codexTailBurstCollectorMaxConcurrency: 'tail_burst_collector_concurrency_range',
    });
    harness.unmount();
  });
  it('reads and writes routing high-cache mode without changing other routing options', () => {
    const harness = mountUseVisualConfig();
    const yaml = [
      'routing:',
      '  strategy: round-robin',
      '  session-affinity: true',
      '  session-affinity-ttl: 1h',
      '  future-routing-option: preserve-me',
      '',
    ].join('\n');

    act(() => {
      expect(harness.getCurrent().loadVisualValuesFromYaml(yaml).ok).toBe(true);
    });

    expect(harness.getCurrent().visualValues).toEqual(
      expect.objectContaining({
        routingStrategy: 'round-robin',
        routingSessionAffinity: true,
        routingHighCacheMode: false,
        routingSessionAffinityTTL: '1h',
      })
    );

    act(() => {
      harness.getCurrent().setVisualValues({ routingHighCacheMode: true });
    });

    const parsed = parseYaml(harness.getCurrent().applyVisualChangesToYaml(yaml)) as {
      routing?: Record<string, unknown>;
    };
    expect(parsed.routing).toEqual({
      strategy: 'round-robin',
      'session-affinity': true,
      'session-affinity-ttl': '1h',
      'future-routing-option': 'preserve-me',
      'high-cache-mode': true,
    });

    harness.unmount();
  });
});
