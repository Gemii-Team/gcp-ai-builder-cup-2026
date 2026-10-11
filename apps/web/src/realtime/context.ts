import type { ConnectionStatus, CursorStore, PendingQueue, RealtimeClient } from '@kiwi/core';
import { createContext, useContext } from 'react';

export type RealtimeContextValue = {
  client: RealtimeClient;
  /** The last seq of each channel the client has delivered: history loads advance it, so a catch-up asks only for the rest. */
  cursors: CursorStore;
  /** Messages the network refused, kept in order until the connection is back. */
  queue: PendingQueue;
  status: ConnectionStatus;
};

export const RealtimeContext = createContext<RealtimeContextValue | null>(null);

export function useRealtime(): RealtimeContextValue {
  const value = useContext(RealtimeContext);
  if (value === null) {
    throw new Error('useRealtime needs a <RealtimeProvider> above it.');
  }
  return value;
}
