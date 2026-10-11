import { Icon } from '../icons';
import { Numeral } from '../Numeral';
import { PendingBar } from './Pending';

type PendingStepsProps = {
  phase: 'retrieving' | 'writing';
  channelCount: number;
};

/**
 * Ask's two calm steps, with no spinner. The current step is Ledger Ink at 600 with a clock icon and the bar; a
 * finished step shows a check and the past tense; a step still to come shows an empty circle.
 */
export function PendingSteps({ phase, channelCount }: PendingStepsProps) {
  const retrieving = phase === 'retrieving';
  const channels = channelCount === 1 ? 'channel' : 'channels';
  return (
    <ol role="status" aria-label="Answer progress" className="kiwi-steps">
      <li aria-current={retrieving ? 'step' : undefined}>
        <Icon name={retrieving ? 'clock' : 'check'} />
        <span>
          {retrieving ? 'Retrieving' : 'Retrieved'} from <Numeral>{channelCount}</Numeral> {channels}
          {retrieving && '…'}
        </span>
        {retrieving && <PendingBar />}
      </li>
      <li aria-current={retrieving ? undefined : 'step'}>
        <Icon name={retrieving ? 'circle' : 'clock'} />
        <span>{retrieving ? 'Writing the answer' : 'Writing the answer…'}</span>
        {!retrieving && <PendingBar />}
      </li>
    </ol>
  );
}
