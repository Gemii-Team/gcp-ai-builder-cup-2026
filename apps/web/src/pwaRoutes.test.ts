import { describe, expect, it } from 'vitest';
import { SPA_NAVIGATION_ALLOWLIST } from './pwaRoutes';

/** Workbox's NavigationRoute tests each allowlist pattern against the navigated URL's pathname + search. */
function shellAnswers(path: string): boolean {
  const url = new URL(path, 'https://kiwi.example');
  return SPA_NAVIGATION_ALLOWLIST.some((pattern) => pattern.test(url.pathname + url.search));
}

describe('service worker navigation allowlist', () => {
  it.each([
    '/',
    '/signin',
    '/signin?error=access_denied',
    '/c/0194f3a2-7b1e-7c3d-9a54-1f2e3d4c5b6a',
    '/c/0194f3a2-7b1e-7c3d-9a54-1f2e3d4c5b6a?msg=12',
    '/board',
    '/board?channel=0194f3a2',
    '/ask',
    '/ask?channel=0194f3a2',
    '/ask?q=%E0%B8%AA%E0%B8%A7%E0%B8%B1%E0%B8%AA%E0%B8%94%E0%B8%B5',
  ])('answers the SPA route %s with the cached shell', (path) => {
    expect(shellAnswers(path)).toBe(true);
  });

  // Any of these answered with index.html would break a flow the server owns: sign-in is a top-level
  // navigation that /api/auth/login redirects to the OIDC issuer on this same origin.
  it.each([
    '/api/auth/login',
    '/api/auth/callback?code=abc&state=xyz',
    '/api/blob/abc',
    '/mockoidc/authorize',
    '/mockoidc/authorize?client_id=kiwi-local&login_hint=alice%40demo.example',
    '/healthz',
    '/readyz',
    '/assets/x.js',
    '/sw.js',
    '/manifest.webmanifest',
    '/c',
    '/c/',
    '/c/abc/def',
    '/board/anything',
    '/signin/extra',
    '/askme',
  ])('leaves %s to the network', (path) => {
    expect(shellAnswers(path)).toBe(false);
  });
});
