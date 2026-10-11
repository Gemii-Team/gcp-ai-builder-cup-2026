import { formatOffset } from '../format';
import { LangRuns } from '../LangRuns';
import { Numeral } from '../Numeral';
import type { Excerpt, OpenExcerpt, Source } from './ExcerptContext';
import { Pending } from './Pending';

/** The name of the excerpt group, known from the source before its content has loaded. */
function groupName(source: Source): string {
  switch (source.kind) {
    case 'meeting':
      return `Transcript excerpt at ${formatOffset(source.offsetMs) ?? ''}`.trim();
    case 'file':
      return `Document excerpt, ${source.label} page ${source.page ?? ''}`.trim();
    case 'message':
      return `Message excerpt from ${source.label}`;
  }
}

function ExcerptBody({ excerpt }: { excerpt: Excerpt }) {
  switch (excerpt.kind) {
    case 'transcript': {
      const offset = formatOffset(excerpt.offsetMs);
      return (
        <>
          <p className="kiwi-ex-h">
            Transcript · <LangRuns text={excerpt.title} />
            {offset !== null && (
              <>
                {' · '}
                <Numeral>{offset}</Numeral>
              </>
            )}
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
    case 'message':
      return (
        <>
          <p className="kiwi-ex-h">
            <LangRuns text={`Message · #${excerpt.channel} · ${excerpt.day === undefined ? '' : `${excerpt.day} `}`} />
            <Numeral>{excerpt.time}</Numeral>
          </p>
          <div className="kiwi-ex-l">
            <b>
              <LangRuns text={excerpt.author} />
            </b>
            <Numeral className="kiwi-ex-off text-carbon-copy">{excerpt.time}</Numeral>
            <span className="kiwi-ex-t">
              <LangRuns text={excerpt.text} />
            </span>
          </div>
        </>
      );
    case 'document':
      return (
        <>
          <p className="kiwi-ex-h">
            <LangRuns text={`Document · ${excerpt.file} · p. `} />
            <Numeral>{excerpt.page}</Numeral>
          </p>
          {excerpt.quote !== undefined && (
            <p className="kiwi-ex-t kiwi-ex-q">
              <LangRuns text={excerpt.quote} />
            </p>
          )}
          <p className="kiwi-ex-m">
            Page <Numeral>{excerpt.page}</Numeral>
            {excerpt.pages !== undefined && (
              <>
                {' of '}
                <Numeral>{excerpt.pages}</Numeral>
              </>
            )}
            {' · '}
            <Numeral>REF {excerpt.ref}</Numeral>
          </p>
        </>
      );
  }
}

/**
 * The moment a source points at, opened in place under the row that asked for it: Counter Hover with a dashed top
 * edge (a content divider, not a perforation). Close, and Escape through the provider, return focus to the link.
 */
export function SourceExcerpt({ panel, onClose }: { panel: OpenExcerpt; onClose: () => void }) {
  return (
    <div id={panel.panelId} role="group" aria-label={groupName(panel.source)} className="kiwi-ex">
      {panel.status === 'loading' && <Pending label="Loading the excerpt…" />}
      {panel.status === 'error' && (
        <p role="alert" className="kiwi-ex-error">
          Couldn't load this excerpt.
        </p>
      )}
      {panel.status === 'ready' && <ExcerptBody excerpt={panel.excerpt} />}
      <button type="button" className="kiwi-ex-x" onClick={onClose}>
        Close
      </button>
    </div>
  );
}
