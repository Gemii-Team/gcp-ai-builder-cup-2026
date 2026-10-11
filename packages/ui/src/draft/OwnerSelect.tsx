import { Icon } from '../icons';
import { useLabels } from '../labels';
import { langRuns } from '../LangRuns';

/** A person a task can be given to. Mirrors `identity.v1.User`'s id and name with plain strings. */
export type Member = { id: string; name: string };

/**
 * `'th'` when `text` holds Thai, otherwise nothing: the `lang` of a field or an option, which cannot carry the
 * per-run spans that `LangRuns` gives rendered text.
 */
export function thaiLang(text: string): 'th' | undefined {
  return langRuns(text).some((node) => typeof node !== 'string') ? 'th' : undefined;
}

type NativeSelectProps = {
  id?: string;
  'aria-label'?: string;
  value: string;
  options: readonly { value: string; label: string }[];
  /** The option for "nothing chosen", whose value is `''`; without it the select always holds one of `options`. */
  emptyLabel?: string;
  /** The warning look: Draft Amber border, Draft Wash fill, Draft Ink text and a triangle at the left. */
  warn?: boolean;
  onChange: (value: string) => void;
};

/** A native select drawn as the design's 32px control: 44px on phones, a Carbon Copy chevron at the right. */
export function NativeSelect({ id, 'aria-label': ariaLabel, value, options, emptyLabel, warn = false, onChange }: NativeSelectProps) {
  return (
    <span className="kiwi-sel" data-warn={warn || undefined}>
      {warn && <Icon name="warning" className="kiwi-sel-warn" />}
      <select
        id={id}
        aria-label={ariaLabel}
        className="kiwi-sel-ctl"
        data-warn={warn || undefined}
        value={value}
        onChange={(event) => {
          onChange(event.target.value);
        }}
      >
        {emptyLabel !== undefined && <option value="">{emptyLabel}</option>}
        {options.map((option) => (
          <option key={option.value} value={option.value} lang={thaiLang(option.label)}>
            {option.label}
          </option>
        ))}
      </select>
      <Icon name="chevron-down" size={12} className="kiwi-sel-chev" />
    </span>
  );
}

type OwnerSelectProps = {
  id?: string;
  'aria-label'?: string;
  members: readonly Member[];
  /** The owner's id, or `null` for no owner. */
  value: string | null;
  onChange: (ownerId: string | null) => void;
  /**
   * A draft cannot be confirmed without an owner, so there an empty select is a warning ("Choose an owner"); a task a
   * person makes may have none, so there it is an ordinary choice ("No owner").
   */
  warnWhenEmpty?: boolean;
};

/** Who owns a task. With no owner a draft's select takes the amber warning look, and its Confirm stays disabled. */
export function OwnerSelect({ id, 'aria-label': ariaLabel, members, value, onChange, warnWhenEmpty = true }: OwnerSelectProps) {
  const labels = useLabels();
  return (
    <NativeSelect
      id={id}
      aria-label={ariaLabel}
      value={value ?? ''}
      options={members.map((member) => ({ value: member.id, label: member.name }))}
      emptyLabel={labels.text(warnWhenEmpty ? 'chooseOwner' : 'noOwner')}
      warn={warnWhenEmpty && value === null}
      onChange={(next) => {
        onChange(next === '' ? null : next);
      }}
    />
  );
}
