export type SupplyRuntimeErrorTranslator = (
  key: string,
  options?: Record<string, string>
) => string;

const NO_ELIGIBLE_MARKETPLACE_SELLER =
  'no marketplace seller currently passes the automatic quota gate';
const LOW_PRICE_INVENTORY_UNAVAILABLE = 'low-price inventory is no longer available';
const TEMPORARY_SUPPLY_API_ERROR = /supply API returned HTTP\s+(\d{3})\b/i;

const temporarySupplyAPIStatus = (value: string | null | undefined): string => {
  const status = Number(value?.match(TEMPORARY_SUPPLY_API_ERROR)?.[1] ?? 0);
  if (status === 408 || status === 425 || status === 429 || status >= 500) {
    return String(status);
  }
  return '';
};

export const isSupplyRuntimeErrorRetrying = (value: string | null | undefined): boolean =>
  Boolean(temporarySupplyAPIStatus(value));

export const localizeSupplyRuntimeError = (
  value: string | null | undefined,
  translate: SupplyRuntimeErrorTranslator
): string => {
  const message = value?.trim() ?? '';
  if (!message) return '';

  const temporaryStatus = temporarySupplyAPIStatus(message);
  if (temporaryStatus) {
    const markerIndex = message.search(TEMPORARY_SUPPLY_API_ERROR);
    const platform = message
      .slice(0, markerIndex)
      .replace(/[：:]\s*$/, '')
      .trim();
    if (platform) {
      return translate('supply.error_supply_api_retrying_with_platform', {
        platform,
        status: temporaryStatus,
      });
    }
    return translate('supply.error_supply_api_retrying', { status: temporaryStatus });
  }

  if (message.toLowerCase().includes(LOW_PRICE_INVENTORY_UNAVAILABLE)) {
    return translate('supply.error_low_price_inventory_unavailable');
  }

  const markerIndex = message.toLowerCase().indexOf(NO_ELIGIBLE_MARKETPLACE_SELLER);
  if (markerIndex < 0) return message;

  const platform = message
    .slice(0, markerIndex)
    .replace(/[：:]\s*$/, '')
    .trim();
  if (platform) {
    return translate('supply.error_no_eligible_marketplace_seller_with_platform', { platform });
  }
  return translate('supply.error_no_eligible_marketplace_seller');
};
