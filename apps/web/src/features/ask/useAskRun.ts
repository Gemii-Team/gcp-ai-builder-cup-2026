import type { RealtimeClient } from '@kiwi/core';
import { useCallback, useEffect, useRef, useState } from 'react';
import type { Api } from '../../api/client';
import { useApi } from '../../api/client';
import { sendOutcome } from '../../api/errors';
import { mapRun, mapRunProgress } from '../../api/mappers';
import type { ArtifactView, RunProgressView, RunView } from '../../api/types';
import { useRealtime } from '../../realtime/context';

/** The only agent there is: Ask always talks to it. */
const AGENT = 'assistant';

// A frame can be missed (the socket was reconnecting, the tab was in the background), so a run that has not finished is
// asked about with `GetRun`, first after 1.5 s and then ever less often, never rarer than every 10 s. A run that has said
// nothing for 3 minutes is given up on, so the person is offered Retry instead of a spinner that never ends.
export const FIRST_POLL_MS = 1_500;
export const POLL_FACTOR = 1.5;
export const MAX_POLL_MS = 10_000;
export const RUN_BUDGET_MS = 180_000;

/** How a question stands. `failed.reason` is a code `failureReasonKey` knows, empty when the server gave none. */
export type AskOutcome =
  | { kind: 'pending'; phase: 'retrieving' | 'writing' }
  | { kind: 'answered'; answer: ArtifactView }
  | { kind: 'failed'; reason: string; at: string };

/** One question of the thread and what became of it. A not-found answer is `answered`: it has a REF and is no failure. */
export type AskExchange = {
  id: string;
  question: string;
  /** The channel the question was scoped to, when it was. */
  channelId: string | null;
  askedAt: string;
  outcome: AskOutcome;
};

type Settled = Exclude<AskOutcome, { kind: 'pending' }>;

type Follow = {
  api: Api;
  realtime: RealtimeClient;
  question: string;
  channelId: string | null;
  onPhase: (phase: 'writing') => void;
  onSettled: (outcome: Settled) => void;
};

const failed = (reason: string): Settled => ({ kind: 'failed', reason, at: new Date().toISOString() });

/**
 * Starts a run and follows it to its end, from whichever arrives first: the `AgentRunProgress` frame that carries the
 * validated answer, or the answer of a `GetRun` poll. Returns the function that stops following (no outcome is reported
 * after it): its timer, its subscription and its requests are gone.
 *
 * The streamed text itself is never shown: an answer is a slip only once its citations have validated.
 */
function followRun({ api, realtime, question, channelId, onPhase, onSettled }: Follow): () => void {
  const abort = new AbortController();
  const startedAt = Date.now();
  // The frames that come before `StartRun` has said which run is ours are kept, and looked at once it has.
  const early: RunProgressView[] = [];
  let runId: string | null = null;
  let settled = false;
  let attempts = 0;
  let timer: number | undefined;

  const unsubscribe = realtime.on((event) => {
    if ('type' in event || event.event.case !== 'agentRunProgress') {
      return;
    }
    const progress = mapRunProgress(event.event.value);
    if (runId === null) {
      early.push(progress);
    } else {
      onFrame(progress);
    }
  });

  function stop() {
    settled = true;
    window.clearTimeout(timer);
    unsubscribe();
    abort.abort();
  }

  function finish(outcome: Settled) {
    if (!settled) {
      stop();
      onSettled(outcome);
    }
  }

  /** What a run the server says is over comes to. A `done` run with no usable answer issued nothing to show. */
  function absorb(run: RunView) {
    if (run.status === 'done') {
      finish(run.answer === null ? failed('') : { kind: 'answered', answer: run.answer });
    } else if (run.status === 'failed') {
      finish(failed(run.failureReason));
    }
  }

  function schedule() {
    window.clearTimeout(timer);
    if (!settled) {
      timer = window.setTimeout(() => {
        void poll();
      }, Math.min(FIRST_POLL_MS * POLL_FACTOR ** attempts, MAX_POLL_MS));
      attempts += 1;
    }
  }

  async function poll() {
    if (settled || runId === null) {
      return;
    }
    try {
      const { run } = await api.agents.getRun({ runId }, { signal: abort.signal });
      if (settled) {
        return;
      }
      if (run === undefined) {
        throw new Error('GetRun answered without a run.');
      }
      absorb(mapRun(run));
    } catch (error) {
      // A refusal will be the same next time; anything else (the network, a gateway) is tried again.
      if (!settled && sendOutcome(error) === 'rejected') {
        finish(failed(''));
      }
    }
    if (settled) {
      return;
    }
    if (Date.now() - startedAt >= RUN_BUDGET_MS) {
      finish(failed('timed_out'));
    } else {
      schedule();
    }
  }

  /** The frame says the run failed but not why: the run does. */
  async function learnFailure() {
    let reason = '';
    try {
      const { run } = await api.agents.getRun({ runId: runId ?? '' }, { signal: abort.signal });
      reason = run?.failureReason ?? '';
    } catch {
      // Without the run's own word the failure is reported without a reason.
    }
    finish(failed(reason));
  }

  function onFrame(progress: RunProgressView) {
    if (progress.runId !== runId || settled) {
      return;
    }
    switch (progress.state) {
      case 'running':
      case 'streaming':
        onPhase('writing');
        break;
      case 'done':
        if (progress.answer === null) {
          void poll();
        } else {
          finish({ kind: 'answered', answer: progress.answer });
        }
        break;
      case 'failed':
        void learnFailure();
        break;
    }
  }

  api.agents.startRun({ agent: AGENT, question, ...(channelId === null ? {} : { channelId }) }, { signal: abort.signal }).then(
    ({ run }) => {
      if (settled) {
        return;
      }
      if (run === undefined || run.id === '') {
        finish(failed(''));
        return;
      }
      runId = run.id;
      absorb(mapRun(run));
      for (const progress of early.splice(0)) {
        onFrame(progress);
      }
      schedule();
    },
    (error: unknown) => {
      finish(failed(sendOutcome(error) === 'rejected' ? '' : 'not_reached'));
    },
  );

  return stop;
}

/**
 * The Ask thread's data: the questions asked in this visit, newest first, each with its run followed to an outcome.
 * Asking again while a run is still going leaves that run going: each question is followed on its own. Retry starts a
 * NEW run for a failed question and drops the old one. Every timer, subscription and request ends when the screen
 * unmounts, and when a retry supersedes a run.
 */
export function useAskRun() {
  const api = useApi();
  const { client: realtime } = useRealtime();
  const [exchanges, setExchanges] = useState<AskExchange[]>([]);
  const [asked] = useState(() => new Map<string, { question: string; channelId: string | null }>());
  const [following] = useState(() => new Map<string, () => void>());
  const serial = useRef(0);

  const patch = useCallback((id: string, outcome: AskOutcome) => {
    setExchanges((list) => list.map((exchange) => (exchange.id === id ? { ...exchange, outcome } : exchange)));
  }, []);

  const start = useCallback(
    (id: string, question: string, channelId: string | null) => {
      following.get(id)?.();
      following.set(
        id,
        followRun({
          api,
          realtime,
          question,
          channelId,
          onPhase: (phase) => {
            patch(id, { kind: 'pending', phase });
          },
          onSettled: (outcome) => {
            following.delete(id);
            patch(id, outcome);
          },
        }),
      );
    },
    [api, realtime, following, patch],
  );

  useEffect(
    () => () => {
      for (const stop of following.values()) {
        stop();
      }
      following.clear();
    },
    [following],
  );

  /** Asks `question`, scoped to `channelId` when given. Returns the id of the new exchange. */
  const ask = useCallback(
    (question: string, channelId: string | null = null): string => {
      serial.current += 1;
      const id = `ask-${String(serial.current)}`;
      asked.set(id, { question, channelId });
      setExchanges((list) => [{ id, question, channelId, askedAt: new Date().toISOString(), outcome: { kind: 'pending', phase: 'retrieving' } }, ...list]);
      start(id, question, channelId);
      return id;
    },
    [asked, start],
  );

  /** A new run for the question of a failed exchange, which goes back to retrieving. */
  const retry = useCallback(
    (id: string) => {
      const again = asked.get(id);
      if (again !== undefined) {
        patch(id, { kind: 'pending', phase: 'retrieving' });
        start(id, again.question, again.channelId);
      }
    },
    [asked, start, patch],
  );

  return { exchanges, ask, retry };
}
