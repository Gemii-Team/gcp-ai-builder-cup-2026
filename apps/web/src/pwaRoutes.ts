/**
 * The navigations the service worker answers with the cached app shell: the SPA's own routes, and nothing else.
 *
 * An allowlist, not a denylist, because the app shares its origin with servers that answer navigations of their
 * own: `/api/auth/login` redirects the top-level navigation to the OIDC issuer (`/mockoidc/authorize` under the
 * local mock sign-in), and `/api/blob/...` serves downloads. A denylist would shadow every same-origin path
 * somebody forgot to list, with the shell, once the worker controls the tab.
 *
 * Workbox tests each pattern against the URL's pathname plus its query string, so every pattern allows one:
 * the sign-in route is also opened as `/signin?error=...`. Add a route to `router.tsx` and to this list together:
 * `pwaRoutes.test.ts` reads the router's route tree and fails for a route this list does not answer, and for a server
 * path (`/api`, `/ws`, `/mockoidc`) that it does.
 */
export const SPA_NAVIGATION_ALLOWLIST: RegExp[] = [
  /^\/(?:\?.*)?$/,
  /^\/signin(?:\?.*)?$/,
  /^\/c\/[^/?#]+(?:\?.*)?$/,
  /^\/board(?:\?.*)?$/,
  /^\/ask(?:\?.*)?$/,
];
