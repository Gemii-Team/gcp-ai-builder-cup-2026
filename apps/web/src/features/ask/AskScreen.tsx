import '../shell/screen.css';
import './ask.css';
import { Icon, IconButton, usePrefersReducedMotion } from '@kiwi/ui';
import { useRouter } from '@tanstack/react-router';
import { useEffect, useId, useMemo, useRef, useState } from 'react';
import { flushSync } from 'react-dom';
import { useChannels, useMe } from '../../api/hooks';
import { T, useI18n } from '../../i18n';
import { failureReasonKey } from '../../lib/failure';
import { useAnnouncer } from '../../lib/useAnnouncer';
import { useMediaQuery } from '../../lib/useMediaQuery';
import { AppButtonLink, AppLink } from '../shell/AppLink';
import { CommandField } from './CommandField';
import { Exchange } from './Exchange';
import { SampleQuestions } from './SampleQuestions';
import { ScopeDisclosure } from './ScopeDisclosure';
import { Thread } from './Thread';
import { useAskRun } from './useAskRun';

type AskScreenProps = {
  /** `?channel=<id>` of the route: the question searches that channel alone, when the person is in it. */
  initialChannelId: string | null;
};

/**
 * What the route `/ask` shows: command first. The command field on top, with what is searched and two sample questions
 * under it, and under that the thread of questions, newest first, each with its answer slip. `?channel=<id>` scopes the
 * questions to that channel; a channel the person is not in (a stale link) scopes nothing.
 */
export function AskScreen({ initialChannelId }: AskScreenProps) {
  const router = useRouter();
  const { plain } = useI18n();
  const phone = useMediaQuery('(max-width: 1023px)');
  const reducedMotion = usePrefersReducedMotion();
  const channels = useChannels();
  const me = useMe().data;
  const { exchanges, ask, retry } = useAskRun();
  const [text, setText] = useState('');
  const [hit, setHit] = useState<{ id: string; n: number } | null>(null);
  const [announcement, say] = useAnnouncer();
  const scopeTextId = useId();
  const field = useRef<HTMLInputElement>(null);
  const thread = useRef<HTMLDivElement>(null);
  const announced = useRef(new Set<string>());

  const scoped = useMemo(
    () => (initialChannelId === null ? null : (channels.data?.find((channel) => channel.id === initialChannelId) ?? null)),
    [initialChannelId, channels.data],
  );
  const searched = useMemo(() => (scoped === null ? (channels.data ?? []) : [scoped]), [scoped, channels.data]);
  const channelNames = useMemo(() => new Map((channels.data ?? []).map((channel) => [channel.id, channel.name])), [channels.data]);

  // Each answer, not-found answer and failure is said once when it arrives; a Retry that goes back to pending may say its next.
  useEffect(() => {
    for (const exchange of exchanges) {
      const { outcome } = exchange;
      if (outcome.kind === 'pending') {
        announced.current.delete(exchange.id);
      } else if (!announced.current.has(exchange.id)) {
        announced.current.add(exchange.id);
        if (outcome.kind === 'failed') {
          say(plain('askFailed', { reason: plain(failureReasonKey(outcome.reason)) }));
        } else if (outcome.answer.kind === 'not_found') {
          say(plain('askNotFoundAnnounced'));
        } else {
          say(plain('askAnswerReady', { ref: outcome.answer.ref, count: outcome.answer.citations.length }));
        }
      }
    }
  }, [exchanges, say, plain]);

  function submit(question: string) {
    ask(question, scoped?.id ?? null);
    setText('');
    setHit(null);
    thread.current?.scrollTo?.({ top: 0, behavior: reducedMotion ? 'auto' : 'smooth' });
  }

  /** Puts `question` in the field and the caret in it: at the end, or the whole of it selected so typing replaces it. */
  function fill(question: string, select: boolean) {
    flushSync(() => {
      setText(question);
    });
    const input = field.current;
    if (input !== null) {
      input.focus();
      if (select) {
        input.select();
      } else {
        input.setSelectionRange(question.length, question.length);
      }
    }
  }

  const samples = (
    <SampleQuestions
      onPick={(question) => {
        fill(question, false);
      }}
    />
  );

  // The Board, filtered to the channel the questions are scoped to.
  const boardLink = scoped === null ? '/board' : `/board?channel=${scoped.id}`;

  return (
    <div className="ak">
      <main className="main">
        <header className="chead">
          <IconButton
            label={plain('back')}
            className="back"
            onClick={() => {
              router.history.back();
            }}
          >
            <Icon name="back" />
          </IconButton>
          <div className="ttl">
            <h1>
              <T k="navAsk" />
            </h1>
            <p className="sub">
              <T k="askSubtitle" />
            </p>
          </div>
          <div className="acts">
            <AppButtonLink to={boardLink}>
              <Icon name="board" />
              <T k="navBoard" />
            </AppButtonLink>
          </div>
          <AppLink to={boardLink} className="ib ask-ic" aria-label={plain('navBoard')}>
            <Icon name="board" />
          </AppLink>
        </header>
        <section className="ak-cmd" aria-label={plain('navAsk')}>
          <CommandField value={text} onChange={setText} onSubmit={submit} describedBy={scopeTextId} inputRef={field} />
          {channels.data !== undefined && <ScopeDisclosure channels={searched} scopedTo={scoped} textId={scopeTextId} />}
          {!phone && samples}
        </section>
        <div className="ak-work">
          <Thread ref={thread} lead={phone ? samples : undefined}>
            {exchanges.map((exchange) => (
              <Exchange
                key={exchange.id}
                exchange={exchange}
                asker={me?.user.name ?? ''}
                channelNames={channelNames}
                channelCount={searched.length}
                pressed={hit?.id === exchange.id ? hit.n : null}
                onPress={(n) => {
                  setHit((now) => (now?.id === exchange.id && now.n === n ? null : { id: exchange.id, n }));
                }}
                onRetry={() => {
                  retry(exchange.id);
                }}
                onEdit={() => {
                  fill(exchange.question, true);
                }}
              />
            ))}
          </Thread>
        </div>
        <p role="status" className="sr-only">
          {announcement}
        </p>
      </main>
    </div>
  );
}

