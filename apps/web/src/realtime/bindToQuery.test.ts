import { create } from '@bufbuild/protobuf';
import {
  FileStatus,
  FileStatusChangedSchema,
  MeetingStatus,
  MeetingSummaryReadySchema,
  PresenceSchema,
  TaskStatus,
} from '@kiwi/api-client';
import { QueryClient } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { mapChannel, mapFile, mapMeeting, mapMessage, mapSession, mapTask } from '../api/mappers';
import { keys, type MessagesData } from '../api/queries';
import type { ChannelView, FileView, MeetingView, SessionView, TaskView } from '../api/types';
import { channel, file, frame, me, meeting, message, task } from '../testing/fixtures';
import { FakeRealtimeClient } from '../testing/fakeRealtime';
import { bindToQuery } from './bindToQuery';

let queryClient: QueryClient;
let client: FakeRealtimeClient;
let unbind: () => void;
const NOW = new Date('2026-10-13T07:50:00Z');

beforeEach(() => {
  queryClient = new QueryClient({ defaultOptions: { queries: { gcTime: Infinity } } });
  client = new FakeRealtimeClient();
  unbind = bindToQuery(client, queryClient, () => NOW);
});

afterEach(() => {
  unbind();
});

const messagesOf = (channelId: string) => queryClient.getQueryData<MessagesData>(keys.messages(channelId))?.messages ?? [];

describe('bindToQuery', () => {
  it('bindToQuery_MessageCreatedUpdatesCacheOnce', () => {
    queryClient.setQueryData<MessagesData>(keys.messages('ch-q4'), { messages: [mapMessage(message({ seq: 1, body: 'one' }))], hasEarlier: false });
    queryClient.setQueryData<ChannelView[]>(keys.channels(), [mapChannel(channel({ lastSeq: 1n }))]);
    const created = message({ seq: 2, body: 'two' });

    client.emit(frame({ case: 'messageCreated', value: created }));
    client.emit(frame({ case: 'messageCreated', value: created }));

    expect(messagesOf('ch-q4').map((m) => [m.seq, m.body])).toEqual([
      [1, 'one'],
      [2, 'two'],
    ]);
    // The channel's head moves with it: the rail's unread pill reads it.
    expect(queryClient.getQueryData<ChannelView[]>(keys.channels())?.[0]?.lastSeq).toBe(2);
  });

  it('puts a message of a channel nobody has opened nowhere but the rail', () => {
    queryClient.setQueryData<ChannelView[]>(keys.channels(), [mapChannel(channel({ id: 'ch-sales', lastSeq: 8n }))]);

    client.emit(frame({ case: 'messageCreated', value: message({ seq: 9, channelId: 'ch-sales' }) }));

    expect(queryClient.getQueryData(keys.messages('ch-sales'))).toBeUndefined();
    expect(queryClient.getQueryData<ChannelView[]>(keys.channels())?.[0]?.lastSeq).toBe(9);
  });

  it('inserts a message that arrives out of order at its seq', () => {
    queryClient.setQueryData<MessagesData>(keys.messages('ch-q4'), {
      messages: [mapMessage(message({ seq: 1 })), mapMessage(message({ seq: 3 }))],
      hasEarlier: false,
    });

    client.emit(frame({ case: 'messageCreated', value: message({ seq: 2 }) }));

    expect(messagesOf('ch-q4').map((m) => m.seq)).toEqual([1, 2, 3]);
  });

  it('bindToQuery_TaskChangedUpdatesChannelAndWorkspaceLists', () => {
    const existing = mapTask(task({ id: 'task-1', title: 'Send the quotation to ACME' }));
    const other = mapTask(task({ id: 'task-9', channelId: 'ch-sales', title: 'Call the carrier' }));
    queryClient.setQueryData<TaskView[]>(keys.tasks(), [existing, other]);
    queryClient.setQueryData<TaskView[]>(keys.channelTasks('ch-q4'), [existing]);

    client.emit(frame({ case: 'taskChanged', value: task({ id: 'task-1', title: 'Send the signed quotation to ACME', status: TaskStatus.DOING }) }));
    client.emit(frame({ case: 'taskChanged', value: task({ id: 'task-2', title: 'Book the warehouse', ref: 'TSK-2036' }) }));
    client.emit(frame({ case: 'taskChanged', value: task({ id: 'task-2', title: 'Book the Bang Na warehouse', ref: 'TSK-2036' }) }));

    const titles = (tasks: TaskView[] | undefined) => tasks?.map((t) => t.title);
    expect(titles(queryClient.getQueryData(keys.tasks()))).toEqual(['Book the Bang Na warehouse', 'Send the signed quotation to ACME', 'Call the carrier']);
    expect(titles(queryClient.getQueryData(keys.channelTasks('ch-q4')))).toEqual(['Book the Bang Na warehouse', 'Send the signed quotation to ACME']);
    expect(queryClient.getQueryData<TaskView[]>(keys.tasks())?.find((t) => t.id === 'task-1')?.status).toBe('doing');
  });

  it('leaves a task list nobody has loaded alone: its first load fetches the task', () => {
    client.emit(frame({ case: 'taskChanged', value: task() }));

    expect(queryClient.getQueryData(keys.tasks())).toBeUndefined();
    expect(queryClient.getQueryData(keys.channelTasks('ch-q4'))).toBeUndefined();
  });

  it('moves a cached file to the status the event names and remembers when it learned it', () => {
    queryClient.setQueryData<FileView>(keys.file('file-1'), mapFile(file()));

    client.emit(
      frame({
        case: 'fileStatusChanged',
        value: create(FileStatusChangedSchema, { fileId: 'file-1', channelId: 'ch-q4', status: FileStatus.FAILED, failureReason: 'unsupported_type' }),
      }),
    );

    expect(queryClient.getQueryData<FileView>(keys.file('file-1'))).toMatchObject({
      status: 'failed',
      failureReason: 'unsupported_type',
      settledAt: NOW.toISOString(),
    });
  });

  it('moves a cached meeting to the status the event names', () => {
    queryClient.setQueryData<MeetingView>(keys.meeting('meeting-1'), mapMeeting(meeting()));

    client.emit(
      frame({
        case: 'meetingSummaryReady',
        value: create(MeetingSummaryReadySchema, { meetingId: 'meeting-1', channelId: 'ch-q4', status: MeetingStatus.FAILED, failureReason: 'no_speech' }),
      }),
    );

    expect(queryClient.getQueryData<MeetingView>(keys.meeting('meeting-1'))).toMatchObject({ status: 'failed', failureReason: 'no_speech' });
  });

  it('loads a channel that is too far behind to replay from its latest page again', () => {
    queryClient.setQueryData<MessagesData>(keys.messages('ch-q4'), { messages: [mapMessage(message({ seq: 1 }))], hasEarlier: false });

    client.emit({ type: 'reset', channelId: 'ch-q4' });

    expect(queryClient.getQueryState(keys.messages('ch-q4'))?.isInvalidated).toBe(true);
  });

  it('ignores the frames no cache shows: progress, presence and typing', () => {
    queryClient.setQueryData<SessionView>(keys.me(), mapSession(me()));

    client.emit(frame({ case: 'presence', value: create(PresenceSchema, { userId: 'user-arthit', online: true }) }));

    expect(queryClient.getQueryCache().getAll()).toHaveLength(1);
  });

  it('stops applying events once unbound', () => {
    queryClient.setQueryData<MessagesData>(keys.messages('ch-q4'), { messages: [], hasEarlier: false });
    unbind();

    client.emit(frame({ case: 'messageCreated', value: message({ seq: 1 }) }));

    expect(messagesOf('ch-q4')).toEqual([]);
  });
});
