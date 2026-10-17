import { Code } from '@connectrpc/connect';
import type { QueryClient } from '@tanstack/react-query';
import { redirect, type ParsedLocation } from '@tanstack/react-router';
import type { Api } from '../../api/client';
import { hasCode } from '../../api/errors';
import { meQuery } from '../../api/queries';
import { safeReturnTo, signInHref } from './returnTo';

/** What the route guards ask the server who is signed in with: the cache and the clients the screens use, so GetMe is one query. */
export type SessionContext = { queryClient: QueryClient; api: Api };

/**
 * The `beforeLoad` of the signed-in layout, so every route under it is guarded. A person the server does not know goes to
 * the landing page from `/`, and from any other page to sign-in, which brings them back to it (path and query) afterwards; any other failure is thrown to the layout's
 * error state, which offers Retry. A session already in the cache is not asked again on every navigation: a call that is
 * refused later sends the person to sign-in (`useSession`). The redirect throws itself (`throw: true`): it is a router
 * signal, not an `Error`.
 */
export async function requireSession({ context, location }: { context: SessionContext; location: ParsedLocation }): Promise<void> {
  try {
    await context.queryClient.ensureQueryData(meQuery(context.api));
  } catch (error) {
    if (!hasCode(error, Code.Unauthenticated)) {
      throw error;
    }
    // The bare start page is where a newcomer arrives: they see what K.I.W.I. is first. Any deeper link goes straight to
    // sign-in and comes back to that page.
    if (location.pathname === '/') {
      redirect({ href: '/welcome', throw: true });
    }
    redirect({ href: signInHref(location.href), throw: true });
  }
}

/**
 * The `beforeLoad` of `/signin`: a signed-in person is sent on, to `return_to` when it is a path of this app and to the
 * start page otherwise. If the server cannot say who is signed in, the page shows anyway: sign-in is never blocked.
 */
export async function leaveSignInWhenSignedIn({ queryClient, api }: SessionContext, returnTo: string | undefined): Promise<void> {
  try {
    await queryClient.ensureQueryData(meQuery(api));
  } catch {
    return;
  }
  redirect({ href: safeReturnTo(returnTo) ?? '/', throw: true });
}
