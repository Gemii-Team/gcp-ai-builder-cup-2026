import { FailedSlip, formatClock, LangRuns, MarkSmall, Numeral, PendingSteps } from '@kiwi/ui';
import { useId } from 'react';
import type { ArtifactView } from '../../api/types';
import { T, useI18n } from '../../i18n';
import { failureReasonKey } from '../../lib/failure';
import { initialOf } from '../../lib/text';
import { AnswerSlip } from './AnswerSlip';
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
  onPress: (n: number) => void;
  onRetry: () => void;
  /** Puts the question back in the field. */
  onEdit: () => void;
};

/** The label of the assistant's row: which language the answer is in (any Thai character makes it Thai), or that nothing was found. */
function kindKey(answer: ArtifactView): 'askKindNotFound' | 'askKindThai' | 'askKindEnglish' {
  if (answer.kind === 'not_found') {
    return 'askKindNotFound';
  }
  return /[\u0E00-\u0E7F]/.test(answer.bodyMarkdown) ? 'askKindThai' : 'askKindEnglish';
}

/**
 * One question of the thread, as an article: the asker's row, the question as a Title line, the assistant's row (the small
 * mark, `@assistant`, a Carbon Copy kind label and the time) and under it what came of the question: two calm pending steps,
 * an answer slip, a not-found slip (an answer, not a failure) or a failed slip with Retry.
 */
export function Exchange({ exchange, asker, channelNames, channelCount, pressed, onPress, onRetry, onEdit }: ExchangeProps) {
  const { t } = useI18n();
  const questionId = useId();
  const { outcome } = exchange;
  const answer = outcome.kind === 'answered' ? outcome.answer : null;
  const kind = answer === null ? null : kindKey(answer);
  const settledAt = answer?.generatedAt ?? (outcome.kind === 'failed' ? outcome.at : null);

  return (
    <article className="ak-qa" data-exchange={exchange.id} aria-labelledby={questionId}>
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
        {answer?.kind === 'answer' && <AnswerSlip answer={answer} channelNames={channelNames} pressed={pressed} onPress={onPress} />}
      </div>
    </article>
  );
}
