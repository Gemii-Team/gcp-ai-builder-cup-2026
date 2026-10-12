import { formatClock, formatDay, Numeral } from '@kiwi/ui';
import { useI18n } from '../../i18n';
import { dayKey } from '../../lib/day';

const isToday = (iso: string, now: Date) => dayKey(iso) === dayKey(now.toISOString());

/** A wall-clock instant: `14:02` on the current day, `Mon 12 Oct 17:05` on any other, the time in mono. */
export function When({ iso, now }: { iso: string; now: Date }) {
  const { lang } = useI18n();
  return (
    <span className="bd-nw">
      {!isToday(iso, now) && `${formatDay(iso, lang)} `}
      <Numeral>{formatClock(iso)}</Numeral>
    </span>
  );
}

/** When a task was finished: the clock time today, the day label on any other day. */
export function DoneWhen({ iso, now }: { iso: string; now: Date }) {
  const { lang } = useI18n();
  return isToday(iso, now) ? <Numeral>{formatClock(iso)}</Numeral> : <>{formatDay(iso, lang)}</>;
}
