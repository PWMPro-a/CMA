export type SupplyRuntimeErrorTranslator = (
  key: string,
  options?: Record<string, string>
) => string;

const NO_ELIGIBLE_MARKETPLACE_SELLER =
  'no marketplace seller currently passes the automatic quota gate';
const LOW_PRICE_INVENTORY_UNAVAILABLE = 'low-price inventory is no longer available';

export const localizeSupplyRuntimeError = (
  value: string | null | undefined,
  translate: SupplyRuntimeErrorTranslator
): string => {
  const message = value?.trim() ?? '';
  if (!message) return '';

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
