import { createRouter, type Router, type RouterHistory } from '@tanstack/react-router';
import type { SessionContext } from './features/auth/guard';
import { rootRoute } from './routes/__root';
import { appRoute } from './routes/app';
import { boardRoute } from './routes/board';
import { channelRoute } from './routes/channel';
import { indexRoute } from './routes/index';
import { signinRoute } from './routes/signin';

// Every route added here is also added to the service worker's navigation allowlist (`pwaRoutes.ts`). `/signin` sits under
// the root and the rest under the signed-in layout, whose `beforeLoad` is the session guard.
const routeTree = rootRoute.addChildren([signinRoute, appRoute.addChildren([indexRoute, boardRoute, channelRoute])]);

/** The app's router, typed for every route of the tree. */
export type AppRouter = Router<typeof routeTree>;

/** `context` is what the guards ask the server with; `history` is for tests (a memory history). */
export function createAppRouter(context: SessionContext, history?: RouterHistory): AppRouter {
  return createRouter({ routeTree, context, history });
}

declare module '@tanstack/react-router' {
  interface Register {
    router: AppRouter;
  }
}
