import type { ReactNode } from 'react';
import { cx } from '../cx';
import { Icon } from '../icons';
import { useLabels } from '../labels';
import { LangRuns } from '../LangRuns';
import { Numeral } from '../Numeral';
import { usePrefersReducedMotion } from '../slip/Pending';
import { RefCode } from '../slip/RefCode';
import { Slip, SlipBody } from '../slip/Slip';
import { Stub, StubRow } from '../slip/Stub';

export type CreatedTaskCardProps = {
  /** The REF code without its label: `TSK-2035`. */
  ref: string;
  title: string;
  /** Without an owner the meta line reads `No owner`, which is neutral: a person's own task need not have one. */
  ownerName?: string;
  /** Without a due date the meta line reads `No due date`. */
  dueLabel?: string;
  /** Where the task came from: `From #q4-launch · Arthit K. · 14:02` or `Added by Pimchanok S. · 14:53`. */
  sourceLabel: ReactNode;
  boardHref: string;
};

type CardProps = Omit<CreatedTaskCardProps, 'ref'> & { code: string };

/**
 * The card a task leaves behind once a person has made it: confirmed at once, so a plain slip with no Draft tag,
 * perforation, seal or shadow, and a line with an Ink Teal check. It fades in over 320ms, and takes focus when the app
 * focuses it (`tabIndex={-1}`).
 */
export function CreatedTaskCard({ ref, ...rest }: CreatedTaskCardProps) {
  // React treats a prop named `ref` as a ref, and the hooks lint refuses to see one passed to a function such as the
  // label formatter: the card below takes the REF code as an ordinary `code`.
  return <Card code={ref} {...rest} />;
}

function Card({ code, title, ownerName, dueLabel, sourceLabel, boardHref }: CardProps) {
  const labels = useLabels();
  const reduced = usePrefersReducedMotion();
  return (
    <Slip role="group" tabIndex={-1} aria-label={labels.text('taskNamed', { code })} className={cx('kiwi-made', !reduced && 'kiwi-fade')}>
      <SlipBody>
        <h3 className="kiwi-d-title">
          <LangRuns text={title} />
        </h3>
        <p className="kiwi-d-meta">
          {ownerName === undefined ? labels.node('noOwner') : <LangRuns text={ownerName} />} ·{' '}
          {dueLabel === undefined ? labels.node('noDueDate') : <LangRuns text={dueLabel} />}
        </p>
      </SlipBody>
      <Stub className="kiwi-made-stub">
        <StubRow>
          <RefCode code={code} />
          <span className="kiwi-made-src">{typeof sourceLabel === 'string' ? <LangRuns text={sourceLabel} /> : sourceLabel}</span>
        </StubRow>
        <p className="kiwi-made-line">
          <Icon name="check" className="text-primary" />
          <span>
            {labels.node('taskCreated', {
              code: <Numeral lang="en">{code}</Numeral>,
              link: (
                <a className="kiwi-ts" href={boardHref}>
                  {labels.node('viewOnBoard')}
                </a>
              ),
            })}
          </span>
        </p>
      </Stub>
    </Slip>
  );
}
