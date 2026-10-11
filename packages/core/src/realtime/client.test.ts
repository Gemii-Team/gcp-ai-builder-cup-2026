import { create } from '@bufbuild/protobuf';
import { PresenceSchema, ServerEventSchema, type ChannelDelta } from '@kiwi/api-client';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createRealtimeClient } from './client';
import { createMemoryCursorStore } from './cursors';
import { cursorPairs, delta, FakeWebSocket, harness, latestSocket, messageCreated, settle, summarize } from './testing';

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.useRealTimers();
});

describe('seq tracking', () => {
  it('TestSeqGap_TriggersResyncAndEmitsInOrderOnce', async () => {
    const h = harness({ a: 5 });
    const socket = await h.open(['a']);

    h.resync.mockResolvedValueOnce([delta('a', [6, 7, 8])]);
    socket.serverSend(messageCreated('a', 8));
    await settle();

    expect(h.resync).toHaveBeenCalledTimes(1);
    expect(cursorPairs(h.resync.mock.calls[0][0])).toEqual([['a', 5n]]);
    // The delta already holds the frame that revealed the gap: it is emitted once, in order.
    expect(summarize(h.events)).toEqual(['a:6', 'a:7', 'a:8']);

    socket.serverSend(messageCreated('a', 8));
    socket.serverSend(messageCreated('a', 9));
    await settle();

    expect(summarize(h.events)).toEqual(['a:6', 'a:7', 'a:8', 'a:9']);
    expect(h.resync).toHaveBeenCalledTimes(1);
    expect(h.cursorStore.get('a')).toBe(9);
  });

  it('TestDuplicateSeq_Dropped', async () => {
    const h = harness({ a: 5 });
    const socket = await h.open(['a']);

    socket.serverSend(messageCreated('a', 5));
    socket.serverSend(messageCreated('a', 3));
    await settle();
    expect(h.events).toEqual([]);

    socket.serverSend(messageCreated('a', 6));
    socket.serverSend(messageCreated('a', 6));
    await settle();

    expect(summarize(h.events)).toEqual(['a:6']);
    expect(h.resync).not.toHaveBeenCalled();
    expect(h.cursorStore.get('a')).toBe(6);
  });

  it('keeps a cursor per channel', async () => {
    const h = harness({ a: 5, b: 20 });
    const socket = await h.open(['a', 'b']);

    socket.serverSend(messageCreated('b', 21));
    socket.serverSend(messageCreated('a', 6));
    await settle();

    expect(summarize(h.events)).toEqual(['b:21', 'a:6']);
    expect(h.resync).not.toHaveBeenCalled();
  });

  it('TestReload_EmitsResetEvent', async () => {
    const h = harness({ a: 5 });
    const socket = await h.open(['a']);

    // More than 200 behind: the server answers with the latest page and `reload`.
    h.resync.mockResolvedValueOnce([delta('a', [399, 400], true)]);
    socket.serverSend(messageCreated('a', 400));
    await settle();

    expect(summarize(h.events)).toEqual(['reset:a']);
    expect(h.events[0]).toEqual({ type: 'reset', channelId: 'a' });
    // The page is the app's to show; the cursor moves to its head so the live stream continues from there.
    expect(h.cursorStore.get('a')).toBe(400);

    socket.serverSend(messageCreated('a', 401));
    await settle();

    expect(summarize(h.events)).toEqual(['reset:a', 'a:401']);
    expect(h.resync).toHaveBeenCalledTimes(1);
  });

  it('passes every other event through in order with the messages', async () => {
    const h = harness({ a: 5 });
    const socket = await h.open(['a']);

    socket.serverSend(messageCreated('a', 6));
    socket.serverSend(create(ServerEventSchema, { event: { case: 'presence', value: create(PresenceSchema, { userId: 'u1', online: true }) } }));
    socket.serverSend(messageCreated('a', 7));
    await settle();

    expect(summarize(h.events)).toEqual(['a:6', 'presence', 'a:7']);
  });

  it('holds later frames behind a resync so nothing is emitted out of order', async () => {
    const h = harness({ a: 5 });
    const socket = await h.open(['a']);

    const answer = Promise.withResolvers<ChannelDelta[]>();
    h.resync.mockReturnValueOnce(answer.promise);
    socket.serverSend(messageCreated('a', 8));
    socket.serverSend(messageCreated('a', 9));
    await settle();
    expect(h.events).toEqual([]);

    answer.resolve([delta('a', [6, 7, 8])]);
    await settle();

    expect(summarize(h.events)).toEqual(['a:6', 'a:7', 'a:8', 'a:9']);
    expect(h.resync).toHaveBeenCalledTimes(1);
  });

  it('closes the socket when a resync fails and recovers through the reconnect', async () => {
    const h = harness({ a: 5 });
    const first = await h.open(['a']);

    h.resync.mockRejectedValueOnce(new Error('network'));
    first.serverSend(messageCreated('a', 8));
    await settle();

    expect(first.readyState).toBe(3);
    expect(h.client.status).toBe('connecting');
    expect(h.events).toEqual([]);

    h.resync.mockResolvedValueOnce([delta('a', [6, 7, 8])]);
    await settle();
    latestSocket().serverOpen();
    await settle();

    expect(latestSocket()).not.toBe(first);
    expect(summarize(h.events)).toEqual(['a:6', 'a:7', 'a:8']);
    expect(h.client.status).toBe('open');
  });

  it('keeps delivering to the other handlers when one throws', async () => {
    const h = harness({ a: 5 });
    const socket = await h.open(['a']);
    const seen: string[] = [];
    h.client.on(() => {
      throw new Error('a broken handler');
    });
    h.client.on((event) => seen.push(...summarize([event])));
    // The error is rethrown on a microtask of its own, where the host reports it; capture that task instead.
    const reported: (() => void)[] = [];
    vi.spyOn(globalThis, 'queueMicrotask').mockImplementation((task) => void reported.push(task));

    socket.serverSend(messageCreated('a', 6));
    await settle();

    expect(reported).toHaveLength(1);
    expect(reported[0]).toThrow('a broken handler');
    expect(seen).toEqual(['a:6']);
    expect(h.cursorStore.get('a')).toBe(6);
  });

  it('stops delivering to a handler after it unsubscribes', async () => {
    const h = harness({ a: 5 });
    const socket = await h.open(['a']);
    const seen: string[] = [];
    const off = h.client.on((event) => seen.push(...summarize([event])));

    socket.serverSend(messageCreated('a', 6));
    await settle();
    off();
    socket.serverSend(messageCreated('a', 7));
    await settle();

    expect(seen).toEqual(['a:6']);
  });
});

describe('connection', () => {
  it('TestReconnect_NewTicketAndResyncsAllCursors', async () => {
    const h = harness({ a: 5, b: 9 });
    const first = await h.open(['a', 'b']);
    expect(first.url).toBe('ws://kiwi.test/ws?ticket=t1');
    expect(first.subscribes()).toEqual([['a', 'b']]);

    first.serverClose(1006);
    expect(h.client.status).toBe('connecting');
    await settle();
    const second = latestSocket();
    second.serverOpen();
    await settle();

    expect(second).not.toBe(first);
    expect(h.issueTicket).toHaveBeenCalledTimes(2);
    expect(second.url).toBe('ws://kiwi.test/ws?ticket=t2');
    expect(second.subscribes()).toEqual([['a', 'b']]);
    expect(h.resync).toHaveBeenCalledTimes(1);
    expect(cursorPairs(h.resync.mock.calls[0][0])).toEqual([
      ['a', 5n],
      ['b', 9n],
    ]);
    expect(h.client.status).toBe('open');

    // A third drop: the ticket is single use, so each reconnect asks for a new one.
    second.serverClose(1006);
    await settle();
    expect(h.issueTicket).toHaveBeenCalledTimes(3);
    expect(latestSocket().url).toBe('ws://kiwi.test/ws?ticket=t3');
  });

  it('resyncs from the cursors as they stand after the live frames of the first connection', async () => {
    const h = harness({ a: 5 });
    const first = await h.open(['a']);
    first.serverSend(messageCreated('a', 6));
    await settle();

    first.serverClose(1006);
    await settle();
    latestSocket().serverOpen();
    await settle();

    expect(cursorPairs(h.resync.mock.calls[0][0])).toEqual([['a', 6n]]);
  });

  it('refuses a relative gateway URL when it is created, rather than retry it forever', () => {
    const options = { issueTicket: () => Promise.resolve('t'), resync: () => Promise.resolve([]), cursorStore: createMemoryCursorStore() };

    expect(() => createRealtimeClient({ ...options, wsUrl: '/ws' })).toThrow(TypeError);
    expect(() => createRealtimeClient({ ...options, wsUrl: 'wss://kiwi.test/ws' })).not.toThrow();
  });

  it('catches up on a channel when it is subscribed while the socket is open', async () => {
    const h = harness({ a: 5 });
    const socket = await h.open();
    expect(h.resync).not.toHaveBeenCalled();

    h.resync.mockResolvedValueOnce([delta('a', [6])]);
    h.client.subscribe(['a', 'a']);
    await settle();

    expect(socket.subscribes()).toEqual([['a']]);
    expect(cursorPairs(h.resync.mock.calls[0][0])).toEqual([['a', 5n]]);
    expect(summarize(h.events)).toEqual(['a:6']);

    h.client.subscribe(['a']);
    await settle();
    expect(socket.subscribes()).toEqual([['a']]);
  });

  it('subscribes channels added before the socket opens when it does', async () => {
    const h = harness({ a: 5 });
    h.client.subscribe(['a']);
    h.client.connect();
    await settle();
    expect(latestSocket().url).toBe('ws://kiwi.test/ws?ticket=t1');
    expect(latestSocket().binaryType).toBe('arraybuffer');

    latestSocket().serverOpen();
    await settle();

    expect(latestSocket().subscribes()).toEqual([['a']]);
    expect(h.resync).toHaveBeenCalledTimes(1);
  });

  it('retries with a new attempt when the ticket cannot be issued', async () => {
    const h = harness();
    h.issueTicket.mockRejectedValueOnce(new Error('unauthenticated'));
    h.client.connect();
    await settle();
    expect(h.client.status).toBe('connecting');

    await settle(30_000);
    expect(h.issueTicket).toHaveBeenCalledTimes(2);
    latestSocket().serverOpen();
    await settle();
    expect(h.client.status).toBe('open');
  });

  it('reports each status change and stops reconnecting after close', async () => {
    const h = harness();
    const statuses: string[] = [];
    h.client.onStatus((status) => statuses.push(status));
    expect(h.client.status).toBe('closed');

    const socket = await h.open();
    socket.serverClose(1006);
    await settle();
    latestSocket().serverOpen();
    await settle();
    h.client.close();
    await settle(60_000);

    expect(statuses).toEqual(['connecting', 'open', 'connecting', 'open', 'closed']);
    expect(h.client.status).toBe('closed');
    expect(FakeWebSocket.instances).toHaveLength(2);
    expect(latestSocket().readyState).toBe(3);
  });

  it('delivers nothing after close even when a resync answers late', async () => {
    const h = harness({ a: 5 });
    const socket = await h.open(['a']);

    const answer = Promise.withResolvers<ChannelDelta[]>();
    h.resync.mockReturnValueOnce(answer.promise);
    socket.serverSend(messageCreated('a', 8));
    await settle();
    h.client.close();
    answer.resolve([delta('a', [6, 7, 8])]);
    await settle();

    expect(h.events).toEqual([]);
  });

  it('TestPing_EveryTwentyFiveSeconds', async () => {
    const h = harness();
    const socket = await h.open();
    expect(socket.pings()).toBe(0);

    await settle(24_999);
    expect(socket.pings()).toBe(0);
    await settle(1);
    expect(socket.pings()).toBe(1);
    await settle(50_000);
    expect(socket.pings()).toBe(3);

    h.client.close();
    await settle(100_000);
    expect(socket.pings()).toBe(3);
  });
});

describe('a resync that never answers', () => {
  const DEADLINE_MS = 15_000;
  // The harness's backoff draws 0, but fake timers turn a 0 ms timer set during a tick into 1 ms: the reconnect is due 1 ms after the failure.
  const RECONNECT_MS = 1;

  /** Channel `a` at cursor 5 on an open socket, then a frame for seq 8 whose resync never settles (`late` settles it). */
  async function hang(resyncTimeoutMs?: number) {
    const h = harness({ a: 5 }, { resyncTimeoutMs });
    const first = await h.open(['a']);
    const late = Promise.withResolvers<ChannelDelta[]>();
    h.resync.mockReturnValueOnce(late.promise);
    first.serverSend(messageCreated('a', 8));
    await settle();
    expect(h.resync).toHaveBeenCalledTimes(1);
    return { h, first, late };
  }

  it('closes the socket at the deadline, and the next connection catches up exactly once and in order', async () => {
    const { h, first } = await hang();
    const statuses: string[] = [];
    h.client.onStatus((status) => statuses.push(status));

    await settle(DEADLINE_MS - 1);
    expect(first.readyState).toBe(1);
    expect(h.client.status).toBe('open');
    expect(h.issueTicket).toHaveBeenCalledTimes(1);

    h.resync.mockResolvedValueOnce([delta('a', [6, 7, 8])]);
    await settle(1);
    expect(first.readyState).toBe(3);
    expect(h.client.status).toBe('connecting');
    await settle(RECONNECT_MS);
    expect(h.issueTicket).toHaveBeenCalledTimes(2);
    const second = latestSocket();
    expect(second.url).toBe('ws://kiwi.test/ws?ticket=t2');
    second.serverOpen();
    await settle();

    expect(summarize(h.events)).toEqual(['a:6', 'a:7', 'a:8']);
    expect(cursorPairs(h.resync.mock.calls[1][0])).toEqual([['a', 5n]]);
    expect(h.resync).toHaveBeenCalledTimes(2);
    expect(statuses).toEqual(['connecting', 'open']);
  });

  it('honours resyncTimeoutMs', async () => {
    const { h, first } = await hang(1_000);

    await settle(999);
    expect(first.readyState).toBe(1);
    await settle(1);
    expect(first.readyState).toBe(3);
    expect(h.client.status).toBe('connecting');
  });

  it('ignores what the abandoned call finally returns, and does not report its late failure', async () => {
    const { h, late } = await hang();
    h.resync.mockResolvedValueOnce([delta('a', [6, 7, 8])]);
    await settle(DEADLINE_MS + RECONNECT_MS);
    latestSocket().serverOpen();
    await settle();
    expect(summarize(h.events)).toEqual(['a:6', 'a:7', 'a:8']);

    late.resolve([delta('a', [6, 7, 8, 9])]);
    await settle();
    expect(summarize(h.events)).toEqual(['a:6', 'a:7', 'a:8']);
    expect(h.cursorStore.get('a')).toBe(8);
  });

  it('does not report an unhandled rejection when the abandoned call later fails', async () => {
    const { late } = await hang();
    await settle(DEADLINE_MS + RECONNECT_MS);

    late.reject(new Error('the request died after the client gave up on it'));
    await settle();
  });

  it('drops the frames queued behind the hung call instead of resyncing once for each', async () => {
    const { h, first } = await hang();
    first.serverSend(messageCreated('a', 9));
    first.serverSend(messageCreated('a', 10));
    first.serverSend(messageCreated('a', 11));
    await settle();

    h.resync.mockResolvedValueOnce([delta('a', [6, 7, 8, 9, 10, 11])]);
    await settle(DEADLINE_MS + RECONNECT_MS);
    // The abandoned attempt asked once; its three queued frames asked for nothing.
    expect(FakeWebSocket.instances).toHaveLength(2);
    expect(h.resync).toHaveBeenCalledTimes(1);
    expect(h.events).toEqual([]);

    latestSocket().serverOpen();
    await settle();

    expect(h.resync).toHaveBeenCalledTimes(2);
    expect(summarize(h.events)).toEqual(['a:6', 'a:7', 'a:8', 'a:9', 'a:10', 'a:11']);
  });

  it('aborts the signal it gave resync when the deadline passes', async () => {
    const { h } = await hang();
    const signal = h.resync.mock.calls[0][1];

    expect(signal.aborted).toBe(false);
    await settle(DEADLINE_MS - 1);
    expect(signal.aborted).toBe(false);
    await settle(1);
    expect(signal.aborted).toBe(true);
  });

  it('aborts the signal on close(), and a later connect() is not blocked by the abandoned call', async () => {
    const { h } = await hang();
    const signal = h.resync.mock.calls[0][1];

    h.client.close();
    expect(signal.aborted).toBe(true);
    expect(h.client.status).toBe('closed');

    h.resync.mockResolvedValueOnce([delta('a', [6, 7, 8])]);
    h.client.connect();
    await settle();
    latestSocket().serverOpen();
    await settle();

    expect(summarize(h.events)).toEqual(['a:6', 'a:7', 'a:8']);
    expect(h.client.status).toBe('open');
  });

  it('leaves no deadline timer behind once resync has answered', async () => {
    const h = harness({ a: 5 });
    const socket = await h.open(['a']);
    const timersWhenIdle = vi.getTimerCount();

    h.resync.mockResolvedValueOnce([delta('a', [6, 7, 8])]);
    socket.serverSend(messageCreated('a', 8));
    await settle();

    expect(summarize(h.events)).toEqual(['a:6', 'a:7', 'a:8']);
    expect(vi.getTimerCount()).toBe(timersWhenIdle);
  });
});
