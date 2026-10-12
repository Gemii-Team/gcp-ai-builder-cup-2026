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

/**
 * `Fri 16 Oct` (Thai: `ศ. 16 ต.ค.`): the day label of a due date, a done date or a message day, never a time of day.
 * A date-only string is a calendar day and keeps its day in any zone; an instant is read in `timeZone`.
 */
export function formatDay(d: Date | string, lang: 'en' | 'th', timeZone: string = BANGKOK): string {
  const calendarDay = typeof d === 'string' && DATE_ONLY.test(d);
  const date = calendarDay ? new Date(`${d}T12:00:00Z`) : new Date(d);
  const locale = lang === 'th' ? 'th-TH-u-ca-gregory' : 'en-US';
  const { weekday, day, month } = parts(
    formatter(locale, { weekday: 'short', day: 'numeric', month: 'short', timeZone: calendarDay ? 'UTC' : timeZone }),
    date,
  );
  return `${weekday} ${day} ${month}`;
}
