import { FailedSlip, formatClock, LangRuns, MarkSmall, Numeral, PendingSteps } from '@kiwi/ui';
import { useId } from 'react';
import type { ArtifactView } from '../../api/types';
import { T, useI18n } from '../../i18n';
import { failureReasonKey } from '../../lib/failure';
import { initialOf } from '../../lib/text';
import { ArtifactSlip } from '../channel/ArtifactSlip';
import { AnswerBlock } from './AnswerBlock';
import { NotFoundSlip } from './NotFoundSlip';
import type { AskExchange } from './useAskRun';

type ExchangeProps = {
  exchange: AskExchange;
  /** Who asked: the signed-in person's name. */
  asker: string;
  channelNames: ReadonlyMap<string, string>;
  /** How many channels this question searches, which the pending steps say. */
  channelCount: number;
  /** The citation of this answer whose marker is pressed. */
  pressed: number | null;
  /** The sources of this answer are the ones on show. */
  selected: boolean;
  onPress: (n: number) => void;
  /** Puts this answer's sources on show. */
  onSelect: () => void;
  /** The Sources button: puts this answer's sources on show, and on a phone opens the sheet. */
  onSources: () => void;
  onRetry: () => void;
  /** Puts the question back in the field. */
  onEdit: () => void;
  /** Says something in the screen's live region. */
  say: (text: string) => void;
};

/** The label of the assistant's row: that it is a channel summary, which language the answer is in (any Thai character makes it Thai), or that nothing was found. */
function kindKey(answer: ArtifactView): 'askKindNotFound' | 'kindChannelSummary' | 'askKindThai' | 'askKindEnglish' {
  if (answer.kind === 'not_found') {
    return 'askKindNotFound';
  }
  if (answer.kind === 'channel_summary') {
    return 'kindChannelSummary';
  }
  return /[\u0E00-\u0E7F]/.test(answer.bodyMarkdown) ? 'askKindThai' : 'askKindEnglish';
}

/**
 * One question of the thread, as an article: the asker's row, the question as a Title line, the assistant's row (the small
 * mark, `@assistant`, a Carbon Copy kind label and the time) and under it what came of the question: two calm pending steps,
 * an answer slip, a not-found slip (an answer, not a failure), the channel summary slip of a catch-up (the one the channel
 * shows) or a failed slip with Retry.
 */
export function Exchange({ exchange, asker, channelNames, channelCount, pressed, selected, onPress, onSelect, onSources, onRetry, onEdit, say }: ExchangeProps) {
  const { t } = useI18n();
  const questionId = useId();
  const { outcome } = exchange;
  const answer = outcome.kind === 'answered' ? outcome.answer : null;
  const kind = answer === null ? null : kindKey(answer);
  const settledAt = answer?.generatedAt ?? (outcome.kind === 'failed' ? outcome.at : null);

  return (
    <article
      className="ak-qa"
      data-exchange={exchange.id}
      data-selected={selected || undefined}
      aria-labelledby={questionId}
      onClick={(event) => {
        // Pressing the answer itself, anywhere but on a control, puts its sources on show.
        if (answer?.kind === 'answer' && event.target instanceof Element && event.target.closest('a, button, select, input') === null) {
          onSelect();
        }
      }}
    >
      <div className="ak-row">
        <span className="av" aria-hidden="true">
          {initialOf(asker)}
        </span>
        <b className="ak-who">
          <LangRuns text={asker} />
        </b>
        <time className="ak-time" dateTime={exchange.askedAt}>
          <Numeral>{formatClock(exchange.askedAt)}</Numeral>
        </time>
      </div>
      <h2 id={questionId} className="ak-q ak-body">
        <LangRuns text={exchange.question} />
      </h2>
      <div className="ak-row">
        <MarkSmall size={24} />
        <b className="ak-who" lang="en">
          @assistant
        </b>
        {kind !== null && (
          <span className="ak-kind">
            <T k={kind} />
          </span>
        )}
        {settledAt !== null && (
          <time className="ak-time" dateTime={settledAt}>
            <Numeral>{formatClock(settledAt)}</Numeral>
          </time>
        )}
      </div>
      <div className="ak-body">
        {outcome.kind === 'pending' && <PendingSteps phase={outcome.phase} channelCount={channelCount} />}
        {outcome.kind === 'failed' && (
          <FailedSlip
            reason={<T k="askFailed" params={{ reason: t(failureReasonKey(outcome.reason)) }} />}
            failedAt={outcome.at}
            onRetry={onRetry}
          />
        )}
        {answer?.kind === 'not_found' && (
          <NotFoundSlip answer={answer} channelNames={channelNames} onEdit={onEdit} teamHref={exchange.channelId === null ? '/' : `/c/${exchange.channelId}`} />
        )}
        {answer?.kind === 'answer' && (
          <AnswerBlock answer={answer} channelNames={channelNames} pressed={pressed} onPress={onPress} selected={selected} onSources={onSources} say={say} />
        )}
        {answer?.kind === 'channel_summary' && <ArtifactSlip artifact={answer} />}
      </div>
    </article>
  );
}
