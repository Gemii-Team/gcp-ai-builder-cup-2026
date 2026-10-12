import { CitationMarker, LangRuns } from '@kiwi/ui';
import { Children, createContext, Fragment, isValidElement, useContext, type ReactNode } from 'react';
import ReactMarkdown, { type Components } from 'react-markdown';
import type { CitationView } from '../../api/types';
import './markdown.css';

/** What turns the `[n]` of a cited answer into a marker that presses its source. */
export type Cited = {
  citations: readonly CitationView[];
  /** The citation number that is pressed. */
  pressed: number | null;
  onPress: (n: number) => void;
  /** What a marker's name says it points at: `Source 2: Q4 launch weekly sync`. */
  labelOf: (citation: CitationView) => string;
};

const CitedContext = createContext<Cited | null>(null);

const MARKER = /(\[\d+\])/;
const SAFE_LINK = /^https?:\/\/\S/i;

/**
 * A run of the body's own words. Each `[n]` that names a citation of this answer is a marker button; any other `[n]`
 * stays text, so a number the model made up points at nothing. Thai runs are tagged `lang="th"`; a marker is a Latin
 * token and carries `lang="en"` itself.
 */
function Words({ text }: { text: string }) {
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

/**
 * The text children of an element, each through `Words` (or through `LangRuns` alone where a marker must not go: inside a
 * link or a code span). A newline the model wrote inside the words of a line is a `<br>`: HTML would fold it into a space
 * and run the lines together. Newlines that are not the model's line breaks stay as they are, for HTML to fold: a string
 * that is only whitespace is the separator markdown puts between the elements of a list item (the `\n` around a loose
 * item's paragraph, or between an item's text and its nested list), and a newline at the very start or end of the
 * children would open or close an item with an empty line. A hard break (two spaces or a backslash before the newline)
 * already comes as a `<br>` element followed by its newline, which must not become a second one. Code keeps its own newlines.
 */
function inline(children: ReactNode, { markers, breaks = true }: { markers: boolean; breaks?: boolean }): ReactNode[] {
  const nodes = Children.toArray(children);
  return nodes.map((child, index) => {
    if (typeof child !== 'string') {
      return child;
    }
    let text = child;
    if (breaks) {
      const previous = nodes[index - 1];
      if (index === 0 || (isValidElement(previous) && previous.type === 'br')) {
        text = text.replace(/^\n+/, '');
      }
      if (index === nodes.length - 1) {
        text = text.replace(/\n+$/, '');
      }
      if (text.trim() === '') {
        return text;
      }
    }
    const lines = breaks ? text.split('\n') : [text];
    return (
      <Fragment key={index}>
        {lines.map((line, at) => (
          <Fragment key={at}>
            {at > 0 && <br />}
            {markers ? <Words text={line} /> : <LangRuns text={line} />}
          </Fragment>
        ))}
      </Fragment>
    );
  });
}

function Heading({ children }: { children?: ReactNode }) {
  // A body sits inside a slip, under the slip's own title: a heading the model wrote is a bold line, never a bigger one.
  return <p className="md-heading">{inline(children, { markers: true })}</p>;
}

/**
 * Model output is untrusted, so it is only ever markdown, and what it writes is never markup: raw HTML is not parsed (no
 * rehype-raw), react-markdown turns each raw tag into a text node, so a `<script>` or an `<img onerror>` is the characters
 * it was written with and never an element, while `Result<T>` or `<name>` stay visible. A link is kept only when it is
 * `http(s)`, and then opens in a new tab with `rel="noopener noreferrer"`; any other link (`javascript:`, `data:`,
 * `mailto:`) is its text. An image is its description: a picture the model points at would be fetched, and carry what
 * was asked, to a server of the model's choosing.
 */
const COMPONENTS: Components = {
  p: ({ children }) => <p>{inline(children, { markers: true })}</p>,
  li: ({ children }) => <li>{inline(children, { markers: true })}</li>,
  strong: ({ children }) => <strong>{inline(children, { markers: true })}</strong>,
  em: ({ children }) => <em>{inline(children, { markers: true })}</em>,
  del: ({ children }) => <del>{inline(children, { markers: true })}</del>,
  h1: Heading,
  h2: Heading,
  h3: Heading,
  h4: Heading,
  h5: Heading,
  h6: Heading,
  code: ({ children, className }) => <code className={className}>{inline(children, { markers: false, breaks: false })}</code>,
  a: ({ href, children }) =>
    href !== undefined && SAFE_LINK.test(href) ? (
      <a href={href} target="_blank" rel="noopener noreferrer">
        {inline(children, { markers: false })}
      </a>
    ) : (
      <>{inline(children, { markers: false })}</>
    ),
  img: ({ alt }) => <LangRuns text={alt ?? ''} />,
};

type MarkdownProps = {
  markdown: string;
  /** For a cited answer: its `[n]` markers become buttons that press their source. Without it every `[n]` stays text. */
  cited?: Cited;
};

/**
 * The model's markdown as the body of a slip: paragraphs, lists, line breaks, bold and italic, inline code and safe
 * links, sanitised, its headings flattened to bold lines and its Thai runs tagged. Every slip that shows model text goes
 * through it. The slip it sits in supplies the 76ch prose column.
 */
export function Markdown({ markdown, cited }: MarkdownProps) {
  return (
    <CitedContext value={cited ?? null}>
      <div className="md">
        <ReactMarkdown components={COMPONENTS}>
          {markdown}
        </ReactMarkdown>
      </div>
    </CitedContext>
  );
}
