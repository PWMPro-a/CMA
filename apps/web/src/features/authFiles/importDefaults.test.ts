import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  DEFAULT_AUTH_FILE_IMPORT_DEFAULTS,
  readAuthFileImportDefaults,
  writeAuthFileImportDefaults,
} from './importDefaults';

const createStorage = () => {
  const values = new Map<string, string>();
  return {
    getItem: (key: string) => values.get(key) ?? null,
    setItem: (key: string, value: string) => values.set(key, value),
  };
};

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('auth file import defaults persistence', () => {
  it('enables WebSocket imports by default', () => {
    vi.stubGlobal('window', { localStorage: createStorage() });

    expect(readAuthFileImportDefaults()).toEqual(DEFAULT_AUTH_FILE_IMPORT_DEFAULTS);
  });

  it('persists an operator override', () => {
    const localStorage = createStorage();
    vi.stubGlobal('window', { localStorage });

    writeAuthFileImportDefaults({ websockets: false });

    expect(readAuthFileImportDefaults()).toEqual({ websockets: false });
  });
});
