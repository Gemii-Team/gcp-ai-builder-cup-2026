import { useLabels } from '../labels';
import { Mascot } from '../marks';

/**
 * Nothing waits for a person: the kiwi beside a sealed slip, at 176px in the channel's panel and 132px in the Board's
 * band, where it is a row. It takes focus when the last draft is confirmed, so it can be focused but is not a tab stop.
 */
export function DraftsEmpty({ variant }: { variant: 'panel' | 'band' }) {
  const labels = useLabels();
  return (
    <div className="kiwi-empty" data-drafts-empty="" tabIndex={-1}>
      <Mascot variant="sealed" size={variant === 'band' ? 132 : 176} />
      <div>
        <h3>{labels.node('noDraftsWaiting')}</h3>
        <p>{labels.node('draftsEmptyBody')}</p>
      </div>
    </div>
  );
}
