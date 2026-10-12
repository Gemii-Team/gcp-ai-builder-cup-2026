import react from '@vitejs/plugin-react';
import { defineConfig } from 'vitest/config';

// Stylesheet tests (theme, fonts) read files from disk and need Node's own `URL`, so the default environment
// stays `node`; component tests opt in per file with `// @vitest-environment jsdom`.
export default defineConfig({
  plugins: [react()],
  test: {
    setupFiles: ['./src/test-setup.ts'],
  },
});
