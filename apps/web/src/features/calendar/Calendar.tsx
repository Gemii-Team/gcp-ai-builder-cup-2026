import '../shell/screen.css';
import './calendar.css';
import { Button, Icon, IconButton, Pending } from '@kiwi/ui';
import { useMemo, useRef, useState } from 'react';
import { useChannels, useScheduleMeeting, useUpcomingEvents } from '../../api/hooks';
import { T, useI18n } from '../../i18n';
import { LoadFailed } from '../shell/LoadFailed';
import { addDays, addMonths, place, startOfDay, weekDays, type CalView } from './calendarMath';
import { MonthView } from './MonthView';
import { ScheduleForm } from './ScheduleForm';
import { TimeGrid } from './TimeGrid';

const VIEWS: CalView[] = ['month', 'week', 'day'];
const VIEW_LABEL: Record<CalView, 'calViewMonth' | 'calViewWeek' | 'calViewDay'> = {
  month: 'calViewMonth',
  week: 'calViewWeek',
  day: 'calViewDay',
};

/** The route `/calendar`: a real calendar with Month, Week and Day views, a period navigator, and a form to schedule a meeting. */
export function CalendarScreen() {
  const { lang, plain } = useI18n();
  const channels = useChannels();
  const events = useUpcomingEvents();
  const schedule = useScheduleMeeting();
  const [view, setView] = useState<CalView>('month');
  const [cursor, setCursor] = useState(() => startOfDay(new Date()));
  const [formOpen, setFormOpen] = useState(false);
  const openButton = useRef<HTMLButtonElement>(null);

  const now = useMemo(() => new Date(), []);
  const placed = useMemo(() => (events.data ?? []).map(place), [events.data]);
  const teams = useMemo(() => (channels.data ?? []).map((channel) => ({ id: channel.id, name: channel.name })), [channels.data]);
  const noTeams = teams.length === 0;

  const locale = lang === 'th' ? 'th-TH-u-ca-gregory-nu-latn' : 'en-GB';
  const periodLabel = useMemo(() => {
    if (view === 'month') {
      return new Intl.DateTimeFormat(locale, { month: 'long', year: 'numeric' }).format(cursor);
    }
    if (view === 'week') {
      const week = weekDays(cursor);
      return new Intl.DateTimeFormat(locale, { day: 'numeric', month: 'short', year: 'numeric' }).formatRange(week[0], week[6]);
    }
    return new Intl.DateTimeFormat(locale, { weekday: 'long', day: 'numeric', month: 'long', year: 'numeric' }).format(cursor);
  }, [view, cursor, locale]);

  function step(direction: -1 | 1) {
    setCursor((at) => (view === 'month' ? addMonths(at, direction) : addDays(at, direction * (view === 'week' ? 7 : 1))));
  }

  return (
    <div className="cal">
      <main className="main">
        <header className="chead">
          <div className="ttl">
            <h1>
              <T k="calendarTitle" />
            </h1>
          </div>
          <div className="acts">
            <Button
              ref={openButton}
              variant="primary"
              aria-expanded={formOpen}
              disabled={noTeams}
              onClick={() => {
                setFormOpen((open) => !open);
              }}
            >
              <Icon name="clock" />
              <T k="scheduleMeeting" />
            </Button>
          </div>
        </header>

        <div className="cal-bar">
          <div className="cal-nav">
            <Button
              size="sm"
              onClick={() => {
                setCursor(startOfDay(new Date()));
              }}
            >
              <T k="calToday" />
            </Button>
            <IconButton
              label={plain('calPrev')}
              className="cal-step"
              onClick={() => {
                step(-1);
              }}
            >
              <Icon name="back" />
            </IconButton>
            <IconButton
              label={plain('calNext')}
              className="cal-step"
              onClick={() => {
                step(1);
              }}
            >
              <Icon name="back" className="cal-flip" />
            </IconButton>
            <h2 className="cal-period">{periodLabel}</h2>
          </div>
          <div className="cal-views" role="group" aria-label={plain('calViewSwitch')}>
            {VIEWS.map((option) => (
              <button
                key={option}
                type="button"
                className="cal-view-btn"
                aria-pressed={view === option}
                onClick={() => {
                  setView(option);
                }}
              >
                <T k={VIEW_LABEL[option]} />
              </button>
            ))}
          </div>
        </div>

        {formOpen && !noTeams && (
          <ScheduleForm
            id="schedule-form"
            teams={teams}
            onClose={() => {
              setFormOpen(false);
              openButton.current?.focus();
            }}
            onSubmit={async (input) => {
              await schedule.mutateAsync(input);
              setFormOpen(false);
              setCursor(startOfDay(input.startAt));
              openButton.current?.focus();
            }}
          />
        )}

        <div className="cal-stage">
          {events.isPending && <Pending label={plain('loading')} />}
          {events.isError && events.data === undefined && (
            <LoadFailed
              onRetry={() => {
                void events.refetch();
              }}
            >
              <T k="calendarLoadFailed" />
            </LoadFailed>
          )}
          {events.data !== undefined && view === 'month' && <MonthView cursor={cursor} placed={placed} now={now} />}
          {events.data !== undefined && view === 'week' && <TimeGrid days={weekDays(cursor)} placed={placed} now={now} />}
          {events.data !== undefined && view === 'day' && <TimeGrid days={[startOfDay(cursor)]} placed={placed} now={now} />}
        </div>
      </main>
    </div>
  );
}
