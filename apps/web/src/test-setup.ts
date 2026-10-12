import '@testing-library/jest-dom/vitest';
import { cleanup } from '@testing-library/react';
import { afterEach, beforeEach, vi } from 'vitest';

beforeEach(() => {
  // jsdom does not implement scrolling; the router restores scroll positions and a screen scrolls its stream.
  vi.stubGlobal('scrollTo', vi.fn());
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});
