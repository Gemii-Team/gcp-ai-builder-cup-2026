import type { ReactNode } from 'react';
import { formatClock } from '../format';
import { useLabels } from '../labels';
import { LangRuns } from '../LangRuns';
import { Seal } from '../marks';
import { Numeral } from '../Numeral';
import type { Source } from '../slip/ExcerptContext';
import { RefCode } from '../slip/RefCode';
import { Slip } from '../slip/Slip';
import { SourceRow } from '../slip/SourceRow';

/** A task a person has confirmed, as the slip shows it. */
export type SealedTask = {
  id: string;
  /** The REF code without its label: `TSK-2031`. */
  ref: string;
  title: string;
  ownerName?: string;
  dueLabel?: string;
  source: Source;
  confirmedBy: string;
  /** The instant it was confirmed; shown as a wall-clock `hh:mm`. */
  confirmedAt: Date | string;
  /** A line under the confirmer row, such as `Added to To do` in the phone sheet's Confirmed just now list. */
  note?: string;
};

type SealedSlipProps = Omit<SealedTask, 'id'> & {
  /** What sits under the confirmer row: the Board's Move button. */
  actions?: ReactNode;
};

/**
 * A confirmed draft: the stub is torn away, so there is no perforation, notch or border, and the bottom edge is a zigzag.
 * The `torn` wrapper carries the one shadow in the system, which follows that edge.
 */
export function SealedSlip({ ref, title, ownerName, dueLabel, source, confirmedBy, confirmedAt, note, actions }: SealedSlipProps) {
  const labels = useLabels();
  const meta = [ownerName, dueLabel].filter((part) => part !== undefined).join(' · ');
  // A meeting is named by where it leads, not by its title: "From meeting" over the offset. A file keeps its name.
  const where = source.kind === 'meeting' ? { ...source, label: labels.text('fromMeeting') } : source;
  return (
    <div className="torn">
      <Slip className="kiwi-sealed">
        <Seal className="kiwi-seal" />
        <div>
          <h3 className="kiwi-d-title">
            <LangRuns text={title} />
          </h3>
          {meta !== '' && (
            <p className="kiwi-d-meta">
              <LangRuns text={meta} />
            </p>
          )}
          <p className="kiwi-row">
            <RefCode code={ref} />
          </p>
          <ol className="kiwi-srcs">
            <SourceRow source={where} />
          </ol>
          <p className="kiwi-row">
            <span>
              <span className="sr-only">{labels.node('confirmedBy')} </span>
              <LangRuns text={confirmedBy} />
            </span>
            <Numeral>{formatClock(confirmedAt)}</Numeral>
          </p>
          {note !== undefined && (
            <p className="kiwi-row">
              <span>
                <LangRuns text={note} />
              </span>
            </p>
          )}
          {actions}
        </div>
      </Slip>
    </div>
  );
}
