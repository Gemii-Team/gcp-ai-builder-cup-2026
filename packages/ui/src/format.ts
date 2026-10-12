// The Two Clocks Rule: a transcript offset is `h:mm:ss` and points into a recording; a wall-clock time is
// `hh:mm` on a 24-hour clock and points into the day. The two formatters never share a shape.

/** K.I.W.I.'s zone. Asia/Bangkok has no daylight saving, so a due date is exactly UTC+7. */
const BANGKOK = 'Asia/Bangkok';

const formatters = new Map<string, Intl.DateTimeFormat>();

/** `Intl.DateTimeFormat` construction is slow and a stream formats a time per message, so each shape is built once. */
function formatter(locale: string, options: Intl.DateTimeFormatOptions): Intl.DateTimeFormat {
  const key = `${locale}|${JSON.stringify(options)}`;
  let cached = formatters.get(key);
  if (cached === undefined) {
    cached = new Intl.DateTimeFormat(locale, options);
    formatters.set(key, cached);
  }
  return cached;
}

function parts(format: Intl.DateTimeFormat, date: Date): Record<string, string> {
  return Object.fromEntries(format.formatToParts(date).map((part) => [part.type, part.value]));
}

/** `0:12:41`: no leading hour zero. `null` for a missing or negative offset, so callers render nothing, never `0:00:00`. */
export function formatOffset(ms: number | null | undefined): string | null {
  if (ms == null || !Number.isFinite(ms) || ms < 0) {
    return null;
  }
  const total = Math.floor(ms / 1000);
  const hours = Math.floor(total / 3600);
  const minutes = String(Math.floor((total % 3600) / 60)).padStart(2, '0');
  const seconds = String(total % 60).padStart(2, '0');
  return `${hours}:${minutes}:${seconds}`;
}

/** `14:31`: 24 hours, zero-padded, and `00:05` rather than `24:05` after midnight. */
export function formatClock(d: Date | string, timeZone: string = BANGKOK): string {
  const { hour, minute } = parts(formatter('en-US', { hour: '2-digit', minute: '2-digit', hourCycle: 'h23', timeZone }), new Date(d));
  return `${hour}:${minute}`;
}

/** `14 KB` below a megabyte, `1.5 MB` above, the way the prototype labels an attached file. */
export function formatSize(bytes: number): string {
  if (bytes < 1048576) {
    return `${Math.max(1, Math.round(bytes / 1024))} KB`;
  }
  return `${(bytes / 1048576).toFixed(1)} MB`;
}

/** A due date `YYYY-MM-DD` as midnight in Asia/Bangkok. */
export function dateToDueAt(iso: string): Date {
  return new Date(`${iso}T00:00:00+07:00`);
}

/** The Asia/Bangkok calendar date of an instant as `YYYY-MM-DD`: the inverse of `dateToDueAt`. */
export function dueAtToDate(d: Date): string {
  const { year, month, day } = parts(formatter('en-US', { year: 'numeric', month: '2-digit', day: '2-digit', timeZone: BANGKOK }), d);
  return `${year}-${month}-${day}`;
}

const DATE_ONLY = /^\d{4}-\d{2}-\d{2}$/;

// The weekday and month abbreviations as DESIGN.md and the prototypes write them, indexed by the calendar numbers of a
// date. Tables, never `Intl` names: those are the runtime's CLDR data, which changes between releases (CLDR 48 writes the
// Thai weekday in full, `ศุกร์`, where the design has `ศ.`; en-GB writes September `Sept`), so two browsers would label
// the same day differently. `Intl` is asked for numbers only, which every release writes the same way.
const WEEKDAYS = {
  en: ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'],
  th: ['อา.', 'จ.', 'อ.', 'พ.', 'พฤ.', 'ศ.', 'ส.'],
} as const;

const MONTHS = {
  en: ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'],
  th: ['ม.ค.', 'ก.พ.', 'มี.ค.', 'เม.ย.', 'พ.ค.', 'มิ.ย.', 'ก.ค.', 'ส.ค.', 'ก.ย.', 'ต.ค.', 'พ.ย.', 'ธ.ค.'],
} as const;

/**
 * The weekday, day, month and Gregorian year of an instant in `timeZone`, the weekday and the month as the design
 * abbreviates them (`Fri`, `Oct`; `ศ.`, `ต.ค.`) and the day and the year as plain numbers. The one place a day label gets
 * its words: `formatDay` and the day dividers of a channel both write theirs from it.
 */
export function dayParts(date: Date, lang: 'en' | 'th', timeZone: string): { weekday: string; day: string; month: string; year: string } {
  const { year, month, day } = parts(formatter('en-US', { year: 'numeric', month: 'numeric', day: 'numeric', timeZone }), date);
  const weekday = new Date(Date.UTC(Number(year), Number(month) - 1, Number(day))).getUTCDay();
  return { weekday: WEEKDAYS[lang][weekday], day, month: MONTHS[lang][Number(month) - 1], year };
}

/**
 * `Fri 16 Oct` (Thai: `ศ. 16 ต.ค.`): the day label of a due date, a done date or a message day, never a time of day.
 * A date-only string is a calendar day and keeps its day in any zone; an instant is read in `timeZone`.
 */
export function formatDay(d: Date | string, lang: 'en' | 'th', timeZone: string = BANGKOK): string {
  const calendarDay = typeof d === 'string' && DATE_ONLY.test(d);
  const date = calendarDay ? new Date(`${d}T12:00:00Z`) : new Date(d);
  const { weekday, day, month } = dayParts(date, lang, calendarDay ? 'UTC' : timeZone);
  return `${weekday} ${day} ${month}`;
}
