import { formatClock, formatDay, formatOffset, LangRuns, Numeral, Pending, RefCode, Slip, SlipBody, Stub, StubRow, usePrefersReducedMotion, type Excerpt, type Source } from '@kiwi/ui';
import { useQuery } from '@tanstack/react-query';
import { useEffect, useRef, type ReactNode } from 'react';
import { useApi } from '../../api/client';
import { fileQuery, meetingQuery } from '../../api/queries';
import type { CitationView } from '../../api/types';
import { T, TSlots, useI18n } from '../../i18n';
import { sourceLocation, sourceName } from './sources';

type SourceSlipProps = {
  citation: CitationView;
  channelNames: ReadonlyMap<string, string>;
  /** This source's citation is pressed: it takes Citation Wash for 200ms and scrolls into view. */
  hit: boolean;
  /** Loads the transcript lines around a meeting source (`GetTranscriptExcerpt`). */
  load: (source: Source) => Promise<Excerpt>;
};

/** One line of an excerpt: who said it (600) with its offset at the right, then the words. */
function Line({ who, at, text }: { who?: string | null; at?: ReactNode; text: string }) {
  return (
    <div className="ak-ex-l">
      {who !== undefined && who !== null && (
        <b>
          <LangRuns text={who} />
        </b>
      )}
      {at}
      <span className="ak-ex-t">
        <LangRuns text={text} />
      </span>
    </div>
  );
}

/**
 * A cited source shown as its own slip: not an AI output, so it has no perforation. The citation's marker as a label (not
 * a button), the source's name at 600 and its offset, page or time at the right; a kind line; then the excerpt: the
 * transcript lines around the cited moment (`GetTranscriptExcerpt`), or the passage the citation quotes for a document and
 * a message. A meeting or a document ends with a stub, `REF MTG-1014 · Transcript`, whose REF, date and page count are
 * `GetMeeting`'s and `GetFile`'s (cached per id); a message has none, since it is not an issued output.
 */
export function SourceSlip({ citation, channelNames, hit, load }: SourceSlipProps) {
  const { lang, plain } = useI18n();
  const api = useApi();
  const reducedMotion = usePrefersReducedMotion();
  const item = useRef<HTMLLIElement>(null);
  const source: Source = { ...citation.source, quote: citation.quote };
  const channel = channelNames.get(source.channelId) ?? '';

  const meeting = useQuery({ ...meetingQuery(api, source.id), enabled: source.kind === 'meeting', staleTime: 60_000 });
  const file = useQuery({ ...fileQuery(api, source.id), enabled: source.kind === 'file', staleTime: 60_000 });
  const transcript = useQuery({
    queryKey: ['ask-excerpt', source.id, source.entryIdx ?? null],
    queryFn: () => load(source),
    enabled: source.kind === 'meeting',
    staleTime: Infinity,
  });

  useEffect(() => {
    if (hit) {
      // Not every environment can scroll (jsdom has no `scrollIntoView`).
      item.current?.scrollIntoView?.({ block: 'nearest', behavior: reducedMotion ? 'auto' : 'smooth' });
    }
  }, [hit, reducedMotion]);

  const location = sourceLocation(source, plain);
  let kind: ReactNode;
  let excerpt: ReactNode;
  let stub: ReactNode = null;

  if (source.kind === 'meeting') {
    const day = meeting.data?.startedAt;
    kind = day === undefined || day === null ? <T k="askKindMeetingUndated" /> : <T k="askKindMeeting" params={{ day: formatDay(day, lang) }} />;
    if (transcript.isError) {
      excerpt = (
        <p role="alert" className="ak-ex-err">
          <T k="askExcerptFailed" />
        </p>
      );
    } else if (transcript.data?.kind === 'transcript') {
      excerpt = transcript.data.lines.map((line, index) => {
        const offset = formatOffset(line.offsetMs);
        return <Line key={index} who={line.speaker} at={offset === null ? undefined : <Numeral className="ak-ex-n">{offset}</Numeral>} text={line.text} />;
      });
    } else {
      excerpt = <Pending label={plain('askExcerptLoading')} />;
    }
    if (meeting.data !== undefined && meeting.data.ref !== '') {
      stub = (
        <Stub>
          <StubRow>
            <RefCode code={meeting.data.ref} />
            <span>
              <T k="askStubTranscript" />
            </span>
          </StubRow>
        </Stub>
      );
    }
  } else if (source.kind === 'file') {
    const pages = file.data?.pages ?? 0;
    const page = source.page;
    if (page === undefined) {
      kind = <T k="askKindDocumentNoPage" />;
    } else if (pages > 0) {
      kind = <TSlots k="askKindDocument" slots={{ page: <Numeral>{page}</Numeral>, pages: <Numeral>{pages}</Numeral> }} />;
    } else {
      kind = <TSlots k="askKindDocumentNoCount" slots={{ page: <Numeral>{page}</Numeral> }} />;
    }
    excerpt = citation.quote
      .split('\n')
      .filter((paragraph) => paragraph.trim() !== '')
      .map((paragraph, index) => (
        <p key={index} className="ak-ex-t">
          <LangRuns text={paragraph} />
        </p>
      ));
    if (file.data !== undefined && file.data.ref !== '') {
      stub = (
        <Stub>
          <StubRow>
            <RefCode code={file.data.ref} />
            <span>
              <T k="askStubDocument" />
            </span>
          </StubRow>
        </Stub>
      );
    }
  } else {
    const channelSlot = { channel: <LangRuns text={channel} /> };
    if (source.at === undefined) {
      kind = <TSlots k="askKindMessageUntimed" slots={channelSlot} />;
    } else if (formatDay(source.at, lang) === formatDay(new Date(), lang)) {
      kind = <TSlots k="askKindMessageToday" slots={channelSlot} />;
    } else {
      kind = <TSlots k="askKindMessage" params={{ day: formatDay(source.at, lang) }} slots={channelSlot} />;
    }
    excerpt = <Line who={source.label} at={source.at === undefined ? undefined : <Numeral className="ak-ex-n">{formatClock(source.at)}</Numeral>} text={citation.quote} />;
  }

  return (
    <li ref={item} data-n={citation.n}>
      <Slip className="ak-sp" data-hit={hit || undefined}>
        <SlipBody>
          <div className="ak-sp-h">
            <span lang="en" className="ak-mk">{`[${String(citation.n)}]`}</span>
            <b className="ak-sp-t">
              <LangRuns text={sourceName(source, channel, plain)} />
            </b>
            {location !== '' && <Numeral className="ak-sp-n">{location}</Numeral>}
          </div>
          <p className="ak-sp-k">{kind}</p>
          <div className="ak-sp-ex">{excerpt}</div>
        </SlipBody>
        {stub}
      </Slip>
    </li>
  );
}
