import { createRoute } from '@tanstack/react-router';
import { leaveSignInWhenSignedIn } from '../features/auth/guard';
import { LandingPage } from '../features/landing/LandingPage';
import { rootRoute } from './__root';

/**
 * `/welcome`: the landing page, a child of the root like `/signin` (no rail, no tab bar). A signed-out visit to `/` is
 * sent here; a person who is signed in goes on to the app instead.
 */
export const welcomeRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/welcome',
  beforeLoad: ({ context }) => leaveSignInWhenSignedIn(context, undefined),
  component: LandingPage,
});
