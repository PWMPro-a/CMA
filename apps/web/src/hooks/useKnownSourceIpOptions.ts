import { useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { containerOpsApi, type ContainerOpsEgressIPInventory } from '@/services/api';
import { buildSourceIpSelectOptions, type SourceIpUsageCounts } from '@/utils/sourceIp';

export function useKnownSourceIpOptions({
  usageCounts,
  fallbackValues = [],
  enabled = true,
}: {
  usageCounts?: SourceIpUsageCounts;
  fallbackValues?: ReadonlyArray<unknown>;
  enabled?: boolean;
} = {}) {
  const { t } = useTranslation();
  const [inventory, setInventory] = useState<ContainerOpsEgressIPInventory | null>(null);
  const [loading, setLoading] = useState(Boolean(enabled));

  useEffect(() => {
    if (!enabled) return;

    let cancelled = false;
    queueMicrotask(() => {
      if (!cancelled) setLoading(true);
    });

    void containerOpsApi
      .egressIPs()
      .then((data) => {
        if (!cancelled) setInventory(data);
      })
      .catch(() => {
        if (!cancelled) setInventory(null);
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });

    return () => {
      cancelled = true;
    };
  }, [enabled]);

  const activeInventory = enabled ? inventory : null;

  const options = useMemo(
    () =>
      buildSourceIpSelectOptions({
        inventory: activeInventory,
        usageCounts,
        fallbackValues,
        t,
      }),
    [activeInventory, fallbackValues, t, usageCounts]
  );

  return { inventory: activeInventory, loading: enabled && loading, options };
}
