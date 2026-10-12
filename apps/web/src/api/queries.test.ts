import { QueryClient } from '@tanstack/react-query';
import { beforeEach, describe, expect, it } from 'vitest';
import { agentMessage, channel, message, task } from '../testing/fixtures';
import { mapChannel, mapMessage, mapTask } from './mappers';
import {
  addUnsent,
  keys,
  markDelivery,
  prependMessages,
  receiveMessage,
  reconcileMessage,
  removeChannel,
  writeTask,
  type MessagesData,
} from './queries';
import type { ChannelView, ChatMessage, TaskView } from './types';

let queryClient: QueryClient;

beforeEach(() => {
  queryClient = new QueryClient({ defaultOptions: { queries: { gcTime: Infinity } } });
});

const stored = (seq: number, init: Partial<ChatMessage> = {}): ChatMessage => ({ ...mapMessage(message({ seq })), ...init });

/** A message of this person that the server has not stored yet. */
const unsent = (clientMsgId: string, delivery: ChatMessage['delivery'] = 'sending'): ChatMessage => ({
  ...mapMessage(message({ seq: 0, id: clientMsgId, clientMsgId, body: `body ${clientMsgId}` })),
  delivery,
});

describe('reconcileMessage', () => {
  it('replaces the unsent row with the stored message that carries its client_msg_id: never both', () => {
    const list = [stored(1), unsent('client-1')];

    const next = reconcileMessage(list, stored(2, { id: 'msg-2', clientMsgId: 'client-1' }));

    expect(next.map((m) => [m.id, m.delivery])).toEqual([
      ['msg-1', undefined],
      ['msg-2', undefined],
    ]);
  });

  it('ignores a stored message it already holds, by id and by client_msg_id', () => {
    const list = [stored(1), stored(2, { clientMsgId: 'client-2' })];

    expect(reconcileMessage(list, stored(2))).toBe(list);
    expect(reconcileMessage(list, stored(3, { id: 'msg-3', clientMsgId: 'client-2' }))).toBe(list);
  });

  it('keeps the other unsent messages last, in the order they were typed', () => {
    const list = [stored(1), unsent('client-1'), unsent('client-2')];

    const next = reconcileMessage(list, agentMessageView(2));

    expect(next.map((m) => m.id)).toEqual(['msg-1', 'msg-2', 'client-1', 'client-2']);
  });

  it('puts a message that arrives late between its neighbours', () => {
    expect(reconcileMessage([stored(1), stored(3)], stored(2)).map((m) => m.seq)).toEqual([1, 2, 3]);
  });
});

function agentMessageView(seq: number): ChatMessage {
  return mapMessage(agentMessage({ seq }));
}

describe('the message cache', () => {
  it('shows an unsent message, then the state the send reaches', () => {
    queryClient.setQueryData<MessagesData>(keys.messages('ch-q4'), { messages: [stored(1)], hasEarlier: false });

    addUnsent(queryClient, unsent('client-1'));
    markDelivery(queryClient, 'ch-q4', 'client-1', 'failed');

    expect(queryClient.getQueryData<MessagesData>(keys.messages('ch-q4'))?.messages.map((m) => [m.id, m.delivery])).toEqual([
      ['msg-1', undefined],
      ['client-1', 'failed'],
    ]);
  });

  it('adds the older page in front, without the messages it already holds', () => {
    queryClient.setQueryData<MessagesData>(keys.messages('ch-q4'), { messages: [stored(3), stored(4)], hasEarlier: true });

    prependMessages(queryClient, 'ch-q4', { messages: [stored(1), stored(2), stored(3)], hasEarlier: false });

    const data = queryClient.getQueryData<MessagesData>(keys.messages('ch-q4'));
    expect(data?.messages.map((m) => m.seq)).toEqual([1, 2, 3, 4]);
    expect(data?.hasEarlier).toBe(false);
  });

  it('moves the head of the channel and never backwards', () => {
    queryClient.setQueryData<ChannelView[]>(keys.channels(), [mapChannel(channel({ lastSeq: 10n }))]);

    receiveMessage(queryClient, stored(4));
    expect(queryClient.getQueryData<ChannelView[]>(keys.channels())?.[0]?.lastSeq).toBe(10);

    receiveMessage(queryClient, stored(11));
    expect(queryClient.getQueryData<ChannelView[]>(keys.channels())?.[0]?.lastSeq).toBe(11);
  });
});

describe('writeTask', () => {
  const view = (init: Parameters<typeof task>[0]): TaskView => mapTask(task(init));

  it('puts a new task at the top of the workspace list and of its channel list', () => {
    queryClient.setQueryData<TaskView[]>(keys.tasks(), [view({ id: 'task-1' })]);
    queryClient.setQueryData<TaskView[]>(keys.channelTasks('ch-q4'), [view({ id: 'task-1' })]);

    writeTask(queryClient, view({ id: 'task-2', ref: 'TSK-2036' }));

    expect(queryClient.getQueryData<TaskView[]>(keys.tasks())?.map((t) => t.id)).toEqual(['task-2', 'task-1']);
    expect(queryClient.getQueryData<TaskView[]>(keys.channelTasks('ch-q4'))?.map((t) => t.id)).toEqual(['task-2', 'task-1']);
  });

  it('replaces a task that is there already, in place, so it is never listed twice', () => {
    queryClient.setQueryData<TaskView[]>(keys.tasks(), [view({ id: 'task-1' }), view({ id: 'task-2' })]);

    writeTask(queryClient, view({ id: 'task-2', title: 'Renamed' }));
    writeTask(queryClient, view({ id: 'task-2', title: 'Renamed again' }));

    expect(queryClient.getQueryData<TaskView[]>(keys.tasks())?.map((t) => t.title)).toEqual(['Send the quotation to ACME', 'Renamed again']);
  });

  it('writes only the lists of its own channel, and none that nobody has loaded', () => {
    queryClient.setQueryData<TaskView[]>(keys.channelTasks('ch-sales'), []);

    writeTask(queryClient, view({ id: 'task-3' }));

    expect(queryClient.getQueryData(keys.channelTasks('ch-sales'))).toEqual([]);
    expect(queryClient.getQueryData(keys.tasks())).toBeUndefined();
    expect(queryClient.getQueryData(keys.channelTasks('ch-q4'))).toBeUndefined();
  });
});

describe('removeChannel', () => {
  it('drops the channel and everything cached for it', () => {
    queryClient.setQueryData<ChannelView[]>(keys.channels(), [mapChannel(channel()), mapChannel(channel({ id: 'ch-sales', name: 'sales-th' }))]);
    queryClient.setQueryData(keys.messages('ch-q4'), { messages: [], hasEarlier: false });
    queryClient.setQueryData(keys.members('ch-q4'), []);
    queryClient.setQueryData<TaskView[]>(keys.channelTasks('ch-q4'), []);
    queryClient.setQueryData<TaskView[]>(keys.tasks(), [mapTask(task()), mapTask(task({ id: 'task-9', channelId: 'ch-sales' }))]);

    removeChannel(queryClient, 'ch-q4');

    expect(queryClient.getQueryData<ChannelView[]>(keys.channels())?.map((c) => c.id)).toEqual(['ch-sales']);
    expect(queryClient.getQueryData(keys.messages('ch-q4'))).toBeUndefined();
    expect(queryClient.getQueryData(keys.members('ch-q4'))).toBeUndefined();
    expect(queryClient.getQueryData(keys.channelTasks('ch-q4'))).toBeUndefined();
    expect(queryClient.getQueryData<TaskView[]>(keys.tasks())?.map((t) => t.id)).toEqual(['task-9']);
  });
});
