import { createRootRouteWithContext, Outlet } from '@tanstack/react-router';
import type { SessionContext } from '../features/auth/guard';

/**
 * Every route renders inside it; the signed-in shell (rail, tab bar) is added by the routes that need it. The router's
 * context is the query cache and the API clients, which the guards ask the server who is signed in with.
 */
export const rootRoute = createRootRouteWithContext<SessionContext>()({ component: Outlet });
