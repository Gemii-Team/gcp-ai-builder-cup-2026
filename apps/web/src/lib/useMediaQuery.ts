import { useSyncExternalStore } from 'react';

/**
 * Whether a media query matches, and again when it changes. Where there is no `matchMedia` (a test environment) it
 * does not match, so the screen shows its wide form.
 */
export function useMediaQuery(query: string): boolean {
  return useSyncExternalStore(
    (onChange) => {
      if (typeof window.matchMedia !== 'function') {
        return () => undefined;
      }
      const list = window.matchMedia(query);
      list.addEventListener('change', onChange);
      return () => {
        list.removeEventListener('change', onChange);
      };
    },
    () => typeof window.matchMedia === 'function' && window.matchMedia(query).matches,
    () => false,
  );
}
