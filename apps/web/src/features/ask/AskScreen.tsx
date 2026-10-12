import '../shell/screen.css';
import './ask.css';
import './sources.css';
import { ExcerptProvider, Icon, IconButton, usePrefersReducedMotion } from '@kiwi/ui';
import { useQueryClient } from '@tanstack/react-query';
import { useRouter } from '@tanstack/react-router';
import { useId, useMemo, useRef, useState } from 'react';
import { flushSync } from 'react-dom';
import { useApi } from '../../api/client';
import { createExcerptLoader } from '../../api/excerpts';
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
import { sourceLocation, sourceName } from './sources';
import { SourcesPanel } from './SourcesPanel';
import { SourcesSheet } from './SourcesSheet';
import { Thread } from './Thread';
import { useAskRun } from './useAskRun';

type AskScreenProps = {
  /** `?channel=<id>` of the route: the question searches that channel alone, when the person is in it. */
  initialChannelId: string | null;
};

/**
 * What the route `/ask` shows: command first. The command field on top, with what is searched and two sample questions
 * under it, and under that the thread of questions, newest first, each with its answer slip, and the sources of the
 * answer on show beside it (a bottom sheet on phones). `?channel=<id>` scopes the questions to that channel; a channel the
 * person is not in (a stale link) scopes nothing.
 */
export function AskScreen({ initialChannelId }: AskScreenProps) {
  const api = useApi();
  const queryClient = useQueryClient();
  const router = useRouter();
  const { lang, plain } = useI18n();
  const phone = useMediaQuery('(max-width: 1023px)');
  const reducedMotion = usePrefersReducedMotion();
  const channels = useChannels();
  const me = useMe().data;
  const [text, setText] = useState('');
  /** The exchange whose sources are on show, and the citation of it that is pressed. */
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [hit, setHit] = useState<{ id: string; n: number } | null>(null);
  const [sheetOpen, setSheetOpen] = useState(false);
  const [announcement, say] = useAnnouncer();
  const scopeTextId = useId();
  const field = useRef<HTMLInputElement>(null);
  const thread = useRef<HTMLDivElement>(null);
  /** The control last pressed in the thread: the sheet it opens returns focus to it (never to whatever is active). */
  const pressed = useRef<HTMLElement | null>(null);
  const opener = useRef<HTMLElement | null>(null);

  const { exchanges, ask, retry } = useAskRun((id, outcome) => {
    // Each answer, not-found answer, channel summary and failure is said once, when it arrives. A new cited answer puts its sources on show.
    if (outcome.kind === 'failed') {
      say(plain('askFailed', { reason: plain(failureReasonKey(outcome.reason)) }));
    } else if (outcome.answer.kind === 'not_found') {
      say(plain('askNotFoundAnnounced'));
    } else if (outcome.answer.kind === 'channel_summary') {
      say(plain('askAnswerReady', { ref: outcome.answer.ref, count: outcome.answer.sources.length }));
    } else {
      say(plain('askAnswerReady', { ref: outcome.answer.ref, count: outcome.answer.citations.length }));
      setSelectedId(id);
      setHit(null);
    }
  });

  const scoped = useMemo(
    () => (initialChannelId === null ? null : (channels.data?.find((channel) => channel.id === initialChannelId) ?? null)),
    [initialChannelId, channels.data],
  );
  const searched = useMemo(() => (scoped === null ? (channels.data ?? []) : [scoped]), [scoped, channels.data]);
  const channelNames = useMemo(() => new Map((channels.data ?? []).map((channel) => [channel.id, channel.name])), [channels.data]);
  const load = useMemo(() => createExcerptLoader(api, queryClient, lang), [api, queryClient, lang]);

  const shownExchange = exchanges.find((exchange) => exchange.id === selectedId);
  const shown =
    shownExchange?.outcome.kind === 'answered' && shownExchange.outcome.answer.kind === 'answer'
      ? { answer: shownExchange.outcome.answer, question: shownExchange.question }
      : null;

  function submit(question: string) {
    ask(question, scoped?.id ?? null);
    setText('');
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

  function openSheet() {
    opener.current = pressed.current;
    setSheetOpen(true);
  }

  /** Puts an answer's sources on show; another answer's pressed citation is let go. */
  function select(id: string) {
    if (selectedId !== id) {
      setSelectedId(id);
      setHit((now) => (now?.id === id ? now : null));
    }
  }

  /**
   * A citation press, from a marker or a row's offset or page: the marker fills, the matching source slip is tinted and
   * scrolled into view, the source is announced and a phone's sheet opens. Pressing it again lets it go.
   */
  function press(id: string, n: number) {
    const exchange = exchanges.find((candidate) => candidate.id === id);
    const citation = exchange?.outcome.kind === 'answered' ? exchange.outcome.answer.citations.find((candidate) => candidate.n === n) : undefined;
    if (citation === undefined) {
      return;
    }
    select(id);
    if (selectedId === id && hit?.id === id && hit.n === n) {
      setHit(null);
      return;
    }
    setHit({ id, n });
    const name = sourceName(citation.source, channelNames.get(citation.source.channelId), plain);
    const location = sourceLocation(citation.source, plain);
    say(location === '' ? plain('askSourceAnnouncedNoLocation', { n, name }) : plain('askSourceAnnounced', { n, name, location }));
    if (phone) {
      openSheet();
    }
  }

  function showSources(id: string, ref: string) {
    select(id);
    if (phone) {
      openSheet();
    } else {
      say(plain('askShowingSources', { ref }));
    }
  }

  const samples = (
    <SampleQuestions
      onPick={(question) => {
        fill(question, false);
      }}
    />
  );
  const panel = (sheet: boolean) => (
    <SourcesPanel shown={shown} channelNames={channelNames} hit={hit !== null && hit.id === selectedId ? hit.n : null} load={load} sheet={sheet} />
  );

  // The Board, filtered to the channel the questions are scoped to.
  const boardLink = scoped === null ? '/board' : `/board?channel=${scoped.id}`;

  return (
    <ExcerptProvider load={load}>
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
            <Thread
              ref={thread}
              lead={phone ? samples : undefined}
              onClickCapture={(event) => {
                pressed.current = event.target instanceof Element ? event.target.closest<HTMLElement>('button') : null;
              }}
            >
              {exchanges.map((exchange) => (
                <Exchange
                  key={exchange.id}
                  exchange={exchange}
                  asker={me?.user.name ?? ''}
                  channelNames={channelNames}
                  channelCount={searched.length}
                  pressed={hit?.id === exchange.id ? hit.n : null}
                  selected={selectedId === exchange.id}
                  onPress={(n) => {
                    press(exchange.id, n);
                  }}
                  onSelect={() => {
                    select(exchange.id);
                  }}
                  onSources={() => {
                    showSources(exchange.id, exchange.outcome.kind === 'answered' ? exchange.outcome.answer.ref : '');
                  }}
                  onRetry={() => {
                    retry(exchange.id);
                  }}
                  onEdit={() => {
                    fill(exchange.question, true);
                  }}
                  say={say}
                />
              ))}
            </Thread>
            {!phone && panel(false)}
          </div>
          <p role="status" className="sr-only">
            {announcement}
          </p>
        </main>
      </div>
      {phone && (
        <SourcesSheet open={sheetOpen} onOpenChange={setSheetOpen} opener={opener}>
          {panel(true)}
        </SourcesSheet>
      )}
    </ExcerptProvider>
  );
}
