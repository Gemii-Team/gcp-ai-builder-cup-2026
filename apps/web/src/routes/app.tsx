import { createRoute } from '@tanstack/react-router';
import { AppShell } from '../features/shell/AppShell';
import { rootRoute } from './__root';

/** The signed-in shell (rail, tab bar, live connection): every route that shows them is a child of this one. */
export const appRoute = createRoute({ getParentRoute: () => rootRoute, id: 'app', component: AppShell });
