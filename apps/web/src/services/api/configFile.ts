/**
 * 配置文件相关 API（/config.yaml）
 */

import { apiClient } from './client';

export const configFileApi = {
  async fetchConfigYamlWithMetadata(): Promise<{ content: string; lastModified: string | null }> {
    const response = await apiClient.getRaw('/config.yaml', {
      responseType: 'text',
      headers: { Accept: 'application/yaml, text/yaml, text/plain' },
    });
    const data: unknown = response.data;
    const content =
      typeof data === 'string' ? data : data === undefined || data === null ? '' : String(data);
    const headers = response.headers as unknown as {
      get?: (name: string) => string | null | undefined;
      [key: string]: unknown;
    };
    const lastModified =
      (typeof headers?.get === 'function' ? headers.get('last-modified') : undefined) ||
      (typeof headers?.['last-modified'] === 'string' ? headers['last-modified'] : null);
    return { content, lastModified: lastModified || null };
  },

  async fetchConfigYaml(): Promise<string> {
    const response = await apiClient.getRaw('/config.yaml', {
      responseType: 'text',
      headers: { Accept: 'application/yaml, text/yaml, text/plain' }
    });
    const data: unknown = response.data;
    if (typeof data === 'string') return data;
    if (data === undefined || data === null) return '';
    return String(data);
  },

  async saveConfigYaml(content: string): Promise<void> {
    await apiClient.put('/config.yaml', content, {
      headers: {
        'Content-Type': 'application/yaml',
        Accept: 'application/json, text/plain, */*'
      }
    });
  }
};
