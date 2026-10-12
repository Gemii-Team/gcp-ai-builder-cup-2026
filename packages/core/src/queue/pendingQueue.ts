import type { SendMessageRequest } from '@kiwi/api-client';

/** What one send came to: `duplicate` means the server already has it (same `client_msg_id`), which is as good as sent. */
export type SendResult = 'sent' | 'duplicate' | 'rejected';

/** `failed` is a message the server refused: it stays in the list for the person to see and is not retried. */
export type PendingEntry = { readonly request: SendMessageRequest; readonly state: 'pending' | 'failed' };

export type PendingQueue = {
  /** Keeps a message that could not be sent (offline) until `replay`. The request carries its `client_msg_id`. */
  enqueue: (request: SendMessageRequest) => void;
  /**
   * Sends the pending messages one after another, in the order they were enqueued. `sent` and `duplicate` remove the
   * entry; `rejected` marks it `failed` and keeps it; a thrown error (the network is still down) keeps it pending and
   * stops the pass, so nothing is sent ahead of an earlier message. Calls that overlap share one pass.
   */
  replay: (send: (request: SendMessageRequest) => Promise<SendResult>) => Promise<void>;
  /** A snapshot of every entry, oldest first. */
  list: () => readonly PendingEntry[];
};

type Entry = { request: SendMessageRequest; state: 'pending' | 'failed' };

/** A queue in memory: a page reload drops the messages not yet sent. */
export function createPendingQueue(): PendingQueue {
  const entries: Entry[] = [];
  let running: Promise<void> | undefined;

  async function pass(send: (request: SendMessageRequest) => Promise<SendResult>) {
    for (const entry of entries.filter(({ state }) => state === 'pending')) {
      let result: SendResult;
      try {
        result = await send(entry.request);
      } catch {
        return;
      }
      if (result === 'rejected') {
        entry.state = 'failed';
      } else {
        entries.splice(entries.indexOf(entry), 1);
      }
    }
  }

  return {
    enqueue(request) {
      entries.push({ request, state: 'pending' });
    },
    replay(send) {
      running ??= pass(send).finally(() => {
        running = undefined;
      });
      return running;
    },
    list: () => entries.map(({ request, state }) => ({ request, state })),
  };
}
