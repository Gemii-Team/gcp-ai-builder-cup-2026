import { Icon, LangRuns, Numeral, Slip, SlipBody, useLabels } from '@kiwi/ui';
import type { TaskView } from '../../api/types';
import { TSlots } from '../../i18n';
import { AppLink } from '../shell/AppLink';

/**
 * What the handoff slip becomes once someone has confirmed its draft (on the Board, in a channel's drafts panel, in another
 * tab): a plain task card, since it is no longer waiting on a person. It has no Draft tag, no perforation and no Confirm or
 * Discard; its line, with an Ink Teal check (never Seal Green: the seal is the Board's), says where the task is.
 */
export function OnBoardSlip({ task }: { task: TaskView }) {
  const labels = useLabels();
  return (
    <Slip role="group" aria-label={labels.text('taskNamed', { code: task.ref })} className="ak-onboard">
      <SlipBody>
        <h3 className="kiwi-d-title">
          <LangRuns text={task.title} />
        </h3>
        <p className="kiwi-made-line">
          <Icon name="check" className="text-primary" />
          <span>
            <TSlots k="askOnBoard" slots={{ code: <Numeral lang="en">{task.ref}</Numeral> }} /> ·{' '}
            <AppLink to="/board" className="kiwi-ts">
              {labels.node('reviewOnBoard')}
            </AppLink>
          </span>
        </p>
      </SlipBody>
    </Slip>
  );
}
