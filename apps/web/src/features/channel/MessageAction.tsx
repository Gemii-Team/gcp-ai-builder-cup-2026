import { Icon } from '@kiwi/ui';
import { T, useI18n } from '../../i18n';

type MessageActionProps = {
  authorName: string;
  /** The message's wall-clock time, `14:02`. */
  time: string;
  expanded: boolean;
  /** The id of the form the action opens. */
  controls: string;
  onPress: () => void;
};

/**
 * Make a task, at the top right of a human message, left of its time: the words with the task-add icon (the icon alone on
 * phones). Its name says whose message it acts on, and it reports whether its form is open.
 */
export function MessageAction({ authorName, time, expanded, controls, onPress }: MessageActionProps) {
  const { plain } = useI18n();
  return (
    <button
      type="button"
      className="mact"
      title={plain('makeTask')}
      aria-label={plain('makeTaskNamed', { author: authorName, time })}
      aria-expanded={expanded}
      aria-controls={expanded ? controls : undefined}
      onClick={onPress}
    >
      <Icon name="task-add" />
      <span className="lbl">
        <T k="makeTask" />
      </span>
    </button>
  );
}
