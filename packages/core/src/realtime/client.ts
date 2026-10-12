import { create, fromBinary, toBinary } from '@bufbuild/protobuf';
import {
  ClientFrameSchema,
  CursorSchema,
  PingSchema,
  ServerEventSchema,
  SubscribeSchema,
  type ChannelDelta,
  type Cursor,
  type Message,
  type ServerEvent,
} from '@kiwi/api-client';
import { toSafeNumber } from '../int64';
import { createBackoff, type BackoffOptions } from './backoff';
import type { CursorStore } from './cursors';

/** A channel's cursor was too far behind the server's: drop what you hold for it and load its latest page again. */
export type ResetEvent = { type: 'reset'; channelId: string };

/** What `RealtimeClient.on` delivers: the gateway's frames as the generated API types carry them, plus resets. */
export type RealtimeEvent = ServerEvent | ResetEvent;

/**
 * `closed`: not connected and not trying (never connected, or `close()` was called). `connecting`: trying, which includes
 * the wait before the next attempt. `open`: the socket is open.
 */
export type ConnectionStatus = 'connecting' | 'open' | 'closed';

export type RealtimeOptions = {
  /** `IdentityService.IssueRealtimeTicket`: a ticket is single use, so every connection asks for a new one. */
  issueTicket: () => Promise<string>;
  /** The gateway's WebSocket URL, absolute (`wss://host/ws`; a relative one throws a TypeError at creation); the ticket is added as `?ticket=`. */
  wsUrl: string;
  /**
   * `ChatService.SyncChannels`: what each cursor is missing, per channel. The client waits at most `resyncTimeoutMs`
   * for the answer; a call that rejects or has not answered by then counts as failed: the socket is closed and the
   * reconnect, with a new ticket, resyncs every subscribed channel from the stored cursors. A late answer of an abandoned
   * call is ignored. `signal` is aborted when the call is abandoned (deadline, failure, `close()`, a dropped socket):
   * pass it to the request so the HTTP call is really dropped; a function that ignores it still cannot block the client.
   */
  resync: (cursors: Cursor[], signal: AbortSignal) => Promise<ChannelDelta[]>;
  /** How long one `resync` call may take before it counts as failed. Default 15 000 ms. */
  resyncTimeoutMs?: number;
  cursorStore: CursorStore;
  backoff?: BackoffOptions;
  WebSocketImpl?: typeof WebSocket;
};

export type RealtimeClient = {
  /** Starts connecting, and keeps reconnecting until `close()`. Does nothing when already connecting or open. */
  connect: () => void;
  /** Closes the socket for good: no reconnect, no further events. */
  close: () => void;
  /**
   * Asks for the frames of these channels, now if the socket is open and on every later connection. Each channel is also
   * caught up from its stored cursor as soon as its subscription is live.
   */
  subscribe: (channelIds: string[]) => void;
  /** Handlers run in order of arrival; a handler that throws does not stop the others. Returns the unsubscribe function. */
  on: (handler: (event: RealtimeEvent) => void) => () => void;
  /** Handlers for `status` changes, e.g. to replay the pending queue when it becomes `open`. Returns the unsubscribe function. */
  onStatus: (handler: (status: ConnectionStatus) => void) => () => void;
  readonly status: ConnectionStatus;
};

const PING_INTERVAL_MS = 25_000;
const DEFAULT_RESYNC_TIMEOUT_MS = 15_000;
const CLOSE_GOING_AWAY = 1001;
const PING_FRAME = toBinary(ClientFrameSchema, create(ClientFrameSchema, { frame: { case: 'ping', value: create(PingSchema) } }));

/** Calls every handler; one that throws is reported on its own microtask, where the host shows it, and the rest still run. */
function notify<T>(handlers: Set<(value: T) => void>, value: T) {
  for (const handler of [...handlers]) {
    try {
      handler(value);
    } catch (error) {
      queueMicrotask(() => {
        throw error;
      });
    }
  }
}

/**
 * The realtime client. Frames are hints, the API is the truth: per channel it tracks the last `seq` it delivered, drops
 * a frame at or below it, and on a frame beyond `cursor + 1` fetches the missing range through `resync` and delivers it
 * in order. Frames and resync results pass through one queue, so nothing is delivered out of order or twice. A resync
 * that fails or does not answer in time ends the connection, and the reconnect resyncs everything from the stored cursors.
 */
export function createRealtimeClient(o: RealtimeOptions): RealtimeClient {
  const wsUrl = new URL(o.wsUrl);
  const backoff = createBackoff(o.backoff);
  const resyncTimeoutMs = o.resyncTimeoutMs ?? DEFAULT_RESYNC_TIMEOUT_MS;
  const subscribed = new Set<string>();
  const handlers = new Set<(event: RealtimeEvent) => void>();
  const statusHandlers = new Set<(status: ConnectionStatus) => void>();

  let status: ConnectionStatus = 'closed';
  let socket: WebSocket | undefined;
  /**
   * The id of the current connection attempt. Every callback and queued task carries the id it was made for and does
   * nothing once this has moved on, which is how a dead connection's leftovers are dropped.
   */
  let attempt = 0;
  /** Aborted when the current attempt ends, so the `resync` call it may have in flight is dropped. */
  let attemptSignal = new AbortController();
  /** Failed attempts in a row: the exponent of the backoff. Cleared once a connection has caught up. */
  let failures = 0;
  let retryTimer: ReturnType<typeof setTimeout> | undefined;
  let pingTimer: ReturnType<typeof setInterval> | undefined;
  let queue: Promise<void> = Promise.resolve();

  function setStatus(next: ConnectionStatus) {
    if (status !== next) {
      status = next;
      notify(statusHandlers, next);
    }
  }

  /**
   * Ends the current attempt: its queued tasks are skipped, its `resync` call is aborted, its timers stop. Returns the
   * socket it had, for the caller to close.
   */
  function endAttempt(): WebSocket | undefined {
    attempt++;
    attemptSignal.abort();
    attemptSignal = new AbortController();
    clearTimeout(retryTimer);
    retryTimer = undefined;
    clearInterval(pingTimer);
    pingTimer = undefined;
    const ws = socket;
    socket = undefined;
    return ws;
  }

  /** Attempt `owner` failed: close its socket and reconnect after the backoff. A newer attempt is left alone. */
  function fail(owner: number) {
    if (owner === attempt) {
      const ws = endAttempt();
      setStatus('connecting');
      scheduleRetry(backoff.delay(failures++));
      ws?.close();
    }
  }

  /**
   * Runs `task` after everything queued before it, unless attempt `owner` has ended by then. A task that fails ends the
   * attempt, so the reconnect rebuilds the state from the stored cursors.
   */
  function enqueue(task: () => Promise<void>, owner: number) {
    queue = queue
      .then(() => (owner === attempt ? task() : undefined))
      .catch(() => {
        fail(owner);
      });
  }

  /** `o.resync`, abandoned when `signal` aborts or after `resyncTimeoutMs`; its timer and listener never outlive the call. */
  async function resyncWithin(cursors: Cursor[], signal: AbortSignal): Promise<ChannelDelta[]> {
    let abandon: (error: Error) => void = () => undefined;
    const abandoned = new Promise<never>((_, reject) => {
      abandon = reject;
    });
    const deadline = setTimeout(() => {
      abandon(new Error(`resync did not answer within ${String(resyncTimeoutMs)} ms`));
    }, resyncTimeoutMs);
    const onAbort = () => {
      abandon(new Error('resync was abandoned'));
    };
    signal.addEventListener('abort', onAbort, { once: true });
    try {
      return await Promise.race([o.resync(cursors, signal), abandoned]);
    } finally {
      clearTimeout(deadline);
      signal.removeEventListener('abort', onAbort);
    }
  }

  function deliver(message: Message) {
    notify(handlers, create(ServerEventSchema, { event: { case: 'messageCreated', value: message } }));
  }

  /** Fetches what each channel's cursor is missing and delivers it, for attempt `owner`. */
  async function catchUp(channelIds: string[], owner: number) {
    if (channelIds.length === 0) {
      return;
    }
    const cursors = channelIds.map((channelId) => create(CursorSchema, { channelId, lastSeq: BigInt(o.cursorStore.get(channelId)) }));
    const deltas = await resyncWithin(cursors, attemptSignal.signal);
    if (owner !== attempt) {
      return;
    }
    for (const { channelId, messages, reload } of deltas) {
      if (reload) {
        // Too far behind to replay: the page stands for everything up to its head, and the app shows it afresh.
        for (const message of messages) {
          o.cursorStore.advance(channelId, toSafeNumber(message.seq));
        }
        notify(handlers, { type: 'reset', channelId });
        continue;
      }
      for (const message of messages) {
        const seq = toSafeNumber(message.seq);
        if (seq > o.cursorStore.get(channelId)) {
          o.cursorStore.advance(channelId, seq);
          deliver(message);
        }
      }
    }
  }

  async function receive(event: ServerEvent, owner: number) {
    if (event.event.case !== 'messageCreated') {
      notify(handlers, event);
      return;
    }
    const message = event.event.value;
    const seq = toSafeNumber(message.seq);
    if (seq > o.cursorStore.get(message.channelId) + 1) {
      await catchUp([message.channelId], owner);
    }
    // Next in line, or already delivered by the catch-up, or a duplicate: only the first is news.
    if (owner === attempt && seq === o.cursorStore.get(message.channelId) + 1) {
      o.cursorStore.advance(message.channelId, seq);
      notify(handlers, event);
    }
  }

  /** Asks the gateway for the channels; the caller queues the catch-up from their cursors right after. */
  function subscribeOnSocket(channelIds: string[]) {
    if (channelIds.length > 0) {
      socket?.send(toBinary(ClientFrameSchema, create(ClientFrameSchema, { frame: { case: 'subscribe', value: create(SubscribeSchema, { channelIds }) } })));
    }
  }

  function scheduleRetry(delayMs: number) {
    retryTimer = setTimeout(() => {
      retryTimer = undefined;
      void open();
    }, delayMs);
  }

  async function open() {
    const mine = ++attempt;
    let ws: WebSocket;
    try {
      const ticket = await o.issueTicket();
      if (mine !== attempt) {
        return;
      }
      const url = new URL(wsUrl);
      url.searchParams.set('ticket', ticket);
      ws = new (o.WebSocketImpl ?? WebSocket)(url);
    } catch {
      if (mine === attempt) {
        scheduleRetry(backoff.delay(failures++));
      }
      return;
    }
    ws.binaryType = 'arraybuffer';
    socket = ws;
    ws.onopen = () => {
      if (mine !== attempt) {
        return;
      }
      setStatus('open');
      pingTimer = setInterval(() => {
        ws.send(PING_FRAME);
      }, PING_INTERVAL_MS);
      const channelIds = [...subscribed];
      subscribeOnSocket(channelIds);
      enqueue(async () => {
        await catchUp(channelIds, mine);
        // A connection that caught up is a good one: only then does the backoff start over.
        if (mine === attempt) {
          failures = 0;
        }
      }, mine);
    };
    ws.onmessage = ({ data }) => {
      if (mine !== attempt || !(data instanceof ArrayBuffer)) {
        return;
      }
      // A frame that cannot be decoded is dropped: the next seq gap or the next reconnect repairs what it carried.
      let event: ServerEvent;
      try {
        event = fromBinary(ServerEventSchema, new Uint8Array(data));
      } catch {
        return;
      }
      enqueue(() => receive(event, mine), mine);
    };
    ws.onclose = ({ code }) => {
      if (mine !== attempt) {
        return;
      }
      endAttempt();
      setStatus('connecting');
      scheduleRetry(code === CLOSE_GOING_AWAY ? backoff.goingAway() : backoff.delay(failures++));
    };
  }

  return {
    connect() {
      if (status === 'closed') {
        setStatus('connecting');
        void open();
      }
    },
    close() {
      const ws = endAttempt();
      setStatus('closed');
      ws?.close(1000);
    },
    subscribe(channelIds) {
      const added = [...new Set(channelIds)].filter((channelId) => !subscribed.has(channelId));
      for (const channelId of added) {
        subscribed.add(channelId);
      }
      if (status === 'open' && added.length > 0) {
        const owner = attempt;
        subscribeOnSocket(added);
        enqueue(() => catchUp(added, owner), owner);
      }
    },
    on(handler) {
      handlers.add(handler);
      return () => handlers.delete(handler);
    },
    onStatus(handler) {
      statusHandlers.add(handler);
      return () => statusHandlers.delete(handler);
    },
    get status() {
      return status;
    },
  };
}
