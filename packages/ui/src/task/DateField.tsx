import { parseDate, toCalendarDate } from '@internationalized/date';
import { useContext, useEffect, useEffectEvent, useState } from 'react';
import {
  DateFieldStateContext,
  DateField as AriaDateField,
  DateInput,
  DateSegment,
  I18nProvider,
  Label,
  type DateValue,
} from 'react-aria-components';
import type { UiLang } from '../labels';
import { LangRuns } from '../LangRuns';

/** The date's order and calendar follow the interface language, never the browser's: day first, Gregorian, Latin digits. */
const LOCALE: Record<UiLang, string> = { en: 'en-GB', th: 'th-TH-u-ca-gregory-nu-latn' };

type DateFieldProps = {
  label: string;
  lang: UiLang;
  /** `YYYY-MM-DD`, or `null` for no date. */
  value: string | null;
  /** The complete date as `YYYY-MM-DD`, or `null` while the field is empty or only partly filled. */
  onChange: (value: string | null) => void;
};

/** A year typed as `26` is year 26 to the field, which would reach the server as a date two thousand years ago. */
const FIRST_FULL_YEAR = 1000;

/** The value to report for what the field holds: `null` unless it is a whole date with a four-digit year. */
function reported(date: DateValue | null): string | null {
  return date !== null && date.year >= FIRST_FULL_YEAR ? toCalendarDate(date).toString() : null;
}

/**
 * Reports a partly filled field as `null`. The field itself keeps the last whole date while a segment is being retyped,
 * so it says nothing when a segment is cleared; this watches for that and for the date becoming whole again.
 */
function Completion({ settle }: { settle: (incomplete: boolean, date: DateValue | null) => void }) {
  const state = useContext(DateFieldStateContext);
  const incomplete = state?.segments.some((segment) => segment.isEditable && segment.isPlaceholder) ?? true;
  const date = state?.value ?? null;
  const notify = useEffectEvent(() => {
    settle(incomplete, date);
  });
  useEffect(() => {
    notify();
  }, [incomplete]);
  return null;
}

/**
 * The due date: day, month and year as separate segments, 32px (44px on phones). The order and calendar follow `lang`,
 * not the browser's locale. A date that is not whole reports `null`, and the segments the person has typed stay put.
 */
export function DateField({ label, lang, value, onChange }: DateFieldProps) {
  // What the field shows. It is kept apart from `value`: handing it `null` for a partly typed date would wipe the segments.
  const [shown, setShown] = useState<DateValue | null>(() => (value === null ? null : parseDate(value)));
  const [seen, setSeen] = useState(value);
  if (value !== seen) {
    // A date set from outside replaces the one shown. `null` is not taken from outside: it is what a partly filled field reports.
    setSeen(value);
    if (value !== null && value !== shown?.toString()) {
      setShown(parseDate(value));
    }
  }

  return (
    <I18nProvider locale={LOCALE[lang]}>
      <AriaDateField
        className="kiwi-fld"
        granularity="day"
        value={shown}
        onChange={(date) => {
          setShown(date);
          const next = reported(date);
          if (next !== value) {
            onChange(next);
          }
        }}
      >
        <Label className="kiwi-label">
          <LangRuns text={label} />
        </Label>
        <DateInput className="kiwi-date">{(segment) => <DateSegment segment={segment} className="kiwi-date-seg" />}</DateInput>
        <Completion
          settle={(incomplete, date) => {
            const next = incomplete ? null : reported(date);
            if (next !== value) {
              onChange(next);
            }
          }}
        />
      </AriaDateField>
    </I18nProvider>
  );
}
