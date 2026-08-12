import { useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { authFilesApi, type AuthFileFieldsPatch } from '@/services/api';
import type { AuthFileItem } from '@/types';
import { useNotificationStore } from '@/stores';
import {
  applyAuthFileWebsockets,
  normalizeProviderKey,
  parseNonNegativeIntegerValue,
  parsePriorityValue,
  readAuthFileBooleanField,
  readAuthFileIntegerField,
  readAuthFileWebsockets,
  supportsAuthFileWebsockets,
  supportsAuthFileUsingApi,
} from '@/features/authFiles/constants';
import {
  AUTH_FILE_CONFIGURATION_INVALID_JSON,
  buildRedactedAuthFileConfigurationText,
  parseAuthFileConfigurationSource,
  type ParsedAuthFileConfigurationSource,
} from '@/features/authFiles/model/authFileConfiguration';
import { getAuthFilePatchTarget } from '@/features/authFiles/model/authFilesPageModel';
import {
  getAuthFileStatusIdentityKey,
  resolveAuthFileStatusMutationTarget,
} from '@/utils/authFileStatusMutation';

type AuthFileHeaders = Record<string, string>;
type AuthFileHeadersErrorKey =
  | 'auth_files.headers_invalid_json'
  | 'auth_files.headers_invalid_object'
  | 'auth_files.headers_invalid_value';
type AuthFileContentErrorKey =
  | 'auth_files.prefix_proxy_invalid_json'
  | 'auth_files.prefix_proxy_html_challenge';

export type PrefixProxyEditorField =
  | 'prefix'
  | 'proxyUrl'
  | 'sourceIp'
  | 'priority'
  | 'maxConcurrency'
  | 'rateLimitMaxRequests'
  | 'rateLimitWindowSeconds'
  | 'selectionErrorFreezeSeconds'
  | 'disableStickyOnNextRequest'
  | 'websockets'
  | 'usingApi'
  | 'codexCliOnly'
  | 'codexCliOnlyAllowAppServer'
  | 'note'
  | 'headersText';

export type PrefixProxyEditorFieldValue = string | boolean;

export type PrefixProxyEditorState = {
  authFile: AuthFileItem;
  fileName: string;
  credentialKey: string;
  fileInfoText: string;
  loading: boolean;
  saving: boolean;
  error: string | null;
  json: Record<string, unknown> | null;
  providerKey: string;
  prefix: string;
  proxyUrl: string;
  sourceIp: string;
  priority: string;
  maxConcurrency: string;
  rateLimitMaxRequests: string;
  rateLimitWindowSeconds: string;
  selectionErrorFreezeSeconds: string;
  disableStickyOnNextRequest: boolean;
  disableStickyOnNextRequestTouched: boolean;
  websockets: boolean;
  websocketsTouched: boolean;
  usingApi: boolean;
  usingApiTouched: boolean;
  codexCliOnly: boolean;
  codexCliOnlyTouched: boolean;
  codexCliOnlyAllowAppServer: boolean;
  codexCliOnlyAllowAppServerTouched: boolean;
  note: string;
  noteTouched: boolean;
  headersText: string;
  headersTouched: boolean;
  headersError: string | null;
};

export type UseAuthFilesPrefixProxyEditorOptions = {
  disableControls: boolean;
  loadFiles: () => Promise<void>;
  onSaved?: (fileName: string) => void;
};

export type UseAuthFilesPrefixProxyEditorResult = {
  prefixProxyEditor: PrefixProxyEditorState | null;
  prefixProxyUpdatedText: string;
  prefixProxyDirty: boolean;
  openPrefixProxyEditor: (file: AuthFileItem) => Promise<void>;
  closePrefixProxyEditor: () => void;
  handlePrefixProxyChange: (
    field: PrefixProxyEditorField,
    value: PrefixProxyEditorFieldValue
  ) => void;
  handlePrefixProxySave: () => Promise<void>;
};

const isRecordObject = (value: unknown): value is Record<string, unknown> =>
  Boolean(value) && typeof value === 'object' && !Array.isArray(value);

const validateHeadersValue = (value: unknown): AuthFileHeadersErrorKey | null => {
  if (!isRecordObject(value)) {
    return 'auth_files.headers_invalid_object';
  }
  return Object.values(value).every((item) => typeof item === 'string')
    ? null
    : 'auth_files.headers_invalid_value';
};

const parseHeadersText = (
  text: string
): { value: AuthFileHeaders | null; errorKey: AuthFileHeadersErrorKey | null } => {
  const trimmed = text.trim();
  if (!trimmed) {
    return { value: null, errorKey: null };
  }

  let parsed: unknown;
  try {
    parsed = JSON.parse(text) as unknown;
  } catch {
    return { value: null, errorKey: 'auth_files.headers_invalid_json' };
  }

  const errorKey = validateHeadersValue(parsed);
  if (errorKey) {
    return { value: null, errorKey };
  }

  return { value: parsed as AuthFileHeaders, errorKey: null };
};

const normalizeTextField = (value: unknown): string =>
  typeof value === 'string' ? value.trim() : '';

const getAuthFileContentErrorKey = (text: string): AuthFileContentErrorKey => {
  const head = text.trimStart().slice(0, 4096).toLowerCase();
  const looksLikeHtml =
    head.startsWith('<!doctype html') ||
    head.startsWith('<html') ||
    head.includes('<head') ||
    head.includes('<body');
  const looksLikeChallenge =
    head.includes('cf_chl') ||
    head.includes('__cf_chl_tk') ||
    head.includes('challenge-platform') ||
    head.includes('cloudflare');

  return looksLikeHtml || looksLikeChallenge
    ? 'auth_files.prefix_proxy_html_challenge'
    : 'auth_files.prefix_proxy_invalid_json';
};

const buildEditorAuthFileReference = (file: AuthFileItem): AuthFileItem => {
  const target = getAuthFilePatchTarget(file);
  const provider = String(target.provider ?? '').trim();
  const runtimeId = String(target.runtimeId ?? '').trim();
  const accountId = String(target.accountId ?? '').trim();
  const accountSnapshot = String(target.accountSnapshot ?? '').trim();
  return {
    name: target.name,
    ...(runtimeId ? { id: runtimeId } : {}),
    ...(target.authIndex !== undefined && target.authIndex !== null
      ? { authIndex: target.authIndex }
      : {}),
    ...(provider ? { provider, type: provider } : {}),
    ...(accountId ? { account_id: accountId } : {}),
    ...(accountSnapshot ? { account: accountSnapshot } : {}),
  } as AuthFileItem;
};

const pickEditableAuthFileFields = (content: Record<string, unknown>): Record<string, unknown> => {
  const editable: Record<string, unknown> = {};
  const copyField = (field: string) => {
    if (field in content) editable[field] = content[field];
  };
  [
    'prefix',
    'proxy_url',
    'priority',
    'websocket',
    'websockets',
    'using_api',
    'codex_cli_only',
    'codex_cli_only_allow_app_server',
    'note',
  ].forEach(copyField);
  if (isRecordObject(content.headers)) {
    editable.headers = Object.fromEntries(
      Object.entries(content.headers).filter(
        (entry): entry is [string, string] => typeof entry[1] === 'string'
      )
    );
  }
  return editable;
};

const hasKeys = (value: Record<string, unknown> | AuthFileFieldsPatch | null): boolean =>
  Boolean(value && Object.keys(value).length > 0);

const normalizeHeaders = (value: unknown): AuthFileHeaders => {
  if (!isRecordObject(value)) return {};

  return Object.entries(value).reduce<AuthFileHeaders>((result, [key, rawValue]) => {
    if (typeof rawValue !== 'string') return result;
    const name = key.trim();
    const headerValue = rawValue.trim();
    if (!name || !headerValue) return result;
    result[name] = headerValue;
    return result;
  }, {});
};

const buildHeadersPatch = (
  originalHeaders: AuthFileHeaders,
  nextHeaders: AuthFileHeaders
): AuthFileHeaders | undefined => {
  const patch: AuthFileHeaders = {};
  const nextNames = new Set(Object.keys(nextHeaders));

  Object.entries(nextHeaders).forEach(([name, value]) => {
    if (originalHeaders[name] !== value) {
      patch[name] = value;
    }
  });

  Object.keys(originalHeaders).forEach((name) => {
    if (!nextNames.has(name)) {
      patch[name] = '';
    }
  });

  return Object.keys(patch).length > 0 ? patch : undefined;
};

const applyHeadersPatch = (
  value: Record<string, unknown>,
  headersPatch: AuthFileHeaders | undefined
) => {
  if (!headersPatch) return;

  const nextHeaders = normalizeHeaders(value.headers);
  Object.entries(headersPatch).forEach(([name, rawValue]) => {
    const headerName = name.trim();
    if (!headerName) return;
    const headerValue = rawValue.trim();
    if (!headerValue) {
      delete nextHeaders[headerName];
      return;
    }
    nextHeaders[headerName] = headerValue;
  });

  if (Object.keys(nextHeaders).length > 0) {
    value.headers = nextHeaders;
  } else {
    delete value.headers;
  }
};

const buildAuthFileFieldsPatch = (
  editor: PrefixProxyEditorState,
  resolveHeadersError: (key: AuthFileHeadersErrorKey) => string
): AuthFileFieldsPatch => {
  const original = editor.json ?? {};
  const patch: AuthFileFieldsPatch = {};

  const originalPrefix = normalizeTextField(original.prefix);
  const nextPrefix = editor.prefix.trim();
  if (nextPrefix !== originalPrefix) {
    patch.prefix = nextPrefix;
  }

  const originalProxyURL = normalizeTextField(original.proxy_url);
  const nextProxyURL = editor.proxyUrl.trim();
  if (nextProxyURL !== originalProxyURL) {
    patch.proxy_url = nextProxyURL;
  }

  const originalSourceIP = normalizeTextField(
    original.source_ip ?? original['source-ip'] ?? original.sourceIp
  );
  const nextSourceIP = editor.sourceIp.trim();
  if (nextSourceIP !== originalSourceIP) {
    patch.source_ip = nextSourceIP;
  }

  const originalPriority = parsePriorityValue(original.priority);
  const priorityText = editor.priority.trim();
  const nextPriority = parsePriorityValue(priorityText);
  if (!priorityText) {
    if (originalPriority !== undefined && originalPriority !== 0) {
      patch.priority = 0;
    }
  } else if (nextPriority !== undefined) {
    if (nextPriority === 0) {
      if (originalPriority !== undefined && originalPriority !== 0) {
        patch.priority = 0;
      }
    } else if (nextPriority !== originalPriority) {
      patch.priority = nextPriority;
    }
  }

  const maybeSetIntegerPatch = (
    field: keyof Pick<
      AuthFileFieldsPatch,
      | 'max_concurrency'
      | 'rate_limit_max_requests'
      | 'rate_limit_window_seconds'
      | 'selection_error_freeze_seconds'
    >,
    originalKey: string,
    originalCamelKey: string,
    nextText: string
  ) => {
    const trimmed = nextText.trim();
    if (!trimmed) return;
    const nextValue = parseNonNegativeIntegerValue(trimmed);
    if (nextValue === undefined) return;
    const originalValue = readAuthFileIntegerField(original, originalKey, originalCamelKey);
    if (nextValue !== originalValue) {
      patch[field] = nextValue;
    }
  };

  maybeSetIntegerPatch(
    'max_concurrency',
    'max_concurrency',
    'maxConcurrency',
    editor.maxConcurrency
  );
  maybeSetIntegerPatch(
    'rate_limit_max_requests',
    'rate_limit_max_requests',
    'rateLimitMaxRequests',
    editor.rateLimitMaxRequests
  );
  maybeSetIntegerPatch(
    'rate_limit_window_seconds',
    'rate_limit_window_seconds',
    'rateLimitWindowSeconds',
    editor.rateLimitWindowSeconds
  );
  maybeSetIntegerPatch(
    'selection_error_freeze_seconds',
    'selection_error_freeze_seconds',
    'selectionErrorFreezeSeconds',
    editor.selectionErrorFreezeSeconds
  );

  if (editor.disableStickyOnNextRequestTouched) {
    const originalDisableSticky =
      readAuthFileBooleanField(
        original,
        'disable_sticky_on_next_request',
        'disableStickyOnNextRequest'
      ) ?? false;
    const nextDisableSticky = Boolean(editor.disableStickyOnNextRequest);
    if (nextDisableSticky !== originalDisableSticky) {
      patch.disable_sticky_on_next_request = nextDisableSticky;
    }
  }

  if (editor.noteTouched) {
    const originalNote = normalizeTextField(original.note);
    const nextNote = editor.note.trim();
    if (nextNote !== originalNote) {
      patch.note = nextNote;
    }
  }

  if (editor.headersTouched) {
    const { value: parsedHeaders, errorKey } = parseHeadersText(editor.headersText);
    if (errorKey) {
      throw new Error(resolveHeadersError(errorKey));
    }
    const headersPatch = buildHeadersPatch(
      normalizeHeaders(original.headers),
      normalizeHeaders(parsedHeaders ?? {})
    );
    if (headersPatch) {
      patch.headers = headersPatch;
    }
  }

  if (supportsAuthFileWebsockets(editor.providerKey) && editor.websocketsTouched) {
    const originalWebsockets = readAuthFileWebsockets(original);
    const nextWebsockets = Boolean(editor.websockets);
    if (nextWebsockets !== originalWebsockets) {
      patch.websockets = nextWebsockets;
    }
  }
  if (supportsAuthFileUsingApi(editor.providerKey) && editor.usingApiTouched) {
    const originalUsingApi = original.using_api === true;
    if (editor.usingApi !== originalUsingApi) patch.using_api = editor.usingApi;
  }
  if (editor.providerKey === 'codex' && editor.codexCliOnlyTouched) {
    const originalCodexCliOnly = original.codex_cli_only === true;
    if (editor.codexCliOnly !== originalCodexCliOnly) {
      patch.codex_cli_only = editor.codexCliOnly;
    }
  }
  if (editor.providerKey === 'codex' && editor.codexCliOnlyAllowAppServerTouched) {
    const originalAllowAppServer = original.codex_cli_only_allow_app_server === true;
    if (editor.codexCliOnlyAllowAppServer !== originalAllowAppServer) {
      patch.codex_cli_only_allow_app_server = editor.codexCliOnlyAllowAppServer;
    }
  }

  return patch;
};

const buildPrefixProxyUpdatedText = (
  editor: PrefixProxyEditorState | null,
  resolveHeadersError: (key: AuthFileHeadersErrorKey) => string
): string => {
  if (!editor?.json) return '';
  const patch = buildAuthFileFieldsPatch(editor, resolveHeadersError);
  let next: Record<string, unknown> = { ...editor.json };
  if (patch.prefix !== undefined) {
    if (patch.prefix) {
      next.prefix = patch.prefix;
    } else {
      delete next.prefix;
    }
  }
  if (patch.proxy_url !== undefined) {
    if (patch.proxy_url) {
      next.proxy_url = patch.proxy_url;
    } else {
      delete next.proxy_url;
    }
  }
  if (patch.source_ip !== undefined) {
    if (patch.source_ip) {
      next.source_ip = patch.source_ip;
    } else {
      delete next.source_ip;
    }
  }

  if (patch.priority !== undefined) {
    if (patch.priority === 0) {
      delete next.priority;
    } else {
      next.priority = patch.priority;
    }
  }

  if (patch.max_concurrency !== undefined) {
    next.max_concurrency = patch.max_concurrency;
  }

  if (patch.rate_limit_max_requests !== undefined) {
    next.rate_limit_max_requests = patch.rate_limit_max_requests;
  }

  if (patch.rate_limit_window_seconds !== undefined) {
    next.rate_limit_window_seconds = patch.rate_limit_window_seconds;
  }

  if (patch.selection_error_freeze_seconds !== undefined) {
    next.selection_error_freeze_seconds = patch.selection_error_freeze_seconds;
  }

  if (patch.disable_sticky_on_next_request !== undefined) {
    next.disable_sticky_on_next_request = patch.disable_sticky_on_next_request;
  }

  if (patch.note !== undefined) {
    if (patch.note) {
      next.note = patch.note;
    } else if ('note' in next) {
      delete next.note;
    }
  }

  applyHeadersPatch(next, patch.headers);

  if (patch.websockets !== undefined) {
    next = applyAuthFileWebsockets(next, patch.websockets);
  }
  if (patch.using_api !== undefined) next.using_api = patch.using_api;
  if (patch.codex_cli_only !== undefined) next.codex_cli_only = patch.codex_cli_only;
  if (patch.codex_cli_only_allow_app_server !== undefined) {
    next.codex_cli_only_allow_app_server = patch.codex_cli_only_allow_app_server;
  }

  return JSON.stringify(next);
};

export function useAuthFilesPrefixProxyEditor(
  options: UseAuthFilesPrefixProxyEditorOptions
): UseAuthFilesPrefixProxyEditorResult {
  const { disableControls, loadFiles, onSaved } = options;
  const { t } = useTranslation();
  const showNotification = useNotificationStore((state) => state.showNotification);

  const [prefixProxyEditor, setPrefixProxyEditor] = useState<PrefixProxyEditorState | null>(null);
  const editorGenerationRef = useRef(0);

  const hasBlockingValidationError = Boolean(
    prefixProxyEditor?.headersTouched && prefixProxyEditor.headersError
  );
  const prefixProxyUpdatedText =
    prefixProxyEditor && !hasBlockingValidationError
      ? buildPrefixProxyUpdatedText(prefixProxyEditor, (key) => t(key))
      : '';

  const prefixProxyPatch =
    prefixProxyEditor?.json && !hasBlockingValidationError
      ? buildAuthFileFieldsPatch(prefixProxyEditor, (key) => t(key))
      : null;

  const prefixProxyDirty = hasKeys(prefixProxyPatch);

  const closePrefixProxyEditor = () => {
    editorGenerationRef.current += 1;
    setPrefixProxyEditor(null);
  };

  const openPrefixProxyEditor = async (file: AuthFileItem) => {
    const name = file.name;
    const credentialKey = getAuthFileStatusIdentityKey(file);
    const authFile = buildEditorAuthFileReference(file);
    const fileProviderKey = normalizeProviderKey(String(file.type ?? file.provider ?? ''));

    if (disableControls) return;
    if (prefixProxyEditor?.credentialKey === credentialKey) {
      editorGenerationRef.current += 1;
      setPrefixProxyEditor(null);
      return;
    }
    const editorGeneration = editorGenerationRef.current + 1;
    editorGenerationRef.current = editorGeneration;

    setPrefixProxyEditor({
      authFile,
      fileName: name,
      credentialKey,
      fileInfoText: buildRedactedAuthFileConfigurationText({}),
      loading: true,
      saving: false,
      error: null,
      json: null,
      providerKey: fileProviderKey,
      prefix: '',
      proxyUrl: '',
      sourceIp: '',
      priority: '',
      maxConcurrency: '',
      rateLimitMaxRequests: '',
      rateLimitWindowSeconds: '',
      selectionErrorFreezeSeconds: '',
      disableStickyOnNextRequest: false,
      disableStickyOnNextRequestTouched: false,
      websockets: false,
      websocketsTouched: false,
      usingApi: false,
      usingApiTouched: false,
      codexCliOnly: false,
      codexCliOnlyTouched: false,
      codexCliOnlyAllowAppServer: false,
      codexCliOnlyAllowAppServerTouched: false,
      note: '',
      noteTouched: false,
      headersText: '',
      headersTouched: false,
      headersError: null,
    });

    try {
      const rawText = await authFilesApi.downloadText(name);
      let parsed: ParsedAuthFileConfigurationSource;
      try {
        parsed = parseAuthFileConfigurationSource(rawText, file);
      } catch (err: unknown) {
        const isInvalidJson =
          err instanceof Error && err.message === AUTH_FILE_CONFIGURATION_INVALID_JSON;
        const errorKey = isInvalidJson
          ? getAuthFileContentErrorKey(rawText)
          : 'auth_files.status_mutation_scope_ambiguous';
        setPrefixProxyEditor((prev) => {
          if (
            editorGenerationRef.current !== editorGeneration ||
            !prev ||
            prev.credentialKey !== credentialKey
          ) {
            return prev;
          }
          return {
            ...prev,
            loading: false,
            error: t(errorKey, isInvalidJson ? undefined : { name }),
          };
        });
        return;
      }

      const content = parsed.record;
      const json = pickEditableAuthFileFields(content);
      const prefix = typeof json.prefix === 'string' ? json.prefix : '';
      const proxyUrl = typeof json.proxy_url === 'string' ? json.proxy_url : '';
      const sourceIp =
        typeof json.source_ip === 'string'
          ? json.source_ip
          : typeof json['source-ip'] === 'string'
            ? json['source-ip']
            : typeof json.sourceIp === 'string'
              ? json.sourceIp
              : '';
      const priority = parsePriorityValue(json.priority);
      const maxConcurrency = readAuthFileIntegerField(json, 'max_concurrency', 'maxConcurrency');
      const rateLimitMaxRequests = readAuthFileIntegerField(
        json,
        'rate_limit_max_requests',
        'rateLimitMaxRequests'
      );
      const rateLimitWindowSeconds = readAuthFileIntegerField(
        json,
        'rate_limit_window_seconds',
        'rateLimitWindowSeconds'
      );
      const selectionErrorFreezeSeconds = readAuthFileIntegerField(
        json,
        'selection_error_freeze_seconds',
        'selectionErrorFreezeSeconds'
      );
      const disableStickyOnNextRequest =
        readAuthFileBooleanField(
          json,
          'disable_sticky_on_next_request',
          'disableStickyOnNextRequest'
        ) ?? false;
      const providerKey = parsed.providerKey;
      const websockets = supportsAuthFileWebsockets(providerKey)
        ? readAuthFileWebsockets(json)
        : false;
      const usingApi = supportsAuthFileUsingApi(providerKey) && json.using_api === true;
      const codexCliOnly = providerKey === 'codex' && json.codex_cli_only === true;
      const codexCliOnlyAllowAppServer =
        providerKey === 'codex' && json.codex_cli_only_allow_app_server === true;
      const note = typeof json.note === 'string' ? json.note : '';
      const headers = json.headers;
      let headersText = '';
      let headersError: string | null = null;
      if (headers !== undefined) {
        headersText = JSON.stringify(headers, null, 2);
        const { errorKey } = parseHeadersText(headersText);
        headersError = errorKey ? t(errorKey) : null;
      }

      setPrefixProxyEditor((prev) => {
        if (
          editorGenerationRef.current !== editorGeneration ||
          !prev ||
          prev.credentialKey !== credentialKey
        ) {
          return prev;
        }
        return {
          ...prev,
          loading: false,
          fileInfoText: buildRedactedAuthFileConfigurationText(content),
          json,
          providerKey,
          prefix,
          proxyUrl,
          sourceIp,
          priority: priority !== undefined ? String(priority) : '',
          maxConcurrency: maxConcurrency !== undefined ? String(maxConcurrency) : '',
          rateLimitMaxRequests:
            rateLimitMaxRequests !== undefined ? String(rateLimitMaxRequests) : '',
          rateLimitWindowSeconds:
            rateLimitWindowSeconds !== undefined ? String(rateLimitWindowSeconds) : '',
          selectionErrorFreezeSeconds:
            selectionErrorFreezeSeconds !== undefined ? String(selectionErrorFreezeSeconds) : '',
          disableStickyOnNextRequest,
          disableStickyOnNextRequestTouched: false,
          websockets,
          websocketsTouched: false,
          usingApi,
          usingApiTouched: false,
          codexCliOnly,
          codexCliOnlyTouched: false,
          codexCliOnlyAllowAppServer,
          codexCliOnlyAllowAppServerTouched: false,
          note,
          noteTouched: false,
          headersText,
          headersTouched: false,
          headersError,
          error: null,
        };
      });
    } catch (err: unknown) {
      const errorMessage = err instanceof Error ? err.message : t('notification.download_failed');
      setPrefixProxyEditor((prev) => {
        if (
          editorGenerationRef.current !== editorGeneration ||
          !prev ||
          prev.credentialKey !== credentialKey
        ) {
          return prev;
        }
        return { ...prev, loading: false, error: errorMessage };
      });
      showNotification(`${t('notification.download_failed')}: ${errorMessage}`, 'error');
    }
  };

  const handlePrefixProxyChange = (
    field: PrefixProxyEditorField,
    value: PrefixProxyEditorFieldValue
  ) => {
    setPrefixProxyEditor((prev) => {
      if (!prev) return prev;
      if (field === 'prefix') return { ...prev, prefix: String(value) };
      if (field === 'proxyUrl') return { ...prev, proxyUrl: String(value) };
      if (field === 'sourceIp') return { ...prev, sourceIp: String(value) };
      if (field === 'priority') return { ...prev, priority: String(value) };
      if (field === 'maxConcurrency') return { ...prev, maxConcurrency: String(value) };
      if (field === 'rateLimitMaxRequests') {
        return { ...prev, rateLimitMaxRequests: String(value) };
      }
      if (field === 'rateLimitWindowSeconds') {
        return { ...prev, rateLimitWindowSeconds: String(value) };
      }
      if (field === 'selectionErrorFreezeSeconds') {
        return { ...prev, selectionErrorFreezeSeconds: String(value) };
      }
      if (field === 'disableStickyOnNextRequest') {
        return {
          ...prev,
          disableStickyOnNextRequest: Boolean(value),
          disableStickyOnNextRequestTouched: true,
        };
      }
      if (field === 'websockets') {
        return { ...prev, websockets: Boolean(value), websocketsTouched: true };
      }
      if (field === 'usingApi') {
        return { ...prev, usingApi: Boolean(value), usingApiTouched: true };
      }
      if (field === 'codexCliOnly') {
        const codexCliOnly = Boolean(value);
        return {
          ...prev,
          codexCliOnly,
          codexCliOnlyTouched: true,
          codexCliOnlyAllowAppServer: codexCliOnly
            ? prev.codexCliOnlyAllowAppServer
            : false,
          codexCliOnlyAllowAppServerTouched: codexCliOnly
            ? prev.codexCliOnlyAllowAppServerTouched
            : true,
        };
      }
      if (field === 'codexCliOnlyAllowAppServer') {
        return {
          ...prev,
          codexCliOnlyAllowAppServer: Boolean(value),
          codexCliOnlyAllowAppServerTouched: true,
        };
      }
      if (field === 'note') return { ...prev, note: String(value), noteTouched: true };
      if (field === 'headersText') {
        const headersText = String(value);
        const { errorKey } = parseHeadersText(headersText);
        return {
          ...prev,
          headersText,
          headersTouched: true,
          headersError: errorKey ? t(errorKey) : null,
        };
      }
      return prev;
    });
  };

  const handlePrefixProxySave = async () => {
    if (!prefixProxyEditor?.json) return;
    if (!prefixProxyDirty) return;

    const name = prefixProxyEditor.fileName;
    const credentialKey = prefixProxyEditor.credentialKey;
    const editorGeneration = editorGenerationRef.current;
    let payload: AuthFileFieldsPatch;
    try {
      payload = buildAuthFileFieldsPatch(prefixProxyEditor, (key) => t(key));
    } catch (err: unknown) {
      const errorMessage = err instanceof Error ? err.message : 'Invalid format';
      showNotification(errorMessage, 'error');
      return;
    }
    if (!hasKeys(payload)) return;

    setPrefixProxyEditor((prev) => {
      if (
        editorGenerationRef.current !== editorGeneration ||
        !prev ||
        prev.credentialKey !== credentialKey
      ) {
        return prev;
      }
      return { ...prev, saving: true };
    });

    try {
      const response = await authFilesApi.list();
      const currentFiles = Array.isArray(response.files) ? response.files : [];
      const resolution = resolveAuthFileStatusMutationTarget(
        currentFiles,
        getAuthFilePatchTarget(prefixProxyEditor.authFile)
      );
      if (!resolution.target || resolution.failure !== null || resolution.scope !== 'credential') {
        throw new Error(t('auth_files.status_mutation_scope_ambiguous', { name }));
      }
      await authFilesApi.patchFieldsWithPluginSourceFallback(
        getAuthFilePatchTarget(resolution.target),
        payload,
        currentFiles
          .filter((file) => file.name.trim() === resolution.target?.name.trim())
          .map(getAuthFilePatchTarget)
      );
      showNotification(t('auth_files.prefix_proxy_saved_success', { name }), 'success');
      await loadFiles();
      onSaved?.(name);
      setPrefixProxyEditor((prev) => {
        if (
          editorGenerationRef.current !== editorGeneration ||
          prev?.credentialKey !== credentialKey
        ) {
          return prev;
        }
        return null;
      });
    } catch (err: unknown) {
      const errorMessage = err instanceof Error ? err.message : '';
      showNotification(`${t('notification.update_failed')}: ${errorMessage}`, 'error');
      setPrefixProxyEditor((prev) => {
        if (
          editorGenerationRef.current !== editorGeneration ||
          !prev ||
          prev.credentialKey !== credentialKey
        ) {
          return prev;
        }
        return { ...prev, saving: false };
      });
    }
  };

  return {
    prefixProxyEditor,
    prefixProxyUpdatedText,
    prefixProxyDirty,
    openPrefixProxyEditor,
    closePrefixProxyEditor,
    handlePrefixProxyChange,
    handlePrefixProxySave,
  };
}
