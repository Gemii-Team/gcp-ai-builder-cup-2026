import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
  type HTMLAttributes,
  type ReactNode,
} from 'react';
import { SourceExcerpt } from './SourceExcerpt';

/**
 * Where an AI output got a claim from. Mirrors `chat.v1.Source` with plain numbers and strings (`@kiwi/ui` never
 * imports the generated client): `entryIdx` and `seq` come from the `anchor` (meeting `<entry idx>`, message `<seq>`)
 * and `page` from `page:<n>`; `label` is the meeting title, the file name or a message's author.
 */
export type Source = {
  kind: 'meeting' | 'file' | 'message';
  id: string;
  label: string;
  channelId: string;
  at?: string;
  offsetMs?: number | null;
  entryIdx?: number | null;
  page?: number;
  seq?: number;
  quote?: string;
};

/** The moment, message or passage a source points at, as the app loads it for the in-place excerpt. */
export type Excerpt =
  | {
      kind: 'transcript';
      title: string;
      offsetMs: number | null;
      lines: { speaker: string | null; offsetMs: number | null; text: string }[];
    }
  | { kind: 'message'; channel: string; day?: string; time: string; author: string; text: string }
  | { kind: 'document'; file: string; ref: string; page: number; pages?: number; quote?: string };

/** The one excerpt that is open: which link and row asked for it, and how far its loading has got. */
export type OpenExcerpt = {
  linkId: string;
  rowId: string | null;
  panelId: string;
  source: Source;
} & ({ status: 'loading' | 'error' } | { status: 'ready'; excerpt: Excerpt });

type Excerpts = {
  open: OpenExcerpt | null;
  show: (opener: HTMLElement, linkId: string, rowId: string | null, source: Source) => void;
  close: (restoreFocus: boolean) => void;
};

const ExcerptsContext = createContext<Excerpts | null>(null);
const RowContext = createContext<string | null>(null);

/**
 * Owns the single open excerpt: opening another closes the first, and Escape or Close closes it and returns focus
 * to the link that opened it. `load` fetches the excerpt for a source (the app's GetExcerpt call).
 */
export function ExcerptProvider({ load, children }: { load: (source: Source) => Promise<Excerpt>; children: ReactNode }) {
  const [open, setOpen] = useState<OpenExcerpt | null>(null);
  const loadRef = useRef(load);
  const opener = useRef<HTMLElement | null>(null);
  const requests = useRef(0);
  const panelBase = useId();

  useEffect(() => {
    loadRef.current = load;
  });

  const close = useCallback((restoreFocus: boolean) => {
    requests.current += 1;
    const link = opener.current;
    opener.current = null;
    setOpen(null);
    if (restoreFocus && link?.isConnected) {
      link.focus();
    }
  }, []);

  const show = useCallback((link: HTMLElement, linkId: string, rowId: string | null, source: Source) => {
    requests.current += 1;
    const request = requests.current;
    opener.current = link;
    const panel = { linkId, rowId, panelId: `${panelBase}-${request}`, source };
    setOpen({ ...panel, status: 'loading' });
    // A load that lost to a newer press, or to Close, must not resurrect its panel.
    loadRef.current(source).then(
      (excerpt) => {
        if (requests.current === request) {
          setOpen({ ...panel, status: 'ready', excerpt });
        }
      },
      () => {
        if (requests.current === request) {
          setOpen({ ...panel, status: 'error' });
        }
      },
    );
  }, [panelBase]);

  const isOpen = open !== null;
  useEffect(() => {
    if (!isOpen) {
      return;
    }
    // Capture phase, so Escape closes the excerpt before a sheet or menu that holds it sees the key.
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key !== 'Escape' || event.defaultPrevented) {
        return;
      }
      if (event.target instanceof Element && event.target.closest('input, select, textarea, [role="menu"]')) {
        return;
      }
      event.preventDefault();
      event.stopPropagation();
      close(true);
    };
    document.addEventListener('keydown', onKeyDown, true);
    return () => {
      document.removeEventListener('keydown', onKeyDown, true);
    };
  }, [isOpen, close]);

  const value = useMemo(() => ({ open, show, close }), [open, show, close]);
  return <ExcerptsContext value={value}>{children}</ExcerptsContext>;
}

/** The row a source link sits in. The excerpt a link opens appears at the foot of this element, under that row. */
export function ExcerptRow({
  as: Tag = 'div',
  children,
  ...rest
}: { as?: 'div' | 'li' } & HTMLAttributes<HTMLElement>) {
  const rowId = useId();
  const excerpts = useContext(ExcerptsContext);
  const own = excerpts?.open?.rowId === rowId ? excerpts.open : null;
  return (
    <Tag {...rest}>
      <RowContext value={rowId}>{children}</RowContext>
      {own !== null && <SourceExcerpt panel={own} onClose={() => excerpts?.close(true)} />}
    </Tag>
  );
}

/**
 * What a link needs to open its source's excerpt: whether it is the one expanded, which element it controls, and the
 * press handler. A link with no enclosing `ExcerptRow` renders its excerpt itself, straight after the link.
 */
export function useExcerptLink(source: Source, hosted: boolean) {
  const excerpts = useContext(ExcerptsContext);
  const rowId = useContext(RowContext);
  const linkId = useId();
  if (excerpts === null && !hosted) {
    throw new Error('A source link opens its excerpt through an <ExcerptProvider>; wrap the slip in one.');
  }
  const own = excerpts?.open?.linkId === linkId ? excerpts.open : null;
  return {
    expanded: own !== null,
    controls: own?.panelId,
    toggle(link: HTMLElement) {
      if (own === null) {
        excerpts?.show(link, linkId, rowId, source);
      } else {
        excerpts?.close(false);
      }
    },
    panel:
      own !== null && rowId === null ? (
        <SourceExcerpt panel={own} onClose={() => excerpts?.close(true)} />
      ) : null,
  };
}
