import { CitationMarker, LangRuns, MarkSmall, Numeral, ProofSlip, useLabels, type Source } from '@kiwi/ui';
import { Fragment, useState, type ReactNode } from 'react';
import type { ArtifactView, CitationView } from '../../api/types';
import { useI18n } from '../../i18n';

/** A caption the server wrote, such as `Google Meet · 42 min · 6 people`, with its counts and times set in mono. */
function Caption({ text }: { text: string }) {
  return text.split(/(\d+(?::\d+)*)/).map((part, index) =>
    index % 2 === 1 ? <Numeral key={index}>{part}</Numeral> : <LangRuns key={index} text={part} />,
  );
}

type CitedTextProps = {
  text: string;
  citations: readonly CitationView[];
  pressed: number | null;
  onPress: (n: number) => void;
};

/** An answer's paragraphs, in which each `[n]` that names a citation is a marker button and any other `[n]` stays text. */
function CitedText({ text, citations, pressed, onPress }: CitedTextProps) {
  const labels = useLabels();
  const nameOf = (source: Source) => (source.page === undefined ? source.label : `${source.label}, ${labels.text('pageShort', { page: source.page })}`);
  return text.split(/\n{2,}/).map((paragraph, index) => (
    <p key={index}>
      {paragraph.split(/(\[\d+\])/).map((part, at) => {
        const citation = /^\[(\d+)\]$/.exec(part)?.[1];
        const cited = citation === undefined ? undefined : citations.find((c) => c.n === Number(citation));
        return cited === undefined ? (
          <LangRuns key={at} text={part} />
        ) : (
          <CitationMarker
            key={at}
            n={cited.n}
            label={nameOf(cited.source)}
            pressed={pressed === cited.n}
            onPress={() => {
              onPress(cited.n);
            }}
          />
        );
      })}
    </p>
  ));
}

type ArtifactSlipProps = {
  artifact: ArtifactView;
  /** For a meeting summary: how many drafts it made that still wait on a person; unknown until the tasks have loaded. */
  draftCount?: number;
};

/**
 * The proof slip of a message that carries an artifact, whatever its kind: a meeting summary with its tracks, a file
 * summary under its small mark, a cited answer whose markers press their source rows, a not-found answer (a completed
 * answer, with its REF) or a channel summary. Every one carries its REF and its sources.
 */
export function ArtifactSlip({ artifact, draftCount }: ArtifactSlipProps) {
  const { plain } = useI18n();
  const [pressed, setPressed] = useState<number | null>(null);
  const common = { ref: artifact.ref, generatedAt: artifact.generatedAt };
  const heading = {
    ...(artifact.title === '' ? {} : { title: artifact.title }),
    ...(artifact.meta === '' ? {} : { meta: <Caption text={artifact.meta} /> }),
  };

  switch (artifact.kind) {
    case 'meeting_summary':
      return (
        <ProofSlip
          {...common}
          {...heading}
          summary={artifact.bodyMarkdown}
          tracks={artifact.tracks}
          sources={artifact.sources}
          {...(artifact.meetingId === undefined ? {} : { meetingId: artifact.meetingId })}
          {...(draftCount === undefined ? {} : { draftCount })}
        />
      );
    case 'doc_summary': {
      const summary: ReactNode = (
        <Fragment>
          <MarkSmall size={16} label={plain('summarizedBy')} className="mini" />
          <LangRuns text={artifact.bodyMarkdown} />
        </Fragment>
      );
      return <ProofSlip {...common} summary={summary} sources={artifact.sources} />;
    }
    case 'answer': {
      const cited = [...artifact.citations].sort((a, b) => a.n - b.n);
      return (
        <ProofSlip
          {...common}
          summary={
            <CitedText
              text={artifact.bodyMarkdown}
              citations={cited}
              pressed={pressed}
              onPress={(n) => {
                setPressed(pressed === n ? null : n);
              }}
            />
          }
          sources={cited.map((citation) => citation.source)}
          sourceNumbers={cited.map((citation) => citation.n)}
          hitNumber={pressed}
        />
      );
    }
    case 'not_found':
      return <ProofSlip {...common} summary={artifact.bodyMarkdown} sources={[]} />;
    case 'channel_summary':
      return <ProofSlip {...common} {...heading} summary={artifact.bodyMarkdown} sources={artifact.sources} />;
  }
}
