// Entry point of @kiwi/core: the realtime client with its seq resync, the in-memory pending-send queue, the screen
// strings (English and Thai) and the int64 boundary helper. Test support (realtime/testing.ts) is not exported.
export { toSafeNumber } from './int64';
export { createI18n, type I18n, type I18nKey, type I18nParams, type Lang } from './i18n';
export { createPendingQueue, type PendingEntry, type PendingQueue, type SendResult } from './queue/pendingQueue';
export type { BackoffOptions } from './realtime/backoff';
export {
  createRealtimeClient,
  type ConnectionStatus,
  type RealtimeClient,
  type RealtimeEvent,
  type RealtimeOptions,
  type ResetEvent,
} from './realtime/client';
export { createMemoryCursorStore, type CursorStore } from './realtime/cursors';
