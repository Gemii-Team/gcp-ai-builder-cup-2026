import { createRoute } from '@tanstack/react-router';
import { MeetingDetailScreen } from '../features/meetings/MeetingDetail';
import { appRoute } from './app';

/** `/meeting/$meetingId`: one meeting's AI summary and its draft tasks. */
export const meetingRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/meeting/$meetingId',
  component: function Meeting() {
    const { meetingId } = meetingRoute.useParams();
    return <MeetingDetailScreen key={meetingId} meetingId={meetingId} />;
  },
});
