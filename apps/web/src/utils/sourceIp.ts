import type { SelectOption } from '@/components/ui/Select';
import type { ContainerOpsEgressIPInventory } from '@/services/api';

export type SourceIpUsageCounts = Readonly<Record<string, number>>;

export type SourceIpTranslateFn = (key: string, options?: Record<string, unknown>) => string;

const normalizeSourceIp = (value: unknown): string => String(value ?? '').trim();

export const collectSourceIpUsageCounts = (
  values: ReadonlyArray<unknown>
): Record<string, number> =>
  values.reduce<Record<string, number>>((counts, value) => {
    const sourceIp = normalizeSourceIp(value);
    if (!sourceIp) return counts;
    counts[sourceIp] = (counts[sourceIp] ?? 0) + 1;
    return counts;
  }, {});

const formatSourceIpAccountCount = (count: number, t: SourceIpTranslateFn): string => {
  return t('auth_files.source_ip_bound_accounts', { count });
};

const parseIPv4 = (value: string): [number, number, number, number] | null => {
  const parts = value.split('.');
  if (parts.length !== 4) return null;

  const octets = parts.map((part) => {
    if (!/^\d+$/.test(part)) return Number.NaN;
    return Number(part);
  });
  if (octets.some((octet) => !Number.isInteger(octet) || octet < 0 || octet > 255)) return null;
  return octets as [number, number, number, number];
};

const isPublicIPv4 = (value: string): boolean => {
  const octets = parseIPv4(value);
  if (!octets) return false;
  const [first, second, third] = octets;

  if (first === 0) return false;
  if (first === 10) return false;
  if (first === 100 && second >= 64 && second <= 127) return false;
  if (first === 127) return false;
  if (first === 169 && second === 254) return false;
  if (first === 172 && second >= 16 && second <= 31) return false;
  if (first === 192 && second === 168) return false;
  if (first === 198 && (second === 18 || second === 19)) return false;
  if (first === 224 || first >= 240) return false;
  if (first === 192 && second === 0 && third === 0) return false;

  return true;
};

const pushUniqueSourceIp = (
  options: SelectOption[],
  seen: Set<string>,
  value: string,
  label: string
) => {
  if (!value || seen.has(value)) return;
  seen.add(value);
  options.push({ value, label });
};

export const buildSourceIpSelectOptions = ({
  inventory,
  usageCounts,
  fallbackValues = [],
  t,
}: {
  inventory?: Pick<ContainerOpsEgressIPInventory, 'nativeOutboundIp' | 'addresses'> | null;
  usageCounts?: SourceIpUsageCounts;
  fallbackValues?: ReadonlyArray<unknown>;
  t: SourceIpTranslateFn;
}): SelectOption[] => {
  const options: SelectOption[] = [{ value: '', label: t('common.not_set') }];
  const seen = new Set<string>(['']);
  const counts = usageCounts ?? {};

  const appendCountedIp = (value: unknown) => {
    const sourceIp = normalizeSourceIp(value);
    if (!sourceIp || seen.has(sourceIp) || !isPublicIPv4(sourceIp)) return;
    pushUniqueSourceIp(
      options,
      seen,
      sourceIp,
      `${sourceIp} · ${formatSourceIpAccountCount(counts[sourceIp] ?? 0, t)}`
    );
  };

  if (inventory?.nativeOutboundIp) {
    appendCountedIp(inventory.nativeOutboundIp);
  }

  inventory?.addresses
    ?.filter((address) => address?.scope !== 'host')
    .forEach((address) => appendCountedIp(address?.address));

  fallbackValues.forEach((value) => {
    const sourceIp = normalizeSourceIp(value);
    if (!sourceIp || seen.has(sourceIp)) return;
    pushUniqueSourceIp(options, seen, sourceIp, `${sourceIp} · ${t('auth_files.source_ip_current_option')}`);
  });

  return options;
};
