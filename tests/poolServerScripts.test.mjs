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

describe('pool-server deployment template', () => {
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

  it('renders valid Compose interpolation when Docker Compose is available', () => {
    const probe = spawnSync('docker', ['compose', 'version'], { encoding: 'utf8' });
    if (probe.status !== 0) return;

    const dir = copyFixture();
    try {
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
