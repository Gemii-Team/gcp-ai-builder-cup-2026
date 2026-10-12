import { createRoute } from '@tanstack/react-router';
import { AskScreen } from '../features/ask/AskScreen';
import { appRoute } from './app';

type AskSearch = { channel?: string };

/** `/ask`: Ask, in the signed-in shell. `?channel=<id>` scopes the questions to that channel. */
export const askRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/ask',
  validateSearch: (search: Record<string, unknown>): AskSearch => {
    // The router reads a numeric-looking id as a number; a channel id is always text.
    const channel = typeof search.channel === 'number' ? String(search.channel) : search.channel;
    return typeof channel === 'string' && channel !== '' ? { channel } : {};
  },
  component: function Ask() {
    const { channel } = askRoute.useSearch();
    // A different `?channel` starts the screen afresh: its thread belongs to the channel it was asked in.
    return <AskScreen key={channel ?? ''} initialChannelId={channel ?? null} />;
  },
});
