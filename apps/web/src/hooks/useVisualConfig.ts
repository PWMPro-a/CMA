import { useCallback, useMemo, useReducer } from 'react';
import { isMap, parse as parseYaml, parseDocument } from 'yaml';
import type {
  CodexClientRestrictionEntry,
  CodexEngineFingerprintSignal,
  CodexFingerprintSignalType,
  DisableImageGenerationMode,
  PluginStoreAuthApplyTo,
  PluginStoreAuthRule,
  PluginStoreAuthType,
  VisualConfigValues,
  VisualConfigValidationErrors,
} from '@/types/visualConfig';
import { DEFAULT_VISUAL_VALUES, makeClientId } from '@/types/visualConfig';
import {
  arePayloadFilterRulesEqual,
  arePayloadRulesEqual,
  hasPayloadParamValidationErrors,
  parsePayloadFilterRules,
  parsePayloadRules,
  parseRawPayloadRules,
  serializePayloadFilterRulesForYaml,
  serializePayloadRulesForYaml,
  serializeRawPayloadRulesForYaml,
} from './visualConfigPayloadRules';

export {
  getPayloadParamValidationError,
  VISUAL_CONFIG_PAYLOAD_VALUE_TYPE_OPTIONS,
  VISUAL_CONFIG_PROTOCOL_OPTIONS,
} from './visualConfigPayloadRules';

function asRecord(value: unknown): Record<string, unknown> | null {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) return null;
  return value as Record<string, unknown>;
}

function extractApiKeyValue(raw: unknown): string | null {
  if (typeof raw === 'string') {
    const trimmed = raw.trim();
    return trimmed ? trimmed : null;
  }

  const record = asRecord(raw);
  if (!record) return null;

  const candidates = [record['api-key'], record.apiKey, record.key, record.Key];
  for (const candidate of candidates) {
    if (typeof candidate === 'string') {
      const trimmed = candidate.trim();
      if (trimmed) return trimmed;
    }
  }

  return null;
}

function parseApiKeysText(raw: unknown): string {
  if (!Array.isArray(raw)) return '';

  const keys: string[] = [];
  for (const item of raw) {
    const key = extractApiKeyValue(item);
    if (key) keys.push(key);
  }
  return keys.join('\n');
}

function parseStringArrayText(raw: unknown): string {
  if (!Array.isArray(raw)) return '';
  return raw
    .map((item) => (typeof item === 'string' ? item.trim() : ''))
    .filter(Boolean)
    .join('\n');
}

function parseStringList(raw: unknown): string[] {
  if (!Array.isArray(raw)) return [];
  return raw.map((item) => String(item ?? '').trim()).filter(Boolean);
}

const CODEX_FINGERPRINT_SIGNAL_TYPES: CodexFingerprintSignalType[] = [
  'header_exact',
  'header_prefix',
  'body_path',
];

function parseCodexClientRestrictionEntries(raw: unknown): CodexClientRestrictionEntry[] {
  if (!Array.isArray(raw)) return [];
  return raw.reduce<CodexClientRestrictionEntry[]>((result, value) => {
    const entry = asRecord(value);
    if (!entry) return result;
    result.push({
      id: makeClientId(),
      originator: typeof entry.originator === 'string' ? entry.originator : '',
      uaContains: parseStringList(entry['ua-contains'] ?? entry.uaContains),
      skipEngineFingerprint: Boolean(
        entry['skip-engine-fingerprint'] ?? entry.skipEngineFingerprint
      ),
    });
    return result;
  }, []);
}

function parseCodexEngineFingerprintSignals(raw: unknown): CodexEngineFingerprintSignal[] | null {
  if (!Array.isArray(raw)) return null;
  return raw.reduce<CodexEngineFingerprintSignal[]>((result, value) => {
    const signal = asRecord(value);
    const type = String(signal?.type ?? '').trim() as CodexFingerprintSignalType;
    if (!signal || !CODEX_FINGERPRINT_SIGNAL_TYPES.includes(type)) return result;
    result.push({
      id: makeClientId(),
      type,
      match: parseStringList(signal.match),
      required: signal.required === true,
    });
    return result;
  }, []);
}

const PLUGIN_STORE_AUTH_TYPES: PluginStoreAuthType[] = [
  'none',
  'bearer',
  'basic',
  'header',
  'github-token',
];
const PLUGIN_STORE_AUTH_APPLY_TO: PluginStoreAuthApplyTo[] = ['registry', 'metadata', 'artifact'];

function parsePluginStoreAuthType(raw: unknown): PluginStoreAuthType {
  const value = String(raw ?? '')
    .trim()
    .toLowerCase();
  return PLUGIN_STORE_AUTH_TYPES.includes(value as PluginStoreAuthType)
    ? (value as PluginStoreAuthType)
    : 'none';
}

function parsePluginStoreAuthApplyTo(raw: unknown): PluginStoreAuthApplyTo[] {
  return parseStringList(raw)
    .map((item) => item.toLowerCase())
    .filter((item): item is PluginStoreAuthApplyTo =>
      PLUGIN_STORE_AUTH_APPLY_TO.includes(item as PluginStoreAuthApplyTo)
    );
}

function parsePluginStoreAuthRules(raw: unknown): PluginStoreAuthRule[] {
  if (!Array.isArray(raw)) return [];
  return raw
    .map((item, index): PluginStoreAuthRule | null => {
      const record = asRecord(item);
      if (!record) return null;
      const rule: PluginStoreAuthRule = {
        id: `plugin-store-auth-${index}`,
        match: typeof record.match === 'string' ? record.match : '',
        applyTo: parsePluginStoreAuthApplyTo(record['apply-to'] ?? record.apply_to),
        type: parsePluginStoreAuthType(record.type),
        tokenEnv: typeof record['token-env'] === 'string' ? record['token-env'] : '',
        usernameEnv: typeof record['username-env'] === 'string' ? record['username-env'] : '',
        passwordEnv: typeof record['password-env'] === 'string' ? record['password-env'] : '',
        headerName: typeof record['header-name'] === 'string' ? record['header-name'] : '',
        headerValueEnv:
          typeof record['header-value-env'] === 'string' ? record['header-value-env'] : '',
        allowInsecure: Boolean(record['allow-insecure'] ?? record.allow_insecure),
      };
      return rule.match.trim() ||
        rule.type !== 'none' ||
        rule.applyTo.length > 0 ||
        rule.tokenEnv.trim() ||
        rule.usernameEnv.trim() ||
        rule.passwordEnv.trim() ||
        rule.headerName.trim() ||
        rule.headerValueEnv.trim() ||
        rule.allowInsecure
        ? rule
        : null;
    })
    .filter((rule): rule is PluginStoreAuthRule => Boolean(rule));
}

function resolveApiKeysText(parsed: Record<string, unknown>): string {
  if (Object.prototype.hasOwnProperty.call(parsed, 'api-keys')) {
    return parseApiKeysText(parsed['api-keys']);
  }

  const auth = asRecord(parsed.auth);
  const providers = asRecord(auth?.providers);
  const configApiKeyProvider = asRecord(providers?.['config-api-key']);
  if (!configApiKeyProvider) return '';

  if (Object.prototype.hasOwnProperty.call(configApiKeyProvider, 'api-key-entries')) {
    return parseApiKeysText(configApiKeyProvider['api-key-entries']);
  }

  return parseApiKeysText(configApiKeyProvider['api-keys']);
}

type YamlDocument = ReturnType<typeof parseDocument>;
type YamlPath = string[];

function docHas(doc: YamlDocument, path: YamlPath): boolean {
  return doc.hasIn(path);
}

function ensureMapInDoc(doc: YamlDocument, path: YamlPath): void {
  const existing = doc.getIn(path, true);
  if (isMap(existing)) return;
  // Use a YAML node here; plain objects are not treated as collections by subsequent `setIn`.
  doc.setIn(path, doc.createNode({}));
}

function yamlValueAsRecord(value: unknown): Record<string, unknown> | null {
  if (value && typeof value === 'object' && 'toJSON' in value) {
    const toJSON = (value as { toJSON?: () => unknown }).toJSON;
    if (typeof toJSON === 'function') return asRecord(toJSON.call(value));
  }
  return asRecord(value);
}

function canonicalizeCodexClientRestrictionValue(key: string, value: unknown): unknown {
  if (key === 'whitelist' || key === 'blacklist') {
    if (!Array.isArray(value)) return value;
    return value.map((item) => {
      const entry = yamlValueAsRecord(item);
      if (!entry) return item;
      const next: Record<string, unknown> = { ...entry };
      if (next['ua-contains'] === undefined && next.uaContains !== undefined) {
        next['ua-contains'] = next.uaContains;
      }
      if (
        next['skip-engine-fingerprint'] === undefined &&
        next.skipEngineFingerprint !== undefined
      ) {
        next['skip-engine-fingerprint'] = next.skipEngineFingerprint;
      }
      delete next.uaContains;
      delete next.skipEngineFingerprint;
      return next;
    });
  }

  if (key === 'engineFingerprintSignals' || key === 'engine-fingerprint-signals') {
    if (!Array.isArray(value)) return value;
    return value.map((item) => {
      const signal = yamlValueAsRecord(item);
      if (!signal) return item;
      const next: Record<string, unknown> = { ...signal };
      if (next.match === undefined && next.matches !== undefined) next.match = next.matches;
      delete next.matches;
      return next;
    });
  }

  return value;
}

function migrateLegacyCodexClientRestriction(doc: YamlDocument): void {
  const legacyPath = ['codex', 'clientRestriction'];
  const canonicalPath = ['codex', 'client-restriction'];
  if (!docHas(doc, legacyPath)) return;

  const legacy = yamlValueAsRecord(doc.getIn(legacyPath, true));
  const canonical = yamlValueAsRecord(doc.getIn(canonicalPath, true));
  if (!legacy) {
    doc.deleteIn(legacyPath);
    return;
  }

  const keyAliases: Record<string, string> = {
    forceCodexCli: 'force-codex-cli',
    minCodexVersion: 'min-codex-version',
    maxCodexVersion: 'max-codex-version',
    allowAppServerClients: 'allow-app-server-clients',
    engineFingerprintSignals: 'engine-fingerprint-signals',
  };
  const merged: Record<string, unknown> = {};
  Object.entries(legacy).forEach(([key, value]) => {
    const canonicalKey = keyAliases[key] ?? key;
    merged[canonicalKey] = canonicalizeCodexClientRestrictionValue(canonicalKey, value);
  });
  Object.entries(canonical ?? {}).forEach(([key, value]) => {
    merged[key] = canonicalizeCodexClientRestrictionValue(key, value);
  });

  doc.setIn(canonicalPath, doc.createNode(merged));
  doc.deleteIn(legacyPath);
}

function migrateLegacyCodexCacheAffinity(doc: YamlDocument): void {
  const legacyPath = ['codex', 'cacheAffinity'];
  const canonicalPath = ['codex', 'cache-affinity'];
  const legacy = yamlValueAsRecord(doc.getIn(legacyPath, true));
  const canonical = yamlValueAsRecord(doc.getIn(canonicalPath, true));
  if (!legacy && !canonical) return;

  const keyAliases: Record<string, string> = {
    maxConcurrency: 'max-concurrency',
    maxEntries: 'max-entries',
    maxRetryCredentials: 'max-retry-credentials',
    websocketPoolSlots: 'websocket-pool-slots',
    maxSessionRequests: 'max-session-requests',
    maxSessionDuration: 'max-session-duration',
    maxShareRatio: 'max-share-ratio',
    max_share_ratio: 'max-share-ratio',
    quotaPreemptUsedRatio: 'quota-preempt-used-ratio',
    quotaHardStopUsedRatio: 'quota-hard-stop-used-ratio',
  };
  const merged: Record<string, unknown> = {};
  Object.entries(legacy ?? {}).forEach(([key, value]) => {
    merged[keyAliases[key] ?? key] = value;
  });
  Object.entries(canonical ?? {}).forEach(([key, value]) => {
    merged[keyAliases[key] ?? key] = value;
  });

  doc.setIn(canonicalPath, doc.createNode(merged));
  if (docHas(doc, legacyPath)) doc.deleteIn(legacyPath);
}

function deleteIfMapEmpty(doc: YamlDocument, path: YamlPath): void {
  const value = doc.getIn(path, true);
  if (!isMap(value)) return;
  if (value.items.length === 0) doc.deleteIn(path);
}

function setBooleanInDoc(doc: YamlDocument, path: YamlPath, value: boolean): void {
  if (value) {
    doc.setIn(path, true);
    return;
  }
  if (docHas(doc, path)) doc.setIn(path, false);
}

function setStringInDoc(doc: YamlDocument, path: YamlPath, value: unknown): void {
  const safe = typeof value === 'string' ? value : '';
  const trimmed = safe.trim();
  if (trimmed !== '') {
    doc.setIn(path, safe);
    return;
  }
  // Preserve existing empty-string keys to avoid dropping template blocks/comments.
  // Only keep the key when it already exists in the YAML.
  if (docHas(doc, path)) {
    doc.setIn(path, '');
  }
}

function setIntFromStringInDoc(doc: YamlDocument, path: YamlPath, value: unknown): void {
  const safe = typeof value === 'string' ? value : '';
  const trimmed = safe.trim();
  if (trimmed === '') {
    if (docHas(doc, path)) doc.deleteIn(path);
    return;
  }

  if (!/^-?\d+$/.test(trimmed)) {
    return;
  }

  const parsed = Number(trimmed);
  if (Number.isFinite(parsed)) {
    doc.setIn(path, parsed);
    return;
  }
}

function setDisableImageGenerationInDoc(
  doc: YamlDocument,
  path: YamlPath,
  value: DisableImageGenerationMode
): void {
  if (value === 'chat' || value === 'passthrough') {
    doc.setIn(path, value);
    return;
  }

  if (value === 'true') {
    doc.setIn(path, true);
    return;
  }

  if (docHas(doc, path)) doc.setIn(path, false);
}

function serializeStringListForYaml(items?: string[]): string[] {
  return (items ?? []).map((item) => item.trim()).filter(Boolean);
}

function serializePluginStoreAuthForYaml(
  rules: PluginStoreAuthRule[]
): Array<Record<string, unknown>> {
  return rules
    .map((rule) => {
      const match = rule.match.trim();
      if (!match) return null;
      const item: Record<string, unknown> = {
        match,
        type: rule.type,
      };
      const applyTo = serializeStringListForYaml(rule.applyTo);
      if (applyTo.length > 0) item['apply-to'] = applyTo;
      if (rule.tokenEnv.trim()) item['token-env'] = rule.tokenEnv.trim();
      if (rule.usernameEnv.trim()) item['username-env'] = rule.usernameEnv.trim();
      if (rule.passwordEnv.trim()) item['password-env'] = rule.passwordEnv.trim();
      if (rule.headerName.trim()) item['header-name'] = rule.headerName.trim();
      if (rule.headerValueEnv.trim()) item['header-value-env'] = rule.headerValueEnv.trim();
      if (rule.allowInsecure) item['allow-insecure'] = true;
      return item;
    })
    .filter((rule): rule is Record<string, unknown> => Boolean(rule));
}

function serializeCodexClientRestrictionEntries(
  entries: CodexClientRestrictionEntry[],
  whitelist: boolean
): Array<Record<string, unknown>> {
  return entries
    .map((entry) => {
      const originator = entry.originator.trim();
      const uaContains = serializeStringListForYaml(entry.uaContains);
      if (whitelist && (!originator || uaContains.length === 0)) return null;
      if (!whitelist && !originator && uaContains.length === 0) return null;
      const value: Record<string, unknown> = {};
      if (originator) value.originator = originator;
      if (uaContains.length > 0) value['ua-contains'] = uaContains;
      if (whitelist && entry.skipEngineFingerprint) {
        value['skip-engine-fingerprint'] = true;
      }
      return value;
    })
    .filter((entry): entry is Record<string, unknown> => Boolean(entry));
}

function serializeCodexEngineFingerprintSignals(
  signals: CodexEngineFingerprintSignal[]
): Array<Record<string, unknown>> {
  const result: Array<Record<string, unknown>> = [];
  signals.forEach((signal) => {
    const match = serializeStringListForYaml(signal.match);
    if (!CODEX_FINGERPRINT_SIGNAL_TYPES.includes(signal.type) || match.length === 0) return;
    result.push({ type: signal.type, match, required: signal.required });
  });
  return result;
}

function areStringArraysEqual(left: string[] | undefined, right: string[] | undefined): boolean {
  const leftItems = left ?? [];
  const rightItems = right ?? [];
  if (leftItems.length !== rightItems.length) return false;
  return leftItems.every((item, index) => item === rightItems[index]);
}

function arePluginStoreAuthRulesEqual(
  left: PluginStoreAuthRule[] | undefined,
  right: PluginStoreAuthRule[] | undefined
): boolean {
  const leftItems = left ?? [];
  const rightItems = right ?? [];
  if (leftItems.length !== rightItems.length) return false;
  return leftItems.every((a, index) => {
    const b = rightItems[index];
    return (
      Boolean(b) &&
      a.match === b.match &&
      a.type === b.type &&
      a.tokenEnv === b.tokenEnv &&
      a.usernameEnv === b.usernameEnv &&
      a.passwordEnv === b.passwordEnv &&
      a.headerName === b.headerName &&
      a.headerValueEnv === b.headerValueEnv &&
      a.allowInsecure === b.allowInsecure &&
      areStringArraysEqual(a.applyTo, b.applyTo)
    );
  });
}

function areCodexClientRestrictionEntriesEqual(
  left: CodexClientRestrictionEntry[] | undefined,
  right: CodexClientRestrictionEntry[] | undefined
): boolean {
  const leftItems = left ?? [];
  const rightItems = right ?? [];
  return (
    leftItems.length === rightItems.length &&
    leftItems.every((entry, index) => {
      const other = rightItems[index];
      return (
        Boolean(other) &&
        entry.originator === other.originator &&
        entry.skipEngineFingerprint === other.skipEngineFingerprint &&
        areStringArraysEqual(entry.uaContains, other.uaContains)
      );
    })
  );
}

function areCodexEngineFingerprintSignalsEqual(
  left: CodexEngineFingerprintSignal[] | undefined,
  right: CodexEngineFingerprintSignal[] | undefined
): boolean {
  const leftItems = left ?? [];
  const rightItems = right ?? [];
  return (
    leftItems.length === rightItems.length &&
    leftItems.every((signal, index) => {
      const other = rightItems[index];
      return (
        Boolean(other) &&
        signal.type === other.type &&
        signal.required === other.required &&
        areStringArraysEqual(signal.match, other.match)
      );
    })
  );
}

function getNonNegativeIntegerError(value: string): 'non_negative_integer' | undefined {
  const trimmed = value.trim();
  if (!trimmed) return undefined;
  if (!/^-?\d+$/.test(trimmed)) return 'non_negative_integer';
  return Number(trimmed) >= 0 ? undefined : 'non_negative_integer';
}

function getPositiveIntegerError(value: string): 'positive_integer' | undefined {
  const trimmed = value.trim();
  if (!/^\d+$/.test(trimmed)) return 'positive_integer';
  const parsed = Number(trimmed);
  return Number.isSafeInteger(parsed) && parsed >= 1 ? undefined : 'positive_integer';
}

function getIntegerError(value: string): 'integer' | undefined {
  const trimmed = value.trim();
  if (!trimmed) return undefined;
  return /^-?\d+$/.test(trimmed) ? undefined : 'integer';
}

function getPortError(value: string): 'port_range' | undefined {
  const trimmed = value.trim();
  if (!trimmed) return undefined;
  if (!/^\d+$/.test(trimmed)) return 'port_range';
  const parsed = Number(trimmed);
  return parsed >= 1 && parsed <= 65535 ? undefined : 'port_range';
}

function getRedisUsageQueueRetentionError(value: string): 'retention_seconds_range' | undefined {
  const trimmed = value.trim();
  if (!trimmed) return undefined;
  if (!/^\d+$/.test(trimmed)) return 'retention_seconds_range';
  const parsed = Number(trimmed);
  return parsed >= 1 && parsed <= 3600 ? undefined : 'retention_seconds_range';
}

function parseDisableImageGenerationMode(raw: unknown): DisableImageGenerationMode {
  if (raw === true) return 'true';
  if (typeof raw === 'string') {
    const normalized = raw.trim().toLowerCase();
    if (normalized === 'true') return 'true';
    if (normalized === 'chat') return 'chat';
    if (normalized === 'passthrough') return 'passthrough';
  }
  return 'false';
}

function getTailBurstCollectorConcurrencyError(
  value: string
): 'tail_burst_collector_concurrency_range' | undefined {
  const trimmed = value.trim();
  if (!trimmed) return undefined;
  if (!/^\d+$/.test(trimmed)) return 'tail_burst_collector_concurrency_range';
  const parsed = Number(trimmed);
  return parsed >= 1 && parsed <= 16 ? undefined : 'tail_burst_collector_concurrency_range';
}

function getTailBurstTriggerPercentError(
  value: string
): 'tail_burst_trigger_percent_range' | undefined {
  const trimmed = value.trim();
  if (!trimmed) return undefined;
  const parsed = Number(trimmed);
  return Number.isFinite(parsed) && parsed > 0 && parsed < 100
    ? undefined
    : 'tail_burst_trigger_percent_range';
}

function getCacheAffinityWebsocketSlotsError(
  value: string
): 'cache_affinity_websocket_slots_range' | undefined {
  const trimmed = value.trim();
  if (!/^\d+$/.test(trimmed)) return 'cache_affinity_websocket_slots_range';
  const parsed = Number(trimmed);
  return parsed >= 1 && parsed <= 30 ? undefined : 'cache_affinity_websocket_slots_range';
}

function getPositiveDurationError(value: string): 'positive_duration' | undefined {
  const trimmed = value.trim();
  if (!trimmed) return 'positive_duration';
  return /^(?:\d+(?:\.\d+)?(?:ns|us|µs|μs|ms|s|m|h))+$/.test(trimmed) && /[1-9]/.test(trimmed)
    ? undefined
    : 'positive_duration';
}

function getCacheAffinityPreemptPercentError(
  value: string
): 'cache_affinity_preempt_percent_range' | undefined {
  const parsed = Number(value.trim());
  return Number.isFinite(parsed) && parsed > 0 && parsed < 100
    ? undefined
    : 'cache_affinity_preempt_percent_range';
}

function getCacheAffinitySharePercentError(
  value: string
): 'cache_affinity_share_percent_range' | undefined {
  const trimmed = value.trim();
  if (!trimmed) return undefined;
  const parsed = Number(trimmed);
  return Number.isFinite(parsed) && parsed >= 0 && parsed <= 100
    ? undefined
    : 'cache_affinity_share_percent_range';
}

function getCacheAffinityHardStopPercentError(
  hardStopValue: string,
  preemptValue: string
): 'cache_affinity_hard_stop_percent_range' | undefined {
  const hardStop = Number(hardStopValue.trim());
  const preempt = Number(preemptValue.trim());
  return Number.isFinite(hardStop) &&
    Number.isFinite(preempt) &&
    hardStop > preempt &&
    hardStop <= 100
    ? undefined
    : 'cache_affinity_hard_stop_percent_range';
}

function getCodexVersionError(value: string): 'codex_version' | undefined {
  const trimmed = value.trim();
  if (!trimmed) return undefined;
  return /^\d+\.\d+\.\d+$/.test(trimmed) ? undefined : 'codex_version';
}

function compareCodexVersions(left: string, right: string): number {
  const leftParts = left.split('.').map(Number);
  const rightParts = right.split('.').map(Number);
  for (let index = 0; index < 3; index += 1) {
    const difference = (leftParts[index] ?? 0) - (rightParts[index] ?? 0);
    if (difference !== 0) return difference;
  }
  return 0;
}

function readTailBurstRemainingPercent(remainingValue: unknown, legacyUsedValue: unknown): string {
  const remainingRatio = Number(remainingValue);
  if (Number.isFinite(remainingRatio) && remainingRatio > 0 && remainingRatio < 1) {
    return String(Number((remainingRatio * 100).toFixed(4)));
  }
  const usedRatio = Number(legacyUsedValue);
  if (Number.isFinite(usedRatio) && usedRatio > 0 && usedRatio < 1) {
    return String(Number(((1 - usedRatio) * 100).toFixed(4)));
  }
  return '2';
}

function readTailBurstDuration(value: unknown, fallback: string): string {
  return typeof value === 'string' && value.trim() ? value : fallback;
}

function readRatioPercent(value: unknown, fallback: string, allowOne: boolean): string {
  const ratio = Number(value);
  const valid = Number.isFinite(ratio) && ratio > 0 && (allowOne ? ratio <= 1 : ratio < 1);
  if (!valid) return fallback;
  return String(Number((ratio * 100).toFixed(4)));
}

function readNonNegativeRatioPercent(value: unknown, fallback: string): string {
  const ratio = Number(value);
  const valid = Number.isFinite(ratio) && ratio >= 0 && ratio <= 1;
  if (!valid) return fallback;
  return String(Number((ratio * 100).toFixed(4)));
}

function setRatioPercentInDoc(doc: YamlDocument, path: YamlPath, value: string): void {
  const percent = Number(value.trim());
  if (Number.isFinite(percent) && percent > 0 && percent <= 100) {
    doc.setIn(path, percent / 100);
  }
}

function setNonNegativeRatioPercentInDoc(doc: YamlDocument, path: YamlPath, value: string): void {
  const trimmed = value.trim();
  if (!trimmed) {
    if (docHas(doc, path)) doc.deleteIn(path);
    return;
  }
  const percent = Number(trimmed);
  if (Number.isFinite(percent) && percent >= 0 && percent <= 100) {
    doc.setIn(path, percent / 100);
  }
}

function setTailBurstRemainingRatioInDoc(doc: YamlDocument, path: YamlPath, value: string): void {
  const trimmed = value.trim();
  if (!trimmed) {
    if (docHas(doc, path)) doc.deleteIn(path);
    return;
  }
  const percent = Number(trimmed);
  if (Number.isFinite(percent) && percent > 0 && percent < 100) {
    doc.setIn(path, percent / 100);
  }
}

export function getVisualConfigValidationErrors(
  values: VisualConfigValues
): VisualConfigValidationErrors {
  const minVersionError = getCodexVersionError(values.codexClientMinVersion);
  const maxVersionError = getCodexVersionError(values.codexClientMaxVersion);
  const versionRangeError =
    !minVersionError &&
    !maxVersionError &&
    values.codexClientMinVersion.trim() &&
    values.codexClientMaxVersion.trim() &&
    compareCodexVersions(values.codexClientMaxVersion.trim(), values.codexClientMinVersion.trim()) <
      0
      ? 'codex_version_range'
      : undefined;
  return {
    port: getPortError(values.port),
    errorLogsMaxFiles: getNonNegativeIntegerError(values.errorLogsMaxFiles),
    logsMaxTotalSizeMb: getNonNegativeIntegerError(values.logsMaxTotalSizeMb),
    redisUsageQueueRetentionSeconds: getRedisUsageQueueRetentionError(
      values.redisUsageQueueRetentionSeconds
    ),
    transientErrorCooldownSeconds: getIntegerError(values.transientErrorCooldownSeconds),
    requestRetry: getNonNegativeIntegerError(values.requestRetry),
    maxRetryCredentials: getNonNegativeIntegerError(values.maxRetryCredentials),
    maxRetryInterval: getNonNegativeIntegerError(values.maxRetryInterval),
    authAutoRefreshWorkers: getNonNegativeIntegerError(values.authAutoRefreshWorkers),
    codexClientMinVersion: minVersionError,
    codexClientMaxVersion: maxVersionError ?? versionRangeError,
    codexCacheAffinityMaxConcurrency: values.codexCacheAffinityEnabled
      ? getPositiveIntegerError(values.codexCacheAffinityMaxConcurrency)
      : undefined,
    codexCacheAffinityMaxEntries: values.codexCacheAffinityEnabled
      ? getPositiveIntegerError(values.codexCacheAffinityMaxEntries)
      : undefined,
    codexCacheAffinityMaxRetryCredentials: values.codexCacheAffinityEnabled
      ? getPositiveIntegerError(values.codexCacheAffinityMaxRetryCredentials)
      : undefined,
    codexCacheAffinityWebsocketPoolSlots: values.codexCacheAffinityEnabled
      ? getCacheAffinityWebsocketSlotsError(values.codexCacheAffinityWebsocketPoolSlots)
      : undefined,
    codexCacheAffinityMaxSessionRequests: values.codexCacheAffinityEnabled
      ? getPositiveIntegerError(values.codexCacheAffinityMaxSessionRequests)
      : undefined,
    codexCacheAffinityMaxSessionDuration: values.codexCacheAffinityEnabled
      ? getPositiveDurationError(values.codexCacheAffinityMaxSessionDuration)
      : undefined,
    codexCacheAffinityMaxSharePercent: values.codexCacheAffinityEnabled
      ? getCacheAffinitySharePercentError(values.codexCacheAffinityMaxSharePercent)
      : undefined,
    codexCacheAffinityQuotaPreemptPercent: values.codexCacheAffinityEnabled
      ? getCacheAffinityPreemptPercentError(values.codexCacheAffinityQuotaPreemptPercent)
      : undefined,
    codexCacheAffinityQuotaHardStopPercent: values.codexCacheAffinityEnabled
      ? getCacheAffinityHardStopPercentError(
          values.codexCacheAffinityQuotaHardStopPercent,
          values.codexCacheAffinityQuotaPreemptPercent
        )
      : undefined,
    codexTailBurstTriggerRemainingPercent: values.codexTailBurstEnabled
      ? getTailBurstTriggerPercentError(values.codexTailBurstTriggerRemainingPercent)
      : undefined,
    codexTailBurstExpiryWindow: values.codexTailBurstEnabled
      ? getPositiveDurationError(values.codexTailBurstExpiryWindow)
      : undefined,
    codexTailBurstMaxConcurrency: values.codexTailBurstEnabled
      ? getPositiveIntegerError(values.codexTailBurstMaxConcurrency)
      : undefined,
    codexTailBurstCollectorMaxConcurrency: values.codexTailBurstEnabled
      ? getTailBurstCollectorConcurrencyError(values.codexTailBurstCollectorMaxConcurrency)
      : undefined,
    'streaming.keepaliveSeconds': getNonNegativeIntegerError(values.streaming.keepaliveSeconds),
    'streaming.bootstrapRetries': getNonNegativeIntegerError(values.streaming.bootstrapRetries),
    'streaming.nonstreamKeepaliveInterval': getNonNegativeIntegerError(
      values.streaming.nonstreamKeepaliveInterval
    ),
  };
}

function deleteLegacyApiKeysProvider(doc: YamlDocument): void {
  if (docHas(doc, ['auth', 'providers', 'config-api-key', 'api-key-entries'])) {
    doc.deleteIn(['auth', 'providers', 'config-api-key', 'api-key-entries']);
  }
  if (docHas(doc, ['auth', 'providers', 'config-api-key', 'api-keys'])) {
    doc.deleteIn(['auth', 'providers', 'config-api-key', 'api-keys']);
  }
  deleteIfMapEmpty(doc, ['auth', 'providers', 'config-api-key']);
  deleteIfMapEmpty(doc, ['auth', 'providers']);
  deleteIfMapEmpty(doc, ['auth']);
}

function deepClone<T>(value: T): T {
  if (typeof structuredClone === 'function') return structuredClone(value);
  return JSON.parse(JSON.stringify(value)) as T;
}

type VisualConfigState = {
  visualValues: VisualConfigValues;
  baselineValues: VisualConfigValues;
  dirtyFields: Set<string>;
  visualParseError: string | null;
};

type VisualConfigAction =
  | {
      type: 'load_success';
      values: VisualConfigValues;
    }
  | {
      type: 'load_error';
      error: string;
    }
  | {
      type: 'set_values';
      values: Partial<VisualConfigValues>;
    };

function createInitialVisualConfigState(): VisualConfigState {
  const initialValues = deepClone(DEFAULT_VISUAL_VALUES);
  return {
    visualValues: initialValues,
    baselineValues: deepClone(initialValues),
    dirtyFields: new Set(),
    visualParseError: null,
  };
}

function mergeVisualConfigValues(
  currentValues: VisualConfigValues,
  patch: Partial<VisualConfigValues>
): VisualConfigValues {
  const nextValues: VisualConfigValues = { ...currentValues, ...patch } as VisualConfigValues;
  if (patch.streaming) {
    nextValues.streaming = { ...currentValues.streaming, ...patch.streaming };
  }
  return nextValues;
}

function getNextDirtyFields(
  currentDirtyFields: Set<string>,
  patch: Partial<VisualConfigValues>,
  nextValues: VisualConfigValues,
  baselineValues: VisualConfigValues
): Set<string> {
  const nextDirtyFields = new Set(currentDirtyFields);
  const updateDirty = (key: string, isEqual: boolean) => {
    if (isEqual) {
      nextDirtyFields.delete(key);
    } else {
      nextDirtyFields.add(key);
    }
  };
  const updateScalarDirty = (key: keyof VisualConfigValues) => {
    if (Object.prototype.hasOwnProperty.call(patch, key)) {
      updateDirty(key, nextValues[key] === baselineValues[key]);
    }
  };

  (
    [
      'rmDisableAutoUpdatePanel',
      'errorLogsMaxFiles',
      'pluginsEnabled',
      'pluginsDir',
      'pluginStoreSourcesText',
      'passthroughHeaders',
      'disableCooling',
      'saveCooldownStatus',
      'transientErrorCooldownSeconds',
      'disableClaudeCloakMode',
      'disableImageGeneration',
      'gptImage2BaseModel',
      'videoResultAuthCacheTtl',
      'authAutoRefreshWorkers',
      'pprofEnable',
      'pprofAddr',
      'antigravitySignatureCacheEnabled',
      'antigravitySignatureBypassStrict',
      'claudeHeaderUserAgent',
      'claudeHeaderPackageVersion',
      'claudeHeaderRuntimeVersion',
      'claudeHeaderOs',
      'claudeHeaderArch',
      'claudeHeaderTimeout',
      'claudeHeaderStabilizeDeviceProfile',
      'codexHeaderUserAgent',
      'codexHeaderBetaFeatures',
      'codexIdentityConfuse',
      'codexClientForceAllow',
      'codexClientMinVersion',
      'codexClientMaxVersion',
      'codexClientAllowAppServer',
      'codexCacheAffinityEnabled',
      'codexCacheAffinityShadow',
      'codexCacheAffinityMaxConcurrency',
      'codexCacheAffinityMaxEntries',
      'codexCacheAffinityMaxRetryCredentials',
      'codexCacheAffinityWebsocketPoolSlots',
      'codexCacheAffinityMaxSessionRequests',
      'codexCacheAffinityMaxSessionDuration',
      'codexCacheAffinityMaxSharePercent',
      'codexCacheAffinityQuotaPreemptPercent',
      'codexCacheAffinityQuotaHardStopPercent',
      'codexTailBurstEnabled',
      'codexTailBurstTriggerRemainingPercent',
      'codexTailBurstSnapshotTtl',
      'codexTailBurstExpiryWindow',
      'codexTailBurstMaxConcurrency',
      'codexTailBurstCollectorInterval',
      'codexTailBurstCollectorMaxConcurrency',
      'codexTailBurstCollectorTimeout',
      'codexTailBurstToolInjectionEnabled',
    ] as Array<keyof VisualConfigValues>
  ).forEach(updateScalarDirty);

  if (Object.prototype.hasOwnProperty.call(patch, 'pluginStoreAuth')) {
    updateDirty(
      'pluginStoreAuth',
      arePluginStoreAuthRulesEqual(nextValues.pluginStoreAuth, baselineValues.pluginStoreAuth)
    );
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'codexClientWhitelist')) {
    updateDirty(
      'codexClientWhitelist',
      areCodexClientRestrictionEntriesEqual(
        nextValues.codexClientWhitelist,
        baselineValues.codexClientWhitelist
      )
    );
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'codexClientBlacklist')) {
    updateDirty(
      'codexClientBlacklist',
      areCodexClientRestrictionEntriesEqual(
        nextValues.codexClientBlacklist,
        baselineValues.codexClientBlacklist
      )
    );
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'codexClientFingerprintSignals')) {
    updateDirty(
      'codexClientFingerprintSignals',
      areCodexEngineFingerprintSignalsEqual(
        nextValues.codexClientFingerprintSignals,
        baselineValues.codexClientFingerprintSignals
      )
    );
  }

  if (Object.prototype.hasOwnProperty.call(patch, 'host')) {
    updateDirty('host', nextValues.host === baselineValues.host);
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'port')) {
    updateDirty('port', nextValues.port === baselineValues.port);
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'tlsEnable')) {
    updateDirty('tlsEnable', nextValues.tlsEnable === baselineValues.tlsEnable);
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'tlsCert')) {
    updateDirty('tlsCert', nextValues.tlsCert === baselineValues.tlsCert);
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'tlsKey')) {
    updateDirty('tlsKey', nextValues.tlsKey === baselineValues.tlsKey);
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'rmAllowRemote')) {
    updateDirty('rmAllowRemote', nextValues.rmAllowRemote === baselineValues.rmAllowRemote);
  }
  if (
    Object.prototype.hasOwnProperty.call(patch, 'rmSecretKey') ||
    Object.prototype.hasOwnProperty.call(patch, 'rmSecretKeyAction')
  ) {
    updateDirty(
      'rmSecretKey',
      nextValues.rmSecretKeyAction === baselineValues.rmSecretKeyAction &&
        nextValues.rmSecretKey === baselineValues.rmSecretKey
    );
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'rmDisableControlPanel')) {
    updateDirty(
      'rmDisableControlPanel',
      nextValues.rmDisableControlPanel === baselineValues.rmDisableControlPanel
    );
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'rmPanelRepo')) {
    updateDirty('rmPanelRepo', nextValues.rmPanelRepo === baselineValues.rmPanelRepo);
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'authDir')) {
    updateDirty('authDir', nextValues.authDir === baselineValues.authDir);
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'apiKeysText')) {
    updateDirty('apiKeysText', nextValues.apiKeysText === baselineValues.apiKeysText);
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'debug')) {
    updateDirty('debug', nextValues.debug === baselineValues.debug);
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'commercialMode')) {
    updateDirty('commercialMode', nextValues.commercialMode === baselineValues.commercialMode);
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'usageStatisticsEnabled')) {
    updateDirty(
      'usageStatisticsEnabled',
      nextValues.usageStatisticsEnabled === baselineValues.usageStatisticsEnabled
    );
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'loggingToFile')) {
    updateDirty('loggingToFile', nextValues.loggingToFile === baselineValues.loggingToFile);
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'logsMaxTotalSizeMb')) {
    updateDirty(
      'logsMaxTotalSizeMb',
      nextValues.logsMaxTotalSizeMb === baselineValues.logsMaxTotalSizeMb
    );
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'redisUsageQueueRetentionSeconds')) {
    updateDirty(
      'redisUsageQueueRetentionSeconds',
      nextValues.redisUsageQueueRetentionSeconds === baselineValues.redisUsageQueueRetentionSeconds
    );
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'proxyUrl')) {
    updateDirty('proxyUrl', nextValues.proxyUrl === baselineValues.proxyUrl);
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'forceModelPrefix')) {
    updateDirty(
      'forceModelPrefix',
      nextValues.forceModelPrefix === baselineValues.forceModelPrefix
    );
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'requestRetry')) {
    updateDirty('requestRetry', nextValues.requestRetry === baselineValues.requestRetry);
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'maxRetryCredentials')) {
    updateDirty(
      'maxRetryCredentials',
      nextValues.maxRetryCredentials === baselineValues.maxRetryCredentials
    );
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'maxRetryInterval')) {
    updateDirty(
      'maxRetryInterval',
      nextValues.maxRetryInterval === baselineValues.maxRetryInterval
    );
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'wsAuth')) {
    updateDirty('wsAuth', nextValues.wsAuth === baselineValues.wsAuth);
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'quotaSwitchProject')) {
    updateDirty(
      'quotaSwitchProject',
      nextValues.quotaSwitchProject === baselineValues.quotaSwitchProject
    );
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'quotaSwitchPreviewModel')) {
    updateDirty(
      'quotaSwitchPreviewModel',
      nextValues.quotaSwitchPreviewModel === baselineValues.quotaSwitchPreviewModel
    );
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'quotaAntigravityCredits')) {
    updateDirty(
      'quotaAntigravityCredits',
      nextValues.quotaAntigravityCredits === baselineValues.quotaAntigravityCredits
    );
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'routingStrategy')) {
    updateDirty('routingStrategy', nextValues.routingStrategy === baselineValues.routingStrategy);
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'routingSessionAffinity')) {
    updateDirty(
      'routingSessionAffinity',
      nextValues.routingSessionAffinity === baselineValues.routingSessionAffinity
    );
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'routingHighCacheMode')) {
    updateDirty(
      'routingHighCacheMode',
      nextValues.routingHighCacheMode === baselineValues.routingHighCacheMode
    );
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'routingSessionAffinityTTL')) {
    updateDirty(
      'routingSessionAffinityTTL',
      nextValues.routingSessionAffinityTTL === baselineValues.routingSessionAffinityTTL
    );
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'payloadDefaultRules')) {
    updateDirty(
      'payloadDefaultRules',
      arePayloadRulesEqual(nextValues.payloadDefaultRules, baselineValues.payloadDefaultRules)
    );
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'payloadDefaultRawRules')) {
    updateDirty(
      'payloadDefaultRawRules',
      arePayloadRulesEqual(nextValues.payloadDefaultRawRules, baselineValues.payloadDefaultRawRules)
    );
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'payloadOverrideRules')) {
    updateDirty(
      'payloadOverrideRules',
      arePayloadRulesEqual(nextValues.payloadOverrideRules, baselineValues.payloadOverrideRules)
    );
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'payloadOverrideRawRules')) {
    updateDirty(
      'payloadOverrideRawRules',
      arePayloadRulesEqual(
        nextValues.payloadOverrideRawRules,
        baselineValues.payloadOverrideRawRules
      )
    );
  }
  if (Object.prototype.hasOwnProperty.call(patch, 'payloadFilterRules')) {
    updateDirty(
      'payloadFilterRules',
      arePayloadFilterRulesEqual(nextValues.payloadFilterRules, baselineValues.payloadFilterRules)
    );
  }
  if (patch.streaming) {
    const streamingPatch = patch.streaming;
    if (Object.prototype.hasOwnProperty.call(streamingPatch, 'keepaliveSeconds')) {
      updateDirty(
        'streaming.keepaliveSeconds',
        nextValues.streaming.keepaliveSeconds === baselineValues.streaming.keepaliveSeconds
      );
    }
    if (Object.prototype.hasOwnProperty.call(streamingPatch, 'bootstrapRetries')) {
      updateDirty(
        'streaming.bootstrapRetries',
        nextValues.streaming.bootstrapRetries === baselineValues.streaming.bootstrapRetries
      );
    }
    if (Object.prototype.hasOwnProperty.call(streamingPatch, 'nonstreamKeepaliveInterval')) {
      updateDirty(
        'streaming.nonstreamKeepaliveInterval',
        nextValues.streaming.nonstreamKeepaliveInterval ===
          baselineValues.streaming.nonstreamKeepaliveInterval
      );
    }
  }

  return nextDirtyFields;
}

function visualConfigReducer(
  state: VisualConfigState,
  action: VisualConfigAction
): VisualConfigState {
  switch (action.type) {
    case 'load_success':
      return {
        visualValues: action.values,
        baselineValues: deepClone(action.values),
        dirtyFields: new Set(),
        visualParseError: null,
      };
    case 'load_error':
      return {
        ...state,
        visualParseError: action.error,
      };
    case 'set_values': {
      const nextValues = mergeVisualConfigValues(state.visualValues, action.values);
      const nextDirtyFields = getNextDirtyFields(
        state.dirtyFields,
        action.values,
        nextValues,
        state.baselineValues
      );

      return {
        ...state,
        visualValues: nextValues,
        dirtyFields: nextDirtyFields,
      };
    }
    default:
      return state;
  }
}

export function useVisualConfig() {
  const [state, dispatch] = useReducer(
    visualConfigReducer,
    undefined,
    createInitialVisualConfigState
  );
  const { visualValues, visualParseError, dirtyFields } = state;
  const visualDirty = dirtyFields.size > 0;
  const visualValidationErrors = useMemo(
    () => getVisualConfigValidationErrors(visualValues),
    [visualValues]
  );
  const visualHasPayloadValidationErrors = useMemo(
    () =>
      hasPayloadParamValidationErrors(visualValues.payloadDefaultRules) ||
      hasPayloadParamValidationErrors(visualValues.payloadDefaultRawRules) ||
      hasPayloadParamValidationErrors(visualValues.payloadOverrideRules) ||
      hasPayloadParamValidationErrors(visualValues.payloadOverrideRawRules),
    [
      visualValues.payloadDefaultRules,
      visualValues.payloadDefaultRawRules,
      visualValues.payloadOverrideRules,
      visualValues.payloadOverrideRawRules,
    ]
  );

  const loadVisualValuesFromYaml = useCallback((yamlContent: string) => {
    try {
      const document = parseDocument(yamlContent);
      if (document.errors.length > 0) {
        throw new Error(document.errors[0]?.message ?? 'Invalid YAML');
      }

      const parsedRaw: unknown = parseYaml(yamlContent) || {};
      const parsed = asRecord(parsedRaw) ?? {};
      const tls = asRecord(parsed.tls);
      const remoteManagement = asRecord(parsed['remote-management']);
      const pprof = asRecord(parsed.pprof);
      const quotaExceeded = asRecord(parsed['quota-exceeded']);
      const routing = asRecord(parsed.routing);
      const plugins = asRecord(parsed.plugins);
      const payload = asRecord(parsed.payload);
      const streaming = asRecord(parsed.streaming);
      const claudeHeaderDefaults = asRecord(parsed['claude-header-defaults']);
      const codexHeaderDefaults = asRecord(parsed['codex-header-defaults']);
      const codex = asRecord(parsed.codex);
      const codexClientRestriction = asRecord(
        codex?.['client-restriction'] ?? codex?.clientRestriction
      );
      const codexCacheAffinity = asRecord(codex?.['cache-affinity'] ?? codex?.cacheAffinity);
      const codexTailBurst = asRecord(codex?.['tail-burst'] ?? codex?.tailBurst);
      const codexTailBurstCollector = asRecord(
        codexTailBurst?.['quota-collector'] ?? codexTailBurst?.quotaCollector
      );
      const codexTailBurstToolInjection = asRecord(
        codexTailBurst?.['tool-injection'] ?? codexTailBurst?.toolInjection
      );

      const newValues: VisualConfigValues = {
        host: typeof parsed.host === 'string' ? parsed.host : '',
        port: String(parsed.port ?? ''),

        tlsEnable: Boolean(tls?.enable),
        tlsCert: typeof tls?.cert === 'string' ? tls.cert : '',
        tlsKey: typeof tls?.key === 'string' ? tls.key : '',

        rmAllowRemote: Boolean(remoteManagement?.['allow-remote']),
        rmSecretKey: '',
        rmSecretKeyAction: 'unchanged',
        rmSecretKeyConfigured:
          typeof remoteManagement?.['secret-key'] === 'string' &&
          remoteManagement['secret-key'].length > 0,
        rmDisableControlPanel: Boolean(remoteManagement?.['disable-control-panel']),
        rmDisableAutoUpdatePanel: remoteManagement?.['disable-auto-update-panel'] !== false,
        rmPanelRepo:
          typeof remoteManagement?.['panel-github-repository'] === 'string'
            ? remoteManagement['panel-github-repository']
            : typeof remoteManagement?.['panel-repo'] === 'string'
              ? remoteManagement['panel-repo']
              : '',

        authDir: typeof parsed['auth-dir'] === 'string' ? parsed['auth-dir'] : '',
        apiKeysText: resolveApiKeysText(parsed),
        pluginsEnabled: Boolean(plugins?.enabled),
        pluginsDir: typeof plugins?.dir === 'string' ? plugins.dir : '',
        pluginStoreSourcesText: parseStringArrayText(
          plugins?.['store-sources'] ?? plugins?.storeSources
        ),
        pluginStoreAuth: parsePluginStoreAuthRules(plugins?.['store-auth'] ?? plugins?.storeAuth),

        debug: Boolean(parsed.debug),
        pprofEnable: Boolean(pprof?.enable),
        pprofAddr: typeof pprof?.addr === 'string' ? pprof.addr : '127.0.0.1:8316',
        commercialMode: Boolean(parsed['commercial-mode']),
        usageStatisticsEnabled: Boolean(
          parsed['usage-statistics-enabled'] ?? parsed.usageStatisticsEnabled
        ),
        loggingToFile: Boolean(parsed['logging-to-file']),
        logsMaxTotalSizeMb: String(parsed['logs-max-total-size-mb'] ?? ''),
        errorLogsMaxFiles: String(parsed['error-logs-max-files'] ?? ''),
        redisUsageQueueRetentionSeconds: String(
          parsed['redis-usage-queue-retention-seconds'] ??
            parsed.redisUsageQueueRetentionSeconds ??
            ''
        ),

        proxyUrl: typeof parsed['proxy-url'] === 'string' ? parsed['proxy-url'] : '',
        forceModelPrefix: Boolean(parsed['force-model-prefix']),
        passthroughHeaders: Boolean(parsed['passthrough-headers']),
        requestRetry: String(parsed['request-retry'] ?? ''),
        maxRetryCredentials: String(parsed['max-retry-credentials'] ?? ''),
        maxRetryInterval: String(parsed['max-retry-interval'] ?? ''),
        disableCooling: Boolean(parsed['disable-cooling']),
        saveCooldownStatus: Boolean(parsed['save-cooldown-status']),
        transientErrorCooldownSeconds: String(parsed['transient-error-cooldown-seconds'] ?? ''),
        disableClaudeCloakMode: Boolean(parsed['disable-claude-cloak-mode']),
        disableImageGeneration: parseDisableImageGenerationMode(parsed['disable-image-generation']),
        gptImage2BaseModel:
          typeof parsed['gpt-image-2-base-model'] === 'string'
            ? parsed['gpt-image-2-base-model']
            : '',
        videoResultAuthCacheTtl:
          typeof parsed['video-result-auth-cache-ttl'] === 'string'
            ? parsed['video-result-auth-cache-ttl']
            : '',
        authAutoRefreshWorkers: String(parsed['auth-auto-refresh-workers'] ?? ''),
        wsAuth: Boolean(parsed['ws-auth'] ?? true),
        antigravitySignatureCacheEnabled: Boolean(
          parsed['antigravity-signature-cache-enabled'] ?? true
        ),
        antigravitySignatureBypassStrict: Boolean(parsed['antigravity-signature-bypass-strict']),
        claudeHeaderUserAgent:
          typeof claudeHeaderDefaults?.['user-agent'] === 'string'
            ? claudeHeaderDefaults['user-agent']
            : '',
        claudeHeaderPackageVersion:
          typeof claudeHeaderDefaults?.['package-version'] === 'string'
            ? claudeHeaderDefaults['package-version']
            : '',
        claudeHeaderRuntimeVersion:
          typeof claudeHeaderDefaults?.['runtime-version'] === 'string'
            ? claudeHeaderDefaults['runtime-version']
            : '',
        claudeHeaderOs: typeof claudeHeaderDefaults?.os === 'string' ? claudeHeaderDefaults.os : '',
        claudeHeaderArch:
          typeof claudeHeaderDefaults?.arch === 'string' ? claudeHeaderDefaults.arch : '',
        claudeHeaderTimeout:
          typeof claudeHeaderDefaults?.timeout === 'string' ? claudeHeaderDefaults.timeout : '',
        claudeHeaderStabilizeDeviceProfile: Boolean(
          claudeHeaderDefaults?.['stabilize-device-profile']
        ),
        codexHeaderUserAgent:
          typeof codexHeaderDefaults?.['user-agent'] === 'string'
            ? codexHeaderDefaults['user-agent']
            : '',
        codexHeaderBetaFeatures:
          typeof codexHeaderDefaults?.['beta-features'] === 'string'
            ? codexHeaderDefaults['beta-features']
            : '',
        codexIdentityConfuse: Boolean(codex?.['identity-confuse'] ?? codex?.identityConfuse),
        codexClientForceAllow: Boolean(
          codexClientRestriction?.['force-codex-cli'] ?? codexClientRestriction?.forceCodexCli
        ),
        codexClientMinVersion:
          typeof codexClientRestriction?.['min-codex-version'] === 'string'
            ? codexClientRestriction['min-codex-version']
            : typeof codexClientRestriction?.minCodexVersion === 'string'
              ? codexClientRestriction.minCodexVersion
              : '',
        codexClientMaxVersion:
          typeof codexClientRestriction?.['max-codex-version'] === 'string'
            ? codexClientRestriction['max-codex-version']
            : typeof codexClientRestriction?.maxCodexVersion === 'string'
              ? codexClientRestriction.maxCodexVersion
              : '',
        codexClientAllowAppServer: Boolean(
          codexClientRestriction?.['allow-app-server-clients'] ??
          codexClientRestriction?.allowAppServerClients
        ),
        codexClientWhitelist: parseCodexClientRestrictionEntries(codexClientRestriction?.whitelist),
        codexClientBlacklist: parseCodexClientRestrictionEntries(codexClientRestriction?.blacklist),
        codexClientFingerprintSignals:
          parseCodexEngineFingerprintSignals(
            codexClientRestriction?.['engine-fingerprint-signals'] ??
              codexClientRestriction?.engineFingerprintSignals
          ) ?? deepClone(DEFAULT_VISUAL_VALUES.codexClientFingerprintSignals),
        codexCacheAffinityEnabled: Boolean(codexCacheAffinity?.enabled),
        codexCacheAffinityShadow: Boolean(codexCacheAffinity?.shadow),
        codexCacheAffinityMaxConcurrency: String(
          codexCacheAffinity?.['max-concurrency'] ??
            codexCacheAffinity?.maxConcurrency ??
            8
        ),
        codexCacheAffinityMaxEntries: String(
          codexCacheAffinity?.['max-entries'] ?? codexCacheAffinity?.maxEntries ?? 65536
        ),
        codexCacheAffinityMaxRetryCredentials: String(
          codexCacheAffinity?.['max-retry-credentials'] ??
            codexCacheAffinity?.maxRetryCredentials ??
            2
        ),
        codexCacheAffinityWebsocketPoolSlots: String(
          codexCacheAffinity?.['websocket-pool-slots'] ??
            codexCacheAffinity?.websocketPoolSlots ??
            8
        ),
        codexCacheAffinityMaxSessionRequests: String(
          codexCacheAffinity?.['max-session-requests'] ??
            codexCacheAffinity?.maxSessionRequests ??
            50
        ),
        codexCacheAffinityMaxSessionDuration: readTailBurstDuration(
          codexCacheAffinity?.['max-session-duration'] ?? codexCacheAffinity?.maxSessionDuration,
          '5m'
        ),
        codexCacheAffinityMaxSharePercent: readNonNegativeRatioPercent(
          codexCacheAffinity?.['max-share-ratio'] ??
            codexCacheAffinity?.maxShareRatio ??
            codexCacheAffinity?.max_share_ratio,
          '0'
        ),
        codexCacheAffinityQuotaPreemptPercent: readRatioPercent(
          codexCacheAffinity?.['quota-preempt-used-ratio'] ??
            codexCacheAffinity?.quotaPreemptUsedRatio,
          '97',
          false
        ),
        codexCacheAffinityQuotaHardStopPercent: readRatioPercent(
          codexCacheAffinity?.['quota-hard-stop-used-ratio'] ??
            codexCacheAffinity?.quotaHardStopUsedRatio,
          '99',
          true
        ),
        codexTailBurstEnabled: Boolean(codexTailBurst?.enabled),
        codexTailBurstTriggerRemainingPercent: readTailBurstRemainingPercent(
          codexTailBurst?.['trigger-remaining-ratio'] ?? codexTailBurst?.triggerRemainingRatio,
          codexTailBurst?.['trigger-used-ratio'] ?? codexTailBurst?.triggerUsedRatio
        ),
        codexTailBurstSnapshotTtl: readTailBurstDuration(
          codexTailBurst?.['snapshot-ttl'] ?? codexTailBurst?.snapshotTTL,
          '90s'
        ),
        codexTailBurstExpiryWindow: readTailBurstDuration(
          codexTailBurst?.['expiry-window'] ?? codexTailBurst?.expiryWindow,
          '10m'
        ),
        codexTailBurstMaxConcurrency: String(
          codexTailBurst?.['max-concurrency'] ?? codexTailBurst?.maxConcurrency ?? 32
        ),
        codexTailBurstCollectorInterval: readTailBurstDuration(
          codexTailBurstCollector?.interval,
          '45s'
        ),
        codexTailBurstCollectorMaxConcurrency: String(
          codexTailBurstCollector?.['max-concurrency'] ??
            codexTailBurstCollector?.maxConcurrency ??
            4
        ),
        codexTailBurstCollectorTimeout: readTailBurstDuration(
          codexTailBurstCollector?.timeout,
          '8s'
        ),
        codexTailBurstToolInjectionEnabled: Boolean(codexTailBurstToolInjection?.enabled),

        quotaSwitchProject: Boolean(quotaExceeded?.['switch-project'] ?? false),
        quotaSwitchPreviewModel: Boolean(quotaExceeded?.['switch-preview-model'] ?? false),
        quotaAntigravityCredits: Boolean(quotaExceeded?.['antigravity-credits'] ?? false),

        routingStrategy:
          routing?.strategy === 'fill-first'
            ? 'fill-first'
            : routing?.strategy === 'round-robin'
              ? 'round-robin'
              : 'concurrency-balanced',
        routingSessionAffinity: Boolean(
          routing?.['session-affinity'] ?? routing?.sessionAffinity ?? routing?.['sessionAffinity']
        ),
        routingHighCacheMode: Boolean(
          routing?.['high-cache-mode'] ?? routing?.highCacheMode ?? routing?.['highCacheMode']
        ),
        routingSessionAffinityTTL:
          typeof routing?.['session-affinity-ttl'] === 'string'
            ? routing['session-affinity-ttl']
            : typeof routing?.sessionAffinityTTL === 'string'
              ? routing.sessionAffinityTTL
              : typeof routing?.['sessionAffinityTTL'] === 'string'
                ? routing['sessionAffinityTTL']
                : '',

        payloadDefaultRules: parsePayloadRules(payload?.default),
        payloadDefaultRawRules: parseRawPayloadRules(payload?.['default-raw']),
        payloadOverrideRules: parsePayloadRules(payload?.override),
        payloadOverrideRawRules: parseRawPayloadRules(payload?.['override-raw']),
        payloadFilterRules: parsePayloadFilterRules(payload?.filter),

        streaming: {
          keepaliveSeconds: String(streaming?.['keepalive-seconds'] ?? ''),
          bootstrapRetries: String(streaming?.['bootstrap-retries'] ?? ''),
          nonstreamKeepaliveInterval: String(parsed['nonstream-keepalive-interval'] ?? ''),
        },
      };

      dispatch({ type: 'load_success', values: newValues });
      return { ok: true as const };
    } catch (error: unknown) {
      const message = error instanceof Error ? error.message : 'Invalid YAML';
      dispatch({ type: 'load_error', error: message });
      return { ok: false as const, error: message };
    }
  }, []);

  const applyVisualChangesToYaml = useCallback(
    (currentYaml: string): string => {
      try {
        const doc = parseDocument(currentYaml);
        if (doc.errors.length > 0) return currentYaml;
        if (!isMap(doc.contents)) {
          doc.contents = doc.createNode({}) as unknown as typeof doc.contents;
        }
        const values = visualValues;
        const isDirty = (key: string) => dirtyFields.has(key);

        [
          ['codex', 'tail-burst', 'normal-max-concurrency'],
          ['codex', 'tail-burst', 'normalMaxConcurrency'],
          ['codex', 'tailBurst', 'normal-max-concurrency'],
          ['codex', 'tailBurst', 'normalMaxConcurrency'],
        ].forEach((path) => {
          if (docHas(doc, path)) doc.deleteIn(path);
        });
        deleteIfMapEmpty(doc, ['codex', 'tail-burst']);
        deleteIfMapEmpty(doc, ['codex', 'tailBurst']);
        deleteIfMapEmpty(doc, ['codex']);

        if (isDirty('host')) setStringInDoc(doc, ['host'], values.host);
        if (isDirty('port')) setIntFromStringInDoc(doc, ['port'], values.port);

        const tlsDirty = isDirty('tlsEnable') || isDirty('tlsCert') || isDirty('tlsKey');
        if (tlsDirty) {
          ensureMapInDoc(doc, ['tls']);
          if (isDirty('tlsEnable')) setBooleanInDoc(doc, ['tls', 'enable'], values.tlsEnable);
          if (isDirty('tlsCert')) setStringInDoc(doc, ['tls', 'cert'], values.tlsCert);
          if (isDirty('tlsKey')) setStringInDoc(doc, ['tls', 'key'], values.tlsKey);
          deleteIfMapEmpty(doc, ['tls']);
        }

        const hasRemoteManagementSecretKeyUpdate =
          isDirty('rmSecretKey') &&
          (values.rmSecretKeyAction === 'clear' ||
            (values.rmSecretKeyAction === 'replace' && values.rmSecretKey.length > 0));
        const remoteManagementDirty =
          isDirty('rmAllowRemote') ||
          isDirty('rmSecretKey') ||
          isDirty('rmDisableControlPanel') ||
          isDirty('rmDisableAutoUpdatePanel') ||
          isDirty('rmPanelRepo');
        if (remoteManagementDirty) {
          ensureMapInDoc(doc, ['remote-management']);
          if (isDirty('rmAllowRemote')) {
            setBooleanInDoc(doc, ['remote-management', 'allow-remote'], values.rmAllowRemote);
          }
          if (
            hasRemoteManagementSecretKeyUpdate &&
            values.rmSecretKeyAction === 'replace' &&
            values.rmSecretKey.length > 0
          ) {
            doc.setIn(['remote-management', 'secret-key'], values.rmSecretKey);
          } else if (hasRemoteManagementSecretKeyUpdate && values.rmSecretKeyAction === 'clear') {
            doc.setIn(['remote-management', 'secret-key'], '');
          }
          if (isDirty('rmDisableControlPanel')) {
            setBooleanInDoc(
              doc,
              ['remote-management', 'disable-control-panel'],
              values.rmDisableControlPanel
            );
          }
          if (isDirty('rmDisableAutoUpdatePanel')) {
            setBooleanInDoc(
              doc,
              ['remote-management', 'disable-auto-update-panel'],
              values.rmDisableAutoUpdatePanel
            );
          }
          if (isDirty('rmPanelRepo')) {
            setStringInDoc(
              doc,
              ['remote-management', 'panel-github-repository'],
              values.rmPanelRepo
            );
            if (docHas(doc, ['remote-management', 'panel-repo'])) {
              doc.deleteIn(['remote-management', 'panel-repo']);
            }
          }
          deleteIfMapEmpty(doc, ['remote-management']);
        }

        if (isDirty('authDir')) setStringInDoc(doc, ['auth-dir'], values.authDir);
        if (isDirty('apiKeysText')) {
          const apiKeys = values.apiKeysText
            .split('\n')
            .map((key) => key.trim())
            .filter(Boolean);
          if (apiKeys.length > 0) {
            doc.setIn(['api-keys'], apiKeys);
          } else if (docHas(doc, ['api-keys'])) {
            doc.deleteIn(['api-keys']);
          }
          deleteLegacyApiKeysProvider(doc);
        }

        if (isDirty('debug')) setBooleanInDoc(doc, ['debug'], values.debug);

        const shouldWritePprofEnable = isDirty('pprofEnable');
        const shouldWritePprofAddr = isDirty('pprofAddr');
        if (shouldWritePprofEnable || shouldWritePprofAddr) {
          ensureMapInDoc(doc, ['pprof']);
          if (shouldWritePprofEnable) doc.setIn(['pprof', 'enable'], values.pprofEnable);
          if (shouldWritePprofAddr) setStringInDoc(doc, ['pprof', 'addr'], values.pprofAddr);
          deleteIfMapEmpty(doc, ['pprof']);
        }

        if (isDirty('commercialMode')) {
          setBooleanInDoc(doc, ['commercial-mode'], values.commercialMode);
        }
        if (isDirty('usageStatisticsEnabled')) {
          setBooleanInDoc(doc, ['usage-statistics-enabled'], values.usageStatisticsEnabled);
        }
        if (isDirty('loggingToFile')) {
          setBooleanInDoc(doc, ['logging-to-file'], values.loggingToFile);
        }
        if (isDirty('logsMaxTotalSizeMb')) {
          setIntFromStringInDoc(doc, ['logs-max-total-size-mb'], values.logsMaxTotalSizeMb);
        }
        if (isDirty('errorLogsMaxFiles')) {
          setIntFromStringInDoc(doc, ['error-logs-max-files'], values.errorLogsMaxFiles);
        }
        if (isDirty('redisUsageQueueRetentionSeconds')) {
          setIntFromStringInDoc(
            doc,
            ['redis-usage-queue-retention-seconds'],
            values.redisUsageQueueRetentionSeconds
          );
        }

        const pluginStoreSources = values.pluginStoreSourcesText
          .split('\n')
          .map((source) => source.trim())
          .filter(Boolean);
        const shouldWritePluginStoreAuth = isDirty('pluginStoreAuth');
        const shouldWritePluginsEnabled = isDirty('pluginsEnabled');
        const shouldWritePluginsDir = isDirty('pluginsDir');
        const shouldWritePluginStoreSources = isDirty('pluginStoreSourcesText');
        if (
          shouldWritePluginsEnabled ||
          shouldWritePluginsDir ||
          shouldWritePluginStoreSources ||
          shouldWritePluginStoreAuth
        ) {
          ensureMapInDoc(doc, ['plugins']);
          if (shouldWritePluginsEnabled) {
            doc.setIn(['plugins', 'enabled'], values.pluginsEnabled);
          }
          if (shouldWritePluginsDir) {
            if (values.pluginsDir.trim()) {
              doc.setIn(['plugins', 'dir'], values.pluginsDir);
            } else if (docHas(doc, ['plugins', 'dir'])) {
              doc.deleteIn(['plugins', 'dir']);
            }
          }
          if (shouldWritePluginStoreSources) {
            if (pluginStoreSources.length > 0) {
              doc.setIn(['plugins', 'store-sources'], pluginStoreSources);
            } else if (docHas(doc, ['plugins', 'store-sources'])) {
              doc.deleteIn(['plugins', 'store-sources']);
            }
          }
          if (shouldWritePluginStoreAuth) {
            const storeAuth = serializePluginStoreAuthForYaml(values.pluginStoreAuth);
            if (storeAuth.length > 0) {
              doc.setIn(['plugins', 'store-auth'], storeAuth);
            } else if (docHas(doc, ['plugins', 'store-auth'])) {
              doc.deleteIn(['plugins', 'store-auth']);
            }
          }
          deleteIfMapEmpty(doc, ['plugins']);
        }

        if (isDirty('proxyUrl')) setStringInDoc(doc, ['proxy-url'], values.proxyUrl);
        if (isDirty('forceModelPrefix')) {
          setBooleanInDoc(doc, ['force-model-prefix'], values.forceModelPrefix);
        }
        if (isDirty('passthroughHeaders')) {
          setBooleanInDoc(doc, ['passthrough-headers'], values.passthroughHeaders);
        }
        if (isDirty('requestRetry'))
          setIntFromStringInDoc(doc, ['request-retry'], values.requestRetry);
        if (isDirty('maxRetryCredentials')) {
          setIntFromStringInDoc(doc, ['max-retry-credentials'], values.maxRetryCredentials);
        }
        if (isDirty('maxRetryInterval')) {
          setIntFromStringInDoc(doc, ['max-retry-interval'], values.maxRetryInterval);
        }
        if (isDirty('disableCooling'))
          setBooleanInDoc(doc, ['disable-cooling'], values.disableCooling);
        if (isDirty('saveCooldownStatus')) {
          setBooleanInDoc(doc, ['save-cooldown-status'], values.saveCooldownStatus);
        }
        if (isDirty('transientErrorCooldownSeconds')) {
          setIntFromStringInDoc(
            doc,
            ['transient-error-cooldown-seconds'],
            values.transientErrorCooldownSeconds
          );
        }
        if (isDirty('disableClaudeCloakMode')) {
          setBooleanInDoc(doc, ['disable-claude-cloak-mode'], values.disableClaudeCloakMode);
        }
        if (isDirty('disableImageGeneration')) {
          setDisableImageGenerationInDoc(
            doc,
            ['disable-image-generation'],
            values.disableImageGeneration
          );
        }
        if (isDirty('gptImage2BaseModel')) {
          setStringInDoc(doc, ['gpt-image-2-base-model'], values.gptImage2BaseModel);
        }
        if (isDirty('videoResultAuthCacheTtl')) {
          setStringInDoc(doc, ['video-result-auth-cache-ttl'], values.videoResultAuthCacheTtl);
        }
        if (isDirty('authAutoRefreshWorkers')) {
          setIntFromStringInDoc(doc, ['auth-auto-refresh-workers'], values.authAutoRefreshWorkers);
        }
        if (isDirty('wsAuth')) {
          doc.setIn(['ws-auth'], values.wsAuth);
        }
        if (isDirty('antigravitySignatureCacheEnabled')) {
          doc.setIn(
            ['antigravity-signature-cache-enabled'],
            values.antigravitySignatureCacheEnabled
          );
        }
        if (isDirty('antigravitySignatureBypassStrict')) {
          setBooleanInDoc(
            doc,
            ['antigravity-signature-bypass-strict'],
            values.antigravitySignatureBypassStrict
          );
        }

        const claudeHeadersDirty =
          isDirty('claudeHeaderUserAgent') ||
          isDirty('claudeHeaderPackageVersion') ||
          isDirty('claudeHeaderRuntimeVersion') ||
          isDirty('claudeHeaderOs') ||
          isDirty('claudeHeaderArch') ||
          isDirty('claudeHeaderTimeout') ||
          isDirty('claudeHeaderStabilizeDeviceProfile');
        if (claudeHeadersDirty) {
          ensureMapInDoc(doc, ['claude-header-defaults']);
          if (isDirty('claudeHeaderUserAgent')) {
            setStringInDoc(
              doc,
              ['claude-header-defaults', 'user-agent'],
              values.claudeHeaderUserAgent
            );
          }
          if (isDirty('claudeHeaderPackageVersion')) {
            setStringInDoc(
              doc,
              ['claude-header-defaults', 'package-version'],
              values.claudeHeaderPackageVersion
            );
          }
          if (isDirty('claudeHeaderRuntimeVersion')) {
            setStringInDoc(
              doc,
              ['claude-header-defaults', 'runtime-version'],
              values.claudeHeaderRuntimeVersion
            );
          }
          if (isDirty('claudeHeaderOs')) {
            setStringInDoc(doc, ['claude-header-defaults', 'os'], values.claudeHeaderOs);
          }
          if (isDirty('claudeHeaderArch')) {
            setStringInDoc(doc, ['claude-header-defaults', 'arch'], values.claudeHeaderArch);
          }
          if (isDirty('claudeHeaderTimeout')) {
            setStringInDoc(doc, ['claude-header-defaults', 'timeout'], values.claudeHeaderTimeout);
          }
          if (isDirty('claudeHeaderStabilizeDeviceProfile')) {
            setBooleanInDoc(
              doc,
              ['claude-header-defaults', 'stabilize-device-profile'],
              values.claudeHeaderStabilizeDeviceProfile
            );
          }
          deleteIfMapEmpty(doc, ['claude-header-defaults']);
        }

        const codexHeadersDirty =
          isDirty('codexHeaderUserAgent') || isDirty('codexHeaderBetaFeatures');
        if (codexHeadersDirty) {
          ensureMapInDoc(doc, ['codex-header-defaults']);
          if (isDirty('codexHeaderUserAgent')) {
            setStringInDoc(
              doc,
              ['codex-header-defaults', 'user-agent'],
              values.codexHeaderUserAgent
            );
          }
          if (isDirty('codexHeaderBetaFeatures')) {
            setStringInDoc(
              doc,
              ['codex-header-defaults', 'beta-features'],
              values.codexHeaderBetaFeatures
            );
          }
          deleteIfMapEmpty(doc, ['codex-header-defaults']);
        }

        const codexIdentityConfusePath = ['codex', 'identity-confuse'];
        const codexIdentityConfuseLegacyPath = ['codex', 'identityConfuse'];
        if (isDirty('codexIdentityConfuse')) {
          ensureMapInDoc(doc, ['codex']);
          doc.setIn(codexIdentityConfusePath, values.codexIdentityConfuse);
          if (docHas(doc, codexIdentityConfuseLegacyPath)) {
            doc.deleteIn(codexIdentityConfuseLegacyPath);
          }
          deleteIfMapEmpty(doc, ['codex']);
        }

        const codexClientRestrictionDirty =
          isDirty('codexClientForceAllow') ||
          isDirty('codexClientMinVersion') ||
          isDirty('codexClientMaxVersion') ||
          isDirty('codexClientAllowAppServer') ||
          isDirty('codexClientWhitelist') ||
          isDirty('codexClientBlacklist') ||
          isDirty('codexClientFingerprintSignals');
        if (codexClientRestrictionDirty) {
          ensureMapInDoc(doc, ['codex']);
          migrateLegacyCodexClientRestriction(doc);
          ensureMapInDoc(doc, ['codex', 'client-restriction']);
          if (isDirty('codexClientForceAllow')) {
            doc.setIn(
              ['codex', 'client-restriction', 'force-codex-cli'],
              values.codexClientForceAllow
            );
            if (docHas(doc, ['codex', 'client-restriction', 'forceCodexCli'])) {
              doc.deleteIn(['codex', 'client-restriction', 'forceCodexCli']);
            }
          }
          if (isDirty('codexClientMinVersion')) {
            doc.setIn(
              ['codex', 'client-restriction', 'min-codex-version'],
              values.codexClientMinVersion.trim()
            );
            if (docHas(doc, ['codex', 'client-restriction', 'minCodexVersion'])) {
              doc.deleteIn(['codex', 'client-restriction', 'minCodexVersion']);
            }
          }
          if (isDirty('codexClientMaxVersion')) {
            doc.setIn(
              ['codex', 'client-restriction', 'max-codex-version'],
              values.codexClientMaxVersion.trim()
            );
            if (docHas(doc, ['codex', 'client-restriction', 'maxCodexVersion'])) {
              doc.deleteIn(['codex', 'client-restriction', 'maxCodexVersion']);
            }
          }
          if (isDirty('codexClientAllowAppServer')) {
            doc.setIn(
              ['codex', 'client-restriction', 'allow-app-server-clients'],
              values.codexClientAllowAppServer
            );
            if (docHas(doc, ['codex', 'client-restriction', 'allowAppServerClients'])) {
              doc.deleteIn(['codex', 'client-restriction', 'allowAppServerClients']);
            }
          }
          if (isDirty('codexClientWhitelist')) {
            doc.setIn(
              ['codex', 'client-restriction', 'whitelist'],
              serializeCodexClientRestrictionEntries(values.codexClientWhitelist, true)
            );
          }
          if (isDirty('codexClientBlacklist')) {
            doc.setIn(
              ['codex', 'client-restriction', 'blacklist'],
              serializeCodexClientRestrictionEntries(values.codexClientBlacklist, false)
            );
          }
          if (isDirty('codexClientFingerprintSignals')) {
            doc.setIn(
              ['codex', 'client-restriction', 'engine-fingerprint-signals'],
              serializeCodexEngineFingerprintSignals(values.codexClientFingerprintSignals)
            );
            if (docHas(doc, ['codex', 'client-restriction', 'engineFingerprintSignals'])) {
              doc.deleteIn(['codex', 'client-restriction', 'engineFingerprintSignals']);
            }
          }
        }

        const codexCacheAffinityDirty =
          isDirty('codexCacheAffinityEnabled') ||
          isDirty('codexCacheAffinityShadow') ||
          isDirty('codexCacheAffinityMaxConcurrency') ||
          isDirty('codexCacheAffinityMaxEntries') ||
          isDirty('codexCacheAffinityMaxRetryCredentials') ||
          isDirty('codexCacheAffinityWebsocketPoolSlots') ||
          isDirty('codexCacheAffinityMaxSessionRequests') ||
          isDirty('codexCacheAffinityMaxSessionDuration') ||
          isDirty('codexCacheAffinityMaxSharePercent') ||
          isDirty('codexCacheAffinityQuotaPreemptPercent') ||
          isDirty('codexCacheAffinityQuotaHardStopPercent');
        if (codexCacheAffinityDirty) {
          ensureMapInDoc(doc, ['codex']);
          migrateLegacyCodexCacheAffinity(doc);
          ensureMapInDoc(doc, ['codex', 'cache-affinity']);
          if (isDirty('codexCacheAffinityEnabled')) {
            doc.setIn(['codex', 'cache-affinity', 'enabled'], values.codexCacheAffinityEnabled);
          }
          if (isDirty('codexCacheAffinityShadow')) {
            doc.setIn(['codex', 'cache-affinity', 'shadow'], values.codexCacheAffinityShadow);
          }
          if (isDirty('codexCacheAffinityMaxConcurrency')) {
            setIntFromStringInDoc(
              doc,
              ['codex', 'cache-affinity', 'max-concurrency'],
              values.codexCacheAffinityMaxConcurrency
            );
          }
          if (isDirty('codexCacheAffinityMaxEntries')) {
            setIntFromStringInDoc(
              doc,
              ['codex', 'cache-affinity', 'max-entries'],
              values.codexCacheAffinityMaxEntries
            );
          }
          if (isDirty('codexCacheAffinityMaxRetryCredentials')) {
            setIntFromStringInDoc(
              doc,
              ['codex', 'cache-affinity', 'max-retry-credentials'],
              values.codexCacheAffinityMaxRetryCredentials
            );
          }
          if (isDirty('codexCacheAffinityWebsocketPoolSlots')) {
            setIntFromStringInDoc(
              doc,
              ['codex', 'cache-affinity', 'websocket-pool-slots'],
              values.codexCacheAffinityWebsocketPoolSlots
            );
          }
          if (isDirty('codexCacheAffinityMaxSessionRequests')) {
            setIntFromStringInDoc(
              doc,
              ['codex', 'cache-affinity', 'max-session-requests'],
              values.codexCacheAffinityMaxSessionRequests
            );
          }
          if (isDirty('codexCacheAffinityMaxSessionDuration')) {
            setStringInDoc(
              doc,
              ['codex', 'cache-affinity', 'max-session-duration'],
              values.codexCacheAffinityMaxSessionDuration
            );
          }
          if (isDirty('codexCacheAffinityMaxSharePercent')) {
            setNonNegativeRatioPercentInDoc(
              doc,
              ['codex', 'cache-affinity', 'max-share-ratio'],
              values.codexCacheAffinityMaxSharePercent
            );
          }
          if (isDirty('codexCacheAffinityQuotaPreemptPercent')) {
            setRatioPercentInDoc(
              doc,
              ['codex', 'cache-affinity', 'quota-preempt-used-ratio'],
              values.codexCacheAffinityQuotaPreemptPercent
            );
          }
          if (isDirty('codexCacheAffinityQuotaHardStopPercent')) {
            setRatioPercentInDoc(
              doc,
              ['codex', 'cache-affinity', 'quota-hard-stop-used-ratio'],
              values.codexCacheAffinityQuotaHardStopPercent
            );
          }
          deleteIfMapEmpty(doc, ['codex', 'cache-affinity']);
          deleteIfMapEmpty(doc, ['codex']);
        }

        const codexTailBurstDirty =
          isDirty('codexTailBurstEnabled') ||
          isDirty('codexTailBurstTriggerRemainingPercent') ||
          isDirty('codexTailBurstSnapshotTtl') ||
          isDirty('codexTailBurstExpiryWindow') ||
          isDirty('codexTailBurstMaxConcurrency') ||
          isDirty('codexTailBurstCollectorInterval') ||
          isDirty('codexTailBurstCollectorMaxConcurrency') ||
          isDirty('codexTailBurstCollectorTimeout') ||
          isDirty('codexTailBurstToolInjectionEnabled');
        if (codexTailBurstDirty) {
          ensureMapInDoc(doc, ['codex']);
          ensureMapInDoc(doc, ['codex', 'tail-burst']);
          if (isDirty('codexTailBurstEnabled')) {
            doc.setIn(['codex', 'tail-burst', 'enabled'], values.codexTailBurstEnabled);
          }
          if (isDirty('codexTailBurstTriggerRemainingPercent')) {
            setTailBurstRemainingRatioInDoc(
              doc,
              ['codex', 'tail-burst', 'trigger-remaining-ratio'],
              values.codexTailBurstTriggerRemainingPercent
            );
            if (docHas(doc, ['codex', 'tail-burst', 'trigger-used-ratio'])) {
              doc.deleteIn(['codex', 'tail-burst', 'trigger-used-ratio']);
            }
          }
          if (isDirty('codexTailBurstSnapshotTtl')) {
            setStringInDoc(
              doc,
              ['codex', 'tail-burst', 'snapshot-ttl'],
              values.codexTailBurstSnapshotTtl
            );
          }
          if (isDirty('codexTailBurstExpiryWindow')) {
            setStringInDoc(
              doc,
              ['codex', 'tail-burst', 'expiry-window'],
              values.codexTailBurstExpiryWindow
            );
          }
          if (isDirty('codexTailBurstMaxConcurrency')) {
            setIntFromStringInDoc(
              doc,
              ['codex', 'tail-burst', 'max-concurrency'],
              values.codexTailBurstMaxConcurrency
            );
          }
          const collectorDirty =
            isDirty('codexTailBurstCollectorInterval') ||
            isDirty('codexTailBurstCollectorMaxConcurrency') ||
            isDirty('codexTailBurstCollectorTimeout');
          if (collectorDirty) {
            ensureMapInDoc(doc, ['codex', 'tail-burst', 'quota-collector']);
            if (isDirty('codexTailBurstCollectorInterval')) {
              setStringInDoc(
                doc,
                ['codex', 'tail-burst', 'quota-collector', 'interval'],
                values.codexTailBurstCollectorInterval
              );
            }
            if (isDirty('codexTailBurstCollectorMaxConcurrency')) {
              setIntFromStringInDoc(
                doc,
                ['codex', 'tail-burst', 'quota-collector', 'max-concurrency'],
                values.codexTailBurstCollectorMaxConcurrency
              );
            }
            if (isDirty('codexTailBurstCollectorTimeout')) {
              setStringInDoc(
                doc,
                ['codex', 'tail-burst', 'quota-collector', 'timeout'],
                values.codexTailBurstCollectorTimeout
              );
            }
            deleteIfMapEmpty(doc, ['codex', 'tail-burst', 'quota-collector']);
          }
          if (isDirty('codexTailBurstToolInjectionEnabled')) {
            ensureMapInDoc(doc, ['codex', 'tail-burst', 'tool-injection']);
            doc.setIn(
              ['codex', 'tail-burst', 'tool-injection', 'enabled'],
              values.codexTailBurstToolInjectionEnabled
            );
          }
          deleteIfMapEmpty(doc, ['codex', 'tail-burst', 'tool-injection']);
          deleteIfMapEmpty(doc, ['codex', 'tail-burst']);
          deleteIfMapEmpty(doc, ['codex']);
        }

        const writeQuotaSwitchProject = isDirty('quotaSwitchProject');
        const writeQuotaSwitchPreviewModel = isDirty('quotaSwitchPreviewModel');
        const writeQuotaAntigravityCredits = isDirty('quotaAntigravityCredits');
        if (
          writeQuotaSwitchProject ||
          writeQuotaSwitchPreviewModel ||
          writeQuotaAntigravityCredits
        ) {
          ensureMapInDoc(doc, ['quota-exceeded']);
          if (writeQuotaSwitchProject) {
            doc.setIn(['quota-exceeded', 'switch-project'], values.quotaSwitchProject);
          }
          if (writeQuotaSwitchPreviewModel) {
            doc.setIn(['quota-exceeded', 'switch-preview-model'], values.quotaSwitchPreviewModel);
          }
          if (writeQuotaAntigravityCredits) {
            doc.setIn(['quota-exceeded', 'antigravity-credits'], values.quotaAntigravityCredits);
          }
          deleteIfMapEmpty(doc, ['quota-exceeded']);
        }

        const routingDirty =
          isDirty('routingStrategy') ||
          isDirty('routingSessionAffinity') ||
          isDirty('routingHighCacheMode') ||
          isDirty('routingSessionAffinityTTL');
        if (routingDirty) {
          ensureMapInDoc(doc, ['routing']);
          if (isDirty('routingStrategy')) {
            doc.setIn(['routing', 'strategy'], values.routingStrategy);
          }
          if (isDirty('routingSessionAffinity')) {
            setBooleanInDoc(doc, ['routing', 'session-affinity'], values.routingSessionAffinity);
          }
          if (isDirty('routingHighCacheMode')) {
            setBooleanInDoc(doc, ['routing', 'high-cache-mode'], values.routingHighCacheMode);
          }
          if (isDirty('routingSessionAffinityTTL')) {
            setStringInDoc(
              doc,
              ['routing', 'session-affinity-ttl'],
              values.routingSessionAffinityTTL
            );
          }
          deleteIfMapEmpty(doc, ['routing']);
        }

        const keepaliveSeconds =
          typeof values.streaming?.keepaliveSeconds === 'string'
            ? values.streaming.keepaliveSeconds
            : '';
        const bootstrapRetries =
          typeof values.streaming?.bootstrapRetries === 'string'
            ? values.streaming.bootstrapRetries
            : '';
        const nonstreamKeepaliveInterval =
          typeof values.streaming?.nonstreamKeepaliveInterval === 'string'
            ? values.streaming.nonstreamKeepaliveInterval
            : '';

        const streamingDirty =
          isDirty('streaming.keepaliveSeconds') || isDirty('streaming.bootstrapRetries');
        if (streamingDirty) {
          ensureMapInDoc(doc, ['streaming']);
          if (isDirty('streaming.keepaliveSeconds')) {
            setIntFromStringInDoc(doc, ['streaming', 'keepalive-seconds'], keepaliveSeconds);
          }
          if (isDirty('streaming.bootstrapRetries')) {
            setIntFromStringInDoc(doc, ['streaming', 'bootstrap-retries'], bootstrapRetries);
          }
          deleteIfMapEmpty(doc, ['streaming']);
        }

        if (isDirty('streaming.nonstreamKeepaliveInterval')) {
          setIntFromStringInDoc(doc, ['nonstream-keepalive-interval'], nonstreamKeepaliveInterval);
        }

        const payloadDirty =
          isDirty('payloadDefaultRules') ||
          isDirty('payloadDefaultRawRules') ||
          isDirty('payloadOverrideRules') ||
          isDirty('payloadOverrideRawRules') ||
          isDirty('payloadFilterRules');
        if (payloadDirty) {
          ensureMapInDoc(doc, ['payload']);
          if (isDirty('payloadDefaultRules')) {
            if (values.payloadDefaultRules.length > 0) {
              doc.setIn(
                ['payload', 'default'],
                serializePayloadRulesForYaml(values.payloadDefaultRules)
              );
            } else if (docHas(doc, ['payload', 'default'])) {
              doc.deleteIn(['payload', 'default']);
            }
          }
          if (isDirty('payloadDefaultRawRules')) {
            if (values.payloadDefaultRawRules.length > 0) {
              doc.setIn(
                ['payload', 'default-raw'],
                serializeRawPayloadRulesForYaml(values.payloadDefaultRawRules)
              );
            } else if (docHas(doc, ['payload', 'default-raw'])) {
              doc.deleteIn(['payload', 'default-raw']);
            }
          }
          if (isDirty('payloadOverrideRules')) {
            if (values.payloadOverrideRules.length > 0) {
              doc.setIn(
                ['payload', 'override'],
                serializePayloadRulesForYaml(values.payloadOverrideRules)
              );
            } else if (docHas(doc, ['payload', 'override'])) {
              doc.deleteIn(['payload', 'override']);
            }
          }
          if (isDirty('payloadOverrideRawRules')) {
            if (values.payloadOverrideRawRules.length > 0) {
              doc.setIn(
                ['payload', 'override-raw'],
                serializeRawPayloadRulesForYaml(values.payloadOverrideRawRules)
              );
            } else if (docHas(doc, ['payload', 'override-raw'])) {
              doc.deleteIn(['payload', 'override-raw']);
            }
          }
          if (isDirty('payloadFilterRules')) {
            if (values.payloadFilterRules.length > 0) {
              doc.setIn(
                ['payload', 'filter'],
                serializePayloadFilterRulesForYaml(values.payloadFilterRules)
              );
            } else if (docHas(doc, ['payload', 'filter'])) {
              doc.deleteIn(['payload', 'filter']);
            }
          }
          deleteIfMapEmpty(doc, ['payload']);
        }

        return doc.toString({ indent: 2, lineWidth: 120, minContentWidth: 0 });
      } catch {
        return currentYaml;
      }
    },
    [dirtyFields, visualValues]
  );

  const setVisualValues = useCallback((newValues: Partial<VisualConfigValues>) => {
    dispatch({ type: 'set_values', values: newValues });
  }, []);

  return {
    visualValues,
    visualDirty,
    visualParseError,
    visualValidationErrors,
    visualHasPayloadValidationErrors,
    loadVisualValuesFromYaml,
    applyVisualChangesToYaml,
    setVisualValues,
  };
}
