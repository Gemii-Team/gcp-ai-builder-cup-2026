import { Button, formatClock, Icon, LangRuns, Numeral, Perforation, RefCode, Slip, SlipBody, Stub, StubRow } from '@kiwi/ui';
import type { ArtifactView } from '../../api/types';
import { T, TCount, TSlots, useI18n } from '../../i18n';
import { AppButtonLink } from '../shell/AppLink';

type NotFoundSlipProps = {
  answer: ArtifactView;
  channelNames: ReadonlyMap<string, string>;
  /** Puts the question back in the field. */
  onEdit: () => void;
  /** Where the team is asked: the channel the question was scoped to, or the start page, which opens the channel last read. */
  teamHref: string;
};

/**
 * "Not found in channels you can access" is a completed answer, never a failure: Slip Paper and Ledger Ink, a search icon
 * crossed by a bar, its perforation kept, a REF, and its proof of scope (the channels that were searched) where sources
 * would be. It offers what to do next.
 */
export function NotFoundSlip({ answer, channelNames, onEdit, teamHref }: NotFoundSlipProps) {
  const { plain } = useI18n();
  return (
    <Slip className="ak-nf">
      <SlipBody>
        <p className="ak-nf-msg">
          <Icon name="search-none" />
          <span>
            <b>
              <T k="askNotFoundTitle" />
            </b>
            <br />
            <span className="ak-nf-why">
              <TCount k="askNotFoundWhy" count={answer.scopeChannelIds.length} />
            </span>
          </span>
        </p>
        <div className="ak-next">
          <h3>
            <T k="askNextHeading" />
          </h3>
          <ul>
            <li>
              <T k="askNextRephrase" />
            </li>
            <li>
              <T k="askNextAsk" />
            </li>
            <li>
              <T k="askNextWait" />
            </li>
          </ul>
        </div>
      </SlipBody>
      <Perforation />
      <Stub>
        <StubRow>
          <RefCode code={answer.ref} />
          <span>
            <TSlots k="askSearched" slots={{ time: <Numeral>{formatClock(answer.generatedAt)}</Numeral> }} />
          </span>
        </StubRow>
        <div className="ak-scoped">
          <span>
            <T k="askSearchedOnly" />
          </span>
          <ul className="ak-chans-l" aria-label={plain('askScopeList')}>
            {answer.scopeChannelIds.map((id) => {
              const name = channelNames.get(id);
              return (
                <li key={id} className="ak-chip">
                  {name === undefined ? <T k="askChannelGone" /> : <># <LangRuns text={name} /></>}
                </li>
              );
            })}
          </ul>
        </div>
        <div className="kiwi-acts">
          <Button size="sm" onClick={onEdit}>
            <T k="askEditQuestion" />
          </Button>
          <AppButtonLink size="sm" to={teamHref}>
            <T k="askAskTeam" />
          </AppButtonLink>
        </div>
      </Stub>
    </Slip>
  );
}
