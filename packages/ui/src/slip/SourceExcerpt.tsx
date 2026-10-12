import { useEffect, useRef, type ReactNode } from 'react';
import { formatOffset } from '../format';
import { useLabels, type Labels } from '../labels';
import { LangRuns } from '../LangRuns';
import { Numeral } from '../Numeral';
import type { Excerpt, OpenExcerpt, Source } from './ExcerptContext';
import { Pending, usePrefersReducedMotion } from './Pending';

/** The name of the excerpt group, known from the source before its content has loaded. */
function groupName(labels: Labels, source: Source): string {
  switch (source.kind) {
    case 'meeting': {
      const offset = formatOffset(source.offsetMs);
      return offset === null ? labels.text('transcriptExcerpt') : labels.text('transcriptExcerptAt', { time: offset });
    }
    case 'file':
      return source.page === undefined
        ? labels.text('documentExcerptNoPage', { file: source.label })
        : labels.text('documentExcerpt', { file: source.label, page: source.page });
    case 'message':
      return labels.text('messageExcerpt', { author: source.label });
  }
}

function ExcerptBody({ excerpt }: { excerpt: Excerpt }) {
  const labels = useLabels();
  switch (excerpt.kind) {
    case 'transcript': {
      const offset = formatOffset(excerpt.offsetMs);
      const title = <LangRuns text={excerpt.title} />;
      return (
        <>
          <p className="kiwi-ex-h">
            {offset === null
              ? labels.node('transcriptHeaderUntimed', { title })
              : labels.node('transcriptHeader', { title, time: <Numeral>{offset}</Numeral> })}
          </p>
          {excerpt.lines.map((line, index) => {
            const at = formatOffset(line.offsetMs);
            return (
              <div key={index} className="kiwi-ex-l">
                {line.speaker !== null && (
                  <b>
                    <LangRuns text={line.speaker} />
                  </b>
                )}
                {at !== null && <Numeral className="kiwi-ex-off text-carbon-copy">{at}</Numeral>}
                <span className="kiwi-ex-t">
                  <LangRuns text={line.text} />
                </span>
              </div>
            );
          })}
        </>
      );
    }
    case 'message': {
      const channel = <LangRuns text={excerpt.channel} />;
      // A message with no time is shown without one: the header and the line say nothing, never a wrong time.
      const time = excerpt.time === undefined ? undefined : <Numeral>{excerpt.time}</Numeral>;
      let header: ReactNode;
      if (time === undefined) {
        header = labels.node('messageHeaderUntimed', { channel });
      } else if (excerpt.day === undefined) {
        header = labels.node('messageHeaderToday', { channel, time });
      } else {
        header = labels.node('messageHeader', { channel, time, day: <LangRuns text={excerpt.day} /> });
      }
      return (
        <>
          <p className="kiwi-ex-h">{header}</p>
          <div className="kiwi-ex-l">
            <b>
              <LangRuns text={excerpt.author} />
            </b>
            {excerpt.time !== undefined && <Numeral className="kiwi-ex-off text-carbon-copy">{excerpt.time}</Numeral>}
            <span className="kiwi-ex-t">
              <LangRuns text={excerpt.text} />
            </span>
          </div>
        </>
      );
    }
    case 'document': {
      const ref = <Numeral lang="en">{`REF ${excerpt.ref}`}</Numeral>;
      return (
        <>
          <p className="kiwi-ex-h">
            {labels.node('documentHeader', { file: <LangRuns text={excerpt.file} />, page: excerpt.page })}
          </p>
          {excerpt.quote !== undefined && (
            <p className="kiwi-ex-t kiwi-ex-q">
              <LangRuns text={excerpt.quote} />
            </p>
          )}
          <p className="kiwi-ex-m">
            {excerpt.pages === undefined
              ? labels.node('documentPageNoCount', { page: excerpt.page, ref })
              : labels.node('documentPage', { page: excerpt.page, pages: excerpt.pages, ref })}
          </p>
        </>
      );
    }
  }
}

/**
 * The moment a source points at, opened in place under the row that asked for it: Counter Hover with a dashed top
 * edge (a content divider, not a perforation). Close, and Escape through the provider, return focus to the link. It
 * scrolls itself into view when it opens and again when its content arrives, so one opened at the foot of a scroller is
 * never left below the fold.
 */
export function SourceExcerpt({ panel, onClose }: { panel: OpenExcerpt; onClose: () => void }) {
  const labels = useLabels();
  const reduced = usePrefersReducedMotion();
  const group = useRef<HTMLDivElement>(null);
  useEffect(() => {
    // Not every environment can scroll (jsdom has no `scrollIntoView`).
    group.current?.scrollIntoView?.({ block: 'nearest', behavior: reduced ? 'auto' : 'smooth' });
  }, [panel.status, reduced]);
  return (
    <div ref={group} id={panel.panelId} role="group" aria-label={groupName(labels, panel.source)} className="kiwi-ex">
      {panel.status === 'loading' && <Pending label={labels.text('excerptLoading')} />}
      {panel.status === 'error' && (
        <p role="alert" className="kiwi-ex-error">
          {labels.node('excerptError')}
        </p>
      )}
      {panel.status === 'ready' && <ExcerptBody excerpt={panel.excerpt} />}
      <button type="button" className="kiwi-ex-x" onClick={onClose}>
        {labels.node('close')}
      </button>
    </div>
  );
}
