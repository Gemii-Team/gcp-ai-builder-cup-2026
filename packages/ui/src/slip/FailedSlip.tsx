import { Button } from '../Button';
import { formatClock } from '../format';
import { Icon } from '../icons';
import { LangRuns } from '../LangRuns';
import { Numeral } from '../Numeral';
import { Perforation } from './Perforation';
import { Slip, SlipBody } from './Slip';
import { Stub, StubRow } from './Stub';

type FailedSlipProps = {
  /** What failed and why, in a plain sentence. */
  reason: string;
  failedAt: Date | string;
  onRetry: () => void;
};

/**
 * An attempt that did not complete: the Returned family, its perforation kept, Retry and the time of the failure.
 * It carries no REF, since no output was issued.
 */
export function FailedSlip({ reason, failedAt, onRetry }: FailedSlipProps) {
  return (
    <Slip tone="failed">
      <SlipBody>
        <p className="kiwi-fmsg">
          <Icon name="warning" />
          <span>
            <LangRuns text={reason} />
          </span>
        </p>
      </SlipBody>
      <Perforation />
      <Stub>
        <StubRow>
          <Button size="sm" onClick={onRetry}>
            Retry
          </Button>
          <span>
            Failed <Numeral>{formatClock(failedAt)}</Numeral>
          </span>
        </StubRow>
      </Stub>
    </Slip>
  );
}
