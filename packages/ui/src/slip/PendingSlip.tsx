import { Pending } from './Pending';
import { Slip, SlipBody } from './Slip';

/** The pending row inside a slip: a calm wait that has no perforation and no REF, since nothing is issued yet. */
export function PendingSlip({ label }: { label: string }) {
  return (
    <Slip>
      <SlipBody>
        <Pending label={label} />
      </SlipBody>
    </Slip>
  );
}
