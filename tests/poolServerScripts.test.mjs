import { spawnSync } from 'node:child_process';
import {
  chmodSync,
  existsSync,
  mkdtempSync,
  readFileSync,
  realpathSync,
  rmSync,
  writeFileSync,
  mkdirSync,
  copyFileSync,
  statSync,
} from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const sourceDir = path.join(repoRoot, 'deploy/pool-server');
const shellFiles = ['bootstrap.sh', 'preflight.sh'];

const copyFixture = () => {
  const dir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-pool-server-'));
  for (const name of [
    '.env.example',
    '.gitignore',
    'compose.yml',
    'config.yaml.template',
    'bootstrap.sh',
    'preflight.sh',
  ]) {
    copyFileSync(path.join(sourceDir, name), path.join(dir, name));
  }
  chmodSync(path.join(dir, 'bootstrap.sh'), 0o755);
  chmodSync(path.join(dir, 'preflight.sh'), 0o755);
  return dir;
};

const run = (dir, script, args = [], env = {}) =>
  spawnSync('bash', [path.join(dir, script), ...args], {
    cwd: dir,
    env: { ...process.env, ...env },
    encoding: 'utf8',
  });

const writeStorefrontSecret = (dir, value = 'storefront-issued-secret-for-test') => {
  const secretDir = path.join(dir, 'secrets');
  mkdirSync(secretDir, { recursive: true, mode: 0o700 });
  const secretFile = path.join(secretDir, 'cpa-license-client-secret');
  writeFileSync(secretFile, `${value}\n`, { mode: 0o600 });
  chmodSync(secretFile, 0o600);
  return realpathSync(secretFile);
};

describe('pool-server deployment template', () => {
  it('passes the configured Agent port into the Agent container and healthcheck', () => {
    const compose = readFileSync(path.join(sourceDir, 'compose.yml'), 'utf8');
    expect(compose).toContain('CPAMP_AGENT_ADDR: "0.0.0.0:${CPAMP_AGENT_PORT:-18417}"');
    expect(compose).toContain('CPAMP_AGENT_PORT: "${CPAMP_AGENT_PORT:-18417}"');
    expect(compose).toContain('127.0.0.1:$${CPAMP_AGENT_PORT:-18417}/agent/info');
    expect(compose).toContain('test: ["CMD-SHELL", "wget -qO- http://127.0.0.1:$${CPAMP_INTERNAL_PORT:-18317}/health >/dev/null"]');
  });

  it.each(shellFiles)('%s passes shell syntax validation', (name) => {
    const result = spawnSync('bash', ['-n', path.join(sourceDir, name)], {
      cwd: repoRoot,
      encoding: 'utf8',
    });
    expect(result.status).toBe(0);
    expect(result.stderr).toBe('');
  });

  it('renders a first-install stack with absolute paths and preserves CPA defaults', () => {
    const dir = copyFixture();
    try {
      const secretFile = writeStorefrontSecret(dir);
      const result = run(dir, 'bootstrap.sh', ['--render']);
      expect(result.status).toBe(0);
      expect(result.stdout).toContain('Pool stack files are ready');

      const env = readFileSync(path.join(dir, '.env'), 'utf8');
      const canonicalDir = realpathSync(dir);
      expect(env).toContain(`CPA_DATA_DIR=${canonicalDir}/data/cpa`);
      expect(env).toContain(`CPAMP_STACK_ROOT=${canonicalDir}`);
      expect(env).toContain('CPA_IMAGE=ghcr.io/abc124774961/cli-proxy-api-cpa:v7.2.148-cpa.3');
      expect(env).toContain('CPAMP_IMAGE=ghcr.io/seakee/cpa-manager-plus:v1.12.8');
      expect(env).toMatch(/^CPAMP_AGENT_TOKEN=(?!replace-with)/m);
      expect(env).toMatch(/^CPA_MANAGEMENT_KEY=(?!replace-with)/m);
      expect(env).toMatch(/^CPA_MANAGER_ADMIN_KEY=(?!replace-with)/m);
      expect(env).toContain(`CPA_LICENSE_CLIENT_SECRET_HOST_PATH=${secretFile}`);
      expect(env).toMatch(/^CPA_LICENSE_CLIENT_SECRET=$/m);
      expect(env).toMatch(/^CPA_LICENSE_CLIENT_SECRET_FILE=$/m);
      expect(readFileSync(secretFile, 'utf8')).toBe('storefront-issued-secret-for-test\n');
      expect(statSync(secretFile).mode & 0o777).toBe(0o600);

      const config = readFileSync(path.join(dir, 'data/cpa/config.yaml'), 'utf8');
      expect(config).toContain('secret-key: "cpa_');
      expect(config).toContain('quota-hard-stop-used-ratio: 0.99');
      expect(config).toContain('tail-burst:');
      expect(config).not.toMatch(/__CPA_[A-Z0-9_]+__/);

      const preflight = run(dir, 'preflight.sh', ['--skip-docker']);
      expect(preflight.status).toBe(0);
      expect(preflight.stdout).toContain('Preflight passed');
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('does not overwrite an existing CPA config during a rerun', () => {
    const dir = copyFixture();
    try {
      writeStorefrontSecret(dir);
      mkdirSync(path.join(dir, 'data/cpa'), { recursive: true });
      const sentinel = 'existing-config-sentinel\n';
      writeFileSync(path.join(dir, 'data/cpa/config.yaml'), sentinel);
      const result = run(dir, 'bootstrap.sh', ['--render']);
      expect(result.status).toBe(0);
      expect(readFileSync(path.join(dir, 'data/cpa/config.yaml'), 'utf8')).toBe(sentinel);
      expect(result.stdout).toContain('Existing CPA config preserved');
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('dry-run leaves the fixture untouched', () => {
    const dir = copyFixture();
    try {
      const result = run(dir, 'bootstrap.sh', ['--dry-run']);
      expect(result.status).toBe(0);
      expect(result.stdout).toContain('Dry-run enabled');
      expect(existsSync(path.join(dir, '.env'))).toBe(false);
      expect(existsSync(path.join(dir, 'data'))).toBe(false);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('blocks an external storefront install until a matching secret file is injected', () => {
    const dir = copyFixture();
    try {
      const result = run(dir, 'bootstrap.sh', ['--render']);
      expect(result.status).not.toBe(0);
      expect(`${result.stdout}\n${result.stderr}`).toContain('Storefront client secret is required');
      expect(`${result.stdout}\n${result.stderr}`).toContain('cpa-license-client-secret');
      expect(`${result.stdout}\n${result.stderr}`).not.toContain('storefront-issued-secret-for-test');
      expect(existsSync(path.join(dir, 'secrets/cpa-license-client-secret'))).toBe(false);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('migrates a legacy direct secret into the mode-600 file without logging it', () => {
    const dir = copyFixture();
    const secret = 'legacy-storefront-secret-for-test';
    try {
      const env = readFileSync(path.join(dir, '.env.example'), 'utf8').replace(
        /^CPA_LICENSE_CLIENT_SECRET=$/m,
        `CPA_LICENSE_CLIENT_SECRET=${secret}`
      );
      writeFileSync(path.join(dir, '.env'), env, { mode: 0o600 });
      const result = run(dir, 'bootstrap.sh', ['--render']);
      expect(result.status).toBe(0);
      const secretFile = path.join(dir, 'secrets/cpa-license-client-secret');
      expect(readFileSync(secretFile, 'utf8')).toBe(`${secret}\n`);
      expect(statSync(secretFile).mode & 0o777).toBe(0o600);
      const renderedEnv = readFileSync(path.join(dir, '.env'), 'utf8');
      expect(renderedEnv).toMatch(/^CPA_LICENSE_CLIENT_SECRET=$/m);
      expect(`${result.stdout}\n${result.stderr}`).not.toContain(secret);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('rejects an empty or permissive storefront secret in preflight', () => {
    const dir = copyFixture();
    try {
      const bootstrap = run(dir, 'bootstrap.sh', ['--render']);
      expect(bootstrap.status).not.toBe(0);
      const secretFile = writeStorefrontSecret(dir, '');
      chmodSync(secretFile, 0o644);
      const rejected = run(dir, 'preflight.sh', ['--skip-docker']);
      expect(rejected.status).not.toBe(0);
      expect(rejected.stderr).toContain('mode 600');
      expect(rejected.stderr).toContain('matching storefront-issued secret');

      writeFileSync(secretFile, 'storefront-issued-secret-for-test\n');
      chmodSync(secretFile, 0o600);
      const accepted = run(dir, 'preflight.sh', ['--skip-docker']);
      expect(accepted.status).toBe(0);
      expect(accepted.stdout).toContain('Preflight passed');
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('keeps local/non-storefront providers compatible without a client secret', () => {
    const dir = copyFixture();
    try {
      const envExample = readFileSync(path.join(dir, '.env.example'), 'utf8')
        .replace('CPA_LICENSE_PROVIDER=shop666', 'CPA_LICENSE_PROVIDER=local')
        .replace('CPA_LICENSE_API_BASE_URL=https://p.666ttt.net/api/storefront', 'CPA_LICENSE_API_BASE_URL=http://127.0.0.1:9999/api');
      writeFileSync(path.join(dir, '.env.example'), envExample);
      const result = run(dir, 'bootstrap.sh', ['--render']);
      expect(result.status).toBe(0);
      const secretFile = path.join(dir, 'secrets/cpa-license-client-secret');
      expect(existsSync(secretFile)).toBe(true);
      expect(readFileSync(secretFile, 'utf8')).toBe('');
      expect(statSync(secretFile).mode & 0o777).toBe(0o600);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('renders valid Compose interpolation when Docker Compose is available', () => {
    const probe = spawnSync('docker', ['compose', 'version'], { encoding: 'utf8' });
    if (probe.status !== 0) return;

    const dir = copyFixture();
    try {
      writeStorefrontSecret(dir);
      const bootstrap = run(dir, 'bootstrap.sh', ['--render']);
      expect(bootstrap.status).toBe(0);
      const compose = spawnSync(
        'docker',
        ['compose', '--env-file', path.join(dir, '.env'), '-f', path.join(dir, 'compose.yml'), 'config', '--quiet'],
        { cwd: dir, encoding: 'utf8' }
      );
      expect(compose.status).toBe(0);
      expect(compose.stderr).toBe('');
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});
