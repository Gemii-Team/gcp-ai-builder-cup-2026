import type { Lang } from '@kiwi/core';
import { dayParts, dueAtToDate } from '@kiwi/ui';

const BANGKOK = 'Asia/Bangkok';

/** The Asia/Bangkok calendar day of an instant, `YYYY-MM-DD`: messages of one day sit under one divider. */
export function dayKey(iso: string): string {
  return dueAtToDate(new Date(iso));
}

/**
 * `Tue, 13 Oct 2026` (Thai: `อ., 13 ต.ค. 2026`, in the Gregorian year): the label of a day divider. The weekday and the month
 * are the design's abbreviations from `dayParts`'s tables, never the runtime's own names, so the label is the same under
 * every ICU version. `today` says whether the day is the current one in Bangkok, which the divider names.
 */
export function dayLabel(iso: string, lang: Lang, now: Date): { text: string; today: boolean } {
  const { weekday, day, month, year } = dayParts(new Date(iso), lang, BANGKOK);
  return {
    text: `${weekday}, ${day} ${month} ${year}`,
    today: dayKey(iso) === dayKey(now.toISOString()),
  };
}
