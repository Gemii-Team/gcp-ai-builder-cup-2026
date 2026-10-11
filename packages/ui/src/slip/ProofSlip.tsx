import type { ReactNode } from 'react';
import { formatClock } from '../format';
import { LangRuns } from '../LangRuns';
import { Numeral } from '../Numeral';
import type { Source } from './ExcerptContext';
import { Perforation } from './Perforation';
import { RefCode } from './RefCode';
import { Slip, SlipBody } from './Slip';
import { SourceRow } from './SourceRow';
import { Stub, StubRow } from './Stub';
import { Tracks, type Track } from './Tracks';

type ProofSlipProps = {
  /** The REF code without its label: `MTG-1014`. */
  ref: string;
  title?: string;
  /** The caption beside the title, such as `Google Meet · 42 min`. */
  meta?: ReactNode;
  summary: ReactNode;
  tracks?: readonly Track[];
  sources: readonly Source[];
  generatedAt: Date | string;
  /** Drafts this slip made that still wait on a person; the footer is shown only when the count is known. */
  draftCount?: number;
  /** The meeting the tracks' offsets point into. */
  meetingId?: string;
};

/**
 * Every AI output arrives as a proof slip: the body, a perforation, then the stub with its REF, the time it was
 * generated and the sources it rests on. Offsets and pages in it open their excerpt in place, so the slip needs an
 * `ExcerptProvider` above it.
 */
export function ProofSlip({ ref, title, meta, summary, tracks, sources, generatedAt, draftCount, meetingId }: ProofSlipProps) {
  return (
    <Slip>
      <SlipBody>
        {(title !== undefined || meta !== undefined) && (
          <div className="kiwi-slip-head">
            {title !== undefined && (
              <h2 className="kiwi-slip-title">
                <LangRuns text={title} />
              </h2>
            )}
            {meta !== undefined && <p className="kiwi-slip-meta">{meta}</p>}
          </div>
        )}
        {typeof summary === 'string' ? (
          <p className="kiwi-prose">
            <LangRuns text={summary} />
          </p>
        ) : (
          <div className="kiwi-prose">{summary}</div>
        )}
        {tracks !== undefined && tracks.length > 0 && (
          <Tracks tracks={tracks} meetingId={meetingId ?? ''} title={title ?? ''} />
        )}
        {draftCount !== undefined && (
          <p className="kiwi-ft" data-zero={draftCount === 0 || undefined}>
            {draftCount === 0 ? (
              'No drafts waiting'
            ) : (
              <>
                <Numeral>{draftCount}</Numeral> {draftCount === 1 ? 'draft' : 'drafts'} to confirm
              </>
            )}
          </p>
        )}
      </SlipBody>
      <Perforation />
      <Stub>
        <StubRow>
          <RefCode code={ref} />
          <span>
            Generated <Numeral>{formatClock(generatedAt)}</Numeral>
          </span>
        </StubRow>
        {sources.length > 0 && (
          <ol className="kiwi-srcs" aria-label="Sources">
            {sources.map((source, index) => (
              <SourceRow key={index} source={source} />
            ))}
          </ol>
        )}
      </Stub>
    </Slip>
  );
}
