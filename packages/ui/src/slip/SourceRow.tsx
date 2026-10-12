import { formatClock } from '../format';
import { LangRuns } from '../LangRuns';
import { Numeral } from '../Numeral';
import { MARKER } from './CitationMarker';
import { ExcerptRow, type Source } from './ExcerptContext';
import { OffsetLink, PageLink } from './SourceLinks';

type SourceRowProps = {
  /** The citation number; a summary slip's sources are not numbered. */
  n?: number;
  source: Source;
  /** The matching citation is pressed: the row takes Citation Wash. */
  hit?: boolean;
  /** Replaces opening the excerpt in place when the offset or page is pressed. */
  onPress?: (source: Source) => void;
};

function Where({ source, onPress }: Pick<SourceRowProps, 'source' | 'onPress'>) {
  switch (source.kind) {
    case 'meeting':
      return <OffsetLink source={source} onPress={onPress} />;
    case 'file':
      return <PageLink source={source} onPress={onPress} />;
    case 'message':
      // A message's time is a wall clock, not a position in a recording: plain text, never a link.
      return source.at === undefined ? null : <Numeral>{formatClock(source.at)}</Numeral>;
  }
}

/** One source of a stub: its marker and name at the left, and at the right the offset, page or message time. */
export function SourceRow({ n, source, hit = false, onPress }: SourceRowProps) {
  return (
    <ExcerptRow as="li" className="kiwi-src" data-hit={hit || undefined}>
      <span>
        {n !== undefined && (
          <>
            <span lang="en" className={`${MARKER} bg-citation-wash text-citation-teal`}>{`[${n}]`}</span>{' '}
          </>
        )}
        <LangRuns text={source.label} />
      </span>
      <span>
        <Where source={source} onPress={onPress} />
      </span>
    </ExcerptRow>
  );
}
