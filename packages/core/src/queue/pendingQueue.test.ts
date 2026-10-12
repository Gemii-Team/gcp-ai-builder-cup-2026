import { create } from '@bufbuild/protobuf';
import { SendMessageRequestSchema, type SendMessageRequest } from '@kiwi/api-client';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { harness, latestSocket, settle } from '../realtime/testing';
import { createPendingQueue, type SendResult } from './pendingQueue';

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
});

const request = (clientMsgId: string) => create(SendMessageRequestSchema, { channelId: 'a', clientMsgId, body: `body of ${clientMsgId}` });

const sending = (...results: (SendResult | Error)[]) =>
  vi.fn<(r: SendMessageRequest) => Promise<SendResult>>(() => {
    const result = results.shift() ?? 'sent';
    return result instanceof Error ? Promise.reject(result) : Promise.resolve(result);
  });

describe('PendingQueue', () => {
  it('TestPendingQueue_ReplaysOnceAcrossReconnects', async () => {
    const h = harness();
    const queue = createPendingQueue();
    const send = sending();
    h.client.onStatus((status) => {
      if (status === 'open') {
        void queue.replay(send);
      }
    });

    // Offline: the client is still trying, and two messages are composed meanwhile.
    h.client.connect();
    await settle();
    queue.enqueue(request('m1'));
    queue.enqueue(request('m2'));
    expect(send).not.toHaveBeenCalled();
    expect(queue.list().map((entry) => entry.state)).toEqual(['pending', 'pending']);

    latestSocket().serverOpen();
    await settle();

    expect(send.mock.calls.map(([sent]) => sent.clientMsgId)).toEqual(['m1', 'm2']);
    expect(send.mock.calls.map(([sent]) => sent.body)).toEqual(['body of m1', 'body of m2']);
    expect(queue.list()).toEqual([]);

    // A second reconnect finds nothing left to send.
    latestSocket().serverClose(1006);
    await settle();
    latestSocket().serverOpen();
    await settle();

    expect(h.issueTicket).toHaveBeenCalledTimes(2);
    expect(send).toHaveBeenCalledTimes(2);
  });

  it('TestPendingQueue_DuplicateResultRemovesEntry', async () => {
    const queue = createPendingQueue();
    queue.enqueue(request('m1'));
    const send = sending('duplicate');

    await queue.replay(send);

    expect(send).toHaveBeenCalledTimes(1);
    expect(queue.list()).toEqual([]);
  });

  it('TestPendingQueue_RejectedStaysVisibleAsFailed', async () => {
    const queue = createPendingQueue();
    queue.enqueue(request('m1'));
    queue.enqueue(request('m2'));
    const send = sending('rejected', 'sent');

    await queue.replay(send);

    expect(send.mock.calls.map(([sent]) => sent.clientMsgId)).toEqual(['m1', 'm2']);
    expect(queue.list().map((entry) => [entry.request.clientMsgId, entry.state])).toEqual([['m1', 'failed']]);

    // A failed message is not retried by itself: it stays for the person to see.
    await queue.replay(send);
    expect(send).toHaveBeenCalledTimes(2);
    expect(queue.list()).toHaveLength(1);
  });

  it('keeps an entry after a transport error and sends nothing behind it, so order holds', async () => {
    const queue = createPendingQueue();
    queue.enqueue(request('m1'));
    queue.enqueue(request('m2'));
    const send = sending(new Error('offline'));

    await queue.replay(send);

    expect(send).toHaveBeenCalledTimes(1);
    expect(queue.list().map((entry) => [entry.request.clientMsgId, entry.state])).toEqual([
      ['m1', 'pending'],
      ['m2', 'pending'],
    ]);

    await queue.replay(send);

    expect(send.mock.calls.map(([sent]) => sent.clientMsgId)).toEqual(['m1', 'm1', 'm2']);
    expect(queue.list()).toEqual([]);
  });

  it('shares one replay between overlapping calls, so an entry is never in flight twice', async () => {
    const queue = createPendingQueue();
    queue.enqueue(request('m1'));
    const answer = Promise.withResolvers<SendResult>();
    const send = vi.fn<(r: SendMessageRequest) => Promise<SendResult>>(() => answer.promise);

    const first = queue.replay(send);
    const second = queue.replay(send);
    answer.resolve('sent');
    await Promise.all([first, second]);

    expect(send).toHaveBeenCalledTimes(1);
    expect(queue.list()).toEqual([]);
  });

  it('starts a new replay once the last one has finished', async () => {
    const queue = createPendingQueue();
    const send = sending();
    queue.enqueue(request('m1'));
    await queue.replay(send);

    queue.enqueue(request('m2'));
    await queue.replay(send);

    expect(send.mock.calls.map(([sent]) => sent.clientMsgId)).toEqual(['m1', 'm2']);
  });

  it('lists a copy: changing it changes nothing in the queue', () => {
    const queue = createPendingQueue();
    queue.enqueue(request('m1'));

    const listed = queue.list() as unknown[];
    listed.length = 0;

    expect(queue.list()).toHaveLength(1);
  });
});
