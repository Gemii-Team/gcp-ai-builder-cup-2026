import { create } from '@bufbuild/protobuf';
import { timestampFromDate } from '@bufbuild/protobuf/wkt';
import { ListChannelsResponseSchema, SourceKind, TaskState, TaskStatus } from '@kiwi/api-client';
import { renderHook, waitFor } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { at, channel, task } from '../../testing/fixtures';
import { createTestQueryClient, hookWrapper } from '../../testing/render';
import { createStubApi, seedApi } from '../../testing/stubApi';
import { NO_FILTERS, useBoardData, type BoardFilters } from './useBoardData';

const meeting = (id: string, label: string, anchor = '12') => ({ kind: SourceKind.MEETING, id, label, channelId: 'ch-q4', anchor, offsetMs: 761000n });
const doc = (id: string, label: string) => ({ kind: SourceKind.FILE, id, label, channelId: 'ch-q4', anchor: 'page:4', page: 4 });
const msg = (channelId: string) => ({ kind: SourceKind.MESSAGE, id: `msg-${channelId}`, label: 'Arthit K.', channelId, anchor: '41', at: timestampFromDate(at('14:02')) });

const TASKS = [
  // Added by hand: two channels, one lane. Positions are per channel, so ch-ops' 'a0' ties with ch-q4's 'a0'.
  task({ id: 'hand-c', title: 'Hand C', position: 'a2' }),
  task({ id: 'hand-a', title: 'Hand A', position: 'a0' }),
  task({ id: 'hand-b', title: 'Hand B', position: 'a1' }),
  task({ id: 'hand-ops', title: 'Hand ops', channelId: 'ch-ops', position: 'a0' }),
  task({ id: 'hand-doing', title: 'Hand doing', status: TaskStatus.DOING, position: 'a0' }),
  // Done: newest done_at first, whatever the position.
  task({ id: 'done-old', title: 'Done old', status: TaskStatus.DONE, position: 'a0', doneAt: timestampFromDate(at('09:00', '2026-10-12')) }),
  task({ id: 'done-new', title: 'Done new', status: TaskStatus.DONE, position: 'a9', doneAt: timestampFromDate(at('14:48')) }),
  // Meeting lanes.
  task({ id: 'mtg-2', title: 'Z meeting task', source: meeting('meeting-z', 'Z call'), position: 'a0' }),
  task({ id: 'mtg-1b', title: 'Sync task B', source: meeting('meeting-1', 'Q4 launch weekly sync', '27'), position: 'a1' }),
  task({ id: 'mtg-1a', title: 'Sync task A', source: meeting('meeting-1', 'Q4 launch weekly sync'), position: 'a0' }),
  // A document lane and the messages lanes of two channels.
  task({ id: 'doc-1', title: 'Contract task', source: doc('file-1', 'ACME-contract-v3.pdf') }),
  task({ id: 'msg-q4', title: 'From a q4 message', source: msg('ch-q4') }),
  task({ id: 'msg-ops', title: 'From an ops message', channelId: 'ch-ops', source: msg('ch-ops') }),
  // An AI draft: only in the drafts band, never in a lane.
  task({ id: 'draft-1', title: 'A draft', state: TaskState.DRAFT, source: meeting('meeting-1', 'Q4 launch weekly sync', '33') }),
];

async function board(filters: BoardFilters = NO_FILTERS, pinned: readonly string[] = []) {
  const api = seedApi(createStubApi(), { tasks: TASKS });
  api.chat.listChannels.mockResolvedValue(create(ListChannelsResponseSchema, { channels: [channel(), channel({ id: 'ch-ops', name: 'ops-warehouse' })] }));
  const { result } = renderHook(() => useBoardData(filters, pinned), { wrapper: hookWrapper(api, createTestQueryClient()) });
  await waitFor(() => {
    expect(result.current.board).toBeDefined();
  });
  const data = result.current.board;
  if (data === undefined) {
    throw new Error('the board has no data');
  }
  return { api, data };
}

const ids = (tasks: { id: string }[]) => tasks.map((t) => t.id);

describe('useBoardData', () => {
  it('useBoardData_GroupsTasksIntoSourceLanesOrdersCellsByPositionAndDoneAt', async () => {
    const { api, data } = await board();

    // One workspace-wide read, never one per channel.
    expect(api.tasks.listTasks).toHaveBeenCalledTimes(1);
    expect(api.tasks.listTasks.mock.calls[0]?.[0]).toEqual({});

    // Added by hand first, then meetings, documents and channels' messages, each by name.
    expect(data.lanes.map((lane) => lane.key)).toEqual(['hand', 'meeting:meeting-1', 'meeting:meeting-z', 'file:file-1', 'messages:ch-ops', 'messages:ch-q4']);

    const [hand, sync] = data.lanes;
    // To do and Doing by position, a tie between two channels' cards broken by channel; Done by done_at, newest first.
    expect(ids(hand?.cells.todo ?? [])).toEqual(['hand-ops', 'hand-a', 'hand-b', 'hand-c']);
    expect(ids(hand?.cells.doing ?? [])).toEqual(['hand-doing']);
    expect(ids(hand?.cells.done ?? [])).toEqual(['done-new', 'done-old']);
    expect(hand?.total).toBe(7);
    expect(hand?.channelId).toBeNull();
    // A meeting's tasks are one lane, by position, and its draft is not among them.
    expect(ids(sync?.cells.todo ?? [])).toEqual(['mtg-1a', 'mtg-1b']);
    expect(sync).toMatchObject({ kind: 'meeting', sourceId: 'meeting-1', label: 'Q4 launch weekly sync', channelId: 'ch-q4', total: 2 });
    // A channel's messages lane is named after the channel.
    expect(data.lanes.find((lane) => lane.key === 'messages:ch-ops')).toMatchObject({ kind: 'channel', label: 'ops-warehouse', channelId: 'ch-ops' });

    expect(data.counts).toEqual({ todo: 10, doing: 1, done: 2 });
    expect(data.confirmed).toBe(13);
  });

  it('keeps drafts out of the lanes and out of the filters', async () => {
    const { data } = await board({ channelId: 'ch-ops', sourceType: 'message' });

    expect(ids(data.drafts)).toEqual(['draft-1']);
    expect(data.lanes.flatMap((lane) => Object.values(lane.cells).flat()).map((t) => t.id)).toEqual(['msg-ops']);
    expect(data.counts).toEqual({ todo: 1, doing: 0, done: 0 });
  });

  it('puts the cards just confirmed first in To do', async () => {
    const { data } = await board(NO_FILTERS, ['mtg-1b']);

    expect(ids(data.lanes[1]?.cells.todo ?? [])).toEqual(['mtg-1b', 'mtg-1a']);
  });
});
