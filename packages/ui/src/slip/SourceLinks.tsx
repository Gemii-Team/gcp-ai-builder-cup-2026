import type { ReactNode } from 'react';
import { cx } from '../cx';
import { formatClock, formatOffset } from '../format';
import { useLabels } from '../labels';
import { LangRuns } from '../LangRuns';
import { useExcerptLink, type Source } from './ExcerptContext';

type LinkProps = {
  source: Source;
  /** Replaces opening the excerpt in place: Ask uses it to select the matching source slip instead. */
  onPress?: (source: Source) => void;
};

type SourceButtonProps = LinkProps & {
  name: string;
  className?: string;
  children: ReactNode;
};

/** A link-button that opens its source's excerpt through the provider and reports `aria-expanded` and `aria-controls`. */
function SourceButton({ source, onPress, name, className, children }: SourceButtonProps) {
  const link = useExcerptLink(source, onPress !== undefined);
  return (
    <>
      <button
        type="button"
        aria-label={name}
        {...(onPress === undefined ? { 'aria-expanded': link.expanded, 'aria-controls': link.controls } : {})}
        className={cx('kiwi-ts', className)}
        onClick={(event) => {
          if (onPress === undefined) {
            link.toggle(event.currentTarget);
          } else {
            onPress(source);
          }
        }}
      >
        {children}
      </button>
      {link.panel}
    </>
  );
}

const MONO = 'font-mono text-numeral whitespace-nowrap';

/**
 * A transcript offset: `h:mm:ss`, mono, Citation Teal. It always points into a recording, so it is a control that opens
 * the moment in place. A missing offset renders nothing: never a false `0:00:00`.
 */
export function OffsetLink({ source, onPress }: LinkProps) {
  const labels = useLabels();
  const offset = formatOffset(source.offsetMs);
  if (offset === null) {
    return null;
  }
  return (
    <SourceButton source={source} onPress={onPress} name={labels.text('transcriptAt', { time: offset })} className={MONO}>
      {offset}
    </SourceButton>
  );
}

/** A page of a document, `p. 4`, that opens the quoted passage in place. A source with no page renders nothing. */
export function PageLink({ source, onPress }: LinkProps) {
  const labels = useLabels();
  if (source.page === undefined) {
    return null;
  }
  const page = String(source.page);
  return (
    <SourceButton source={source} onPress={onPress} name={labels.text('pageOf', { file: source.label, page })} className={MONO}>
      {labels.node('pageShort', { page })}
    </SourceButton>
  );
}

/** The channel a message was posted in, in Anuphan, that opens the message in place. `channel` is its name without `#`. */
export function MessageLink({ source, channel, onPress }: LinkProps & { channel: string }) {
  const labels = useLabels();
  const name =
    source.at === undefined
      ? labels.text('messageInUntimed', { channel })
      : labels.text('messageIn', { channel, time: formatClock(source.at) });
  return (
    <SourceButton source={source} onPress={onPress} name={name}>
      <LangRuns text={`#${channel}`} />
    </SourceButton>
  );
}
