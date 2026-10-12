import type { ReactNode } from 'react';
import { cx } from './cx';

type Option<T extends string> = {
  value: T;
  label: ReactNode;
  /** The option's own language, for a label that names itself in it (`ไทย` is `th`). */
  lang?: string;
  /** A count, set in mono beside the label. */
  count?: number;
};

type SegmentedProps<T extends string> = {
  /** Names the group for assistive tech. */
  label: string;
  value: T;
  onChange: (value: T) => void;
  options: readonly Option<T>[];
  /** `language`: the sign-in language switch. `status`: the Board's three-way phone status control. */
  variant?: 'language' | 'status';
};

const TRACK = {
  language: 'inline-flex gap-1 p-1',
  status: 'grid grid-flow-col auto-cols-fr gap-0.5 p-[3px]',
} as const;

// The language switch is 32 by 56px (44 by 64px on phones) and hovers Citation Wash; the status control is 44px
// tall, hovers Counter Hover and sets its pressed segment at 600.
const SEGMENT = {
  language: 'min-h-8 min-w-14 px-3 text-[14px] max-lg:min-h-11 max-lg:min-w-16',
  status: 'min-h-11 gap-2 px-2 text-button',
} as const;

const HOVER = {
  language: 'hover:bg-citation-wash',
  status: 'hover:bg-counter-hover active:bg-citation-wash',
} as const;

/** A Slip Paper track of toggle buttons: the pressed segment is Ink Teal with Rail White text. */
export function Segmented<T extends string>({ label, value, onChange, options, variant = 'language' }: SegmentedProps<T>) {
  return (
    <div
      role="group"
      aria-label={label}
      className={cx('rounded-lg border border-control-line bg-slip-paper', TRACK[variant])}
    >
      {options.map((option) => {
        const pressed = option.value === value;
        return (
          <button
            key={option.value}
            type="button"
            lang={option.lang}
            aria-pressed={pressed}
            onClick={() => onChange(option.value)}
            className={cx(
              'flex items-center justify-center rounded-md border-0 cursor-pointer transition-colors duration-150',
              SEGMENT[variant],
              pressed ? 'bg-primary text-on-primary' : cx('bg-transparent text-ledger-ink', HOVER[variant]),
              pressed && variant === 'status' ? 'font-semibold' : 'font-medium',
            )}
          >
            {option.label}
            {option.count !== undefined && (
              <>
                {' '}
                <span className="font-mono text-numeral">{option.count}</span>
              </>
            )}
          </button>
        );
      })}
    </div>
  );
}
