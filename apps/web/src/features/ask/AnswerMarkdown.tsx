import { CitationMarker, LangRuns } from '@kiwi/ui';
import { Children, createContext, useContext, type ReactNode } from 'react';
import Markdown, { type Components } from 'react-markdown';
import type { CitationView } from '../../api/types';

type CitedState = {
  citations: readonly CitationView[];
  /** The citation number that is pressed. */
  pressed: number | null;
  onPress: (n: number) => void;
  /** What a marker's name says it points at: `Source 2: Q4 launch weekly sync`. */
  labelOf: (citation: CitationView) => string;
};

const CitedContext = createContext<CitedState | null>(null);

const MARKER = /(\[\d+\])/;
const SAFE_LINK = /^(?:https?:|mailto:)/i;

/**
 * A run of the answer's own words. Each `[n]` that names a citation of this answer is a marker button; any other `[n]`
 * stays text, so a number the model made up points at nothing. Thai runs are tagged `lang="th"`; a marker is a Latin
 * token and carries `lang="en"` itself.
 */
function Cited({ text }: { text: string }) {
  const cited = useContext(CitedContext);
  return text.split(MARKER).map((part, index) => {
    const n = /^\[(\d+)\]$/.exec(part)?.[1];
    const citation = n === undefined ? undefined : cited?.citations.find((candidate) => candidate.n === Number(n));
    if (cited === null || citation === undefined) {
      return <LangRuns key={index} text={part} />;
    }
    return (
      <CitationMarker
        key={index}
        n={citation.n}
        label={cited.labelOf(citation)}
        pressed={cited.pressed === citation.n}
        onPress={() => {
          cited.onPress(citation.n);
        }}
      />
    );
  });
}

/** The text children of an element, each through `Cited` (or through `LangRuns` alone where a marker must not go: inside a link or a code span). */
function inline(children: ReactNode, markers: boolean): ReactNode[] {
  return Children.toArray(children).map((child, index) => {
    if (typeof child !== 'string') {
      return child;
    }
    return markers ? <Cited key={index} text={child} /> : <LangRuns key={index} text={child} />;
  });
}

function Heading({ children }: { children?: ReactNode }) {
  // An answer sits inside a slip, under the question that is the page's heading: a heading the model wrote is a bold line.
  return <p className="am-heading">{inline(children, true)}</p>;
}

/**
 * Model output is untrusted, so it is only ever markdown: `skipHtml` drops every raw HTML node (a `<script>` or an
 * `<img onerror>` never reaches the DOM). A link is kept only when it is `http(s)` or `mailto`, and then opens in a new tab
 * with `rel="noopener noreferrer"`; any other link (`javascript:`, `data:`) is its text. An image is its description:
 * a picture the model points at would be fetched, and carry what was asked, to a server of the model's choosing.
 */
const COMPONENTS: Components = {
  p: ({ children }) => <p>{inline(children, true)}</p>,
  li: ({ children }) => <li>{inline(children, true)}</li>,
  strong: ({ children }) => <strong>{inline(children, true)}</strong>,
  em: ({ children }) => <em>{inline(children, true)}</em>,
  del: ({ children }) => <del>{inline(children, true)}</del>,
  h1: Heading,
  h2: Heading,
  h3: Heading,
  h4: Heading,
  h5: Heading,
  h6: Heading,
  code: ({ children, className }) => <code className={className}>{inline(children, false)}</code>,
  a: ({ href, children }) =>
    href !== undefined && SAFE_LINK.test(href) ? (
      <a href={href} target="_blank" rel="noopener noreferrer">
        {inline(children, false)}
      </a>
    ) : (
      <>{inline(children, false)}</>
    ),
  img: ({ alt }) => <LangRuns text={alt ?? ''} />,
};

type AnswerMarkdownProps = Omit<CitedState, 'citations'> & {
  markdown: string;
  citations: readonly CitationView[];
};

/**
 * An answer's markdown as the body of its slip: sanitised, with its `[n]` markers pressable and its Thai runs tagged.
 * The slip it sits in supplies the 76ch prose column.
 */
export function AnswerMarkdown({ markdown, citations, pressed, onPress, labelOf }: AnswerMarkdownProps) {
  return (
    <CitedContext value={{ citations, pressed, onPress, labelOf }}>
      <div className="am">
        <Markdown skipHtml components={COMPONENTS}>
          {markdown}
        </Markdown>
      </div>
    </CitedContext>
  );
}
