export interface PurchasePlatformLike {
  id?: string;
  name?: string;
  type?: string;
  product?: string;
}

export interface PurchaseOrderLike {
  orderId?: string;
  supplierId?: string;
  product?: string;
}

const normalize = (value?: string) => value?.trim().toLowerCase() ?? '';

const platformLabel = (platform: PurchasePlatformLike) =>
  platform.name?.trim() || platform.id?.trim() || '';

const findByType = (platforms: PurchasePlatformLike[], type: string) =>
  platforms.find((platform) => normalize(platform.type) === type);

const uniquePlatforms = (platforms: PurchasePlatformLike[]) => {
  const seen = new Set<string>();
  return platforms.filter((platform) => {
    const key =
      normalize(platform.id) ||
      [normalize(platform.type), normalize(platform.product), normalize(platform.name)].join('|');
    if (!key || seen.has(key)) return false;
    seen.add(key);
    return true;
  });
};

export const resolvePurchasePlatformLabel = (
  order: PurchaseOrderLike,
  platforms: PurchasePlatformLike[] = []
) => {
  platforms = uniquePlatforms(platforms);
  const supplierId = normalize(order.supplierId);
  if (supplierId) {
    const exact = platforms.find((platform) => normalize(platform.id) === supplierId);
    return platformLabel(exact ?? { id: order.supplierId }) || '-';
  }

  const orderId = normalize(order.orderId);
  const inferredType = orderId.startsWith('cus_')
    ? 'bugteam'
    : /^\d+$/.test(order.orderId?.trim() ?? '')
      ? 'legacy'
      : '';
  const product = normalize(order.product);
  const productMatches = product
    ? platforms.filter((platform) => normalize(platform.product) === product)
    : [];
  const inferred = inferredType
    ? (findByType(productMatches, inferredType) ?? findByType(platforms, inferredType))
    : undefined;
  if (inferred) return platformLabel(inferred) || '-';
  if (productMatches.length === 1) return platformLabel(productMatches[0]) || '-';

  if (inferredType === 'bugteam') return 'BugTeam';
  if (inferredType === 'legacy') return 'Legacy';
  return '-';
};
