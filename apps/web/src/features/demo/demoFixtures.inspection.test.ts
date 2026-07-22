import { afterEach, describe, expect, it } from 'vitest';
import { usageServiceApi } from '@/services/api/usageService';
import { apiClient } from '@/services/api/client';
import { normalizeConfigResponse } from '@/services/api/transformers';
import { inspectCodexAccounts } from '@/features/monitoring/codexInspection';
import {
  getDemoApiCallResult,
  getDemoAuthFiles,
  getDemoCodexInspectionLocalRun,
  getDemoCodexInspectionRun,
  getDemoManagerConfig,
  getDemoRawConfig,
} from './demoFixtures';
import { setDemoMode } from './demoMode';

describe('credential health inspection demo fixtures', () => {
  afterEach(() => setDemoMode(false));

  it('enables Codex and xAI real inference in both inspection modes', () => {
    const managerConfig = getDemoManagerConfig().config.codexInspection;
    const localRun = getDemoCodexInspectionLocalRun();

    expect(managerConfig).toMatchObject({
      targetType: 'codex',
      targetTypes: ['codex', 'xai'],
      xaiInferenceEnabled: true,
      xaiInferenceModel: 'grok-4.5',
      sampleSize: 0,
    });
    expect(localRun.settings).toMatchObject({
      targetTypes: ['codex', 'xai'],
      xaiInferenceEnabled: true,
      xaiInferenceModel: 'grok-4.5',
      sampleSize: 0,
    });
  });

  it('keeps run summary counts aligned with the eight result scenarios', () => {
    const detail = getDemoCodexInspectionRun();
    const actions = detail.results.map((item) => item.action);
    const providers = new Set(detail.results.map((item) => item.provider));
    const authFiles = getDemoAuthFiles();

    expect(authFiles.total).toBe(authFiles.files.length);
    expect(detail.results).toHaveLength(8);
    expect(providers).toEqual(new Set(['codex', 'xai']));
    expect(detail.run.sampledCount).toBe(detail.results.length);
    expect(detail.run.disableCount).toBe(actions.filter((action) => action === 'disable').length);
    expect(detail.run.enableCount).toBe(actions.filter((action) => action === 'enable').length);
    expect(detail.run.reauthCount).toBe(actions.filter((action) => action === 'reauth').length);
    expect(detail.run.keepCount).toBe(actions.filter((action) => action === 'keep').length);
  });

  it('covers healthy, spending-limit, and expired xAI inference results', () => {
    const detail = getDemoCodexInspectionRun();
    const healthy = detail.results.find((item) => item.authIndex === 'xai-ops-01');
    const limited = detail.results.find((item) => item.authIndex === 'xai-email-user-01');
    const expired = detail.results.find((item) => item.authIndex === 'xai-expired-01');

    expect(healthy).toMatchObject({
      statusCode: 200,
      action: 'keep',
      errorKind: 'inference_healthy',
      planType: null,
    });
    expect(healthy?.quotaWindows?.[0]).toMatchObject({
      id: 'xai-weekly',
      usedPercent: 3,
    });
    expect(limited).toMatchObject({
      statusCode: 402,
      action: 'disable',
      errorKind: 'spending_limit',
      isQuota: true,
    });
    expect(expired).toMatchObject({
      statusCode: 401,
      action: 'reauth',
      errorKind: 'auth_invalid',
    });
  });

  it('uses localizable conclusion reasons in server and local inspection fixtures', () => {
    const serverReasons = getDemoCodexInspectionRun().results.map((item) => item.actionReason);
    const localReasons = getDemoCodexInspectionLocalRun().results.map((item) => item.actionReason);

    expect(serverReasons.every((reason) => reason.startsWith('monitoring.'))).toBe(true);
    expect(localReasons.every((reason) => reason.startsWith('monitoring.'))).toBe(true);
  });

  it('returns current xAI billing and inference response shapes by URL and credential', () => {
    const weekly = getDemoApiCallResult({
      authIndex: 'xai-ops-01',
      url: 'https://cli-chat-proxy.grok.com/v1/billing?format=credits',
    });
    const inference = getDemoApiCallResult({
      authIndex: 'xai-ops-01',
      url: 'https://cli-chat-proxy.grok.com/v1/responses',
    });
    const spendingLimit = getDemoApiCallResult({
      authIndex: 'xai-email-user-01',
      url: 'https://cli-chat-proxy.grok.com/v1/responses',
    });
    const expired = getDemoApiCallResult({
      authIndex: 'xai-expired-01',
      url: 'https://cli-chat-proxy.grok.com/v1/responses',
    });

    expect(weekly).toMatchObject({
      status_code: 200,
      body: {
        config: {
          currentPeriod: { type: 'USAGE_PERIOD_TYPE_WEEKLY' },
          creditUsagePercent: 3,
        },
      },
    });
    expect(inference).toMatchObject({
      status_code: 200,
      body: {
        status: 'completed',
        output: [{ content: [{ type: 'output_text', text: 'OK' }] }],
      },
    });
    expect(spendingLimit).toMatchObject({
      status_code: 402,
      body: { code: 'personal-team-blocked:spending-limit' },
    });
    expect(expired).toMatchObject({
      status_code: 401,
      body: { code: 'invalid_token' },
    });
  });

  it('returns Codex quota, recovery, and expired-auth responses by credential', () => {
    const pro = getDemoApiCallResult({
      authIndex: 'codex-pro-20x-01',
      url: 'https://chatgpt.com/backend-api/wham/usage',
    });
    const recovered = getDemoApiCallResult({
      authIndex: 'codex-fallback-02',
      url: 'https://chatgpt.com/backend-api/wham/usage',
    });
    const expired = getDemoApiCallResult({
      authIndex: 'codex-email-user-01',
      url: 'https://chatgpt.com/backend-api/wham/usage',
    });

    expect(pro).toMatchObject({
      status_code: 200,
      body: { rate_limit: { secondary_window: { used_percent: 96 } } },
    });
    expect(recovered).toMatchObject({
      status_code: 200,
      body: { rate_limit: { primary_window: { used_percent: 24 } } },
    });
    expect(expired).toMatchObject({
      status_code: 401,
      body: { error: { code: 'token_expired' } },
    });
  });

  it('marks executed demo actions as handled in the returned detail', async () => {
    setDemoMode(true);

    const response = await usageServiceApi.executeCodexInspectionActions(
      'http://demo.local',
      'demo-management-key',
      1001,
      [503]
    );
    const executed = response.detail.results.find((item) => item.id === 503);

    expect(response.outcomes).toEqual([
      expect.objectContaining({ resultId: 503, action: 'disable', success: true }),
    ]);
    expect(executed).toMatchObject({
      action: 'disable',
      actionStatus: 'success',
      executedAction: 'disable',
    });
  });

  it('drives the local inspection executor through all demo provider scenarios', async () => {
    setDemoMode(true);
    apiClient.setConfig({
      apiBase: 'http://demo.local',
      managementKey: 'demo-management-key',
    });
    const fixtureRun = getDemoCodexInspectionLocalRun();
    const result = await inspectCodexAccounts({
      config: normalizeConfigResponse(getDemoRawConfig()),
      apiBase: 'http://demo.local',
      managementKey: 'demo-management-key',
      settings: {
        targetTypes: fixtureRun.settings.targetTypes,
        xaiInferenceEnabled: true,
        xaiInferenceModel: fixtureRun.settings.xaiInferenceModel,
        xaiInferencePrompt: fixtureRun.settings.xaiInferencePrompt,
        xaiInferenceUserAgent: fixtureRun.settings.xaiInferenceUserAgent,
        usedPercentThreshold: fixtureRun.settings.usedPercentThreshold,
        sampleSize: 0,
      },
    });
    const byAuthIndex = new Map(result.results.map((item) => [item.authIndex, item]));

    expect(result.results).toHaveLength(8);
    expect(byAuthIndex.get('codex-upgrade-demo-01')).toMatchObject({
      action: 'keep',
      statusCode: 200,
      planType: 'free',
    });
    expect(byAuthIndex.get('codex-team-01')).toMatchObject({ action: 'keep', statusCode: 200 });
    expect(byAuthIndex.get('codex-email-user-01')).toMatchObject({
      action: 'reauth',
      statusCode: 401,
    });
    expect(byAuthIndex.get('codex-pro-20x-01')).toMatchObject({
      action: 'disable',
      statusCode: 200,
      usedPercent: 96,
    });
    expect(byAuthIndex.get('codex-fallback-02')).toMatchObject({
      action: 'enable',
      statusCode: 200,
    });
    expect(byAuthIndex.get('xai-ops-01')).toMatchObject({
      action: 'keep',
      statusCode: 200,
      errorKind: 'inference_healthy',
    });
    expect(byAuthIndex.get('xai-email-user-01')).toMatchObject({
      action: 'disable',
      statusCode: 402,
      errorKind: 'spending_limit',
    });
    expect(byAuthIndex.get('xai-expired-01')).toMatchObject({
      action: 'reauth',
      statusCode: 401,
      errorKind: 'auth_invalid',
    });
  });
});
