import type { MouseEventHandler, ReactNode, Ref } from 'react';
import { useI18n } from '../../i18n';

type ThreadProps = {
  /** What stands above the exchanges: on phones, the sample questions. */
  lead?: ReactNode;
  /** The exchanges, newest first. */
  children: ReactNode;
  /** Hears every press in the thread before the control pressed does: the screen learns which element asked to open the sheet. */
  onClickCapture?: MouseEventHandler<HTMLDivElement>;
  ref?: Ref<HTMLDivElement>;
};

/** The thread: a scrolling region of questions and answers, newest first, directly under the command band. */
export function Thread({ lead, children, onClickCapture, ref }: ThreadProps) {
  const { plain } = useI18n();
  return (
    <div ref={ref} className="ak-thread" role="region" aria-label={plain('askThread')} tabIndex={0} onClickCapture={onClickCapture}>
      {lead}
      <div className="ak-thread-in" role="log" aria-label={plain('askLog')}>
        {children}
      </div>
    </div>
  );
}
