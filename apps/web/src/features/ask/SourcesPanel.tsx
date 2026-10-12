import * as Dialog from '@radix-ui/react-dialog';
import { Icon, IconButton, LangRuns, type Excerpt, type Source } from '@kiwi/ui';
import { useId } from 'react';
import type { ArtifactView } from '../../api/types';
import { T, TCount, useI18n } from '../../i18n';
import { SourceSlip } from './SourceSlip';

type SourcesPanelProps = {
  /** The answer whose sources are on show, with the question it answers; none until a question has been answered. */
  shown: { answer: ArtifactView; question: string } | null;
  channelNames: ReadonlyMap<string, string>;
  /** The citation of the answer on show that is pressed: its source slip takes Citation Wash. */
  hit: number | null;
  load: (source: Source) => Promise<Excerpt>;
  /** In the phone's sheet the heading is the dialog's title and the panel has a close button; on a wide screen it is a plain column. */
  sheet: boolean;
};

/**
 * The Sources panel: the heading `Sources` with `N cited`, the answer's REF and its question in two lines, then one source
 * slip per citation. Selecting an answer swaps it to that answer's sources. It is the right column of a wide screen
 * and the bottom sheet of a phone.
 */
export function SourcesPanel({ shown, channelNames, hit, load, sheet }: SourcesPanelProps) {
  const { plain } = useI18n();
  const headingId = useId();
  const cited = shown === null ? [] : [...shown.answer.citations].sort((a, b) => a.n - b.n);
  const heading = (
    <h2 {...(sheet ? {} : { id: headingId })} className="ak-ph-t">
      <T k="askSourcesHeading" />
    </h2>
  );
  const content = (
    <>
      <div className="ak-ph">
        {sheet ? <Dialog.Title asChild>{heading}</Dialog.Title> : heading}
        {shown !== null && (
          <span className="ak-pc">
            <TCount k="askCited" count={cited.length} />
          </span>
        )}
        {sheet && (
          <Dialog.Close asChild>
            <IconButton label={plain('close')} className="ak-pclose">
              <Icon name="close" />
            </IconButton>
          </Dialog.Close>
        )}
      </div>
      <div className="ak-pbody">
        {shown === null ? (
          <p className="ak-p-sub">
            <T k="askSourcesEmpty" />
          </p>
        ) : (
          <>
            <p className="ak-p-sub">
              <span lang="en" className="ak-ref">
                {shown.answer.ref}
              </span>{' '}
              <LangRuns text={shown.question} />
            </p>
            <ul className="ak-slips" aria-label={plain('askSourcesList')}>
              {cited.map((citation) => (
                <SourceSlip key={citation.n} citation={citation} channelNames={channelNames} hit={hit === citation.n} load={load} />
              ))}
            </ul>
          </>
        )}
      </div>
    </>
  );
  return sheet ? (
    <div className="ak-panel" data-sheet="">
      {content}
    </div>
  ) : (
    <aside className="ak-panel" aria-labelledby={headingId}>
      {content}
    </aside>
  );
}
