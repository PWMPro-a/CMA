import type { AuthFileImportMetadata, AuthFileImportMethod, AuthFileItem } from '@/types';
import type { AuthJsonInputType } from '@/features/authFiles/sessionAuthConverter';

type JsonRecord = Record<string, unknown>;

export type AuthFileImportPlatform = {
  id: string;
  name: string;
};

const MANUAL_IMPORT_PLATFORMS: Record<AuthJsonInputType, AuthFileImportPlatform> = {
  cpa: { id: 'cpa', name: 'CPA 文件' },
  session: { id: 'chatgpt_session', name: 'ChatGPT Session' },
  sub2api: { id: 'sub2api', name: 'Sub2API' },
};

const readString = (value: unknown): string => (typeof value === 'string' ? value.trim() : '');

const readVersion = (value: unknown): number => {
  if (typeof value === 'number' && Number.isFinite(value) && value > 0) {
    return Math.floor(value);
  }
  if (typeof value === 'string') {
    const parsed = Number(value.trim());
    if (Number.isFinite(parsed) && parsed > 0) return Math.floor(parsed);
  }
  return 1;
};

const isRecord = (value: unknown): value is JsonRecord =>
  Boolean(value) && typeof value === 'object' && !Array.isArray(value);

export const getManualAuthFileImportPlatform = (type: AuthJsonInputType): AuthFileImportPlatform =>
  MANUAL_IMPORT_PLATFORMS[type];

export const buildAuthFileImportMetadata = (options: {
  method: AuthFileImportMethod;
  platform: AuthFileImportPlatform;
  importedAt?: Date;
}): AuthFileImportMetadata => ({
  version: 1,
  source: 'manual',
  method: options.method,
  platform_id: options.platform.id,
  platform_name: options.platform.name,
  imported_by: 'cpa-manager-plus',
  imported_at: (options.importedAt ?? new Date()).toISOString(),
});

export const withAuthFileImportMetadata = (
  authJson: JsonRecord,
  metadata: AuthFileImportMetadata
): JsonRecord => ({
  ...authJson,
  cpamp_import: metadata,
});

export const readAuthFileImportMetadata = (file: AuthFileItem): AuthFileImportMetadata | null => {
  const raw = file.cpamp_import ?? file.cpampImport;
  if (!isRecord(raw)) return null;

  const source = readString(raw.source);
  const method = readString(raw.method);
  const platformId = readString(raw.platform_id ?? raw.platformId);
  const platformName = readString(raw.platform_name ?? raw.platformName);
  const importedBy = readString(raw.imported_by ?? raw.importedBy);
  const importedAt = readString(raw.imported_at ?? raw.importedAt);
  if (!source || !method || (!platformId && !platformName)) return null;

  return {
    version: readVersion(raw.version),
    source,
    method,
    platform_id: platformId,
    platform_name: platformName,
    imported_by: importedBy,
    imported_at: importedAt,
  };
};

export const getAuthFileImportMethodLabelKey = (method: string): string => {
  switch (method.trim().toLowerCase()) {
    case 'file_upload':
      return 'accounts.import_method_file_upload';
    case 'json_paste':
      return 'accounts.import_method_json_paste';
    case 'automatic_supply':
      return 'accounts.import_method_automatic_supply';
    case 'manual_supply':
      return 'accounts.import_method_manual_supply';
    case 'reauth_replacement':
      return 'accounts.import_method_reauth_replacement';
    default:
      return 'accounts.import_method_unknown';
  }
};
