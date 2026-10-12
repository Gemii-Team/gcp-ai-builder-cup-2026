import { useQuery, useQueryClient } from '@tanstack/react-query';
import { createMemoryCursorStore, createPendingQueue, type ConnectionStatus, type RealtimeClient } from '@kiwi/core';
import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react';
import { useApi } from '../api/client';
import { sendOutcome } from '../api/errors';
import { mapMessage } from '../api/mappers';
import { channelsQuery, markDelivery, receiveMessage } from '../api/queries';
import { bindToQuery } from './bindToQuery';
import { createAppRealtimeClient } from './client';
import { RealtimeContext } from './context';

/**
 * The signed-in session's live connection. It connects once for the whole app and subscribes to every channel the
 * person is in, not only the open one, so the rail's unread pills and the drafts of a channel stay current. Events
 * write straight into the query cache (`bindToQuery`); the messages the network refused go out again, in order, when
 * the connection is back. `client` is for tests: the app builds its own against the API.
 */
export function RealtimeProvider({ client: provided, children }: { client?: RealtimeClient; children: ReactNode }) {
  const api = useApi();
  const queryClient = useQueryClient();
  const [cursors] = useState(() => createMemoryCursorStore());
  const [queue] = useState(() => createPendingQueue());
  const [client] = useState(() => provided ?? createAppRealtimeClient(api, cursors));
  const [status, setStatus] = useState<ConnectionStatus>(client.status);

  const replay = useCallback(
    () =>
      queue.replay(async (request) => {
        try {
          const { message } = await api.chat.sendMessage(request);
          if (message !== undefined) {
            receiveMessage(queryClient, mapMessage(message));
          }
          return 'sent';
        } catch (error) {
          const outcome = sendOutcome(error);
          if (outcome === 'rejected') {
            markDelivery(queryClient, request.channelId, request.clientMsgId, 'failed');
            return 'rejected';
          }
          if (outcome === 'duplicate') {
            return 'duplicate';
          }
          throw error;
        }
      }),
    [api, queryClient, queue],
  );

  useEffect(() => {
    const unbind = bindToQuery(client, queryClient);
    const unwatch = client.onStatus((next) => {
      setStatus(next);
      if (next === 'open') {
        void replay();
      }
    });
    const onOnline = () => {
      void replay();
    };
    window.addEventListener('online', onOnline);
    client.connect();
    return () => {
      window.removeEventListener('online', onOnline);
      unwatch();
      unbind();
      client.close();
    };
  }, [client, queryClient, replay]);

  const channels = useQuery(channelsQuery(api));
  useEffect(() => {
    if (channels.data === undefined) {
      return;
    }
    for (const channel of channels.data) {
      // A channel the client has not followed yet starts at its head: its history is what a page load shows, so a catch-up
      // from 0 would only replay all of it. One it has followed keeps its cursor: a refresh of this list must not skip frames.
      if (cursors.get(channel.id) === 0) {
        cursors.advance(channel.id, channel.lastSeq);
      }
    }
    client.subscribe(channels.data.map((channel) => channel.id));
  }, [channels.data, client, cursors]);

  const value = useMemo(() => ({ client, cursors, queue, status }), [client, cursors, queue, status]);
  return <RealtimeContext value={value}>{children}</RealtimeContext>;
}
