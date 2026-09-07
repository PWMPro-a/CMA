import { spawnSync } from 'node:child_process';
import {
  chmodSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  rmSync,
  statSync,
  writeFileSync,
} from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const installerPath = path.join(repoRoot, 'bin/install-cpamp.sh');

const combinedOutput = (result) => `${result.stdout}\n${result.stderr}`;

const runInstaller = (env) =>
  spawnSync('bash', [installerPath], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CPAMP_DRY_RUN: '1',
      CPAMP_NON_INTERACTIVE: '1',
      CPAMP_LANG: 'en-US',
      CPAMP_INSTALL_DIR: '/tmp/cpamp-installer-test',
      ...env,
    },
    encoding: 'utf8',
  });

const runInstallerFromStdin = (env) =>
  spawnSync('bash', ['-c', `bash < ${JSON.stringify(installerPath)}`], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CPAMP_DRY_RUN: '1',
      CPAMP_INSTALL_DIR: '/tmp/cpamp-installer-test',
      ...env,
    },
    encoding: 'utf8',
  });

const writeFakeDocker = (dir) => {
  const fakeDocker = path.join(dir, 'docker');
  writeFileSync(
    fakeDocker,
    `#!/usr/bin/env bash
set -eu
if [ -n "\${FAKE_DOCKER_LOG:-}" ]; then
  printf '%s|%s\n' "\${COMPOSE_PROJECT_NAME:-}" "$*" >> "$FAKE_DOCKER_LOG"
fi
if [ "$1" = "volume" ] && [ "\${2:-}" = "inspect" ]; then
  if [ "\${FAKE_DOCKER_VOLUME_EXISTS:-0}" = "1" ]; then
    exit 0
  fi
  exit 1
fi
if [ "$1" = "info" ] && [ "\${FAKE_DOCKER_DAEMON_OK:-1}" != "1" ]; then
  exit 1
fi
if [ "$1" = "compose" ] && [[ " $* " == *" exec "* ]]; then
  case "$*" in
    *'/status'*)
      if [ "\${FAKE_DOCKER_AUTH_OK:-1}" = "1" ]; then
        exit 0
      fi
      exit 1
      ;;
    *) exit 0 ;;
  esac
fi
exit 0
`
  );
  chmodSync(fakeDocker, 0o755);
  return fakeDocker;
};

describe('installer script', () => {
  it('passes shell syntax validation', () => {
    const result = spawnSync('bash', ['-n', installerPath], {
      cwd: repoRoot,
      encoding: 'utf8',
    });

    expect(result.status).toBe(0);
    expect(result.stderr).toBe('');
  });

  it('refuses interactive execution when stdin is not a terminal', () => {
    const result = runInstallerFromStdin({});

    expect(result.status).toBe(1);
    expect(combinedOutput(result)).toContain('Interactive install requires a terminal on stdin');
  });

  it('keeps explicit non-interactive stdin execution available', () => {
    const result = runInstallerFromStdin({
      CPAMP_NON_INTERACTIVE: '1',
      CPAMP_LANG: 'en-US',
      CPAMP_INSTALL_MODE: 'stack',
      CPAMP_DEPLOY_METHOD: 'docker',
    });

    expect(result.status).toBe(0);
    expect(result.stdout).toContain('Install scope: CPA + CPAMP stack');
    expect(result.stdout).toMatch(/docker compose .* pull/);
  });

  it('prints a full Docker stack dry-run plan', () => {
    const result = runInstaller({
      CPAMP_INSTALL_MODE: 'stack',
      CPAMP_DEPLOY_METHOD: 'docker',
    });

    expect(result.status).toBe(0);
    expect(result.stdout).toContain('Install scope: CPA + CPAMP stack');
    expect(result.stdout).toContain('CPA URL for CPAMP: http://host.docker.internal:8317');
    expect(result.stdout).toMatch(/docker compose .* pull/);
    expect(result.stdout).toContain('Dry-run plan completed');
  });

  it('keeps CPAMP-only non-interactive installs in first-setup mode by default', () => {
    const result = runInstaller({
      CPAMP_INSTALL_MODE: 'cpamp',
      CPAMP_DEPLOY_METHOD: 'docker',
    });

    expect(result.status).toBe(0);
    expect(result.stdout).toContain('CPA connection: enter during first setup');
    expect(result.stdout).toContain('Dry-run plan completed');
  });

  it('rejects native full stack installs', () => {
    const result = runInstaller({
      CPAMP_INSTALL_MODE: 'stack',
      CPAMP_DEPLOY_METHOD: 'native',
    });
    const output = `${result.stdout}\n${result.stderr}`;

    expect(result.status).toBe(1);
    expect(output).toContain('Native stack install is not supported yet');
  });

  it('rejects CPA URLs that would inject extra env lines', () => {
    const result = runInstaller({
      CPAMP_INSTALL_MODE: 'cpamp',
      CPAMP_DEPLOY_METHOD: 'docker',
      CPAMP_CPA_CONNECTION_MODE: 'env',
      CPAMP_CPA_URL: 'http://host.docker.internal:8317\nCPA_MANAGER_ADMIN_KEY=bad',
      CPAMP_CPA_MANAGEMENT_KEY: 'cpa_existing_management_key',
    });

    expect(result.status).toBe(1);
    expect(combinedOutput(result)).toContain('CPA URL must be a single line');
  });

  it('rejects CPA URLs with URL fragments', () => {
    const result = runInstaller({
      CPAMP_INSTALL_MODE: 'cpamp',
      CPAMP_DEPLOY_METHOD: 'docker',
      CPAMP_CPA_CONNECTION_MODE: 'env',
      CPAMP_CPA_URL: 'http://host.docker.internal:8317#fragment',
      CPAMP_CPA_MANAGEMENT_KEY: 'cpa_existing_management_key',
    });

    expect(result.status).toBe(1);
    expect(combinedOutput(result)).toContain('CPA URL contains unsupported characters');
  });

  it('rejects CPA URLs with query strings', () => {
    const result = runInstaller({
      CPAMP_INSTALL_MODE: 'cpamp',
      CPAMP_DEPLOY_METHOD: 'docker',
      CPAMP_CPA_CONNECTION_MODE: 'env',
      CPAMP_CPA_URL: 'http://host.docker.internal:8317?x=y',
      CPAMP_CPA_MANAGEMENT_KEY: 'cpa_existing_management_key',
    });

    expect(result.status).toBe(1);
    expect(combinedOutput(result)).toContain('CPA URL contains unsupported characters');
  });

  it('rejects Docker image references with compose interpolation syntax', () => {
    const result = runInstaller({
      CPAMP_INSTALL_MODE: 'stack',
      CPAMP_DEPLOY_METHOD: 'docker',
      CPAMP_IMAGE: 'seakee/cpa-manager-plus:${BAD}',
    });

    expect(result.status).toBe(1);
    expect(combinedOutput(result)).toContain('CPAMP Docker image contains unsupported characters');
  });

  it('rejects Docker Compose project names that Docker would normalize differently', () => {
    const result = runInstaller({
      CPAMP_PROJECT_NAME: 'CPAMP.Bad',
      CPAMP_INSTALL_MODE: 'stack',
      CPAMP_DEPLOY_METHOD: 'docker',
    });

    expect(result.status).toBe(1);
    expect(combinedOutput(result)).toContain(
      'Docker Compose project name contains unsupported characters'
    );
  });

  it('rejects an empty persisted Docker Compose project name', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));

    try {
      mkdirSync(path.join(installDir, 'secrets'), { recursive: true });
      writeFileSync(path.join(installDir, '.env'), 'COMPOSE_PROJECT_NAME=\nCPAMP_PORT=18317\n');
      writeFileSync(
        path.join(installDir, 'compose.yaml'),
        'services:\n  cpa-manager-plus:\n    image: example/cpamp:v1\n'
      );
      writeFileSync(path.join(installDir, 'secrets/cpamp-admin-key'), 'cpamp_existing_admin_key\n');

      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_DRY_RUN: '1',
          CPAMP_OPERATION: 'upgrade',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_DIR: installDir,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(1);
      expect(combinedOutput(result)).toContain('Docker Compose project name must not be empty');
    } finally {
      rmSync(installDir, { recursive: true, force: true });
    }
  });

  it('fails instead of looping when the random source yields no alphanumeric characters', () => {
    const fakeBin = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-bin-'));

    try {
      const fakeOpenSSL = path.join(fakeBin, 'openssl');
      writeFileSync(fakeOpenSSL, '#!/usr/bin/env bash\nprintf -- "----"\n');
      chmodSync(fakeOpenSSL, 0o755);

      const result = runInstaller({
        CPAMP_INSTALL_MODE: 'stack',
        CPAMP_DEPLOY_METHOD: 'docker',
        PATH: `${fakeBin}${path.delimiter}${process.env.PATH || ''}`,
      });

      expect(result.status).toBe(1);
      expect(combinedOutput(result)).toContain(
        'Random source produced no usable alphanumeric characters'
      );
    } finally {
      rmSync(fakeBin, { recursive: true, force: true });
    }
  });

  it('generates full Docker config with CPA image paths', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));

    try {
      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_SKIP_EXECUTE: '1',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_MODE: 'stack',
          CPAMP_DEPLOY_METHOD: 'docker',
          CPA_LICENSE_CLIENT_SECRET: 'storefront-issued-secret-for-test',
          CPAMP_INSTALL_DIR: installDir,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(0);

      const compose = readFileSync(path.join(installDir, 'compose.yaml'), 'utf8');
      const envFile = readFileSync(path.join(installDir, '.env'), 'utf8');
      const cpaConfig = readFileSync(path.join(installDir, 'cliproxyapi/config.yaml'), 'utf8');
      const adminKey = readFileSync(
        path.join(installDir, 'secrets/cpamp-admin-key'),
        'utf8'
      ).trim();
      const cpaManagementKey = readFileSync(
        path.join(installDir, 'secrets/cpa-management-key'),
        'utf8'
      ).trim();
      const demoClientKey = readFileSync(
        path.join(installDir, 'secrets/cpa-demo-client-key'),
        'utf8'
      ).trim();

      expect(compose).toContain('./cliproxyapi/config.yaml:/CLIProxyAPI/config.yaml');
      expect(envFile).toContain('ghcr.io/abc124774961/cpa-manager-plus:v1.12.8-cpa.1');
      expect(compose).toContain('./cliproxyapi/auths:/root/.cli-proxy-api');
      expect(compose).toContain('./cliproxyapi/logs:/CLIProxyAPI/logs');
      expect(adminKey).toMatch(/^cpamp_[A-Za-z0-9]{32}$/);
      expect(cpaManagementKey).toMatch(/^cpa_[A-Za-z0-9]{32}$/);
      expect(demoClientKey).toMatch(/^sk-[A-Za-z0-9]{64}$/);
      expect(cpaConfig).toContain('auth-dir: "/root/.cli-proxy-api"');
      expect(cpaConfig).toContain('disable-auto-update-panel: true');
      expect(cpaConfig).toContain(`secret-key: "${cpaManagementKey}"`);
      expect(cpaConfig).toContain(`api-keys:\n  - "${demoClientKey}"`);
    } finally {
      rmSync(installDir, { recursive: true, force: true });
    }
  });

  it('blocks a storefront-backed full install before writing files when the client secret is missing', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));

    try {
      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_SKIP_EXECUTE: '1',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_MODE: 'stack',
          CPAMP_DEPLOY_METHOD: 'docker',
          CPAMP_INSTALL_DIR: installDir,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(1);
      expect(combinedOutput(result)).toContain('storefront-issued client secret');
      expect(combinedOutput(result)).toContain('cpa-license-client-secret');
      expect(existsSync(path.join(installDir, 'secrets/cpa-license-client-secret'))).toBe(false);
      expect(existsSync(path.join(installDir, '.env'))).toBe(false);
    } finally {
      rmSync(installDir, { recursive: true, force: true });
    }
  });

  it('migrates a legacy direct storefront secret into the canonical mode-600 file without logging it', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));
    const secret = 'legacy-storefront-secret-for-test';

    try {
      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_SKIP_EXECUTE: '1',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_MODE: 'stack',
          CPAMP_DEPLOY_METHOD: 'docker',
          CPA_LICENSE_CLIENT_SECRET: secret,
          CPAMP_INSTALL_DIR: installDir,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(0);
      const secretFile = path.join(installDir, 'secrets/cpa-license-client-secret');
      expect(readFileSync(secretFile, 'utf8')).toBe(`${secret}\n`);
      expect(statSync(secretFile).mode & 0o777).toBe(0o600);
      expect(combinedOutput(result)).not.toContain(secret);
      expect(readFileSync(path.join(installDir, '.env'), 'utf8')).toContain(
        `CPA_LICENSE_CLIENT_SECRET_FILE=${secretFile}`
      );
    } finally {
      rmSync(installDir, { recursive: true, force: true });
    }
  });

  it.each(['', 'replace-with-storefront-secret'])(
    'falls back to a valid legacy source when the canonical file contains %s',
    (canonicalValue) => {
      const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));
      const legacySource = path.join(installDir, 'legacy-secret');

      try {
        mkdirSync(path.join(installDir, 'secrets'), { recursive: true });
        writeFileSync(
          path.join(installDir, '.env'),
          [
            'COMPOSE_PROJECT_NAME=cpamp',
            'CPAMP_IMAGE=example/cpamp:v1',
            'CPAMP_PORT=18317',
            'CPA_IMAGE=example/cpa:v1',
            'CPA_PORT=8317',
            'CPAMP_AGENT_TOKEN=agent',
            'CPA_LICENSE_PROVIDER=shop666',
            'CPA_LICENSE_PUBLIC_KEY=replace-with-base64url-ed25519-public-key',
            'CPA_LICENSE_CLIENT_SECRET_FILE=legacy-secret',
            '',
          ].join('\n')
        );
        writeFileSync(
          path.join(installDir, 'compose.yaml'),
          'services:\n  cli-proxy-api:\n    image: ${CPA_IMAGE}\n  cpa-manager-plus:\n    image: ${CPAMP_IMAGE}\n'
        );
        writeFileSync(
          path.join(installDir, 'secrets/cpamp-admin-key'),
          'cpamp_existing_admin_key\n'
        );
        writeFileSync(
          path.join(installDir, 'secrets/cpa-license-client-secret'),
          canonicalValue ? `${canonicalValue}\n` : ''
        );
        writeFileSync(legacySource, 'legacy-source-secret-for-test\n');
        chmodSync(path.join(installDir, 'secrets/cpamp-admin-key'), 0o600);
        chmodSync(path.join(installDir, 'secrets/cpa-license-client-secret'), 0o600);
        chmodSync(legacySource, 0o600);

        const result = spawnSync('bash', [installerPath], {
          cwd: repoRoot,
          env: {
            ...process.env,
            CPAMP_OPERATION: 'upgrade',
            CPAMP_SKIP_EXECUTE: '1',
            CPAMP_NON_INTERACTIVE: '1',
            CPAMP_CONFIRM: '1',
            CPAMP_LANG: 'en-US',
            CPAMP_INSTALL_DIR: installDir,
          },
          encoding: 'utf8',
        });

        expect(result.status).toBe(0);
        expect(
          readFileSync(path.join(installDir, 'secrets/cpa-license-client-secret'), 'utf8')
        ).toBe('legacy-source-secret-for-test\n');
      } finally {
        rmSync(installDir, { recursive: true, force: true });
      }
    }
  );

  it('rejects a storefront secret file that is not mode 600', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));

    try {
      mkdirSync(path.join(installDir, 'secrets'), { recursive: true });
      writeFileSync(
        path.join(installDir, '.env'),
        [
          'COMPOSE_PROJECT_NAME=cpamp',
          'CPAMP_IMAGE=example/cpamp:v1',
          'CPAMP_PORT=18317',
          'CPA_IMAGE=example/cpa:v1',
          'CPA_PORT=8317',
          'CPAMP_AGENT_TOKEN=agent',
          'CPA_LICENSE_PROVIDER=shop666',
          'CPA_LICENSE_PUBLIC_KEY=replace-with-base64url-ed25519-public-key',
          '',
        ].join('\n')
      );
      writeFileSync(
        path.join(installDir, 'compose.yaml'),
        'services:\n  cli-proxy-api:\n    image: ${CPA_IMAGE}\n  cpa-manager-plus:\n    image: ${CPAMP_IMAGE}\n'
      );
      writeFileSync(path.join(installDir, 'secrets/cpamp-admin-key'), 'cpamp_existing_admin_key\n');
      const secretFile = path.join(installDir, 'secrets/cpa-license-client-secret');
      writeFileSync(secretFile, 'storefront-issued-secret-for-test\n');
      chmodSync(path.join(installDir, 'secrets/cpamp-admin-key'), 0o600);
      chmodSync(secretFile, 0o644);

      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_OPERATION: 'upgrade',
          CPAMP_SKIP_EXECUTE: '1',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_DIR: installDir,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(1);
      expect(combinedOutput(result)).toContain('must use mode 600');
      expect(statSync(secretFile).mode & 0o777).toBe(0o644);
    } finally {
      rmSync(installDir, { recursive: true, force: true });
    }
  });

  it('ignores a stale external license secret when installing CPAMP only', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));
    const sourceFile = path.join(installDir, 'legacy-secret');

    try {
      writeFileSync(sourceFile, 'replace-with-storefront-secret\n');
      chmodSync(sourceFile, 0o600);
      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_SKIP_EXECUTE: '1',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_MODE: 'cpamp',
          CPAMP_DEPLOY_METHOD: 'docker',
          CPA_LICENSE_CLIENT_SECRET_FILE: sourceFile,
          CPAMP_INSTALL_DIR: installDir,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(0);
      expect(readFileSync(sourceFile, 'utf8')).toBe('replace-with-storefront-secret\n');
      expect(existsSync(path.join(installDir, 'secrets/cpa-license-client-secret'))).toBe(false);
    } finally {
      rmSync(installDir, { recursive: true, force: true });
    }
  });

  it('keeps a non-storefront stack compatible with a stale placeholder source', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));
    const sourceFile = path.join(installDir, 'legacy-secret');

    try {
      writeFileSync(sourceFile, 'replace-with-storefront-secret\n');
      chmodSync(sourceFile, 0o600);
      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_SKIP_EXECUTE: '1',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_MODE: 'stack',
          CPAMP_DEPLOY_METHOD: 'docker',
          CPA_LICENSE_PROVIDER: 'local',
          CPA_LICENSE_API_BASE_URL: 'http://127.0.0.1:9000/api',
          CPA_LICENSE_CLIENT_SECRET_FILE: sourceFile,
          CPAMP_INSTALL_DIR: installDir,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(0);
      expect(readFileSync(path.join(installDir, 'secrets/cpa-license-client-secret'), 'utf8')).toBe(
        ''
      );
      expect(readFileSync(sourceFile, 'utf8')).toBe('replace-with-storefront-secret\n');
    } finally {
      rmSync(installDir, { recursive: true, force: true });
    }
  });

  it('rejects storefront URLs containing userinfo so host matching cannot be bypassed', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));

    try {
      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_SKIP_EXECUTE: '1',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_MODE: 'stack',
          CPAMP_DEPLOY_METHOD: 'docker',
          CPA_LICENSE_PROVIDER: 'custom-storefront',
          CPA_LICENSE_API_BASE_URL: 'https://p.666ttt.net@evil.example/api',
          CPAMP_INSTALL_DIR: installDir,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(1);
      expect(combinedOutput(result)).toContain('contains unsupported characters');
    } finally {
      rmSync(installDir, { recursive: true, force: true });
    }
  });

  it.each(['upgrade', 'regenerate'])(
    'clears a legacy direct secret from a managed env after %s migration',
    (operation) => {
      const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));
      const legacySecret = 'legacy-direct-secret-for-test';

      try {
        mkdirSync(path.join(installDir, 'secrets'), { recursive: true });
        writeFileSync(
          path.join(installDir, '.env'),
          [
            'COMPOSE_PROJECT_NAME=cpamp',
            'CPAMP_IMAGE=example/cpamp:v1',
            'CPAMP_PORT=18317',
            'CPA_IMAGE=example/cpa:v1',
            'CPA_PORT=8317',
            'CPAMP_AGENT_TOKEN=agent',
            'CPA_LICENSE_PROVIDER=shop666',
            'CPA_LICENSE_PUBLIC_KEY=replace-with-base64url-ed25519-public-key',
            `export CPA_LICENSE_CLIENT_SECRET=${legacySecret}`,
            '',
          ].join('\n')
        );
        writeFileSync(
          path.join(installDir, 'compose.yaml'),
          'services:\n  cli-proxy-api:\n    image: ${CPA_IMAGE}\n  cpa-manager-plus:\n    image: ${CPAMP_IMAGE}\n'
        );
        writeFileSync(
          path.join(installDir, 'secrets/cpamp-admin-key'),
          'cpamp_existing_admin_key\n'
        );
        chmodSync(path.join(installDir, 'secrets/cpamp-admin-key'), 0o600);

        const result = spawnSync('bash', [installerPath], {
          cwd: repoRoot,
          env: {
            ...process.env,
            CPAMP_OPERATION: operation,
            CPAMP_SKIP_EXECUTE: '1',
            CPAMP_NON_INTERACTIVE: '1',
            CPAMP_CONFIRM: '1',
            CPAMP_LANG: 'en-US',
            CPAMP_INSTALL_DIR: installDir,
          },
          encoding: 'utf8',
        });

        expect(result.status).toBe(0);
        expect(
          readFileSync(path.join(installDir, 'secrets/cpa-license-client-secret'), 'utf8')
        ).toBe(`${legacySecret}\n`);
        const env = readFileSync(path.join(installDir, '.env'), 'utf8');
        if (operation === 'upgrade') {
          expect(env).toContain('CPA_LICENSE_CLIENT_SECRET=\n');
        } else {
          expect(env).not.toMatch(/(?:^|\n)\s*(?:export\s+)?CPA_LICENSE_CLIENT_SECRET=/);
        }
        expect(env).not.toContain(legacySecret);
        expect(combinedOutput(result)).not.toContain(legacySecret);
      } finally {
        rmSync(installDir, { recursive: true, force: true });
      }
    }
  );

  it('does not create an empty storefront secret during dry-run preview', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));

    try {
      const result = runInstaller({
        CPAMP_INSTALL_MODE: 'stack',
        CPAMP_DEPLOY_METHOD: 'docker',
        CPAMP_INSTALL_DIR: installDir,
      });

      expect(result.status).toBe(0);
      expect(combinedOutput(result)).toContain('storefront-issued client secret');
      expect(existsSync(path.join(installDir, 'secrets/cpa-license-client-secret'))).toBe(false);
    } finally {
      rmSync(installDir, { recursive: true, force: true });
    }
  });

  it('requires a secret when the storefront API host is p.666ttt.net even with a custom provider name', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));

    try {
      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_SKIP_EXECUTE: '1',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_MODE: 'stack',
          CPAMP_DEPLOY_METHOD: 'docker',
          CPA_LICENSE_PROVIDER: 'custom-storefront',
          CPA_LICENSE_API_BASE_URL: 'https://p.666ttt.net/api/storefront',
          CPAMP_INSTALL_DIR: installDir,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(1);
      expect(combinedOutput(result)).toContain('storefront-issued client secret');
    } finally {
      rmSync(installDir, { recursive: true, force: true });
    }
  });

  it('rejects a placeholder value in a legacy storefront secret file without logging its contents', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));
    const sourceFile = path.join(installDir, 'legacy-secret');
    const placeholder = 'replace-with-storefront-secret';

    try {
      writeFileSync(sourceFile, `${placeholder}\n`, { mode: 0o600 });
      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_SKIP_EXECUTE: '1',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_MODE: 'stack',
          CPAMP_DEPLOY_METHOD: 'docker',
          CPA_LICENSE_CLIENT_SECRET_FILE: sourceFile,
          CPAMP_INSTALL_DIR: installDir,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(1);
      expect(combinedOutput(result)).toContain('non-placeholder');
      expect(combinedOutput(result)).not.toContain(placeholder);
      expect(existsSync(path.join(installDir, 'secrets/cpa-license-client-secret'))).toBe(false);
    } finally {
      rmSync(installDir, { recursive: true, force: true });
    }
  });

  it('generates CPAMP-only Docker config for a host CPA URL', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));

    try {
      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_SKIP_EXECUTE: '1',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_MODE: 'cpamp',
          CPAMP_DEPLOY_METHOD: 'docker',
          CPAMP_CPA_CONNECTION_MODE: 'env',
          CPAMP_CPA_URL: 'http://host.docker.internal:8317',
          CPAMP_CPA_MANAGEMENT_KEY: 'cpa_existing_management_key',
          CPAMP_INSTALL_DIR: installDir,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(0);

      const envFile = readFileSync(path.join(installDir, '.env'), 'utf8');
      const compose = readFileSync(path.join(installDir, 'compose.yaml'), 'utf8');

      expect(envFile).toContain('CPA_UPSTREAM_URL=http://host.docker.internal:8317');
      expect(result.stdout).toContain('first setup');
      expect(result.stdout).toContain('Deployment config generated');
      if (process.platform === 'linux') {
        expect(compose).toContain('host.docker.internal:host-gateway');
      }
    } finally {
      rmSync(installDir, { recursive: true, force: true });
    }
  });

  it('reuses an existing CPA Management Key secret for CPAMP-only Docker env installs', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));

    try {
      mkdirSync(path.join(installDir, 'secrets'), { recursive: true });
      writeFileSync(
        path.join(installDir, 'secrets/cpa-management-key'),
        'cpa_reused_management_key\n'
      );

      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_SKIP_EXECUTE: '1',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_MODE: 'cpamp',
          CPAMP_DEPLOY_METHOD: 'docker',
          CPAMP_CPA_CONNECTION_MODE: 'env',
          CPAMP_CPA_URL: 'http://host.docker.internal:8317',
          CPAMP_INSTALL_DIR: installDir,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(0);
      expect(readFileSync(path.join(installDir, 'secrets/cpa-management-key'), 'utf8').trim()).toBe(
        'cpa_reused_management_key'
      );
      expect(readFileSync(path.join(installDir, 'compose.yaml'), 'utf8')).toContain(
        'CPA_MANAGEMENT_KEY_FILE: "/run/secrets/cpa_management_key"'
      );
    } finally {
      rmSync(installDir, { recursive: true, force: true });
    }
  });

  it('reuses an existing CPA Management Key secret during dry runs', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));

    try {
      mkdirSync(path.join(installDir, 'secrets'), { recursive: true });
      writeFileSync(
        path.join(installDir, 'secrets/cpa-management-key'),
        'cpa_reused_management_key\n'
      );

      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_DRY_RUN: '1',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_MODE: 'cpamp',
          CPAMP_DEPLOY_METHOD: 'docker',
          CPAMP_CPA_CONNECTION_MODE: 'env',
          CPAMP_CPA_URL: 'http://host.docker.internal:8317',
          CPAMP_INSTALL_DIR: installDir,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(0);
      expect(combinedOutput(result)).not.toContain('secrets/cpa-management-key must not be empty');
      expect(result.stdout).toContain('first setup');
    } finally {
      rmSync(installDir, { recursive: true, force: true });
    }
  });

  it('blocks non-interactive installs when an orphaned Docker data volume exists', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));
    const fakeBin = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-bin-'));

    try {
      writeFakeDocker(fakeBin);
      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_SKIP_EXECUTE: '1',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_DIR: installDir,
          FAKE_DOCKER_VOLUME_EXISTS: '1',
          PATH: `${fakeBin}${path.delimiter}${process.env.PATH || ''}`,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(1);
      expect(combinedOutput(result)).toContain('old Docker data volume exists');
      expect(existsSync(path.join(installDir, 'compose.yaml'))).toBe(false);
      expect(existsSync(path.join(installDir, 'secrets/cpamp-admin-key'))).toBe(false);
    } finally {
      rmSync(installDir, { recursive: true, force: true });
      rmSync(fakeBin, { recursive: true, force: true });
    }
  });

  it('requires the original install scope for non-interactive orphan-volume repair', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));
    const fakeBin = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-bin-'));

    try {
      writeFakeDocker(fakeBin);
      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_OPERATION: 'repair',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_DIR: installDir,
          FAKE_DOCKER_VOLUME_EXISTS: '1',
          PATH: `${fakeBin}${path.delimiter}${process.env.PATH || ''}`,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(1);
      expect(combinedOutput(result)).toContain('requires CPAMP_INSTALL_MODE=stack or cpamp');
      expect(existsSync(path.join(installDir, 'compose.yaml'))).toBe(false);
    } finally {
      rmSync(installDir, { recursive: true, force: true });
      rmSync(fakeBin, { recursive: true, force: true });
    }
  });

  it('rejects skipped execution for orphan-volume repair before writing a new secret', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));
    const fakeBin = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-bin-'));

    try {
      writeFakeDocker(fakeBin);
      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_OPERATION: 'repair',
          CPAMP_SKIP_EXECUTE: '1',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_MODE: 'stack',
          CPAMP_INSTALL_DIR: installDir,
          FAKE_DOCKER_VOLUME_EXISTS: '1',
          PATH: `${fakeBin}${path.delimiter}${process.env.PATH || ''}`,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(1);
      expect(combinedOutput(result)).toContain('cannot use CPAMP_SKIP_EXECUTE=1');
      expect(existsSync(path.join(installDir, 'secrets/cpamp-admin-key'))).toBe(false);
    } finally {
      rmSync(installDir, { recursive: true, force: true });
      rmSync(fakeBin, { recursive: true, force: true });
    }
  });

  it('fails before writing Docker config when the Docker daemon is unavailable', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));
    const fakeBin = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-bin-'));

    try {
      writeFakeDocker(fakeBin);
      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_MODE: 'stack',
          CPAMP_DEPLOY_METHOD: 'docker',
          CPAMP_INSTALL_DIR: installDir,
          CPA_LICENSE_PUBLIC_KEY: 'MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE',
          CPA_LICENSE_CLIENT_SECRET: 'storefront-issued-secret-for-test',
          FAKE_DOCKER_DAEMON_OK: '0',
          PATH: `${fakeBin}${path.delimiter}${process.env.PATH || ''}`,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(1);
      expect(combinedOutput(result)).toContain('Docker daemon is not available');
      expect(existsSync(path.join(installDir, '.env'))).toBe(false);
      expect(existsSync(path.join(installDir, 'compose.yaml'))).toBe(false);
      expect(existsSync(path.join(installDir, 'secrets/cpamp-admin-key'))).toBe(false);
    } finally {
      rmSync(installDir, { recursive: true, force: true });
      rmSync(fakeBin, { recursive: true, force: true });
    }
  });

  it('does not create a replacement admin secret before repair preflight succeeds', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));
    const fakeBin = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-bin-'));

    try {
      writeFileSync(
        path.join(installDir, '.env'),
        'COMPOSE_PROJECT_NAME=cpamp\nCPAMP_IMAGE=example/cpamp:v1\nCPAMP_PORT=18317\n'
      );
      writeFileSync(
        path.join(installDir, 'compose.yaml'),
        'services:\n  cpa-manager-plus:\n    image: ${CPAMP_IMAGE}\n'
      );
      writeFakeDocker(fakeBin);

      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_OPERATION: 'repair',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_DIR: installDir,
          FAKE_DOCKER_DAEMON_OK: '0',
          PATH: `${fakeBin}${path.delimiter}${process.env.PATH || ''}`,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(1);
      expect(combinedOutput(result)).toContain('Docker daemon is not available');
      expect(existsSync(path.join(installDir, 'secrets/cpamp-admin-key'))).toBe(false);
    } finally {
      rmSync(installDir, { recursive: true, force: true });
      rmSync(fakeBin, { recursive: true, force: true });
    }
  });

  it('does not create a half-repaired admin secret when repair execution is skipped', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));

    try {
      writeFileSync(
        path.join(installDir, '.env'),
        'COMPOSE_PROJECT_NAME=cpamp\nCPAMP_IMAGE=example/cpamp:v1\nCPAMP_PORT=18317\n'
      );
      writeFileSync(
        path.join(installDir, 'compose.yaml'),
        'services:\n  cpa-manager-plus:\n    image: ${CPAMP_IMAGE}\n'
      );

      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_OPERATION: 'repair',
          CPAMP_SKIP_EXECUTE: '1',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_DIR: installDir,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(0);
      expect(existsSync(path.join(installDir, 'secrets/cpamp-admin-key'))).toBe(false);
      expect(result.stdout).toContain('upgrade or repair commands were skipped');
      expect(result.stdout).not.toContain('Admin key saved');
    } finally {
      rmSync(installDir, { recursive: true, force: true });
    }
  });

  it('repairs an orphaned Docker deployment and verifies the generated admin key', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));
    const fakeBin = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-bin-'));
    const dockerLog = path.join(
      os.tmpdir(),
      `cpamp-installer-docker-${process.pid}-${Date.now()}.log`
    );

    try {
      writeFakeDocker(fakeBin);
      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_OPERATION: 'repair',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_MODE: 'stack',
          CPAMP_INSTALL_DIR: installDir,
          FAKE_DOCKER_LOG: dockerLog,
          FAKE_DOCKER_VOLUME_EXISTS: '1',
          FAKE_DOCKER_AUTH_OK: '1',
          PATH: `${fakeBin}${path.delimiter}${process.env.PATH || ''}`,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(0);
      expect(readFileSync(dockerLog, 'utf8')).toMatch(
        /compose .* run --rm cpa-manager-plus reset-admin-key --admin-key-file \/run\/secrets\/cpamp_admin_key/
      );
      expect(readFileSync(path.join(installDir, 'secrets/cpamp-admin-key'), 'utf8').trim()).toMatch(
        /^cpamp_[A-Za-z0-9]{32}$/
      );
      expect(result.stdout).toContain('Admin key verification passed');
    } finally {
      rmSync(installDir, { recursive: true, force: true });
      rmSync(fakeBin, { recursive: true, force: true });
      rmSync(dockerLog, { force: true });
    }
  });

  it('upgrades a managed Docker install without rewriting config or secrets', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));
    const fakeBin = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-bin-'));
    const dockerLog = path.join(
      os.tmpdir(),
      `cpamp-installer-docker-${process.pid}-${Date.now()}.log`
    );
    const envContent =
      'COMPOSE_PROJECT_NAME=oldproject\nCOMPOSE_PROJECT_NAME=cpamp\nCPAMP_IMAGE=example/cpamp:v1\nCPAMP_PORT=19999\nCPAMP_PORT=18317\n';
    const composeContent = 'services:\n  cpa-manager-plus:\n    image: ${CPAMP_IMAGE}\n';
    const secretContent = 'cpamp_existing_admin_key\n';

    try {
      mkdirSync(path.join(installDir, 'secrets'), { recursive: true });
      writeFileSync(path.join(installDir, '.env'), envContent);
      writeFileSync(path.join(installDir, 'compose.yaml'), composeContent);
      writeFileSync(path.join(installDir, 'secrets/cpamp-admin-key'), secretContent);
      writeFakeDocker(fakeBin);

      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_OPERATION: 'upgrade',
          COMPOSE_PROJECT_NAME: 'wrong-project',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_DIR: installDir,
          FAKE_DOCKER_LOG: dockerLog,
          FAKE_DOCKER_AUTH_OK: '1',
          PATH: `${fakeBin}${path.delimiter}${process.env.PATH || ''}`,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(0);
      expect(readFileSync(path.join(installDir, '.env'), 'utf8')).toBe(envContent);
      expect(readFileSync(path.join(installDir, 'compose.yaml'), 'utf8')).toBe(composeContent);
      expect(readFileSync(path.join(installDir, 'secrets/cpamp-admin-key'), 'utf8')).toBe(
        secretContent
      );
      expect(readFileSync(dockerLog, 'utf8')).toMatch(/compose .* pull/);
      expect(readFileSync(dockerLog, 'utf8')).toMatch(/compose .* up -d/);
      expect(readFileSync(dockerLog, 'utf8')).not.toContain('reset-admin-key');
      expect(readFileSync(dockerLog, 'utf8')).toMatch(/cpamp\|compose .* pull/);
      expect(result.stdout).toContain('Public CPAMP port: 18317');
    } finally {
      rmSync(installDir, { recursive: true, force: true });
      rmSync(fakeBin, { recursive: true, force: true });
      rmSync(dockerLog, { force: true });
    }
  });

  it('backfills missing official storefront keys during a managed upgrade', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));
    const oldEnv = [
      '# preserve this environment comment',
      'COMPOSE_PROJECT_NAME=cpamp',
      'CPAMP_IMAGE=example/cpamp:v1',
      'CPAMP_PORT=18317',
      'CPA_IMAGE=example/cpa:v1',
      'CPA_PORT=8317',
      'CPAMP_AGENT_TOKEN=agent',
      'CPA_LICENSE_PROVIDER=shop666',
      'CPA_LICENSE_API_BASE_URL=https://p.666ttt.net/api/storefront',
      'CPA_LICENSE_PUBLIC_KEY=replace-with-base64url-ed25519-public-key',
      'CPA_LICENSE_PLUGIN_PUBLIC_KEY=',
      'CPA_LICENSE_REQUIRE_CLIENT_SECRET=false',
      '',
    ].join('\n');
    const oldConfig = [
      '# preserve this config comment',
      'license:',
      '  provider: "shop666"',
      '  api-base-url: "https://p.666ttt.net/api/storefront"',
      '  public-key: "" # keep this inline comment',
      '  plugin-public-key: replace-with-plugin-key',
      '  client-secret: "authorization-state-sentinel"',
      '# preserve trailing comment',
      '',
    ].join('\n');
    const compose =
      'services:\n  cli-proxy-api:\n    image: ${CPA_IMAGE}\n  cpa-manager-plus:\n    image: ${CPAMP_IMAGE}\n';

    try {
      mkdirSync(path.join(installDir, 'secrets'), { recursive: true });
      mkdirSync(path.join(installDir, 'cliproxyapi'), { recursive: true });
      writeFileSync(path.join(installDir, '.env'), oldEnv, { mode: 0o600 });
      writeFileSync(path.join(installDir, 'compose.yaml'), compose);
      writeFileSync(path.join(installDir, 'cliproxyapi/config.yaml'), oldConfig, { mode: 0o600 });
      writeFileSync(path.join(installDir, 'secrets/cpamp-admin-key'), 'cpamp_existing_admin_key\n');
      chmodSync(path.join(installDir, 'secrets/cpamp-admin-key'), 0o600);

      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_OPERATION: 'upgrade',
          CPAMP_SKIP_EXECUTE: '1',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_DIR: installDir,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(0);
      const env = readFileSync(path.join(installDir, '.env'), 'utf8');
      const config = readFileSync(path.join(installDir, 'cliproxyapi/config.yaml'), 'utf8');
      expect(env).toContain(
        'CPA_LICENSE_PUBLIC_KEY=kJhDRBpfneFdURvPXwiGW3XAmPrd2HVVORfHzP-eYTg'
      );
      expect(env).toContain(
        'CPA_LICENSE_PLUGIN_PUBLIC_KEY=OHRHVVIlFC34K-5AQUkOPcZLeiSpeX_n_VPbrH3agXQ'
      );
      expect(config).toContain(
        'public-key: "kJhDRBpfneFdURvPXwiGW3XAmPrd2HVVORfHzP-eYTg" # keep this inline comment'
      );
      expect(config).toContain(
        'plugin-public-key: "OHRHVVIlFC34K-5AQUkOPcZLeiSpeX_n_VPbrH3agXQ"'
      );
      expect(config).toContain('client-secret: "authorization-state-sentinel"');
      expect(config).toContain('# preserve trailing comment');
      expect(readFileSync(path.join(installDir, 'compose.yaml'), 'utf8')).toBe(compose);
      expect(result.stdout).toContain('Backfilled official CPA_LICENSE_PUBLIC_KEY');
    } finally {
      rmSync(installDir, { recursive: true, force: true });
    }
  });

  it('does not inject release keys into a customer-managed provider or endpoint', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));
    const oldEnv = [
      'COMPOSE_PROJECT_NAME=cpamp',
      'CPAMP_IMAGE=example/cpamp:v1',
      'CPAMP_PORT=18317',
      'CPA_IMAGE=example/cpa:v1',
      'CPA_PORT=8317',
      'CPAMP_AGENT_TOKEN=agent',
      'CPA_LICENSE_PROVIDER=local',
      'CPA_LICENSE_API_BASE_URL=http://customer.example/api',
      'CPA_LICENSE_PUBLIC_KEY=',
      'CPA_LICENSE_PLUGIN_PUBLIC_KEY=',
      '',
    ].join('\n');
    const oldConfig = [
      'license:',
      '  provider: "local"',
      '  api-base-url: "http://customer.example/api"',
      '  public-key: "" # customer key is intentionally unset',
      '  plugin-public-key: ""',
      '',
    ].join('\n');

    try {
      mkdirSync(path.join(installDir, 'secrets'), { recursive: true });
      mkdirSync(path.join(installDir, 'cliproxyapi'), { recursive: true });
      writeFileSync(path.join(installDir, '.env'), oldEnv, { mode: 0o600 });
      writeFileSync(
        path.join(installDir, 'compose.yaml'),
        'services:\n  cli-proxy-api:\n    image: ${CPA_IMAGE}\n  cpa-manager-plus:\n    image: ${CPAMP_IMAGE}\n'
      );
      writeFileSync(path.join(installDir, 'cliproxyapi/config.yaml'), oldConfig, { mode: 0o600 });
      writeFileSync(path.join(installDir, 'secrets/cpamp-admin-key'), 'cpamp_existing_admin_key\n');
      chmodSync(path.join(installDir, 'secrets/cpamp-admin-key'), 0o600);

      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_OPERATION: 'upgrade',
          CPAMP_SKIP_EXECUTE: '1',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_DIR: installDir,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(0);
      expect(readFileSync(path.join(installDir, '.env'), 'utf8')).toBe(oldEnv);
      expect(readFileSync(path.join(installDir, 'cliproxyapi/config.yaml'), 'utf8')).toBe(oldConfig);
      expect(result.stdout).not.toContain('Backfilled official');
    } finally {
      rmSync(installDir, { recursive: true, force: true });
    }
  });

  it('inserts missing official key fields without replacing other license settings', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));
    const oldEnv = [
      'COMPOSE_PROJECT_NAME=cpamp',
      'CPAMP_IMAGE=example/cpamp:v1',
      'CPAMP_PORT=18317',
      'CPA_IMAGE=example/cpa:v1',
      'CPA_PORT=8317',
      'CPAMP_AGENT_TOKEN=agent',
      'CPA_LICENSE_PROVIDER=shop666',
      'CPA_LICENSE_API_BASE_URL=https://p.666ttt.net/api/storefront',
      'CPA_LICENSE_PUBLIC_KEY=',
      'CPA_LICENSE_PLUGIN_PUBLIC_KEY=',
      '',
    ].join('\n');
    const oldConfig = [
      'license:',
      '  provider: "shop666"',
      '  api-base-url: "https://p.666ttt.net/api/storefront"',
      '  client-secret: "keep"',
      '',
      'debug: true',
      '',
    ].join('\n');

    try {
      mkdirSync(path.join(installDir, 'secrets'), { recursive: true });
      mkdirSync(path.join(installDir, 'cliproxyapi'), { recursive: true });
      writeFileSync(path.join(installDir, '.env'), oldEnv, { mode: 0o600 });
      writeFileSync(
        path.join(installDir, 'compose.yaml'),
        'services:\n  cli-proxy-api:\n    image: ${CPA_IMAGE}\n  cpa-manager-plus:\n    image: ${CPAMP_IMAGE}\n'
      );
      writeFileSync(path.join(installDir, 'cliproxyapi/config.yaml'), oldConfig, { mode: 0o600 });
      writeFileSync(path.join(installDir, 'secrets/cpamp-admin-key'), 'cpamp_existing_admin_key\n');
      chmodSync(path.join(installDir, 'secrets/cpamp-admin-key'), 0o600);

      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_OPERATION: 'upgrade',
          CPAMP_SKIP_EXECUTE: '1',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_DIR: installDir,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(0);
      const config = readFileSync(path.join(installDir, 'cliproxyapi/config.yaml'), 'utf8');
      expect(config).toContain(
        '  public-key: "kJhDRBpfneFdURvPXwiGW3XAmPrd2HVVORfHzP-eYTg"'
      );
      expect(config).toContain(
        '  plugin-public-key: "OHRHVVIlFC34K-5AQUkOPcZLeiSpeX_n_VPbrH3agXQ"'
      );
      expect(config).toContain('  client-secret: "keep"');
      expect(config).toContain('debug: true');
    } finally {
      rmSync(installDir, { recursive: true, force: true });
    }
  });

  it('follows a Compose -config path and repairs an active CPA config file', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));
    const activeConfig = path.join(installDir, 'data/cpa/config.active-76a1a7b5.yaml');
    const oldEnv = [
      'COMPOSE_PROJECT_NAME=cpamp',
      'CPAMP_IMAGE=example/cpamp:v1',
      'CPAMP_PORT=18317',
      'CPA_IMAGE=example/cpa:v1',
      'CPA_PORT=8317',
      'CPA_DATA_DIR=./data/cpa',
      'CPAMP_AGENT_TOKEN=agent',
      'CPA_LICENSE_PROVIDER=shop666',
      'CPA_LICENSE_API_BASE_URL=https://p.666ttt.net/api/storefront',
      'CPA_LICENSE_PUBLIC_KEY=',
      'CPA_LICENSE_PLUGIN_PUBLIC_KEY=',
      '',
    ].join('\n');
    const compose = [
      'services:',
      '  cli-proxy-api:',
      '    image: ${CPA_IMAGE}',
      '    command: ["./CLIProxyAPI", "-config", "/app/data/config.active-76a1a7b5.yaml"]',
      '  cpa-manager-plus:',
      '    image: ${CPAMP_IMAGE}',
      '',
    ].join('\n');
    const config = [
      '# active config is selected by Compose',
      'license:',
      '  provider: "shop666"',
      '  api-base-url: "https://p.666ttt.net/api/storefront"',
      '  public-key: "" # preserve active comment',
      '  plugin-public-key: "replace-with-plugin-key"',
      '  client-secret: "keep-active-license-state"',
      '',
    ].join('\n');

    try {
      mkdirSync(path.join(installDir, 'secrets'), { recursive: true });
      mkdirSync(path.dirname(activeConfig), { recursive: true });
      writeFileSync(path.join(installDir, '.env'), oldEnv, { mode: 0o600 });
      writeFileSync(path.join(installDir, 'compose.yaml'), compose);
      writeFileSync(activeConfig, config, { mode: 0o600 });
      writeFileSync(path.join(installDir, 'secrets/cpamp-admin-key'), 'cpamp_existing_admin_key\n');
      chmodSync(path.join(installDir, 'secrets/cpamp-admin-key'), 0o600);

      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_OPERATION: 'upgrade',
          CPAMP_SKIP_EXECUTE: '1',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_DIR: installDir,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(0);
      const repaired = readFileSync(activeConfig, 'utf8');
      expect(repaired).toContain(
        'public-key: "kJhDRBpfneFdURvPXwiGW3XAmPrd2HVVORfHzP-eYTg" # preserve active comment'
      );
      expect(repaired).toContain(
        'plugin-public-key: "OHRHVVIlFC34K-5AQUkOPcZLeiSpeX_n_VPbrH3agXQ"'
      );
      expect(repaired).toContain('client-secret: "keep-active-license-state"');
      expect(result.stdout).toContain(`Backfilled official CPA license key(s): ${activeConfig}`);
    } finally {
      rmSync(installDir, { recursive: true, force: true });
    }
  });

  it('prefers valid keys from the active config before using bundled release keys', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));
    const activeConfig = path.join(installDir, 'data/cpa/config.active-preferred.yaml');
    const configuredPublicKey =
      '0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef';
    const configuredPluginKey =
      'abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789';
    const oldEnv = [
      'COMPOSE_PROJECT_NAME=cpamp',
      'CPAMP_IMAGE=example/cpamp:v1',
      'CPAMP_PORT=18317',
      'CPA_IMAGE=example/cpa:v1',
      'CPA_PORT=8317',
      'CPA_DATA_DIR=./data/cpa',
      'CPAMP_AGENT_TOKEN=agent',
      'CPA_LICENSE_PROVIDER=shop666',
      'CPA_LICENSE_API_BASE_URL=https://p.666ttt.net/api/storefront',
      'CPA_LICENSE_PUBLIC_KEY=',
      'CPA_LICENSE_PLUGIN_PUBLIC_KEY=',
      '',
    ].join('\n');
    const compose = [
      'services:',
      '  cli-proxy-api:',
      '    command: ["./CLIProxyAPI", "-config", "/app/data/config.active-preferred.yaml"]',
      '  cpa-manager-plus:',
      '    image: ${CPAMP_IMAGE}',
      '',
    ].join('\n');
    const config = [
      'license:',
      '  provider: "shop666"',
      '  api-base-url: "https://p.666ttt.net/api/storefront"',
      `  public-key: "${configuredPublicKey}"`,
      `  plugin-public-key: "${configuredPluginKey}"`,
      '',
    ].join('\n');

    try {
      mkdirSync(path.join(installDir, 'secrets'), { recursive: true });
      mkdirSync(path.dirname(activeConfig), { recursive: true });
      writeFileSync(path.join(installDir, '.env'), oldEnv, { mode: 0o600 });
      writeFileSync(path.join(installDir, 'compose.yaml'), compose);
      writeFileSync(activeConfig, config, { mode: 0o600 });
      writeFileSync(path.join(installDir, 'secrets/cpamp-admin-key'), 'cpamp_existing_admin_key\n');
      chmodSync(path.join(installDir, 'secrets/cpamp-admin-key'), 0o600);

      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_OPERATION: 'upgrade',
          CPAMP_SKIP_EXECUTE: '1',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_DIR: installDir,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(0);
      const env = readFileSync(path.join(installDir, '.env'), 'utf8');
      expect(env).toContain(`CPA_LICENSE_PUBLIC_KEY=${configuredPublicKey}`);
      expect(env).toContain(`CPA_LICENSE_PLUGIN_PUBLIC_KEY=${configuredPluginKey}`);
      expect(readFileSync(activeConfig, 'utf8')).toBe(config);
      expect(result.stdout).toContain('Recovered CPA_LICENSE_PUBLIC_KEY from the active CPA config');
    } finally {
      rmSync(installDir, { recursive: true, force: true });
    }
  });

  it('repairs a managed Docker login without pulling unrelated service images', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));
    const fakeBin = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-bin-'));
    const dockerLog = path.join(
      os.tmpdir(),
      `cpamp-installer-docker-${process.pid}-${Date.now()}.log`
    );

    try {
      mkdirSync(path.join(installDir, 'secrets'), { recursive: true });
      writeFileSync(
        path.join(installDir, '.env'),
        'COMPOSE_PROJECT_NAME=cpamp\nCPAMP_IMAGE=example/cpamp:v1\nCPAMP_PORT=18317\n'
      );
      writeFileSync(
        path.join(installDir, 'compose.yaml'),
        'services:\n  cpa-manager-plus:\n    image: ${CPAMP_IMAGE}\n'
      );
      writeFileSync(path.join(installDir, 'secrets/cpamp-admin-key'), 'cpamp_existing_admin_key\n');
      writeFakeDocker(fakeBin);

      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_OPERATION: 'repair',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_DIR: installDir,
          FAKE_DOCKER_LOG: dockerLog,
          FAKE_DOCKER_AUTH_OK: '1',
          PATH: `${fakeBin}${path.delimiter}${process.env.PATH || ''}`,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(0);
      const calls = readFileSync(dockerLog, 'utf8');
      expect(calls).toMatch(
        /compose .* run --rm cpa-manager-plus reset-admin-key --admin-file \/run\/secrets\/cpamp_admin_key|compose .* run --rm cpa-manager-plus reset-admin-key --admin-key-file \/run\/secrets\/cpamp_admin_key/
      );
      expect(calls).not.toMatch(/compose .* pull/);
    } finally {
      rmSync(installDir, { recursive: true, force: true });
      rmSync(fakeBin, { recursive: true, force: true });
      rmSync(dockerLog, { force: true });
    }
  });

  it('does not report success when post-start admin key verification fails', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));
    const fakeBin = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-bin-'));

    try {
      mkdirSync(path.join(installDir, 'secrets'), { recursive: true });
      writeFileSync(
        path.join(installDir, '.env'),
        'COMPOSE_PROJECT_NAME=cpamp\nCPAMP_IMAGE=example/cpamp:v1\nCPAMP_PORT=18317\n'
      );
      writeFileSync(
        path.join(installDir, 'compose.yaml'),
        'services:\n  cpa-manager-plus:\n    image: ${CPAMP_IMAGE}\n'
      );
      writeFileSync(path.join(installDir, 'secrets/cpamp-admin-key'), 'cpamp_wrong_admin_key\n');
      writeFakeDocker(fakeBin);

      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_OPERATION: 'upgrade',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_DIR: installDir,
          FAKE_DOCKER_AUTH_OK: '0',
          PATH: `${fakeBin}${path.delimiter}${process.env.PATH || ''}`,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(1);
      expect(combinedOutput(result)).toContain('admin key verification failed');
      expect(result.stdout).not.toContain('Install steps completed');
    } finally {
      rmSync(installDir, { recursive: true, force: true });
      rmSync(fakeBin, { recursive: true, force: true });
    }
  });

  it('backs up generated config before regenerating a managed Docker install', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));
    const oldEnv = 'COMPOSE_PROJECT_NAME=cpamp\nCPAMP_IMAGE=example/old:v1\nCPAMP_PORT=18317\n';
    const oldCompose = 'services:\n  cpa-manager-plus:\n    image: old\n';

    try {
      mkdirSync(path.join(installDir, 'secrets'), { recursive: true });
      writeFileSync(path.join(installDir, '.env'), oldEnv);
      writeFileSync(path.join(installDir, 'compose.yaml'), oldCompose);
      writeFileSync(path.join(installDir, 'secrets/cpamp-admin-key'), 'cpamp_existing_admin_key\n');

      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_OPERATION: 'regenerate',
          CPAMP_SKIP_EXECUTE: '1',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_MODE: 'cpamp',
          CPAMP_DEPLOY_METHOD: 'docker',
          CPAMP_CPA_CONNECTION_MODE: 'setup',
          CPAMP_INSTALL_DIR: installDir,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(0);
      const backupNames = readdirSync(path.join(installDir, 'backups'));
      expect(backupNames).toHaveLength(1);
      const backupDir = path.join(installDir, 'backups', backupNames[0]);
      expect(readFileSync(path.join(backupDir, '.env'), 'utf8')).toBe(oldEnv);
      expect(readFileSync(path.join(backupDir, 'compose.yaml'), 'utf8')).toBe(oldCompose);
      expect(readFileSync(path.join(installDir, 'secrets/cpamp-admin-key'), 'utf8')).toBe(
        'cpamp_existing_admin_key\n'
      );
      expect(readFileSync(path.join(installDir, '.env'), 'utf8')).toContain(
        'CPAMP_IMAGE=example/old:v1'
      );
      expect(readFileSync(path.join(installDir, '.env'), 'utf8')).toContain('CPAMP_PORT=18317');
      expect(result.stdout).toContain('Previous config backed up to');
    } finally {
      rmSync(installDir, { recursive: true, force: true });
    }
  });

  it('blocks a partial Docker install before writing additional generated files', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));

    try {
      writeFileSync(path.join(installDir, 'compose.yaml'), 'existing compose\n');

      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_SKIP_EXECUTE: '1',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_MODE: 'cpamp',
          CPAMP_DEPLOY_METHOD: 'docker',
          CPAMP_CPA_CONNECTION_MODE: 'setup',
          CPAMP_INSTALL_DIR: installDir,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(1);
      expect(combinedOutput(result)).toContain('Non-interactive mode requires CPAMP_OPERATION');
      expect(existsSync(path.join(installDir, '.env'))).toBe(false);
      expect(existsSync(path.join(installDir, 'secrets/cpamp-admin-key'))).toBe(false);
    } finally {
      rmSync(installDir, { recursive: true, force: true });
    }
  });

  it('does not change existing admin-secret permissions during dry runs', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));
    const secretFile = path.join(installDir, 'secrets/cpamp-admin-key');

    try {
      mkdirSync(path.dirname(secretFile), { recursive: true });
      writeFileSync(
        path.join(installDir, '.env'),
        'COMPOSE_PROJECT_NAME=cpamp\nCPAMP_IMAGE=example/cpamp:v1\nCPAMP_PORT=18317\n'
      );
      writeFileSync(
        path.join(installDir, 'compose.yaml'),
        'services:\n  cpa-manager-plus:\n    image: ${CPAMP_IMAGE}\n'
      );
      writeFileSync(secretFile, 'cpamp_existing_admin_key\n');
      chmodSync(secretFile, 0o644);

      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_DRY_RUN: '1',
          CPAMP_OPERATION: 'upgrade',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_DIR: installDir,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(0);
      expect(statSync(secretFile).mode & 0o777).toBe(0o644);
    } finally {
      rmSync(installDir, { recursive: true, force: true });
    }
  });

  it('rejects empty existing secret files before generating config', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));

    try {
      mkdirSync(path.join(installDir, 'secrets'), { recursive: true });
      writeFileSync(path.join(installDir, 'secrets/cpamp-admin-key'), '');

      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_SKIP_EXECUTE: '1',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_MODE: 'stack',
          CPAMP_DEPLOY_METHOD: 'docker',
          CPAMP_INSTALL_DIR: installDir,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(1);
      expect(combinedOutput(result)).toContain('secrets/cpamp-admin-key must not be empty');
    } finally {
      rmSync(installDir, { recursive: true, force: true });
    }
  });

  it('fails when existing secret file permissions cannot be restricted', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));
    const fakeBin = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-bin-'));

    try {
      mkdirSync(path.join(installDir, 'secrets'), { recursive: true });
      writeFileSync(path.join(installDir, 'secrets/cpamp-admin-key'), 'cpamp_existing_admin_key\n');
      const fakeChmod = path.join(fakeBin, 'chmod');
      writeFileSync(fakeChmod, '#!/usr/bin/env bash\nexit 1\n');
      chmodSync(fakeChmod, 0o755);

      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_SKIP_EXECUTE: '1',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_MODE: 'stack',
          CPAMP_DEPLOY_METHOD: 'docker',
          CPAMP_INSTALL_DIR: installDir,
          PATH: `${fakeBin}${path.delimiter}${process.env.PATH || ''}`,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(1);
      expect(combinedOutput(result)).toContain('Unable to restrict secret file permissions');
    } finally {
      rmSync(installDir, { recursive: true, force: true });
      rmSync(fakeBin, { recursive: true, force: true });
    }
  });

  it('does not leave partial native files when the runtime directory already exists', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));
    const platform = process.platform === 'darwin' ? 'darwin' : 'linux';
    const arch = process.arch === 'arm64' ? 'arm64' : 'amd64';
    const packageName = `cpa-manager-plus_v1.8.1_${platform}_${arch}`;

    try {
      mkdirSync(path.join(installDir, 'runtime', packageName), { recursive: true });

      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_SKIP_EXECUTE: '1',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_MODE: 'cpamp',
          CPAMP_DEPLOY_METHOD: 'native',
          CPAMP_CPA_CONNECTION_MODE: 'setup',
          CPAMP_VERSION: 'v1.8.1',
          CPAMP_INSTALL_DIR: installDir,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(1);
      expect(combinedOutput(result)).toContain('Directory already exists');
      expect(existsSync(path.join(installDir, 'secrets/cpamp-admin-key'))).toBe(false);
      expect(existsSync(path.join(installDir, 'run.sh'))).toBe(false);
    } finally {
      rmSync(installDir, { recursive: true, force: true });
    }
  });

  it('fails native installs when the started process exits before health is ready', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));
    const fakeBin = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-bin-'));
    const fixtureDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-fixture-'));
    const platform = process.platform === 'darwin' ? 'darwin' : 'linux';
    const arch = process.arch === 'arm64' ? 'arm64' : 'amd64';
    const packageName = `cpa-manager-plus_vtest_${platform}_${arch}`;
    const packageDir = path.join(fixtureDir, packageName);
    const archivePath = path.join(fixtureDir, `${packageName}.tar.gz`);

    try {
      mkdirSync(packageDir, { recursive: true });
      const fakeBinary = path.join(packageDir, 'cpa-manager-plus');
      writeFileSync(
        fakeBinary,
        '#!/usr/bin/env bash\necho "fake native process exited" >&2\nexit 42\n'
      );
      chmodSync(fakeBinary, 0o755);
      const tarResult = spawnSync('tar', ['-czf', archivePath, '-C', fixtureDir, packageName], {
        cwd: repoRoot,
        encoding: 'utf8',
      });
      expect(tarResult.status).toBe(0);

      const fakeCurl = path.join(fakeBin, 'curl');
      writeFileSync(
        fakeCurl,
        `#!/usr/bin/env bash
set -euo pipefail
for arg in "$@"; do
  if [ "$arg" = "https://github.com/seakee/CPA-Manager-Plus/releases/latest" ]; then
    printf 'https://github.com/seakee/CPA-Manager-Plus/releases/tag/vtest'
    exit 0
  fi
done
out=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-o" ]; then
    out="$arg"
    break
  fi
  prev="$arg"
done
if [ -n "$out" ]; then
  cp "$CPAMP_FAKE_NATIVE_ARCHIVE" "$out"
  exit 0
fi
exit 22
`
      );
      chmodSync(fakeCurl, 0o755);

      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_SKIP_EXECUTE: '0',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_MODE: 'cpamp',
          CPAMP_DEPLOY_METHOD: 'native',
          CPAMP_CPA_CONNECTION_MODE: 'setup',
          CPAMP_INSTALL_DIR: installDir,
          CPAMP_FAKE_NATIVE_ARCHIVE: archivePath,
          PATH: `${fakeBin}${path.delimiter}${process.env.PATH || ''}`,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(1);
      expect(combinedOutput(result)).toContain(
        'Native CPAMP process exited before becoming healthy'
      );
      expect(combinedOutput(result)).toContain('fake native process exited');
    } finally {
      rmSync(installDir, { recursive: true, force: true });
      rmSync(fakeBin, { recursive: true, force: true });
      rmSync(fixtureDir, { recursive: true, force: true });
    }
  });

  it('generates a Linux systemd unit for native installs', () => {
    const installDir = mkdtempSync(path.join(os.tmpdir(), 'cpamp-installer-'));

    try {
      const result = spawnSync('bash', [installerPath], {
        cwd: repoRoot,
        env: {
          ...process.env,
          CPAMP_SKIP_EXECUTE: '1',
          CPAMP_NON_INTERACTIVE: '1',
          CPAMP_CONFIRM: '1',
          CPAMP_LANG: 'en-US',
          CPAMP_INSTALL_MODE: 'cpamp',
          CPAMP_DEPLOY_METHOD: 'native',
          CPAMP_CPA_CONNECTION_MODE: 'setup',
          CPAMP_VERSION: 'v1.8.1',
          CPAMP_INSTALL_DIR: installDir,
        },
        encoding: 'utf8',
      });

      expect(result.status).toBe(0);

      if (process.platform === 'linux') {
        const service = readFileSync(path.join(installDir, 'cpa-manager-plus.service'), 'utf8');

        expect(service).toContain('[Unit]');
        expect(service).toContain('ExecStart=');
        expect(service).toContain('/cpa-manager-plus');
      }
    } finally {
      rmSync(installDir, { recursive: true, force: true });
    }
  });
});
