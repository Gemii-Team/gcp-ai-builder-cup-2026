import { createRoute } from '@tanstack/react-router';
import { ChannelScreen } from '../features/channel/ChannelScreen';
import { appRoute } from './app';

/** `/c/$channelId`: one channel, in the signed-in shell. */
export const channelRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/c/$channelId',
  component: function Channel() {
    const { channelId } = channelRoute.useParams();
    return <ChannelScreen channelId={channelId} />;
  },
});
