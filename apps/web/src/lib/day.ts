import type { Lang } from '@kiwi/core';
import { dueAtToDate } from '@kiwi/ui';

const BANGKOK = 'Asia/Bangkok';

/** The Asia/Bangkok calendar day of an instant, `YYYY-MM-DD`: messages of one day sit under one divider. */
export function dayKey(iso: string): string {
  return dueAtToDate(new Date(iso));
}

const formats = new Map<Lang, Intl.DateTimeFormat>();

function formatFor(lang: Lang): Intl.DateTimeFormat {
  let format = formats.get(lang);
  if (format === undefined) {
    format = new Intl.DateTimeFormat(lang === 'th' ? 'th-TH-u-ca-gregory' : 'en-GB', {
      weekday: 'short',
      day: 'numeric',
      month: 'short',
      year: 'numeric',
      timeZone: BANGKOK,
    });
    formats.set(lang, format);
  }
  return format;
}

/**
 * `Tue, 13 Oct 2026`: the label of a day divider, built from the parts so the comma and the order are the same under
 * every ICU version. `today` says whether the day is the current one in Bangkok, which the divider names.
 */
export function dayLabel(iso: string, lang: Lang, now: Date): { text: string; today: boolean } {
  const parts = Object.fromEntries(formatFor(lang).formatToParts(new Date(iso)).map((part) => [part.type, part.value]));
  return {
    text: `${parts.weekday ?? ''}, ${parts.day ?? ''} ${parts.month ?? ''} ${parts.year ?? ''}`,
    today: dayKey(iso) === dayKey(now.toISOString()),
  };
}
