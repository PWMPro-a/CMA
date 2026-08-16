import type { ManagerLatestRelease } from '@/services/api/version';

type VersionPayload = Record<string, unknown> | undefined | null;

const compactDeploymentVersionPattern = /^\d{8}-\d{6}$/;

export const resolveManagerPanelVersion = (
  compiledPanelVersion: string | null | undefined,
  managerServerVersion: string | null | undefined
): string => {
  const compiled = compiledPanelVersion?.trim() ?? '';
  const server = managerServerVersion?.trim() ?? '';
  if (compactDeploymentVersionPattern.test(compiled)) return compiled;
  if (compactDeploymentVersionPattern.test(server)) return server;
  return compiled || server;
};

export const readManagerLatestTag = (data: ManagerLatestRelease | VersionPayload): string => {
  if (!data) return '';
  const raw = data.tag_name ?? data.name ?? data.latest_version ?? data.latest;
  return typeof raw === 'string' ? raw : raw == null ? '' : String(raw);
};

export const readApiLatestVersion = (data: VersionPayload): string => {
  if (!data) return '';
  const raw = data['latest-version'] ?? data.latest_version ?? data.latest;
  return typeof raw === 'string' ? raw : raw == null ? '' : String(raw);
};
