import type { I18nKey } from '@kiwi/core';
import type { MeetingView } from '../../api/types';

/** The visual tone of a status badge: it picks the badge colours in meetings.css. */
export type StatusTone = 'ready' | 'processing' | 'failed' | 'none';

/** The badge label key and tone for a meeting's status. Pending and ingesting both read as "Processing". */
export function statusBadge(status: MeetingView['status']): { key: I18nKey; tone: StatusTone } {
  switch (status) {
    case 'ready':
      return { key: 'mStatusReady', tone: 'ready' };
    case 'failed':
      return { key: 'mStatusFailed', tone: 'failed' };
    case 'no_transcript':
      return { key: 'mStatusNoTranscript', tone: 'none' };
    default:
      return { key: 'mStatusProcessing', tone: 'processing' };
  }
}
