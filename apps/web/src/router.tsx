import { createRouter } from '@tanstack/react-router';
import { rootRoute } from './routes/__root';
import { appRoute } from './routes/app';
import { channelRoute } from './routes/channel';
import { indexRoute } from './routes/index';

// Every route added here is also added to the service worker's navigation allowlist (`pwaRoutes.ts`).
const routeTree = rootRoute.addChildren([appRoute.addChildren([indexRoute, channelRoute])]);

export const router = createRouter({ routeTree });

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router;
  }
}
