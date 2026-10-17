import { LangRuns } from '@kiwi/ui';
import { useEffect, useRef } from 'react';
import { useI18n } from '../../i18n';
import { eventsOn, HOURS, minutesOfDay, sameDay, type PlacedEvent } from './calendarMath';

/** The pixel height of one hour row; the whole day is 24 of these, scrolled to the morning on open. */
const HOUR_HEIGHT = 48;

type TimeGridProps = {
  /** The day columns to show: seven for the week view, one for the day view. */
  days: readonly Date[];
  placed: readonly PlacedEvent[];
  now: Date;
};

/** The hour-ruled day columns of the Week and Day views, with each event a block positioned by its start and length. */
export function TimeGrid({ days, placed, now }: TimeGridProps) {
  const { lang } = useI18n();
  const scroller = useRef<HTMLDivElement>(null);

  // Open on the working day, not on midnight: scroll so 7am sits near the top.
  useEffect(() => {
    if (scroller.current !== null) {
      scroller.current.scrollTop = 7 * HOUR_HEIGHT;
    }
  }, []);

  const hourLabel = new Intl.DateTimeFormat(lang === 'th' ? 'th-TH-u-nu-latn' : 'en-GB', { hour: '2-digit', minute: '2-digit', hour12: false });
  const dayNum = new Intl.DateTimeFormat(lang === 'th' ? 'th-TH-u-ca-gregory-nu-latn' : 'en-GB', { weekday: 'short', day: 'numeric' });
  const eventTime = new Intl.DateTimeFormat(lang === 'th' ? 'th-TH-u-nu-latn' : 'en-GB', { hour: '2-digit', minute: '2-digit', hour12: false });

  return (
    <div className="tg">
      <div className="tg-head" style={{ gridTemplateColumns: `var(--tg-gutter) repeat(${days.length}, minmax(0, 1fr))` }}>
        <span className="tg-corner" />
        {days.map((day) => (
          <div key={day.toISOString()} className="tg-day-head" data-today={sameDay(day, now) || undefined}>
            {dayNum.format(day)}
          </div>
        ))}
      </div>
      <div className="tg-scroll" ref={scroller}>
        <div className="tg-body" style={{ gridTemplateColumns: `var(--tg-gutter) repeat(${days.length}, minmax(0, 1fr))`, height: `${24 * HOUR_HEIGHT}px` }}>
          <div className="tg-axis">
            {HOURS.map((hour) => (
              <div key={hour} className="tg-hour" style={{ height: `${HOUR_HEIGHT}px` }}>
                {hour > 0 && <span className="tg-hour-lbl">{hourLabel.format(new Date(2000, 0, 1, hour))}</span>}
              </div>
            ))}
          </div>
          {days.map((day) => {
            const dayEvents = eventsOn(placed, day);
            return (
              <div key={day.toISOString()} className="tg-col" data-today={sameDay(day, now) || undefined}>
                {HOURS.map((hour) => (
                  <div key={hour} className="tg-slot" style={{ height: `${HOUR_HEIGHT}px` }} />
                ))}
                {sameDay(day, now) && <div className="tg-nowline" style={{ top: `${(minutesOfDay(now) / 60) * HOUR_HEIGHT}px` }} aria-hidden="true" />}
                {dayEvents.map((p) => {
                  const top = (minutesOfDay(p.start) / 60) * HOUR_HEIGHT;
                  const minutes = Math.max(20, (p.end.getTime() - p.start.getTime()) / 60000);
                  const height = Math.min((minutes / 60) * HOUR_HEIGHT, 24 * HOUR_HEIGHT - top);
                  const Tag = p.event.meetUri !== '' ? 'a' : 'div';
                  return (
                    <Tag
                      key={p.event.id}
                      className="tg-event"
                      data-org={p.event.orgWide || undefined}
                      style={{ top: `${top}px`, height: `${Math.max(height, 20)}px` }}
                      {...(p.event.meetUri !== '' ? { href: p.event.meetUri, target: '_blank', rel: 'noreferrer noopener' } : {})}
                    >
                      <span className="tg-event-time">{eventTime.format(p.start)}</span>
                      <span className="tg-event-ttl">
                        <LangRuns text={p.event.title} />
                      </span>
                    </Tag>
                  );
                })}
              </div>
            );
          })}
        </div>
      </div>
    </div>
  );
}
