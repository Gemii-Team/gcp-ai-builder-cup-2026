import { LangRuns, MarkSmall, Numeral, ProofSlip, useLabels, type Source } from '@kiwi/ui';
import { useState } from 'react';
import type { ArtifactView } from '../../api/types';
import { useI18n } from '../../i18n';
import { Markdown } from '../markdown/Markdown';

/** A caption the server wrote, such as `Google Meet · 42 min · 6 people`, with its counts and times set in mono. */
function Caption({ text }: { text: string }) {
  return text.split(/(\d+(?::\d+)*)/).map((part, index) =>
    index % 2 === 1 ? <Numeral key={index}>{part}</Numeral> : <LangRuns key={index} text={part} />,
  );
}

type ArtifactSlipProps = {
  artifact: ArtifactView;
  /** For a meeting summary: how many drafts it made that still wait on a person; unknown until the tasks have loaded. */
  draftCount?: number;
  /** Leaves the slip's own title off, where the place that shows it already says it (the Ask thread titles a row with the question). */
  omitTitle?: boolean;
};

/**
 * The proof slip of a message that carries an artifact, whatever its kind: a meeting summary with its tracks, a file
 * summary under its small mark, a cited answer whose markers press their source rows, a not-found answer (a completed
 * answer, with its REF) or a channel summary. Every one carries its REF and its sources, and the body of every one is the
 * model's markdown, shown through `Markdown`: a list is a list, never one paragraph of dashes.
 */
export function ArtifactSlip({ artifact, draftCount, omitTitle = false }: ArtifactSlipProps) {
  const { plain } = useI18n();
  const labels = useLabels();
  const [pressed, setPressed] = useState<number | null>(null);
  const common = { ref: artifact.ref, generatedAt: artifact.generatedAt };
  const heading = {
    ...(artifact.title === '' || omitTitle ? {} : { title: artifact.title }),
    ...(artifact.meta === '' ? {} : { meta: <Caption text={artifact.meta} /> }),
  };
  const body = <Markdown markdown={artifact.bodyMarkdown} />;

  switch (artifact.kind) {
    case 'meeting_summary':
      return (
        <ProofSlip
          {...common}
          {...heading}
          summary={body}
          tracks={artifact.tracks}
          sources={artifact.sources}
          {...(artifact.meetingId === undefined ? {} : { meetingId: artifact.meetingId })}
          {...(draftCount === undefined ? {} : { draftCount })}
        />
      );
    case 'doc_summary':
      return (
        <ProofSlip
          {...common}
          summary={
            <div className="doc-summary">
              <MarkSmall size={16} label={plain('summarizedBy')} className="mini" />
              {body}
            </div>
          }
          sources={artifact.sources}
        />
      );
    case 'answer': {
      const cited = [...artifact.citations].sort((a, b) => a.n - b.n);
      const nameOf = (source: Source) => (source.page === undefined ? source.label : `${source.label}, ${labels.text('pageShort', { page: source.page })}`);
      return (
        <ProofSlip
          {...common}
          summary={
            <Markdown
              markdown={artifact.bodyMarkdown}
              cited={{
                citations: cited,
                pressed,
                onPress: (n) => {
                  setPressed(pressed === n ? null : n);
                },
                labelOf: (citation) => nameOf(citation.source),
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
      return <ProofSlip {...common} summary={body} sources={[]} />;
    case 'channel_summary':
      return <ProofSlip {...common} {...heading} summary={body} sources={artifact.sources} />;
  }
}
