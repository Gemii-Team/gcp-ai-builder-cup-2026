import { createRoute } from '@tanstack/react-router';
import { authConfigQuery } from '../features/auth/authConfig';
import { leaveSignInWhenSignedIn } from '../features/auth/guard';
import { safeReturnTo } from '../features/auth/returnTo';
import { SignInPage } from '../features/auth/SignInPage';
import { rootRoute } from './__root';

/** A search value as text: the router reads `?error=500` as a number. An empty one is no value. */
function text(value: unknown): string | undefined {
  const asText = typeof value === 'number' ? String(value) : value;
  return typeof asText === 'string' && asText !== '' ? asText : undefined;
}

/**
 * What the server and the guards put in the query of `/signin`: `return_to` (where to go after), `error` (a code of the
 * OAuth callback) with `email` (the address a `domain_not_allowed` or `not_invited` rejected) and `signedout=1` (after sign-out).
 */
function parseSignInSearch(search: Record<string, unknown>) {
  return {
    return_to: text(search.return_to),
    error: text(search.error),
    email: text(search.email),
    signedout: text(search.signedout) === '1' || search.signedout === true ? true : undefined,
  };
}

/**
 * `/signin`: a child of the root, not of the signed-in layout, so it has no rail and no tab bar. A person who is signed
 * in leaves it before it renders. The names it shows are read in parallel with that check, so the page opens with them.
 */
export const signinRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/signin',
  validateSearch: parseSignInSearch,
  beforeLoad: async ({ context, search }) => {
    const names = context.queryClient.prefetchQuery(authConfigQuery);
    await leaveSignInWhenSignedIn(context, search.return_to);
    await names;
  },
  component: function SignIn() {
    const { return_to: returnTo, error, email, signedout } = signinRoute.useSearch();
    return <SignInPage returnTo={safeReturnTo(returnTo) ?? '/'} error={error} email={email} signedOut={signedout === true} />;
  },
});
