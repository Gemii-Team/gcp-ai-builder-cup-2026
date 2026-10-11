import { Code, ConnectError } from '@connectrpc/connect';
import { create } from '@bufbuild/protobuf';
import { timestampFromDate } from '@bufbuild/protobuf/wkt';
import { CreateTaskResponseSchema, DiscardDraftResponseSchema, SourceKind, TaskState, UpdateTaskResponseSchema } from '@kiwi/api-client';
import { act, renderHook } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { mapTask } from '../../api/mappers';
import { keys } from '../../api/queries';
import type { ArtifactView, CitationView, TaskView } from '../../api/types';
import { at, task } from '../../testing/fixtures';
import { createTestQueryClient, hookWrapper } from '../../testing/render';
import { createStubApi } from '../../testing/stubApi';
import { answerTitle, useMakeTask } from './useMakeTask';

afterEach(() => {
  sessionStorage.clear();
});

const MEETING_QUOTE = 'งั้นเราเลื่อนวันเปิดตัวไปเป็น 15 พฤศจิกายนนะคะ';

const CITATIONS: CitationView[] = [
  // Out of order on purpose: the task stands on the lowest number, not the first in the list.
  { n: 2, chunkId: 'c2', quote: 'Who approves the extra budget?', source: { kind: 'meeting', id: 'meeting-1', label: 'Q4 launch weekly sync', channelId: 'ch-q4', entryIdx: 33, offsetMs: 1998000 } },
  { n: 1, chunkId: 'c1', quote: MEETING_QUOTE, source: { kind: 'meeting', id: 'meeting-1', label: 'Q4 launch weekly sync', channelId: 'ch-q4', entryIdx: 12, offsetMs: 761000 } },
];

function answer(init: Partial<ArtifactView> = {}): ArtifactView {
  return {
    kind: 'answer',
    ref: 'ANS-2209',
    title: '',
    meta: '',
    bodyMarkdown: '**Decided:** the launch moved to 15 Nov 2026.[1] **Still open:** who approves the budget.[2]',
    tracks: [],
    sources: [],
    citations: CITATIONS,
    scopeChannelIds: ['ch-q4'],
    generatedAt: at('14:51').toISOString(),
    ...init,
  };
}

function setup(artifact: ArtifactView = answer()) {
  const api = createStubApi();
  const queryClient = createTestQueryClient();
  const draft = task({ id: 'draft-1', ref: 'TSK-2036', title: 'Decided: the launch moved to 15 Nov 2026.', state: TaskState.DRAFT, fromRef: 'ANS-2209' });
  api.tasks.createTask.mockResolvedValue(create(CreateTaskResponseSchema, { task: draft }));
  const hook = renderHook(() => useMakeTask(artifact), { wrapper: hookWrapper(api, queryClient) });
  return { api, queryClient, draft, ...hook };
}

describe('answerTitle', () => {
  it('is the first sentence of the answer as plain words', () => {
    expect(answerTitle('**Decided:** the launch moved to 15 Nov 2026.[1] **Still open:** who approves the budget.[2]')).toBe(
      'Decided: the launch moved to 15 Nov 2026.',
    );
    expect(answerTitle('## Summary\n\n- The *contract* ends [on 31 Dec](https://example.com) 2026! Then it renews.[1]')).toBe('Summary');
    expect(answerTitle('\n\n1. The contract ends on 31 Dec 2026 [1]. More.')).toBe('The contract ends on 31 Dec 2026.');
    expect(answerTitle('![pixel](https://evil.example/p.png) Version 2.5 is fine?')).toBe('pixel Version 2.5 is fine?');
  });

  it('takes the first line of a Thai answer whole, abbreviations and all', () => {
    expect(answerTitle('สัญญา ACME ฉบับ v3 สิ้นสุดวันที่ 31 ธ.ค. 2569 และต่ออายุอัตโนมัติ[1]\nค่าปรับคือ 2% ต่อวัน[2]')).toBe(
      'สัญญา ACME ฉบับ v3 สิ้นสุดวันที่ 31 ธ.ค. 2569 และต่ออายุอัตโนมัติ',
    );
  });

  it('is at most 120 runes, cut between characters and ended with an ellipsis', () => {
    const long = answerTitle(`${'ก'.repeat(130)} tail[1]`);

    expect(Array.from(long)).toHaveLength(120);
    expect(long).toBe(`${'ก'.repeat(119)}…`);
  });

  it('is empty for an answer that is only markers', () => {
    expect(answerTitle('[1] [2]')).toBe('');
  });
});

describe('useMakeTask', () => {
  it('MakeTask_CreatesRealDraftThroughTaskService_NotSessionStorage', async () => {
    const { api, queryClient, draft, result } = setup();
    queryClient.setQueryData(keys.tasks(), [mapTask(task({ id: 'existing' }))]);
    queryClient.setQueryData(keys.channelTasks('ch-q4'), [mapTask(task({ id: 'existing' }))]);

    let made: TaskView | null = null;
    await act(async () => {
      made = await result.current.make();
    });

    expect(api.tasks.createTask).toHaveBeenCalledOnce();
    const request = api.tasks.createTask.mock.calls[0]?.[0];
    expect(request).toMatchObject({
      channelId: 'ch-q4',
      title: 'Decided: the launch moved to 15 Nov 2026.',
      state: TaskState.DRAFT,
      fromRef: 'ANS-2209',
      source: { kind: SourceKind.MEETING, id: 'meeting-1', anchor: '12', label: 'Q4 launch weekly sync', channelId: 'ch-q4', offsetMs: 761000n, quote: MEETING_QUOTE },
    });
    expect(request?.source?.at).toBeUndefined();
    // Nothing is kept in the tab: the draft is a row in `tasks`, and the Board reads it from there.
    expect(sessionStorage).toHaveLength(0);
    expect(localStorage).toHaveLength(0);
    expect(made).toMatchObject({ id: draft.id, state: 'draft', fromRef: 'ANS-2209' });
    expect(result.current.draft).toMatchObject({ id: 'draft-1', ref: 'TSK-2036' });
    // It joins the workspace list and its channel's list once, at the top, so the Board's drafts band shows it.
    expect(queryClient.getQueryData<TaskView[]>(keys.tasks())?.map((t) => t.id)).toEqual(['draft-1', 'existing']);
    expect(queryClient.getQueryData<TaskView[]>(keys.channelTasks('ch-q4'))?.map((t) => t.id)).toEqual(['draft-1', 'existing']);
  });

  it('stands a document draft on its page and a message draft on its time and seq, with the passage as the quote', async () => {
    const file = setup(answer({ citations: [{ n: 1, chunkId: 'c1', quote: '4.1 Term. Ends 31 December 2026.', source: { kind: 'file', id: 'file-1', label: 'ACME-contract-v3.pdf', channelId: 'ch-q4', page: 4 } }] }));
    await act(async () => {
      await file.result.current.make();
    });
    expect(file.api.tasks.createTask.mock.calls[0]?.[0]).toMatchObject({
      source: { kind: SourceKind.FILE, id: 'file-1', anchor: 'page:4', page: 4, quote: '4.1 Term. Ends 31 December 2026.' },
    });

    const message = setup(
      answer({ citations: [{ n: 1, chunkId: 'c1', quote: 'ยืนยันแล้วครับ คลังบางนาว่าง', source: { kind: 'message', id: 'msg-41', label: 'Arthit K.', channelId: 'ch-ops', seq: 41, at: at('14:02').toISOString() } }] }),
    );
    await act(async () => {
      await message.result.current.make();
    });
    const request = message.api.tasks.createTask.mock.calls[0]?.[0];
    expect(request).toMatchObject({ channelId: 'ch-ops', source: { kind: SourceKind.MESSAGE, id: 'msg-41', anchor: '41', label: 'Arthit K.', channelId: 'ch-ops' } });
    expect(request?.source?.at).toEqual(timestampFromDate(at('14:02')));
  });

  it('keeps the quote of the task source within the 500 runes the tasks service accepts', async () => {
    const { api, result } = setup(
      answer({ citations: [{ n: 1, chunkId: 'c1', quote: 'ก'.repeat(600), source: { kind: 'file', id: 'file-1', label: 'a.pdf', channelId: 'ch-q4', page: 1 } }] }),
    );

    await act(async () => {
      await result.current.make();
    });

    expect(Array.from(api.tasks.createTask.mock.calls[0]?.[0]?.source?.quote ?? '')).toHaveLength(500);
  });

  it('makes nothing for an answer with no citation to stand on, and says so', async () => {
    const { api, result } = setup(answer({ citations: [] }));

    await act(async () => {
      await result.current.make();
    });

    expect(result.current.available).toBe(false);
    expect(api.tasks.createTask).not.toHaveBeenCalled();
  });

  it('writes nothing and says so when the draft could not be made, and may try again', async () => {
    const { api, queryClient, draft, result } = setup();
    queryClient.setQueryData(keys.tasks(), []);
    api.tasks.createTask.mockRejectedValueOnce(new ConnectError('down', Code.Unavailable));

    await act(async () => {
      await result.current.make();
    });

    expect(result.current.failure).toBe('create');
    expect(result.current.draft).toBeNull();
    expect(queryClient.getQueryData(keys.tasks())).toEqual([]);

    api.tasks.createTask.mockResolvedValueOnce(create(CreateTaskResponseSchema, { task: draft }));
    await act(async () => {
      await result.current.make();
    });
    expect(result.current.failure).toBeNull();
    expect(result.current.draft).toMatchObject({ id: 'draft-1' });
  });

  it('drafts one task per press: a second press while it is made, or after, asks nothing', async () => {
    const { api, result } = setup();

    await act(async () => {
      await Promise.all([result.current.make(), result.current.make()]);
    });
    await act(async () => {
      await result.current.make();
    });

    expect(api.tasks.createTask).toHaveBeenCalledOnce();
  });

  it('saves the owner through UpdateTask and keeps it on the draft', async () => {
    const { api, draft, result } = setup();
    api.tasks.updateTask.mockResolvedValue(create(UpdateTaskResponseSchema, { task: { ...draft, assigneeId: 'user-nok' } }));
    await act(async () => {
      await result.current.make();
    });

    await act(async () => {
      await result.current.edit({ ownerId: 'user-nok' });
    });
    expect(api.tasks.updateTask.mock.calls[0]?.[0]).toEqual({ id: 'draft-1', assigneeId: 'user-nok' });
    expect(result.current.draft?.assigneeId).toBe('user-nok');

    await act(async () => {
      await result.current.edit({ ownerId: null });
    });
    expect(api.tasks.updateTask.mock.calls[1]?.[0]).toEqual({ id: 'draft-1', clearAssignee: true });
    expect(result.current.draft?.assigneeId).toBeNull();
  });

  it('discards through DiscardDraft, takes the draft out of the task lists and frees the press', async () => {
    const { api, queryClient, draft, result } = setup();
    api.tasks.discardDraft.mockResolvedValue(create(DiscardDraftResponseSchema));
    await act(async () => {
      await result.current.make();
    });
    expect(queryClient.getQueryData<TaskView[]>(keys.channelTasks('ch-q4'))).toBeUndefined();
    queryClient.setQueryData(keys.tasks(), [mapTask(draft)]);

    await act(async () => {
      await result.current.discard();
    });

    expect(api.tasks.discardDraft.mock.calls[0]?.[0]).toEqual({ id: 'draft-1' });
    expect(result.current.draft).toBeNull();
    expect(queryClient.getQueryData(keys.tasks())).toEqual([]);
  });

  it('keeps the draft and says so when discarding fails, but treats a draft that is already gone as discarded', async () => {
    const { api, result } = setup();
    await act(async () => {
      await result.current.make();
    });
    api.tasks.discardDraft.mockRejectedValueOnce(new ConnectError('down', Code.Unavailable));

    await act(async () => {
      await result.current.discard();
    });
    expect(result.current.failure).toBe('discard');
    expect(result.current.draft).not.toBeNull();

    api.tasks.discardDraft.mockRejectedValueOnce(new ConnectError('gone', Code.NotFound));
    await act(async () => {
      await result.current.discard();
    });
    expect(result.current.draft).toBeNull();
    expect(result.current.failure).toBeNull();
  });
});

