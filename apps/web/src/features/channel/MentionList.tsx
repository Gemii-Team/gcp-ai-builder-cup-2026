import { LangRuns } from '@kiwi/ui';
import { T, useI18n } from '../../i18n';
import { initialOf } from '../../lib/text';
import { mentionOptionId, type MentionTarget } from './mentions';

type MentionListProps = {
  /** The id of the list, which the composer's input names in `aria-controls`. */
  id: string;
  targets: readonly MentionTarget[];
  active: number;
  onPick: (index: number) => void;
};

/**
 * The listbox that opens above the composer when the person types `@`: the assistant first and in bold, with what it is,
 * then the members. A press picks without taking focus from the input.
 */
export function MentionList({ id, targets, active, onPick }: MentionListProps) {
  const { plain } = useI18n();
  return (
    <ul id={id} className="mention" role="listbox" aria-label={plain('mentionLabel')}>
      {targets.map((target, index) => (
        <li
          key={`${target.kind}:${target.id}`}
          id={mentionOptionId(id, index)}
          role="option"
          aria-selected={index === active}
          onMouseDown={(event) => {
            event.preventDefault();
            onPick(index);
          }}
        >
          {target.kind === 'agent' ? (
            <>
              <b lang="en">@{target.name}</b>
              <small>
                <T k="assistantRole" />
              </small>
            </>
          ) : (
            <>
              <span className="av" aria-hidden="true">
                {initialOf(target.name)}
              </span>
              <b>
                <LangRuns text={target.name} />
              </b>
            </>
          )}
        </li>
      ))}
    </ul>
  );
}
