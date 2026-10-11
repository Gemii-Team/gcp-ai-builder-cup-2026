import { Icon } from '../icons';
import { useLabels } from '../labels';

type DraftsBarProps = {
  /** How many drafts wait for a person. */
  count: number;
  /** Whether the sheet this bar opens is open. */
  expanded: boolean;
  /** The id of that sheet. */
  controls: string;
  onPress: () => void;
};

/**
 * The phone's 44px drafts bar above the composer (40px on the narrowest phones): a dashed amber bar while drafts wait,
 * a plain one when none do. Pressing it opens the drafts sheet; from 1024px the panel is on screen and the bar is hidden.
 */
export function DraftsBar({ count, expanded, controls, onPress }: DraftsBarProps) {
  const labels = useLabels();
  return (
    <button
      type="button"
      className="kiwi-draftbar"
      data-zero={count === 0 || undefined}
      aria-haspopup="dialog"
      aria-expanded={expanded}
      aria-controls={controls}
      onClick={onPress}
    >
      <span>{count === 0 ? labels.node('noDraftsWaiting') : labels.node('draftsToConfirm', { count })}</span>
      <Icon name="chevron-up" />
    </button>
  );
}
