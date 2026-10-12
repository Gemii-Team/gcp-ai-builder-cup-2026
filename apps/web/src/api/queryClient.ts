import { Code } from '@connectrpc/connect';
import { QueryClient } from '@tanstack/react-query';
import { hasCode } from './errors';

/**
 * A failed request is tried once more, except one the server refused for the session: it would be refused again, and the
 * session guard is waiting on that answer to send the person to sign-in.
 */
export function retryQuery(failureCount: number, error: unknown): boolean {
  return failureCount < 1 && !hasCode(error, Code.Unauthenticated);
}

export function createQueryClient(): QueryClient {
  // A request that fails twice is not going to succeed by itself: say so after a moment, not after minutes of retries.
  return new QueryClient({ defaultOptions: { queries: { retry: retryQuery, staleTime: 30_000 } } });
}
