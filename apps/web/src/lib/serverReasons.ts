/**
 * Failure reasons the server stores for an agent run (`agents.runs.failure_reason`, `backend/internal/agents/jobs.go`) that
 * the web already has a sentence for under another word: the server's word, then the web's. `failureReasonKey` reads its
 * code through `webReason`.
 *
 * `timeout` is the run's own deadline (the web's client-side give-up is `timed_out`: same sentence), and a run that kept
 * calling tools (`too_many_steps`) took too long as well. `truncated` is an answer cut at the token limit twice: no answer
 * came back. A reason not named here, and with no sentence of its own, stays the generic one.
 */
const WEB_REASON_OF: Readonly<Record<string, string>> = {
  timeout: 'timed_out',
  too_many_steps: 'timed_out',
  truncated: 'no_answer',
};

/** The web's word for a reason: the one the server gave, unless the web says it another way. */
export function webReason(code: string): string {
  return Object.hasOwn(WEB_REASON_OF, code) ? (WEB_REASON_OF[code] ?? code) : code;
}
