// A realtime client driven by hand. Test support: nothing in the app imports this file.
import type { ConnectionStatus, RealtimeClient, RealtimeEvent } from '@kiwi/core';

export class FakeRealtimeClient implements RealtimeClient {
  status: ConnectionStatus = 'closed';
  /** Every channel id the app asked to follow, in the order it asked. */
  readonly subscribed: string[] = [];
  connects = 0;
  closes = 0;
  private readonly handlers = new Set<(event: RealtimeEvent) => void>();
  private readonly statusHandlers = new Set<(status: ConnectionStatus) => void>();

  connect() {
    this.connects += 1;
    this.setStatus('connecting');
  }

  close() {
    this.closes += 1;
    this.setStatus('closed');
  }

  subscribe(channelIds: string[]) {
    this.subscribed.push(...channelIds);
  }

  on(handler: (event: RealtimeEvent) => void) {
    this.handlers.add(handler);
    return () => this.handlers.delete(handler);
  }

  onStatus(handler: (status: ConnectionStatus) => void) {
    this.statusHandlers.add(handler);
    return () => this.statusHandlers.delete(handler);
  }

  /** The gateway delivers a frame. */
  emit(event: RealtimeEvent) {
    for (const handler of [...this.handlers]) {
      handler(event);
    }
  }

  /** The socket opens. */
  open() {
    this.setStatus('open');
  }

  private setStatus(status: ConnectionStatus) {
    this.status = status;
    for (const handler of [...this.statusHandlers]) {
      handler(status);
    }
  }
}
