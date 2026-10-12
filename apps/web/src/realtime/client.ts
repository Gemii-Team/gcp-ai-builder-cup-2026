import { createRealtimeClient, type CursorStore, type RealtimeClient, type RealtimeOptions } from '@kiwi/core';
import type { Api } from '../api/client';

/** A ticket request that has not answered by then is abandoned: the client retries with a new one instead of waiting forever. */
export const TICKET_TIMEOUT_MS = 10_000;

/**
 * The realtime client's wiring to the API, on the page's own origin: tickets from `IssueRealtimeTicket`, each request
 * bounded by `AbortSignal.timeout`; catch-ups through `SyncChannels` with the client's own `signal`, which it aborts when
 * it abandons the call; the socket at `/ws` on `ws:` or `wss:` to match the page.
 */
export function realtimeOptions(api: Api, cursorStore: CursorStore, where: Pick<Location, 'protocol' | 'host'> = window.location): RealtimeOptions {
  return {
    issueTicket: async () => (await api.identity.issueRealtimeTicket({}, { signal: AbortSignal.timeout(TICKET_TIMEOUT_MS) })).ticket,
    wsUrl: `${where.protocol === 'https:' ? 'wss:' : 'ws:'}//${where.host}/ws`,
    resync: async (cursors, signal) => (await api.chat.syncChannels({ cursors }, { signal })).deltas,
    cursorStore,
  };
}

export function createAppRealtimeClient(api: Api, cursorStore: CursorStore): RealtimeClient {
  return createRealtimeClient(realtimeOptions(api, cursorStore));
}
