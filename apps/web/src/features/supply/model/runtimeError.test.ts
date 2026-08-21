import { describe, expect, it } from 'vitest';
import { isSupplyRuntimeErrorRetrying, localizeSupplyRuntimeError } from './runtimeError';

const translate = (key: string, options?: Record<string, string>) => {
  if (key === 'supply.error_no_eligible_marketplace_seller_with_platform') {
    return `${options?.platform}：当前没有供应商通过自动采购额度门禁`;
  }
  if (key === 'supply.error_no_eligible_marketplace_seller') {
    return '当前没有供应商通过自动采购额度门禁';
  }
  if (key === 'supply.error_low_price_inventory_unavailable') {
    return '低价库存已不可用';
  }
  if (key === 'supply.error_supply_api_retrying_with_platform') {
    return `${options?.platform}：供应商接口暂时不可用（HTTP ${options?.status}），系统正在自动重试`;
  }
  if (key === 'supply.error_supply_api_retrying') {
    return `供应商接口暂时不可用（HTTP ${options?.status}），系统正在自动重试`;
  }
  return key;
};

describe('localizeSupplyRuntimeError', () => {
  it('translates the supplier quota gate error while preserving the platform name', () => {
    expect(
      localizeSupplyRuntimeError(
        'nv: no marketplace seller currently passes the automatic quota gate',
        translate
      )
    ).toBe('nv：当前没有供应商通过自动采购额度门禁');
  });

  it('translates the same error without a platform prefix', () => {
    expect(
      localizeSupplyRuntimeError(
        'no marketplace seller currently passes the automatic quota gate',
        translate
      )
    ).toBe('当前没有供应商通过自动采购额度门禁');
  });

  it('leaves unknown backend errors unchanged', () => {
    expect(localizeSupplyRuntimeError('temporary upstream failure', translate)).toBe(
      'temporary upstream failure'
    );
  });

  it('translates an expired low-price task result', () => {
    expect(
      localizeSupplyRuntimeError('low-price inventory is no longer available', translate)
    ).toBe('低价库存已不可用');
  });

  it('translates transient supplier failures and marks them as retrying', () => {
    const error =
      'nv: supply API returned HTTP 502: The origin web server returned an invalid response';
    expect(localizeSupplyRuntimeError(error, translate)).toBe(
      'nv：供应商接口暂时不可用（HTTP 502），系统正在自动重试'
    );
    expect(isSupplyRuntimeErrorRetrying(error)).toBe(true);
    expect(isSupplyRuntimeErrorRetrying('supply API returned HTTP 401')).toBe(false);
  });
});
