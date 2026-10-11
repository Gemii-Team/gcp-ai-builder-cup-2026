import { useCallback, useState } from 'react';
import { hardNavigate } from '../../navigation';

/**
 * Signs out: `POST /api/auth/logout` ends the session on the server, then the page leaves for the signed-out state of
 * sign-in. A full navigation, not a route change: it drops everything the old session cached and closes its socket.
 * If the server cannot be reached the person is still signed in, so nothing leaves and `failed` says so.
 */
export function useSignOut() {
  const [failed, setFailed] = useState(false);
  const signOut = useCallback(async () => {
    setFailed(false);
    try {
      const response = await fetch('/api/auth/logout', { method: 'POST', credentials: 'same-origin' });
      if (!response.ok) {
        throw new Error(`logout answered ${String(response.status)}`);
      }
      hardNavigate.to('/signin?signedout=1');
    } catch {
      setFailed(true);
    }
  }, []);
  return { signOut, failed };
}
