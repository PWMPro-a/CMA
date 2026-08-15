import { DEFAULT_API_PORT, MANAGEMENT_API_PREFIX } from './constants';

export const DEFAULT_DOCKER_CPA_BASE_URL = 'http://host.docker.internal:8317';

export const normalizeApiBase = (input: string): string => {
  let base = (input || '').trim();
  if (!base) return '';
  // Users commonly paste the full panel URL (including its hash route) into
  // the connection field. Keeping `/management.html` in the API base makes
  // requests resolve to `/management.html/v0/management/...`, so the UI keeps
  // rendering its last in-memory account snapshot after every 404. Normalize
  // both current and persisted legacy values back to the panel origin/path.
  base = base.replace(/[?#].*$/, '');
  base = base.replace(/\/management\.html\/?$/i, '');
  base = base.replace(/\/?v0\/management\/?$/i, '');
  base = base.replace(/\/+$/i, '');
  if (!/^https?:\/\//i.test(base)) {
    base = `http://${base}`;
  }
  return base;
};

export const computeApiUrl = (base: string): string => {
  const normalized = normalizeApiBase(base);
  if (!normalized) return '';
  return `${normalized}${MANAGEMENT_API_PREFIX}`;
};

const readEnvDefaultCPAConnectionBase = (): string => {
  try {
    return import.meta.env.VITE_DEFAULT_CPA_BASE_URL || '';
  } catch {
    return '';
  }
};

export const resolveDefaultCPAConnectionBase = (options?: {
  hostedByUsageService?: boolean;
  currentBase?: string;
  envDefault?: string;
}): string => {
  const envDefault = normalizeApiBase(
    options?.envDefault === undefined ? readEnvDefaultCPAConnectionBase() : options.envDefault
  );
  if (envDefault) return envDefault;

  if (options?.hostedByUsageService) {
    return DEFAULT_DOCKER_CPA_BASE_URL;
  }

  return normalizeApiBase(options?.currentBase || '');
};

export const detectApiBaseFromLocation = (): string => {
  try {
    const { protocol, hostname, port } = window.location;
    const normalizedPort = port ? `:${port}` : '';
    return normalizeApiBase(`${protocol}//${hostname}${normalizedPort}`);
  } catch (error) {
    console.warn('Failed to detect api base from location, fallback to default', error);
    return normalizeApiBase(`http://localhost:${DEFAULT_API_PORT}`);
  }
};

export const isLocalhost = (hostname: string): boolean => {
  const value = (hostname || '').toLowerCase();
  return value === 'localhost' || value === '127.0.0.1' || value === '[::1]';
};
