import react from '@vitejs/plugin-react';
import { defineConfig } from 'vitest/config';

// Separate from vite.config.ts: tests need neither the PWA plugin nor Tailwind.
export default defineConfig({
  plugins: [react()],
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test-setup.ts'],
    // Vitest turns every stylesheet into an empty one; a test that reads one as text (`./x.css?raw`) gets the real text.
    css: { include: [/\.css\?raw$/] },
  },
});
