import { cx } from '../cx';

/** The Citation role: mono 12px, Citation Teal on Citation Wash, 4px corners. A marker is a Latin token, so `lang="en"`. */
export const MARKER = 'inline-block rounded-xs px-1 font-mono text-citation';

type CitationMarkerProps = {
  n: number;
  pressed: boolean;
  onPress: () => void;
  /** The source it points at, such as `ACME-contract-v3.pdf, p. 4`: the marker is named `Source 1: <label>`. */
  label?: string;
};

/** An inline `[1]` button inside an answer. Pressing it marks the matching source row; pressing again clears it. */
export function CitationMarker({ n, pressed, onPress, label }: CitationMarkerProps) {
  return (
    <button
      type="button"
      lang="en"
      aria-pressed={pressed}
      aria-label={label === undefined ? `Source ${n}` : `Source ${n}: ${label}`}
      onClick={onPress}
      className={cx(
        MARKER,
        'mx-0.5 cursor-pointer border-0 align-[1px] transition-colors duration-150',
        pressed
          ? 'bg-citation-teal text-on-primary'
          : 'bg-citation-wash text-citation-teal hover:bg-citation-hover active:bg-citation-press',
      )}
    >
      {`[${n}]`}
    </button>
  );
}
