import { createRootRoute, Outlet } from '@tanstack/react-router';

/** Every route renders inside it; the signed-in shell (rail, tab bar) is added by the routes that need it. */
export const rootRoute = createRootRoute({ component: Outlet });
