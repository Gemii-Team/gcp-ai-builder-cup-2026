import { Button, DateField, NativeSelect } from '@kiwi/ui';
import { useId, useState, type FormEvent } from 'react';
import type { ScheduleMeetingInput } from '../../api/hooks';
import { T, useI18n } from '../../i18n';

/** A team the meeting can be scheduled into. */
export type Team = { id: string; name: string };

type ScheduleFormProps = {
  id: string;
  teams: readonly Team[];
  onSubmit: (input: Omit<ScheduleMeetingInput, 'channelId'> & { channelId: string }) => Promise<void>;
  onClose: () => void;
};

/** The minute lengths a meeting can be, shortest first. */
const DURATIONS = [15, 30, 45, 60, 90] as const;

/**
 * The schedule-a-meeting form: a title, the team it belongs to, a date and time in the future, a length, and whether the
 * whole workspace should see it. It builds the start instant from the local date and time and hands the caller a plain
 * input; the server mints the Meet link. A failed submit keeps what was typed so it can be sent again.
 */
export function ScheduleForm({ id, teams, onSubmit, onClose }: ScheduleFormProps) {
  const { lang, plain } = useI18n();
  const titleId = useId();
  const timeId = useId();
  const [title, setTitle] = useState('');
  const [channelId, setChannelId] = useState(teams[0]?.id ?? '');
  const [date, setDate] = useState<string | null>(null);
  const [time, setTime] = useState('');
  const [duration, setDuration] = useState(30);
  const [orgWide, setOrgWide] = useState(false);
  const [invalid, setInvalid] = useState(false);
  const [failed, setFailed] = useState(false);
  const [sending, setSending] = useState(false);

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (sending) {
      return;
    }
    const trimmed = title.trim();
    const startAt = date !== null && time !== '' ? new Date(`${date}T${time}`) : null;
    if (trimmed === '' || channelId === '' || startAt === null || Number.isNaN(startAt.getTime()) || startAt.getTime() <= Date.now()) {
      setInvalid(true);
      return;
    }
    setInvalid(false);
    setFailed(false);
    setSending(true);
    try {
      await onSubmit({ channelId, title: trimmed, startAt, durationMinutes: duration, orgWide });
    } catch {
      setFailed(true);
      setSending(false);
    }
  }

  return (
    <form id={id} className="cal-form" onSubmit={submit}>
      <div className="cal-fld">
        <label className="kiwi-label" htmlFor={titleId}>
          <T k="scheduleTitleLabel" />
        </label>
        <input
          id={titleId}
          className="cal-input"
          value={title}
          placeholder={plain('scheduleTitlePlaceholder')}
          onChange={(event) => {
            setTitle(event.target.value);
          }}
        />
      </div>

      <div className="cal-fld">
        <label className="kiwi-label" htmlFor={`${id}-team`}>
          <T k="scheduleTeamLabel" />
        </label>
        <NativeSelect
          id={`${id}-team`}
          value={channelId}
          options={teams.map((team) => ({ value: team.id, label: team.name }))}
          onChange={setChannelId}
        />
      </div>

      <div className="cal-row">
        <DateField label={plain('scheduleDateLabel')} lang={lang} value={date} onChange={setDate} />
        <div className="cal-fld">
          <label className="kiwi-label" htmlFor={timeId}>
            <T k="scheduleTimeLabel" />
          </label>
          <input
            id={timeId}
            type="time"
            className="cal-input"
            value={time}
            onChange={(event) => {
              setTime(event.target.value);
            }}
          />
        </div>
        <div className="cal-fld">
          <label className="kiwi-label" htmlFor={`${id}-dur`}>
            <T k="scheduleDurationLabel" />
          </label>
          <NativeSelect
            id={`${id}-dur`}
            value={String(duration)}
            options={DURATIONS.map((minutes) => ({ value: String(minutes), label: plain('scheduleDurationUnit', { count: minutes }) }))}
            onChange={(value) => {
              setDuration(Number(value));
            }}
          />
        </div>
      </div>

      <label className="cal-check">
        <input
          type="checkbox"
          checked={orgWide}
          onChange={(event) => {
            setOrgWide(event.target.checked);
          }}
        />
        <T k="scheduleOrgWide" />
      </label>

      {invalid && (
        <p className="cal-err" role="alert">
          <T k="scheduleInvalid" />
        </p>
      )}
      {failed && (
        <p className="cal-err" role="alert">
          <T k="scheduleFailed" />
        </p>
      )}

      <div className="cal-form-acts">
        <Button type="button" onClick={onClose}>
          <T k="scheduleClose" />
        </Button>
        <Button type="submit" variant="primary" disabled={sending}>
          <T k={sending ? 'scheduleSubmitting' : 'scheduleSubmit'} />
        </Button>
      </div>
    </form>
  );
}
