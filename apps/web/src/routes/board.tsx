import { createRoute } from '@tanstack/react-router';
import { BoardScreen } from '../features/board/Board';
import { appRoute } from './app';

type BoardSearch = { channel?: string };

/** `/board`: the Board, in the signed-in shell. `?channel=<id>` starts the Channel filter on that channel. */
export const boardRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/board',
  validateSearch: (search: Record<string, unknown>): BoardSearch => {
    // The router reads a numeric-looking id as a number; a channel id is always text.
    const channel = typeof search.channel === 'number' ? String(search.channel) : search.channel;
    return typeof channel === 'string' && channel !== '' ? { channel } : {};
  },
  component: function Board() {
    const { channel } = boardRoute.useSearch();
    // A different `?channel` starts the screen afresh, with its filter on that channel.
    return <BoardScreen key={channel ?? ''} initialChannelId={channel ?? null} />;
  },
});
