import tailwindcss from '@tailwindcss/vite';
import react from '@vitejs/plugin-react';
import { defineConfig } from 'vite';
import { VitePWA } from 'vite-plugin-pwa';
import { SPA_NAVIGATION_ALLOWLIST } from './src/pwaRoutes';

export default defineConfig({
  plugins: [
    react(),
    tailwindcss(),
    VitePWA({
      registerType: 'autoUpdate',
      // The glob below already precaches the icons from public/; without this they are listed twice.
      includeManifestIcons: false,
      manifest: {
        id: '/',
        name: 'K.I.W.I.',
        short_name: 'K.I.W.I.',
        description: 'A team workspace with AI agents built in: summaries, draft tasks and cited answers.',
        lang: 'en',
        start_url: '/',
        scope: '/',
        display: 'standalone',
        theme_color: '#0F3D3A',
        background_color: '#E6F2EE',
        icons: [
          { src: 'icons/icon-192.png', sizes: '192x192', type: 'image/png' },
          { src: 'icons/icon-512.png', sizes: '512x512', type: 'image/png' },
          { src: 'icons/icon-maskable-512.png', sizes: '512x512', type: 'image/png', purpose: 'maskable' },
        ],
      },
      workbox: {
        // The shell and every font file it draws, so the app starts offline with Thai in Anuphan.
        globPatterns: ['**/*.{js,css,html,svg,png,woff2}'],
        cleanupOutdatedCaches: true,
        navigateFallback: 'index.html',
        // Only the SPA's own routes get the cached shell; every other navigation (/api/auth/login and the OIDC
        // issuer it redirects to, /api/blob, the probes) reaches the network. See src/pwaRoutes.ts.
        navigateFallbackAllowlist: SPA_NAVIGATION_ALLOWLIST,
      },
    }),
  ],
});
