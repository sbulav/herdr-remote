import react from '@vitejs/plugin-react';
import { defineConfig } from 'vite';
import { VitePWA } from 'vite-plugin-pwa';

export default defineConfig({
  server: {
    fs: { strict: true, allow: ['..'] },
  },
  plugins: [
    react(),
    VitePWA({
      strategies: 'injectManifest',
      srcDir: 'src',
      filename: 'sw.ts',
      registerType: 'prompt',
      injectManifest: {
        injectionPoint: 'self.__WB_MANIFEST',
        globPatterns: ['**/*.{css,js,svg,webmanifest,html}'],
      },
      manifest: false,
    }),
  ],
  build: {
    sourcemap: false,
    target: 'es2022',
  },
});
