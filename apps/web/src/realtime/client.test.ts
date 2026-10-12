import { create } from '@bufbuild/protobuf';
import { ChannelDeltaSchema, CursorSchema, IssueRealtimeTicketResponseSchema, SyncChannelsResponseSchema } from '@kiwi/api-client';
import { createMemoryCursorStore } from '@kiwi/core';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { createStubApi } from '../testing/stubApi';
import { realtimeOptions, TICKET_TIMEOUT_MS } from './client';

afterEach(() => {
  vi.restoreAllMocks();
});

describe('realtimeOptions', () => {
  it('asks for a ticket under AbortSignal.timeout, so a request that hangs fails instead of hanging the connection', async () => {
    const api = createStubApi();
    const timeout = vi.spyOn(AbortSignal, 'timeout').mockReturnValue(AbortSignal.abort(new DOMException('timed out', 'TimeoutError')));
    // What a fetch does with an aborted signal: it fails with the signal's reason.
    api.identity.issueRealtimeTicket.mockImplementation((_request, options) => {
      options?.signal?.throwIfAborted();
      return Promise.resolve(create(IssueRealtimeTicketResponseSchema, { ticket: 't-1' }));
    });

    await expect(realtimeOptions(api, createMemoryCursorStore()).issueTicket()).rejects.toMatchObject({ name: 'TimeoutError' });

    expect(timeout).toHaveBeenCalledWith(TICKET_TIMEOUT_MS);
    expect(api.identity.issueRealtimeTicket).toHaveBeenCalledWith({}, expect.objectContaining({ signal: expect.any(AbortSignal) as AbortSignal }));
  });

  it('returns the ticket of a request that answers in time', async () => {
    const api = createStubApi();
    api.identity.issueRealtimeTicket.mockResolvedValue(create(IssueRealtimeTicketResponseSchema, { ticket: 't-7' }));

    await expect(realtimeOptions(api, createMemoryCursorStore()).issueTicket()).resolves.toBe('t-7');
  });

  it('hands the client its own signal to SyncChannels, so an abandoned catch-up is dropped on the wire', async () => {
    const api = createStubApi();
    const deltas = [create(ChannelDeltaSchema, { channelId: 'ch-q4', reload: true })];
    api.chat.syncChannels.mockResolvedValue(create(SyncChannelsResponseSchema, { deltas }));
    const abandon = new AbortController();
    const cursors = [create(CursorSchema, { channelId: 'ch-q4', lastSeq: 7n })];

    await expect(realtimeOptions(api, createMemoryCursorStore()).resync(cursors, abandon.signal)).resolves.toEqual(deltas);

    expect(api.chat.syncChannels).toHaveBeenCalledWith({ cursors }, { signal: abandon.signal });
  });

  it('opens the socket on the origin of the page: ws for http, wss for https', () => {
    const api = createStubApi();

    expect(realtimeOptions(api, createMemoryCursorStore(), { protocol: 'http:', host: 'localhost:5173' }).wsUrl).toBe('ws://localhost:5173/ws');
    expect(realtimeOptions(api, createMemoryCursorStore(), { protocol: 'https:', host: 'kiwi.example' }).wsUrl).toBe('wss://kiwi.example/ws');
  });
});
