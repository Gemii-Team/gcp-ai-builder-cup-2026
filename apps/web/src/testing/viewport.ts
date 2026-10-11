// A window of a given width for `matchMedia`, so a screen shows its phone form. Test support: nothing in the app imports this file.
import { vi } from 'vitest';

const WIDTH_QUERY = /\((min|max)-width:\s*(\d+)px\)/;

/** Whether a width media query (`(max-width: 1023px)`) matches a window `width` px wide; any other query does not. */
function matches(query: string, width: number): boolean {
  const found = WIDTH_QUERY.exec(query);
  if (found === null) {
    return false;
  }
  const limit = Number(found[2]);
  return found[1] === 'min' ? width >= limit : width <= limit;
}

/** `matchMedia` and `innerWidth` for a window `width` px wide. `test-setup.ts` unstubs both after each test. */
export function stubViewport(width: number): void {
  vi.stubGlobal('innerWidth', width);
  vi.stubGlobal('matchMedia', (query: string) => ({
    matches: matches(query, width),
    media: query,
    addEventListener: () => undefined,
    removeEventListener: () => undefined,
  }));
}
