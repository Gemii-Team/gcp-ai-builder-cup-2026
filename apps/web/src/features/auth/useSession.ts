import { Code } from '@connectrpc/connect';
import { useQueryClient } from '@tanstack/react-query';
import { useRouter } from '@tanstack/react-router';
import { useEffect } from 'react';
import { useMe } from '../../api/hooks';
import { hasCode } from '../../api/errors';
import { hardNavigate } from '../../navigation';
import { signInHref } from './returnTo';

/**
 * The signed-in session: the one `GetMe` query (`meQuery`), and the end of the session. Whenever any call of the signed-in
 * app is answered Unauthenticated (the session expired or was revoked), the page leaves for sign-in with this page as
 * `return_to`. It is a full navigation, like sign-out: it drops what the old session cached and closes its socket.
 * Mount it once, in the signed-in layout.
 */
export function useSession() {
  const queryClient = useQueryClient();
  const router = useRouter();
  useEffect(() => {
    let leaving = false;
    return queryClient.getQueryCache().subscribe((event) => {
      if (leaving || event.type !== 'updated' || event.action.type !== 'error' || !hasCode(event.action.error, Code.Unauthenticated)) {
        return;
      }
      leaving = true;
      hardNavigate.to(signInHref(router.state.location.href));
    });
  }, [queryClient, router]);
  return useMe();
}
