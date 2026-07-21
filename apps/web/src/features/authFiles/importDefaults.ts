import type { AuthFileImportDefaults } from '@/services/api/authFiles';

const AUTH_FILE_IMPORT_DEFAULTS_STORAGE_KEY = 'authFilesPage.importDefaults';

export const DEFAULT_AUTH_FILE_IMPORT_DEFAULTS: Required<AuthFileImportDefaults> = {
  websockets: true,
};

const normalizeAuthFileImportDefaults = (value: unknown): Required<AuthFileImportDefaults> => {
  if (!value || typeof value !== 'object') {
    return { ...DEFAULT_AUTH_FILE_IMPORT_DEFAULTS };
  }

  const record = value as Record<string, unknown>;
  return {
    websockets:
      typeof record.websockets === 'boolean'
        ? record.websockets
        : DEFAULT_AUTH_FILE_IMPORT_DEFAULTS.websockets,
  };
};

export const readAuthFileImportDefaults = (): Required<AuthFileImportDefaults> => {
  if (typeof window === 'undefined') return { ...DEFAULT_AUTH_FILE_IMPORT_DEFAULTS };
  try {
    const raw = window.localStorage.getItem(AUTH_FILE_IMPORT_DEFAULTS_STORAGE_KEY);
    return raw
      ? normalizeAuthFileImportDefaults(JSON.parse(raw) as unknown)
      : { ...DEFAULT_AUTH_FILE_IMPORT_DEFAULTS };
  } catch {
    return { ...DEFAULT_AUTH_FILE_IMPORT_DEFAULTS };
  }
};

export const writeAuthFileImportDefaults = (defaults: AuthFileImportDefaults) => {
  if (typeof window === 'undefined') return;
  try {
    window.localStorage.setItem(
      AUTH_FILE_IMPORT_DEFAULTS_STORAGE_KEY,
      JSON.stringify(normalizeAuthFileImportDefaults(defaults))
    );
  } catch {
    // Ignore unavailable or full browser storage.
  }
};
