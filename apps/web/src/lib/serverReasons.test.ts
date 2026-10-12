import { createI18n } from '@kiwi/core';
import { describe, expect, it } from 'vitest';
import { failureReasonKey } from './failure';

/**
 * Every failure reason the server stores for an agent run or a meeting, by hand, from the Go constants:
 * `backend/internal/agents/jobs.go` (`reasonModelUnavailable` ... `reasonInvalid`, agents.runs.failure_reason) and
 * `backend/internal/meetings/service_impl.go` (`reasonMeetAccess` ... `reasonUnreadable`, meetings.failure_reason).
 * A reason added there is a decision here: give it a sentence, or list it in GENERIC_ON_PURPOSE below.
 */
const SERVER_REASONS = [
  // agents/jobs.go
  'model_unavailable',
  'timeout',
  'too_many_steps',
  'truncated',
  'unknown_agent',
  'no_access',
  'trigger_too_old',
  'no_question',
  'invalid',
  // meetings/service_impl.go
  'meet_access',
  'meet_unavailable',
  'no_speech',
  'internal',
  'transcript_too_long',
  'unsupported_audio',
  'unreadable',
] as const;

/** Reasons with no sentence of their own that a person could act on: the generic "something went wrong." is the honest one. */
const GENERIC_ON_PURPOSE: readonly string[] = [
  'unknown_agent', // the registry has no such agent
  'no_access', // the person may not ask here any more, or the message is not theirs
  'trigger_too_old', // the message that called the agent fell out of the latest messages
  'no_question', // the message had nothing to answer
  'invalid', // the event itself was wrong
  'meet_access', // the owner's Google grant for Meet is gone
  'meet_unavailable', // Meet kept failing until the last attempt
  'internal', // a write of the pipeline kept failing
];

describe('failureReasonKey and the reasons the server gives', () => {
  it('gives each of the server reasons its own sentence, or lists it as generic on purpose', () => {
    const unplaced = SERVER_REASONS.filter((reason) => failureReasonKey(reason) === 'reasonUnknown' && !GENERIC_ON_PURPOSE.includes(reason));

    expect(unplaced).toEqual([]);
  });

  it('keeps GENERIC_ON_PURPOSE honest: each listed reason is a server reason and says the generic sentence', () => {
    for (const reason of GENERIC_ON_PURPOSE) {
      expect(SERVER_REASONS as readonly string[], reason).toContain(reason);
      expect(failureReasonKey(reason), reason).toBe('reasonUnknown');
    }
  });

  it('says "it took too long." for a run that hit its own deadline (`timeout`) and for one that never stopped calling tools', () => {
    expect(failureReasonKey('timeout')).toBe('reasonTimedOut');
    expect(failureReasonKey('too_many_steps')).toBe('reasonTimedOut');
    expect(failureReasonKey('timeout')).toBe(failureReasonKey('timed_out'));
  });

  it('says "no answer with a source came back" for an answer cut at the token limit', () => {
    expect(failureReasonKey('truncated')).toBe('reasonNoAnswer');
  });

  it.each(['en', 'th'] as const)('has a %s sentence for each of them', (lang) => {
    const { plain } = createI18n(lang);

    for (const reason of ['timeout', 'too_many_steps', 'truncated'] as const) {
      expect(plain(failureReasonKey(reason)).trim(), reason).not.toBe('');
      expect(plain(failureReasonKey(reason)), reason).not.toBe(plain('reasonUnknown'));
    }
  });
});
