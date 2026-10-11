import { cx } from '../cx';
import { useLabels } from '../labels';
import { LangRuns } from '../LangRuns';
import { ExcerptRow } from './ExcerptContext';
import { OffsetLink } from './SourceLinks';

/** A decision or an open question of a meeting, with where in the recording it was said (null without timing). */
export type Track = {
  kind: 'decision' | 'open_question';
  text: string;
  offsetMs: number | null;
  entryIdx: number | null;
};

type TracksProps = {
  tracks: readonly Track[];
  /** The meeting the offsets point into. */
  meetingId: string;
  title: string;
};

/**
 * Tracks as the `104px | 1fr | auto` grid: label, text, right-aligned offset. A label that repeats the one above it
 * is visually hidden on wide screens (still read by assistive tech) and shows again where rows reflow on phones.
 */
export function Tracks({ tracks, meetingId, title }: TracksProps) {
  const labels = useLabels();
  return (
    <ul className="kiwi-tracks">
      {tracks.map((track, index) => (
        <ExcerptRow key={index} as="li" className="kiwi-track" data-kind={track.kind}>
          <span className="kiwi-track-label">
            <span className={cx(tracks[index - 1]?.kind === track.kind && 'kiwi-dup')}>{labels.node(track.kind === 'decision' ? 'decision' : 'openQuestion')}</span>
          </span>
          <span className="kiwi-track-text">
            <LangRuns text={track.text} />
          </span>
          <span className="kiwi-track-offset">
            <OffsetLink
              source={{
                kind: 'meeting',
                id: meetingId,
                label: title,
                channelId: '',
                offsetMs: track.offsetMs,
                entryIdx: track.entryIdx,
              }}
            />
          </span>
        </ExcerptRow>
      ))}
    </ul>
  );
}
