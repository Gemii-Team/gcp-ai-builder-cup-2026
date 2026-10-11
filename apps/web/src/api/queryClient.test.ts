import { Code, ConnectError } from '@connectrpc/connect';
import { describe, expect, it } from 'vitest';
import { retryQuery } from './queryClient';

describe('retryQuery', () => {
  it('tries a failed request once more, and no more', () => {
    const unavailable = new ConnectError('down', Code.Unavailable);

    expect(retryQuery(0, unavailable)).toBe(true);
    expect(retryQuery(1, unavailable)).toBe(false);
    expect(retryQuery(0, new TypeError('Failed to fetch'))).toBe(true);
  });

  // The session guard waits on GetMe: a person who is signed out must reach sign-in now, not after a retry.
  it('never retries Unauthenticated: the same session will be refused again', () => {
    expect(retryQuery(0, new ConnectError('no session', Code.Unauthenticated))).toBe(false);
  });
});
