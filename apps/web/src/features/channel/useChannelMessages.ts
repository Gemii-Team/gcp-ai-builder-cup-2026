import { create } from '@bufbuild/protobuf';
import { MentionKind, MentionSchema, SendMessageRequestSchema, type SendMessageRequest } from '@kiwi/api-client';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useCallback, useState } from 'react';
import { useApi } from '../../api/client';
import { sendOutcome } from '../../api/errors';
import { useMe } from '../../api/hooks';
import { mapMessage } from '../../api/mappers';
import { addUnsent, fetchMessagePage, markDelivery, messagesQuery, prependMessages, receiveMessage } from '../../api/queries';
import type { ChatMessage, MentionView } from '../../api/types';
import { useRealtime } from '../../realtime/context';

/** The `SendMessageRequest` of a message: its mentions as the server takes them, the agent by its constant label. */
function sendRequest(channelId: string, clientMsgId: string, body: string, mentions: readonly MentionView[]): SendMessageRequest {
  return create(SendMessageRequestSchema, {
    channelId,
    clientMsgId,
    body,
    mentions: mentions.map((mention) =>
      create(MentionSchema, { kind: mention.kind === 'agent' ? MentionKind.AGENT : MentionKind.USER, id: mention.id, label: mention.label }),
    ),
  });
}

/**
 * A channel's messages and the two things a person does to them. The cache holds the latest 50, oldest first, and the
 * socket keeps it current; `loadEarlier` adds the page before the oldest message held. `send` shows the message at once
 * under a `client_msg_id` of its own, and the stored message replaces it whichever arrives first, the send's answer or
 * the gateway's frame. A send the network refuses waits in the pending queue for the connection; one the server refuses
 * stays in the stream as not sent, and `retry` sends it again.
 */
export function useChannelMessages(channelId: string) {
  const api = useApi();
  const queryClient = useQueryClient();
  const { cursors, queue } = useRealtime();
  const me = useMe();
  const query = useQuery(messagesQuery(api, cursors, channelId));
  const [loadingEarlier, setLoadingEarlier] = useState(false);
  const [earlierFailed, setEarlierFailed] = useState(false);

  const loadEarlier = useCallback(async () => {
    const oldest = query.data?.messages.find((message) => message.delivery === undefined);
    if (oldest === undefined) {
      return;
    }
    setLoadingEarlier(true);
    setEarlierFailed(false);
    try {
      const page = await fetchMessagePage(api, channelId, oldest.seq);
      const newest = page.messages.at(-1);
      if (newest !== undefined) {
        cursors.advance(channelId, newest.seq);
      }
      prependMessages(queryClient, channelId, page);
    } catch {
      setEarlierFailed(true);
    } finally {
      setLoadingEarlier(false);
    }
  }, [api, channelId, cursors, query.data, queryClient]);

  const deliver = useCallback(
    async (request: SendMessageRequest) => {
      try {
        const { message } = await api.chat.sendMessage(request);
        if (message !== undefined) {
          receiveMessage(queryClient, mapMessage(message));
        }
      } catch (error) {
        const outcome = sendOutcome(error);
        if (outcome === 'rejected') {
          markDelivery(queryClient, channelId, request.clientMsgId, 'failed');
        } else if (outcome === 'retry') {
          queue.enqueue(request);
          markDelivery(queryClient, channelId, request.clientMsgId, 'queued');
        }
        // `duplicate`: the server has the message already, and its frame turns the unsent row into the stored one.
      }
    },
    [api, channelId, queryClient, queue],
  );

  const send = useCallback(
    async (body: string, mentions: MentionView[]) => {
      const author = me.data?.user.id;
      if (author === undefined) {
        return;
      }
      const request = sendRequest(channelId, crypto.randomUUID(), body, mentions);
      const unsent: ChatMessage = {
        id: request.clientMsgId,
        channelId,
        seq: 0,
        authorKind: 'user',
        authorId: author,
        body,
        mentions,
        clientMsgId: request.clientMsgId,
        createdAt: new Date().toISOString(),
        files: [],
        delivery: 'sending',
      };
      addUnsent(queryClient, unsent);
      await deliver(request);
    },
    [channelId, deliver, me.data, queryClient],
  );

  const retry = useCallback(
    async (clientMsgId: string) => {
      const failed = query.data?.messages.find((message) => message.clientMsgId === clientMsgId && message.delivery === 'failed');
      if (failed === undefined) {
        return;
      }
      markDelivery(queryClient, channelId, clientMsgId, 'sending');
      await deliver(sendRequest(channelId, clientMsgId, failed.body, failed.mentions));
    },
    [channelId, deliver, query.data, queryClient],
  );

  return {
    messages: query.data?.messages,
    hasEarlier: query.data?.hasEarlier ?? false,
    isPending: query.isPending,
    isError: query.isError,
    refetch: query.refetch,
    loadEarlier,
    loadingEarlier,
    earlierFailed,
    send,
    retry,
  };
}
