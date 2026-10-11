import { createRoute } from '@tanstack/react-router';
import { requireSession } from '../features/auth/guard';
import { AppShell } from '../features/shell/AppShell';
import { SessionError } from '../features/shell/SessionError';
import { rootRoute } from './__root';

/**
 * The signed-in shell (rail, tab bar, live connection): every route that shows them is a child of this one, so the session
 * guard on it covers every current and future child. Nobody signed in is sent to `/signin`; a session that cannot be
 * loaded shows `SessionError`.
 */
export const appRoute = createRoute({
  getParentRoute: () => rootRoute,
  id: 'app',
  beforeLoad: requireSession,
  component: AppShell,
  errorComponent: SessionError,
});
