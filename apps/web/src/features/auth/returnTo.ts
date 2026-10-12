/**
 * Where a sign-in leads on to: a path of this app (with its query), and nothing that could lead somewhere else.
 * `return_to` is a link anybody can craft, so this is the open-redirect guard: a protocol-relative `//host`, an absolute
 * `http:` or `javascript:` URL, and a backslash or a tab or newline (browsers read `/\host` and `/<tab>/host` as `//host`)
 * all come back `undefined`, and the caller falls back to the start page.
 */
export function safeReturnTo(value: string | undefined): string | undefined {
  if (value === undefined || !value.startsWith('/') || value.startsWith('//') || /[\s\\]/.test(value)) {
    return undefined;
  }
  try {
    return new URL(value, window.location.origin).origin === window.location.origin ? value : undefined;
  } catch {
    return undefined;
  }
}

/** The sign-in page, asking to come back to `returnTo` (a path with its query) afterwards. */
export function signInHref(returnTo: string): string {
  return `/signin?return_to=${encodeURIComponent(returnTo)}`;
}

/** The server's sign-in start: a full-page navigation, which redirects to the issuer the server is configured with. */
export function loginHref(returnTo: string): string {
  return `/api/auth/login?return_to=${encodeURIComponent(returnTo)}`;
}
