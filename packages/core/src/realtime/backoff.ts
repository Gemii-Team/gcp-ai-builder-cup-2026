export type BackoffOptions = {
  /** Ceiling of the first retry's delay, doubled for each further one. Default 500 ms. */
  baseMs?: number;
  /** Ceiling no delay exceeds. Default 30 000 ms. */
  capMs?: number;
  /** Ceiling of the short delay after a close with code 1001 (going away). Default 2 000 ms. */
  goingAwayMs?: number;
  /** A uniform draw in [0, 1). Default `Math.random`; tests inject a fixed one. */
  random?: () => number;
};

export type Backoff = {
  /** The wait before retry number `attempt` (0 for the first): full jitter, uniform in [0, min(cap, base · 2^attempt)). */
  delay: (attempt: number) => number;
  /** The wait before reconnecting after the server said it is going away: short, and spread out so a rollout is not a stampede. */
  goingAway: () => number;
};

export function createBackoff({ baseMs = 500, capMs = 30_000, goingAwayMs = 2_000, random = Math.random }: BackoffOptions = {}): Backoff {
  return {
    delay: (attempt) => random() * Math.min(capMs, baseMs * 2 ** attempt),
    goingAway: () => random() * goingAwayMs,
  };
}
