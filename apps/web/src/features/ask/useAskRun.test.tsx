import { Code, ConnectError } from '@connectrpc/connect';
import { create } from '@bufbuild/protobuf';
import { AgentRunState, ArtifactKind, GetRunResponseSchema, RunStatus, SourceKind, StartRunResponseSchema, type StartRunResponse } from '@kiwi/api-client';
import { act, renderHook } from '@testing-library/react';
import type { ReactNode } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { RealtimeProvider } from '../../realtime/provider';
import { artifact, channel, run, runFrame } from '../../testing/fixtures';
import { FakeRealtimeClient } from '../../testing/fakeRealtime';
import { createTestQueryClient, hookWrapper } from '../../testing/render';
import { createStubApi, seedApi } from '../../testing/stubApi';
import { FIRST_POLL_MS, MAX_POLL_MS, POLL_FACTOR, RUN_BUDGET_MS, useAskRun } from './useAskRun';

afterEach(() => {
  vi.useRealTimers();
});

const QUESTION = 'What did we decide about the Q4 launch date?';

const ANSWER = artifact({
  kind: ArtifactKind.ANSWER,
  ref: 'ANS-2209',
  bodyMarkdown: 'The launch moved to 15 Nov 2026.[1]',
  scopeChannelIds: ['ch-q4'],
  citations: [
    {
      n: 1,
      chunkId: 'chunk-1',
      quote: 'งั้นเราเลื่อนวันเปิดตัวไปเป็น 15 พฤศจิกายนนะคะ',
      source: { kind: SourceKind.MEETING, id: 'meeting-1', anchor: '12', label: 'Q4 launch weekly sync', channelId: 'ch-q4', offsetMs: 761000n },
    },
  ],
});

/** The hook in the signed-in shell's providers, on a fake realtime client and a stubbed API where every run call answers by hand. */
function setup(onSettled?: Parameters<typeof useAskRun>[0]) {
  const api = seedApi(createStubApi(), { channels: [channel()] });
  const client = new FakeRealtimeClient();
  const Base = hookWrapper(api, createTestQueryClient());
  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <Base>
        <RealtimeProvider client={client}>{children}</RealtimeProvider>
      </Base>
    );
  }
  const hook = renderHook(() => useAskRun(onSettled), { wrapper: Wrapper });
  const started = (id = 'run-1', status = RunStatus.QUEUED) => api.agents.startRun.mockResolvedValue(create(StartRunResponseSchema, { run: run({ id, status }) }));
  const outcome = () => hook.result.current.exchanges[0]?.outcome;
  async function ask(question = QUESTION, channelId: string | null = null) {
    let id = '';
    await act(() => {
      id = hook.result.current.ask(question, channelId);
      return Promise.resolve();
    });
    return id;
  }
  const emit = (init: Parameters<typeof runFrame>[0]) => {
    act(() => {
      client.emit(runFrame(init));
    });
  };
  return { api, client, ask, emit, started, outcome, ...hook };
}

describe('useAskRun', () => {
  it('useAskRun_FinalisesFromProgressFrame', async () => {
    const { api, ask, emit, started, outcome } = setup();
    started();

    await ask();

    expect(api.agents.startRun.mock.calls[0]?.[0]).toEqual({ agent: 'assistant', question: QUESTION });
    expect(outcome()).toEqual({ kind: 'pending', phase: 'retrieving' });

    // Tool progress ends the retrieving step; the streamed words themselves are never kept.
    emit({ state: AgentRunState.RUNNING });
    expect(outcome()).toEqual({ kind: 'pending', phase: 'writing' });
    emit({ state: AgentRunState.STREAMING, textDelta: 'The launch moved to' });
    expect(outcome()).toEqual({ kind: 'pending', phase: 'writing' });

    // Another run's frame is not ours.
    emit({ runId: 'run-9', state: AgentRunState.DONE, answer: ANSWER });
    expect(outcome()?.kind).toBe('pending');

    emit({ state: AgentRunState.DONE, answer: ANSWER });

    const done = outcome();
    expect(done?.kind).toBe('answered');
    expect(done?.kind === 'answered' && done.answer).toMatchObject({
      kind: 'answer',
      ref: 'ANS-2209',
      scopeChannelIds: ['ch-q4'],
      citations: [{ n: 1, quote: 'งั้นเราเลื่อนวันเปิดตัวไปเป็น 15 พฤศจิกายนนะคะ', source: { kind: 'meeting', entryIdx: 12, offsetMs: 761000 } }],
    });
    // The frame was enough: nothing asked the server again.
    expect(api.agents.getRun).not.toHaveBeenCalled();
  });

  it('useAskRun_MissedFrame_PollsGetRunAndShowsResult', async () => {
    vi.useFakeTimers();
    const { api, ask, started, outcome } = setup();
    started();
    api.agents.getRun
      .mockResolvedValueOnce(create(GetRunResponseSchema, { run: run({ status: RunStatus.RUNNING }) }))
      .mockResolvedValueOnce(create(GetRunResponseSchema, { run: run({ status: RunStatus.DONE, answer: ANSWER, ref: 'ANS-2209' }) }));
    await ask();

    // No frame ever arrives. The first poll finds the run still going...
    await act(async () => {
      await vi.advanceTimersByTimeAsync(FIRST_POLL_MS);
    });
    expect(api.agents.getRun).toHaveBeenCalledTimes(1);
    expect(api.agents.getRun.mock.calls[0]?.[0]).toEqual({ runId: 'run-1' });
    expect(outcome()?.kind).toBe('pending');

    // ...and the next, a little later, finds the answer.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(FIRST_POLL_MS * POLL_FACTOR);
    });
    expect(api.agents.getRun).toHaveBeenCalledTimes(2);
    expect(outcome()).toMatchObject({ kind: 'answered', answer: { kind: 'answer', ref: 'ANS-2209' } });
    // Once it has an answer it asks no more and holds no timer.
    expect(vi.getTimerCount()).toBe(0);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(MAX_POLL_MS * 3);
    });
    expect(api.agents.getRun).toHaveBeenCalledTimes(2);
  });

  it('useAskRun_FailureShowsFailedSlipAndRetryStartsNewRun', async () => {
    const { api, ask, emit, started, outcome, result } = setup();
    started('run-1');
    api.agents.getRun.mockResolvedValue(create(GetRunResponseSchema, { run: run({ status: RunStatus.FAILED, failureReason: 'model_unavailable' }) }));
    const id = await ask();

    emit({ state: AgentRunState.FAILED });
    await act(async () => {});

    // A failed run says why and issues no REF, so there is no answer for a slip to carry.
    expect(outcome()).toMatchObject({ kind: 'failed', reason: 'model_unavailable' });
    expect(outcome()).not.toHaveProperty('answer');

    // Retry is a new run for the same question, in the same place of the thread.
    started('run-2');
    await act(() => {
      result.current.retry(id);
      return Promise.resolve();
    });

    expect(api.agents.startRun).toHaveBeenCalledTimes(2);
    expect(api.agents.startRun.mock.calls[1]?.[0]).toEqual({ agent: 'assistant', question: QUESTION });
    expect(result.current.exchanges).toHaveLength(1);
    expect(outcome()).toEqual({ kind: 'pending', phase: 'retrieving' });

    // The old run's frames no longer count; the new run's do.
    emit({ runId: 'run-1', state: AgentRunState.DONE, answer: ANSWER });
    expect(outcome()?.kind).toBe('pending');
    emit({ runId: 'run-2', state: AgentRunState.DONE, answer: ANSWER });
    expect(outcome()).toMatchObject({ kind: 'answered', answer: { ref: 'ANS-2209' } });
  });

  it('looks at a frame that arrived before StartRun said which run is ours', async () => {
    const { api, client, ask, outcome } = setup();
    const start = Promise.withResolvers<StartRunResponse>();
    api.agents.startRun.mockReturnValue(start.promise);
    await ask();

    act(() => {
      client.emit(runFrame({ state: AgentRunState.DONE, answer: ANSWER }));
    });
    expect(outcome()?.kind).toBe('pending');
    await act(() => {
      start.resolve(create(StartRunResponseSchema, { run: run({ status: RunStatus.RUNNING }) }));
      return Promise.resolve();
    });

    expect(outcome()).toMatchObject({ kind: 'answered', answer: { ref: 'ANS-2209' } });
    expect(api.agents.getRun).not.toHaveBeenCalled();
  });

  it('settles at once on a run the server already finished', async () => {
    const { api, ask, outcome } = setup();
    api.agents.startRun.mockResolvedValue(create(StartRunResponseSchema, { run: run({ status: RunStatus.DONE, answer: ANSWER }) }));

    await ask();

    expect(outcome()).toMatchObject({ kind: 'answered', answer: { ref: 'ANS-2209' } });
    expect(api.agents.getRun).not.toHaveBeenCalled();
  });

  it('settles a run that ended with a channel summary as answered, from the frame and from the run', async () => {
    const summary = artifact({ kind: ArtifactKind.CHANNEL_SUMMARY, ref: 'ANS-2212', bodyMarkdown: 'The launch moves to Friday.' });
    const { ask, emit, started, outcome } = setup();
    started();
    await ask();

    emit({ state: AgentRunState.DONE, answer: summary });

    expect(outcome()).toMatchObject({ kind: 'answered', answer: { kind: 'channel_summary', ref: 'ANS-2212' } });

    const polled = setup();
    polled.api.agents.startRun.mockResolvedValue(create(StartRunResponseSchema, { run: run({ status: RunStatus.DONE, answer: summary }) }));
    await polled.ask();
    expect(polled.outcome()).toMatchObject({ kind: 'answered', answer: { kind: 'channel_summary', ref: 'ANS-2212' } });
    expect(polled.api.agents.getRun).not.toHaveBeenCalled();
  });

  it('reads a finished run with no usable answer (no artifact, or one of no kind) as a failure with its own reason, never as an answer and never as an unknown one', async () => {
    for (const answer of [undefined, artifact({ kind: ArtifactKind.UNSPECIFIED, bodyMarkdown: 'plain text of an older run' })]) {
      const { api, ask, emit, started, outcome } = setup();
      started();
      api.agents.getRun.mockResolvedValue(create(GetRunResponseSchema, { run: run({ status: RunStatus.DONE, ...(answer === undefined ? {} : { answer }) }) }));
      await ask();

      emit({ state: AgentRunState.DONE });
      await act(async () => {});

      expect(outcome()).toMatchObject({ kind: 'failed', reason: 'no_answer' });
    }
  });

  it('scopes a question to a channel', async () => {
    const { api, ask, started } = setup();
    started();

    await ask(QUESTION, 'ch-q4');

    expect(api.agents.startRun.mock.calls[0]?.[0]).toEqual({ agent: 'assistant', question: QUESTION, channelId: 'ch-q4' });
  });

  it('keeps asking through a poll the network drops, and stops at a refusal', async () => {
    vi.useFakeTimers();
    const { api, ask, started, outcome } = setup();
    started();
    api.agents.getRun
      .mockRejectedValueOnce(new ConnectError('bad gateway', Code.Unavailable))
      .mockResolvedValueOnce(create(GetRunResponseSchema, { run: run({ status: RunStatus.DONE, answer: ANSWER }) }));
    await ask();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(FIRST_POLL_MS + FIRST_POLL_MS * POLL_FACTOR);
    });
    expect(outcome()?.kind).toBe('answered');

    api.agents.getRun.mockRejectedValue(new ConnectError('not yours', Code.PermissionDenied));
    await ask('Another question');
    await act(async () => {
      await vi.advanceTimersByTimeAsync(FIRST_POLL_MS);
    });
    expect(outcome()).toMatchObject({ kind: 'failed', reason: '' });
  });

  it('gives up on a run that says nothing for three minutes, and offers Retry rather than a spinner', async () => {
    vi.useFakeTimers();
    const { api, ask, started, outcome } = setup();
    started();
    api.agents.getRun.mockResolvedValue(create(GetRunResponseSchema, { run: run({ status: RunStatus.RUNNING }) }));
    await ask();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(RUN_BUDGET_MS + MAX_POLL_MS);
    });

    expect(outcome()).toMatchObject({ kind: 'failed', reason: 'timed_out' });
    expect(vi.getTimerCount()).toBe(0);
  });

  it('fails a question the server could not be asked, and one it refused', async () => {
    const { api, ask, outcome } = setup();
    api.agents.startRun.mockRejectedValueOnce(new ConnectError('down', Code.Unavailable));
    await ask();
    expect(outcome()).toMatchObject({ kind: 'failed', reason: 'not_reached' });

    api.agents.startRun.mockRejectedValueOnce(new ConnectError('too long', Code.InvalidArgument));
    await ask('x'.repeat(10));
    expect(outcome()).toMatchObject({ kind: 'failed', reason: '' });
  });

  it('follows each question on its own: asking again leaves the earlier run going', async () => {
    const { api, ask, emit, started, result } = setup();
    started('run-1');
    await ask('First question');
    started('run-2');
    await ask('Second question');

    emit({ runId: 'run-1', state: AgentRunState.DONE, answer: ANSWER });

    // Newest first: the second question is still pending, the first has its answer.
    expect(result.current.exchanges.map((exchange) => [exchange.question, exchange.outcome.kind])).toEqual([
      ['Second question', 'pending'],
      ['First question', 'answered'],
    ]);
    expect(api.agents.startRun).toHaveBeenCalledTimes(2);
  });

  it('tells onSettled once of each run that ends, with its exchange id, and of nothing else', async () => {
    const settled: [string, string][] = [];
    const { api, ask, emit, started } = setup((id, outcome) => {
      settled.push([id, outcome.kind]);
    });
    started('run-1');
    const id = await ask();

    emit({ state: AgentRunState.RUNNING });
    expect(settled).toEqual([]);
    emit({ state: AgentRunState.DONE, answer: ANSWER });
    emit({ state: AgentRunState.DONE, answer: ANSWER });

    expect(settled).toEqual([[id, 'answered']]);
    expect(api.agents.getRun).not.toHaveBeenCalled();
  });

  it('holds no timer and no request once the screen is gone', async () => {
    vi.useFakeTimers();
    const { api, ask, started, unmount } = setup();
    started();
    api.agents.getRun.mockResolvedValue(create(GetRunResponseSchema, { run: run({ status: RunStatus.RUNNING }) }));
    await ask();
    expect(vi.getTimerCount()).toBeGreaterThan(0);

    unmount();
    // What is left after a tick is not the hook's: its own poll timer, 1.5 s out, is gone.
    await vi.advanceTimersByTimeAsync(0);

    expect(vi.getTimerCount()).toBe(0);
    await vi.advanceTimersByTimeAsync(RUN_BUDGET_MS);
    expect(api.agents.getRun).not.toHaveBeenCalled();
  });
});
