import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import { viteSingleFile } from 'vite-plugin-singlefile';
import path from 'path';
import { execFileSync } from 'child_process';
import fs from 'fs';

const compactDateVersionPattern = /^\d{8}-\d{6}$/;

// Keep the panel version aligned with Manager and Agent. Repository tags may
// belong to an upstream release line and must never become deployment versions.
function getVersion(): string {
  const explicitVersion = process.env.VERSION?.trim();
  if (explicitVersion) {
    return explicitVersion;
  }

  try {
    const commitVersion = execFileSync(
      'git',
      ['show', '-s', '--date=format-local:%Y%m%d-%H%M%S', '--format=%cd', 'HEAD'],
      {
        cwd: path.resolve(__dirname, '../..'),
        encoding: 'utf8',
        env: { ...process.env, TZ: process.env.CPA_VERSION_TIMEZONE || 'Asia/Shanghai' },
      }
    ).trim();
    if (compactDateVersionPattern.test(commitVersion)) {
      return commitVersion;
    }
  } catch {
    // Git is optional in packaged source builds.
  }

  try {
    const pkg = JSON.parse(fs.readFileSync(path.resolve(__dirname, 'package.json'), 'utf8'));
    if (pkg.version && pkg.version !== '0.0.0') {
      return pkg.version;
    }
  } catch {
    // package.json not readable
  }

  return 'dev';
}

const isDemoSiteBuild = (mode: string) =>
  mode === 'demo' || process.env.DEMO_SITE === 'true' || process.env.VITE_DEMO_SITE === 'true';

// https://vitejs.dev/config/
export default defineConfig(({ mode }) => {
  const demoSite = isDemoSiteBuild(mode);
  const useRealDemoFixtures = demoSite || mode === 'test';

  return {
    plugins: [
      react(),
      viteSingleFile({
        removeViteModuleLoader: true,
      }),
    ],
    define: {
      __APP_VERSION__: JSON.stringify(getVersion()),
      __DEMO_SITE__: JSON.stringify(demoSite || mode === 'test'),
    },
    resolve: {
      alias: [
        {
          find: /^@\/features\/demo\/demoFixtures$/,
          replacement: path.resolve(
            __dirname,
            useRealDemoFixtures
              ? './src/features/demo/demoFixtures.ts'
              : './src/features/demo/demoFixtures.empty.ts'
          ),
        },
        {
          find: '@',
          replacement: path.resolve(__dirname, './src'),
        },
      ],
    },
    css: {
      modules: {
        localsConvention: 'camelCase',
        generateScopedName: '[name]__[local]___[hash:base64:5]',
      },
      preprocessorOptions: {
        scss: {
          additionalData: `@use "@/styles/variables" as *;\n@use "@/styles/mixins" as *;\n`,
        },
      },
    },
    build: {
      target: 'es2020',
      outDir: demoSite ? 'dist-demo' : 'dist',
      assetsInlineLimit: 100000000,
      chunkSizeWarningLimit: 100000000,
      cssCodeSplit: false,
      rolldownOptions: {
        output: {
          codeSplitting: false,
        },
      },
    },
  };
});
