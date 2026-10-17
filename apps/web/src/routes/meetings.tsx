import { createRoute } from '@tanstack/react-router';
import { MeetingsListScreen } from '../features/meetings/MeetingsList';
import { appRoute } from './app';

/** `/meetings`: the list of the person's meetings, in the signed-in shell. */
export const meetingsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/meetings',
  component: MeetingsListScreen,
});
