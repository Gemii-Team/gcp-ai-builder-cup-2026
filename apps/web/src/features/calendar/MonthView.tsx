import { LangRuns } from '@kiwi/ui';
import { useMemo } from 'react';
import { TCount, useI18n } from '../../i18n';
import { eventsOn, monthGrid, sameDay, sameMonth, weekDays, type PlacedEvent } from './calendarMath';

/** How many event chips a day cell shows before it collapses the rest into a "+N" count. */
const MAX_CHIPS = 3;

type MonthViewProps = {
  cursor: Date;
  placed: readonly PlacedEvent[];
  now: Date;
};

/** The month grid: six weeks of day cells, each with its date and the day's events as chips. */
export function MonthView({ cursor, placed, now }: MonthViewProps) {
  const { lang } = useI18n();
  const days = useMemo(() => monthGrid(cursor), [cursor]);
  const weekdayNames = useMemo(() => {
    const fmt = new Intl.DateTimeFormat(lang === 'th' ? 'th-TH' : 'en-GB', { weekday: 'short' });
    return weekDays(now).map((day) => fmt.format(day));
  }, [lang, now]);
  const eventTime = useMemo(() => new Intl.DateTimeFormat(lang === 'th' ? 'th-TH-u-nu-latn' : 'en-GB', { hour: '2-digit', minute: '2-digit', hour12: false }), [lang]);

  return (
    <div className="mv">
      <div className="mv-week-head">
        {weekdayNames.map((name, index) => (
          <span key={index} className="mv-wd">
            {name}
          </span>
        ))}
      </div>
      <div className="mv-grid">
        {days.map((day) => {
          const dayEvents = eventsOn(placed, day);
          const shown = dayEvents.slice(0, MAX_CHIPS);
          const extra = dayEvents.length - shown.length;
          return (
            <div key={day.toISOString()} className="mv-cell" data-outside={!sameMonth(day, cursor) || undefined} data-today={sameDay(day, now) || undefined}>
              <span className="mv-date">{day.getDate()}</span>
              <div className="mv-chips">
                {shown.map((p) => {
                  const Tag = p.event.meetUri !== '' ? 'a' : 'div';
                  return (
                    <Tag
                      key={p.event.id}
                      className="mv-chip"
                      data-org={p.event.orgWide || undefined}
                      {...(p.event.meetUri !== '' ? { href: p.event.meetUri, target: '_blank', rel: 'noreferrer noopener' } : {})}
                    >
                      <span className="mv-chip-time">{eventTime.format(p.start)}</span>
                      <span className="mv-chip-ttl">
                        <LangRuns text={p.event.title} />
                      </span>
                    </Tag>
                  );
                })}
                {extra > 0 && (
                  <span className="mv-more">
                    <TCount k="calMore" count={extra} />
                  </span>
                )}
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}
