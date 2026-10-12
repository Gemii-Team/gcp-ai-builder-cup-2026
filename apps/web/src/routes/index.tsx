import { createRoute } from '@tanstack/react-router';
import { appRoute } from './app';
import { StartPage } from './StartPage';

/** `/`: leads to a channel. */
export const indexRoute = createRoute({ getParentRoute: () => appRoute, path: '/', component: StartPage });
