import { describe, expect, it } from 'vitest';
import { SPA_NAVIGATION_ALLOWLIST } from './pwaRoutes';
import { createAppRouter } from './router';
import { createTestQueryClient } from './testing/render';
import { createStubApi } from './testing/stubApi';

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

describe('the router and the allowlist', () => {
  // Every route of the real tree, as the router lists them (a layout route has no path of its own), with each `$param`
  // segment filled in by an id-shaped value: what a person opens, and what a cold start offline has to answer.
  const router = createAppRouter({ queryClient: createTestQueryClient(), api: createStubApi() });
  const routes = Object.keys(router.routesByPath).map((fullPath) => fullPath.replace(/\$\w+/g, '0194f3a2-7b1e-7c3d-9a54-1f2e3d4c5b6a'));

  it('reads the routes of the tree, so the checks below cannot pass on an empty list', () => {
    expect(routes.length).toBeGreaterThan(0);
    expect(routes).toContain('/signin');
  });

  it.each(routes)('answers the route %s of the router with the cached shell, with or without a query', (path) => {
    expect(shellAnswers(path)).toBe(true);
    expect(shellAnswers(`${path}?x=1`)).toBe(true);
  });

  it('has no route under a path the server owns', () => {
    const owned = routes.filter((path) => /^\/(?:api|ws|mockoidc)(?:[/?]|$)/.test(path));

    expect(owned).toEqual([]);
  });

  it.each(['/api', '/api/', '/ws', '/ws?ticket=abc', '/mockoidc', '/mockoidc/', '/mockoidc/authorize'])('leaves the server path %s to the network', (path) => {
    expect(shellAnswers(path)).toBe(false);
  });
});
