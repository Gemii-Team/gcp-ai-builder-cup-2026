import { create } from '@bufbuild/protobuf';
import { timestampFromDate } from '@bufbuild/protobuf/wkt';
import { AgentRunState, ArtifactKind, RunStatus, SourceKind, SourceSchema, TrackKind, TrackSchema } from '@kiwi/api-client';
import { describe, expect, it } from 'vitest';
import { agentMessage, artifact, at, channel, message, run, runFrame, task } from '../testing/fixtures';
import { mapArtifact, mapChannel, mapMessage, mapRun, mapRunProgress, mapSource, mapTask, mapTrack } from './mappers';

describe('the int64 boundary', () => {
  it('turns seq, size and last_seq into numbers a screen can compare and format', () => {
    const mapped = mapMessage(message({ seq: 41, files: [{ fileId: 'file-1', name: 'a.pdf', mime: 'application/pdf', size: 1048576n }] }));

    expect(mapped.seq).toBe(41);
    expect(mapped.files[0]?.size).toBe(1048576);
    expect(mapChannel(channel({ lastSeq: 12n })).lastSeq).toBe(12);
  });

  it('refuses a value that would lose precision instead of showing a wrong number', () => {
    expect(() => mapChannel(channel({ lastSeq: BigInt(Number.MAX_SAFE_INTEGER) + 1n }))).toThrow(RangeError);
  });

  it('keeps the author kind, the mentions and the time as plain values', () => {
    const mapped = mapMessage(
      agentMessage({ seq: 3, time: '14:40', mentions: [{ kind: 1, id: 'user-pim', label: 'Pimchanok S.' }], clientMsgId: 'client-1' }),
    );

    expect(mapped).toMatchObject({ authorKind: 'agent', createdAt: at('14:40').toISOString(), clientMsgId: 'client-1' });
    expect(mapped.mentions).toEqual([{ kind: 'user', id: 'user-pim', label: 'Pimchanok S.' }]);
  });
});

describe('transcript offsets', () => {
  it('keeps the offset of a track that can open its moment', () => {
    const track = mapTrack(create(TrackSchema, { kind: TrackKind.DECISION, text: 'Move the launch', offsetMs: 761000n, entryIdx: 12 }), 'meeting-1');

    expect(track).toEqual({ kind: 'decision', text: 'Move the launch', offsetMs: 761000, entryIdx: 12 });
  });

  it('drops the offset of every track of a summary that names no meeting, so no link opens nothing', () => {
    const mapped = mapArtifact(
      artifact({
        kind: ArtifactKind.MEETING_SUMMARY,
        ref: 'MTG-1014',
        tracks: [{ kind: TrackKind.DECISION, text: 'Move the launch', offsetMs: 761000n, entryIdx: 12 }],
      }),
    );

    expect(mapped?.tracks).toEqual([{ kind: 'decision', text: 'Move the launch', offsetMs: null, entryIdx: null }]);
  });

  it('shows no offset for a track of a recording with no timing', () => {
    const mapped = mapArtifact(
      artifact({
        kind: ArtifactKind.MEETING_SUMMARY,
        ref: 'MTG-1015',
        meetingId: 'meeting-2',
        tracks: [{ kind: TrackKind.OPEN_QUESTION, text: 'Who approves the budget?' }],
      }),
    );

    expect(mapped?.tracks).toEqual([{ kind: 'open_question', text: 'Who approves the budget?', offsetMs: null, entryIdx: null }]);
  });

  it('keeps a meeting source live only when its entry index is known', () => {
    const live = mapSource(create(SourceSchema, { kind: SourceKind.MEETING, id: 'meeting-1', anchor: '12', label: 'Q4 sync', offsetMs: 761000n }));
    const dead = mapSource(create(SourceSchema, { kind: SourceKind.MEETING, id: 'meeting-1', anchor: '', label: 'Q4 sync', offsetMs: 761000n }));

    expect(live).toMatchObject({ kind: 'meeting', entryIdx: 12, offsetMs: 761000 });
    expect(dead).toMatchObject({ kind: 'meeting', entryIdx: null, offsetMs: null });
  });
});

describe('sources', () => {
  it('reads the page of a file source from its field or its anchor, and the seq of a message source from its anchor', () => {
    const page = mapSource(create(SourceSchema, { kind: SourceKind.FILE, id: 'file-1', anchor: 'page:4', label: 'ACME-contract-v3.pdf', channelId: 'ch-q4' }));
    const quoted = mapSource(create(SourceSchema, { kind: SourceKind.FILE, id: 'file-1', anchor: '', page: 7, label: 'a.pdf', quote: 'Late delivery is 2% a day' }));
    const sent = mapSource(
      create(SourceSchema, { kind: SourceKind.MESSAGE, id: 'msg-41', anchor: '41', label: 'Arthit K.', channelId: 'ch-q4', at: timestampFromDate(at('14:02')) }),
    );

    expect(page).toMatchObject({ kind: 'file', page: 4, channelId: 'ch-q4' });
    expect(quoted).toMatchObject({ page: 7, quote: 'Late delivery is 2% a day' });
    expect(sent).toMatchObject({ kind: 'message', seq: 41, at: at('14:02').toISOString() });
  });

  it('skips a source of a kind this build does not know', () => {
    expect(mapSource(create(SourceSchema, { kind: SourceKind.UNSPECIFIED, id: 'x' }))).toBeNull();
  });
});

describe('tasks', () => {
  it('reads a draft, its owner, its due date, its answer and its source as plain values', () => {
    const mapped = mapTask(
      task({
        id: 'task-7',
        state: 1,
        assigneeId: 'user-nok',
        dueAt: timestampFromDate(at('00:00', '2026-10-16')),
        fromRef: 'ANS-2209',
        source: { kind: SourceKind.MESSAGE, id: 'msg-3', anchor: '3', label: 'Arthit K.', channelId: 'ch-q4' },
      }),
    );

    expect(mapped).toMatchObject({
      id: 'task-7',
      state: 'draft',
      status: 'todo',
      assigneeId: 'user-nok',
      dueAt: '2026-10-15T17:00:00.000Z',
      fromRef: 'ANS-2209',
      source: { kind: 'message', seq: 3 },
    });
  });

  it('reads a task added by hand as one with no source and no owner', () => {
    expect(mapTask(task())).toMatchObject({ source: null, assigneeId: null, dueAt: null, doneAt: null, fromRef: null });
  });
});

describe('Ask runs', () => {
  const found = artifact({
    kind: ArtifactKind.ANSWER,
    ref: 'ANS-2209',
    bodyMarkdown: 'The launch moved.[1]',
    scopeChannelIds: ['ch-q4', 'ch-ops'],
    citations: [{ n: 1, chunkId: 'chunk-1', quote: 'the passage', source: { kind: SourceKind.MEETING, id: 'meeting-1', anchor: '12', label: 'Q4 sync', channelId: 'ch-q4', offsetMs: 761000n } }],
  });

  it('reads a finished run with its answer, its citations, the channels searched and its REF', () => {
    const mapped = mapRun(run({ status: RunStatus.DONE, answer: found, channelId: 'ch-q4' }));

    expect(mapped).toMatchObject({ id: 'run-1', status: 'done', channelId: 'ch-q4', failureReason: '' });
    expect(mapped.answer).toMatchObject({
      kind: 'answer',
      ref: 'ANS-2209',
      scopeChannelIds: ['ch-q4', 'ch-ops'],
      citations: [{ n: 1, chunkId: 'chunk-1', quote: 'the passage', source: { kind: 'meeting', entryIdx: 12, offsetMs: 761000 } }],
    });
  });

  it('reads a not-found answer as an answer with its REF and the channels that were searched, not as a failure', () => {
    const mapped = mapRun(run({ status: RunStatus.DONE, answer: artifact({ kind: ArtifactKind.NOT_FOUND, ref: 'ANS-2210', scopeChannelIds: ['ch-q4'] }) }));

    expect(mapped.status).toBe('done');
    expect(mapped.answer).toMatchObject({ kind: 'not_found', ref: 'ANS-2210', scopeChannelIds: ['ch-q4'], citations: [] });
  });

  it('takes the REF of the run when the answer left its own empty', () => {
    expect(mapRun(run({ status: RunStatus.DONE, ref: 'ANS-2211', answer: artifact({ kind: ArtifactKind.ANSWER, ref: '' }) })).answer?.ref).toBe('ANS-2211');
  });

  it('has no answer for a failed run, whose failure reason is the server code, and none for an artifact Ask cannot show', () => {
    expect(mapRun(run({ status: RunStatus.FAILED, failureReason: 'model_unavailable' }))).toMatchObject({ status: 'failed', answer: null, failureReason: 'model_unavailable' });
    expect(mapRun(run({ status: RunStatus.DONE, answer: artifact({ kind: ArtifactKind.MEETING_SUMMARY, ref: 'MTG-1014' }) })).answer).toBeNull();
  });

  it('reads a run that has not started as queued, and one with no channel as unscoped', () => {
    expect(mapRun(run({ status: RunStatus.UNSPECIFIED }))).toMatchObject({ status: 'queued', channelId: null });
    expect(mapRun(run({ status: RunStatus.QUEUED, channelId: '' })).channelId).toBeNull();
  });

  it('reads a progress frame: tool progress, streamed text, the validated answer and the failure', () => {
    const frame = (init: Parameters<typeof runFrame>[0]) => {
      const event = runFrame(init).event;
      if (event.case !== 'agentRunProgress') {
        throw new Error('not a progress frame');
      }
      return mapRunProgress(event.value);
    };

    expect(frame({ state: AgentRunState.RUNNING })).toMatchObject({ runId: 'run-1', state: 'running', answer: null });
    expect(frame({ state: AgentRunState.STREAMING, textDelta: 'The launch' })).toMatchObject({ state: 'streaming', textDelta: 'The launch' });
    expect(frame({ state: AgentRunState.DONE, answer: found }).answer).toMatchObject({ kind: 'answer', ref: 'ANS-2209' });
    expect(frame({ state: AgentRunState.FAILED }).state).toBe('failed');
  });
});
