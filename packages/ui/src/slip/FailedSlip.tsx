import type { ReactNode } from 'react';
import { Button } from '../Button';
import { formatClock } from '../format';
import { Icon } from '../icons';
import { useLabels } from '../labels';
import { LangRuns } from '../LangRuns';
import { Numeral } from '../Numeral';
import { Perforation } from './Perforation';
import { Slip, SlipBody } from './Slip';
import { Stub, StubRow } from './Stub';

type FailedSlipProps = {
  /**
   * What failed and why, in a plain sentence. A string has its Thai runs tagged `lang="th"`; a node is shown as given,
   * for a sentence whose Latin words inside Thai prose the app has already tagged `lang="en"`.
   */
  reason: ReactNode;
  failedAt: Date | string;
  onRetry: () => void;
};

/**
 * An attempt that did not complete: the Returned family, its perforation kept, Retry and the time of the failure.
 * It carries no REF, since no output was issued.
 */
export function FailedSlip({ reason, failedAt, onRetry }: FailedSlipProps) {
  const labels = useLabels();
  return (
    <Slip tone="failed">
      <SlipBody>
        <p className="kiwi-fmsg">
          <Icon name="warning" />
          <span>
            {typeof reason === 'string' ? <LangRuns text={reason} /> : reason}
          </span>
        </p>
      </SlipBody>
      <Perforation />
      <Stub>
        <StubRow>
          <Button size="sm" onClick={onRetry}>
            {labels.node('retry')}
          </Button>
          <span>{labels.node('failed', { time: <Numeral>{formatClock(failedAt)}</Numeral> })}</span>
        </StubRow>
      </Stub>
    </Slip>
  );
}
