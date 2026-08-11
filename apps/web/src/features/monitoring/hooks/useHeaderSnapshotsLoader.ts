import { useCallback, useLayoutEffect, useRef } from 'react';
import {
  monitoringAnalyticsApi,
  type UsageHeaderSnapshotsResponse,
} from '@/services/api/usageService';
import {
  buildUsageHeaderSnapshotScopeKey,
  useUsageHeaderSnapshotStore,
} from '@/stores/useUsageHeaderSnapshotStore';

interface UseHeaderSnapshotsLoaderOptions {
  serviceBase: string;
  managementKey: string;
  onResponse?: (response: UsageHeaderSnapshotsResponse) => void;
  onReset?: () => void;
}

interface HeaderSnapshotsRequest {
  serviceBase: string;
  managementKey: string;
  controller: AbortController;
  promise: Promise<void>;
}

export function useHeaderSnapshotsLoader({
  serviceBase,
  managementKey,
  onResponse,
  onReset,
}: UseHeaderSnapshotsLoaderOptions) {
  const scopeKey = buildUsageHeaderSnapshotScopeKey(serviceBase, managementKey);
  const inFlightRef = useRef<HeaderSnapshotsRequest | null>(null);
  const onResponseRef = useRef(onResponse ?? null);
  const onResetRef = useRef(onReset ?? null);
  const scopeVersionRef = useRef(0);
  const initializedScopeRef = useRef(false);
  onResponseRef.current = onResponse ?? null;
  onResetRef.current = onReset ?? null;

  useLayoutEffect(() => {
    scopeVersionRef.current += 1;
    inFlightRef.current?.controller.abort();
    inFlightRef.current = null;
    const scopeChanged = useUsageHeaderSnapshotStore.getState().activateScope(scopeKey);
    if (scopeChanged && initializedScopeRef.current) {
      onResetRef.current?.();
    }
    initializedScopeRef.current = true;
    return () => {
      scopeVersionRef.current += 1;
      inFlightRef.current?.controller.abort();
      inFlightRef.current = null;
    };
  }, [scopeKey]);

  return useCallback(async () => {
    if (!serviceBase) {
      inFlightRef.current?.controller.abort();
      inFlightRef.current = null;
      useUsageHeaderSnapshotStore.getState().activateScope('');
      onResetRef.current?.();
      return;
    }
    const currentRequest = inFlightRef.current;
    if (
      currentRequest?.serviceBase === serviceBase &&
      currentRequest.managementKey === managementKey
    ) {
      await currentRequest.promise;
      return;
    }

    const scopeVersion = scopeVersionRef.current;
    const controller = new AbortController();
    const inFlight: HeaderSnapshotsRequest = {
      serviceBase,
      managementKey,
      controller,
      promise: Promise.resolve(),
    };
    inFlight.promise = (async () => {
      try {
        const response = await monitoringAnalyticsApi.getHeaderSnapshots(
          serviceBase,
          managementKey,
          { days: 30, limit: 1000 },
          controller.signal
        );
        if (
          inFlightRef.current === inFlight &&
          scopeVersionRef.current === scopeVersion &&
          !controller.signal.aborted
        ) {
          const committed = useUsageHeaderSnapshotStore
            .getState()
            .commitResponse(scopeKey, response);
          if (committed) onResponseRef.current?.(response);
        }
      } catch {
        // Preserve the last successful snapshot when a refresh fails.
      } finally {
        if (inFlightRef.current === inFlight) {
          inFlightRef.current = null;
        }
      }
    })();
    inFlightRef.current = inFlight;
    await inFlight.promise;
  }, [managementKey, scopeKey, serviceBase]);
}
