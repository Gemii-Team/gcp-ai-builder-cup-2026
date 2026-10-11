// Test support for the realtime client and the pending queue: a WebSocket driven by hand and a client harness.
// Never exported from the package entry.
import { create, fromBinary, toBinary } from '@bufbuild/protobuf';
import {
  ChannelDeltaSchema,
  ClientFrameSchema,
  MessageSchema,
  ServerEventSchema,
  type ChannelDelta,
  type ClientFrame,
  type Cursor,
  type ServerEvent,
} from '@kiwi/api-client';
import { vi } from 'vitest';
import { createRealtimeClient, type RealtimeEvent } from './client';
import { createMemoryCursorStore } from './cursors';

/** A WebSocket whose server side is played by the test: `serverOpen`, `serverSend`, `serverClose`. */
export class FakeWebSocket {
  static instances: FakeWebSocket[] = [];

  readonly url: string;
  binaryType: BinaryType = 'blob';
  readyState = 0;
  onopen: (() => void) | null = null;
  onmessage: ((event: { data: unknown }) => void) | null = null;
  onclose: ((event: { code: number }) => void) | null = null;
  readonly sent: Uint8Array[] = [];

  constructor(url: string | URL) {
    this.url = String(url);
    FakeWebSocket.instances.push(this);
  }

  send(data: Uint8Array): void {
    this.sent.push(data);
  }

  /** The client closing its own socket: the close event follows, as in a browser. */
  close(code = 1005): void {
    this.serverClose(code);
  }

  serverOpen(): void {
    this.readyState = 1;
    this.onopen?.();
  }

  serverSend(event: ServerEvent): void {
    this.onmessage?.({ data: new Uint8Array(toBinary(ServerEventSchema, event)).buffer });
  }

  serverClose(code: number): void {
    if (this.readyState === 3) {
      return;
    }
    this.readyState = 3;
    this.onclose?.({ code });
  }

  /** Every frame the client has sent on this socket, decoded. */
  frames(): ClientFrame[] {
    return this.sent.map((bytes) => fromBinary(ClientFrameSchema, bytes));
  }

  /** The channel ids of every `Subscribe` frame sent on this socket, one array per frame. */
  subscribes(): string[][] {
    return this.frames().flatMap((frame) => (frame.frame.case === 'subscribe' ? [frame.frame.value.channelIds] : []));
  }

  pings(): number {
    return this.frames().filter((frame) => frame.frame.case === 'ping').length;
  }
}

export function latestSocket(): FakeWebSocket {
  const socket = FakeWebSocket.instances.at(-1);
  if (socket === undefined) {
    throw new Error('The client has not opened a socket.');
  }
  return socket;
}

export function message(channelId: string, seq: number) {
  return create(MessageSchema, { id: `${channelId}-${String(seq)}`, channelId, seq: BigInt(seq), body: `message ${String(seq)}` });
}

/** The frame the gateway sends for a new message. */
export function messageCreated(channelId: string, seq: number): ServerEvent {
  return create(ServerEventSchema, { event: { case: 'messageCreated', value: message(channelId, seq) } });
}

/** What `SyncChannels` returns for a channel: the given seqs ascending, or the latest page with `reload`. */
export function delta(channelId: string, seqs: number[], reload = false): ChannelDelta {
  return create(ChannelDeltaSchema, { channelId, messages: seqs.map((seq) => message(channelId, seq)), reload });
}

/** One line per event, for readable assertions: `a:6` is a message, `reset:a` a reset. */
export function summarize(events: RealtimeEvent[]): string[] {
  return events.map((event) => {
    if ('type' in event) {
      return `${event.type}:${event.channelId}`;
    }
    return event.event.case === 'messageCreated' ? `${event.event.value.channelId}:${String(event.event.value.seq)}` : (event.event.case ?? 'empty');
  });
}

export function cursorPairs(cursors: Cursor[]): [string, bigint][] {
  return cursors.map((cursor) => [cursor.channelId, cursor.lastSeq]);
}

/** Lets timers due now fire and every pending promise continuation run. */
export const settle = (ms = 0) => vi.advanceTimersByTimeAsync(ms);

/**
 * A realtime client on fake timers and fake sockets. Call `vi.useFakeTimers()` first. The backoff draws 0, so a
 * reconnect is due at once; `cursors` seeds the cursor store.
 */
export function harness(cursors: Record<string, number> = {}) {
  FakeWebSocket.instances = [];
  const cursorStore = createMemoryCursorStore();
  for (const [channelId, seq] of Object.entries(cursors)) {
    cursorStore.advance(channelId, seq);
  }
  let tickets = 0;
  const issueTicket = vi.fn<() => Promise<string>>(() => Promise.resolve(`t${String(++tickets)}`));
  const resync = vi.fn<(cursors: Cursor[]) => Promise<ChannelDelta[]>>(() => Promise.resolve([]));
  const client = createRealtimeClient({
    issueTicket,
    wsUrl: 'ws://kiwi.test/ws',
    resync,
    cursorStore,
    backoff: { random: () => 0 },
    WebSocketImpl: FakeWebSocket as unknown as typeof WebSocket,
  });
  const events: RealtimeEvent[] = [];
  client.on((event) => events.push(event));

  /** Connects and opens the socket; with `channels`, subscribes them and forgets the catch-up that caused. */
  async function open(channels: string[] = []): Promise<FakeWebSocket> {
    client.connect();
    await settle();
    const socket = latestSocket();
    socket.serverOpen();
    await settle();
    if (channels.length > 0) {
      client.subscribe(channels);
      await settle();
      resync.mockClear();
      events.length = 0;
    }
    return socket;
  }

  return { client, cursorStore, issueTicket, resync, events, open };
}
