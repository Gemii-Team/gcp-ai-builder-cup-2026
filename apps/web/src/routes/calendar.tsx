import { createRoute } from '@tanstack/react-router';
import { CalendarScreen } from '../features/calendar/Calendar';
import { appRoute } from './app';

/** `/calendar`: upcoming meetings grouped by team and the workspace, in the signed-in shell. */
export const calendarRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/calendar',
  component: CalendarScreen,
});
