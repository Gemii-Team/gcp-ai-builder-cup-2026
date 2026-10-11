// @vitest-environment jsdom
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { ReactElement } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ExcerptProvider, type Excerpt, type Source } from './ExcerptContext';
import { ProofSlip } from './ProofSlip';

const sources: Source[] = [
  { kind: 'file', id: 'file-1', label: 'ACME-contract-v3.pdf', channelId: 'channel-1', page: 4 },
  { kind: 'meeting', id: 'meeting-1', label: 'Q4 sync', channelId: 'channel-1', offsetMs: 1102000, entryIdx: 18 },
];

const loadExcerpt = (source: Source) =>
  Promise.resolve<Excerpt>(
    source.kind === 'file'
      ? { kind: 'document', file: source.label, ref: 'DOC-0371', page: source.page ?? 1, pages: 18 }
      : { kind: 'transcript', title: source.label, offsetMs: source.offsetMs ?? null, lines: [] },
  );

function withExcerpts(ui: ReactElement) {
  return render(<ExcerptProvider load={loadExcerpt}>{ui}</ExcerptProvider>);
}

afterEach(() => {
  vi.unstubAllGlobals();
  Reflect.deleteProperty(Element.prototype, 'scrollIntoView');
});

describe('a cited answer slip', () => {
  const answer = (hitNumber: number | null) => (
    <ProofSlip
      ref="ANS-2207"
      summary="The contract ends on 31 Dec 2026."
      sources={sources}
      sourceNumbers={[1, 2]}
      hitNumber={hitNumber}
      generatedAt="2026-10-13T07:40:00Z"
    />
  );

  it('numbers its source rows by citation', () => {
    withExcerpts(answer(null));

    const rows = within(screen.getByRole('list', { name: 'Sources' })).getAllByRole('listitem');
    expect(rows[0]).toHaveTextContent('[1] ACME-contract-v3.pdf');
    expect(rows[1]).toHaveTextContent('[2] Q4 sync');
    expect(rows[0]?.querySelector('[lang="en"]')).toHaveTextContent('[1]');
  });

  it('tints the row of the citation that is pressed, and only that row', () => {
    withExcerpts(answer(2));

    const rows = within(screen.getByRole('list', { name: 'Sources' })).getAllByRole('listitem');
    expect(rows[0]).not.toHaveAttribute('data-hit');
    expect(rows[1]).toHaveAttribute('data-hit');
  });

  it('tints no row when no marker is pressed', () => {
    withExcerpts(answer(null));

    for (const row of within(screen.getByRole('list', { name: 'Sources' })).getAllByRole('listitem')) {
      expect(row).not.toHaveAttribute('data-hit');
    }
  });
});

describe('an excerpt that opens', () => {
  it('scrolls itself into view, and again when its content has loaded', async () => {
    const scrollIntoView = vi.fn();
    Element.prototype.scrollIntoView = scrollIntoView;
    const user = userEvent.setup();
    withExcerpts(<ProofSlip ref="ANS-2207" summary="x" sources={sources} generatedAt="2026-10-13T07:40:00Z" />);

    await user.click(screen.getByRole('button', { name: 'ACME-contract-v3.pdf, page 4' }));

    await screen.findByText('DOC-0371', { exact: false });
    expect(scrollIntoView).toHaveBeenCalledWith({ block: 'nearest', behavior: 'smooth' });
    expect(scrollIntoView.mock.calls.length).toBeGreaterThanOrEqual(2);
  });

  it('jumps instead of gliding when the person prefers less motion', async () => {
    const scrollIntoView = vi.fn();
    Element.prototype.scrollIntoView = scrollIntoView;
    vi.stubGlobal('matchMedia', (query: string) => ({
      matches: query.includes('prefers-reduced-motion'),
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    }));
    const user = userEvent.setup();
    withExcerpts(<ProofSlip ref="ANS-2207" summary="x" sources={sources} generatedAt="2026-10-13T07:40:00Z" />);

    await user.click(screen.getByRole('button', { name: 'ACME-contract-v3.pdf, page 4' }));

    await screen.findByText('DOC-0371', { exact: false });
    expect(scrollIntoView).toHaveBeenCalledWith({ block: 'nearest', behavior: 'auto' });
    expect(scrollIntoView).not.toHaveBeenCalledWith({ block: 'nearest', behavior: 'smooth' });
  });

  it('opens without scrolling where the environment cannot scroll an element', async () => {
    const user = userEvent.setup();
    withExcerpts(<ProofSlip ref="ANS-2207" summary="x" sources={sources} generatedAt="2026-10-13T07:40:00Z" />);

    await user.click(screen.getByRole('button', { name: 'ACME-contract-v3.pdf, page 4' }));

    expect(await screen.findByRole('group', { name: 'Document excerpt, ACME-contract-v3.pdf page 4' })).toBeInTheDocument();
  });
});

describe('Escape with an open excerpt', () => {
  it('closes the excerpt before a modal layer that holds it, such as a sheet, sees the key', async () => {
    // A sheet listens for Escape on the document in the capture phase, registered before any excerpt opens.
    const sheetEscape = vi.fn();
    document.addEventListener('keydown', sheetEscape, true);
    const user = userEvent.setup();
    withExcerpts(<ProofSlip ref="ANS-2207" summary="x" sources={sources} generatedAt="2026-10-13T07:40:00Z" />);
    await user.click(screen.getByRole('button', { name: 'ACME-contract-v3.pdf, page 4' }));
    await screen.findByRole('group', { name: 'Document excerpt, ACME-contract-v3.pdf page 4' });

    await user.keyboard('{Escape}');

    expect(screen.queryByRole('group', { name: /excerpt/ })).toBeNull();
    expect(sheetEscape).not.toHaveBeenCalled();
    document.removeEventListener('keydown', sheetEscape, true);
  });

  it('lets the key through to the sheet once no excerpt is open', async () => {
    const sheetEscape = vi.fn();
    document.addEventListener('keydown', sheetEscape, true);
    const user = userEvent.setup();
    withExcerpts(<ProofSlip ref="ANS-2207" summary="x" sources={sources} generatedAt="2026-10-13T07:40:00Z" />);

    await user.keyboard('{Escape}');

    expect(sheetEscape).toHaveBeenCalledOnce();
    document.removeEventListener('keydown', sheetEscape, true);
  });
});

describe('a slip that handles its own source presses', () => {
  it('hands a pressed offset or page to onSourcePress with its source and position, and opens no excerpt (no provider is needed)', async () => {
    const onSourcePress = vi.fn();
    const user = userEvent.setup();
    render(<ProofSlip ref="ANS-2207" summary="x" sources={sources} sourceNumbers={[1, 2]} onSourcePress={onSourcePress} generatedAt="2026-10-13T07:40:00Z" />);

    await user.click(screen.getByRole('button', { name: 'ACME-contract-v3.pdf, page 4' }));
    await user.click(screen.getByRole('button', { name: 'Transcript at 0:18:22' }));

    expect(onSourcePress.mock.calls).toEqual([
      [sources[0], 0],
      [sources[1], 1],
    ]);
    expect(screen.queryByRole('group', { name: /excerpt/ })).toBeNull();
  });

  it('puts its actions in a row at the foot of the stub, after the sources', () => {
    render(
      <ProofSlip ref="ANS-2207" summary="x" sources={sources} generatedAt="2026-10-13T07:40:00Z" actions={<button type="button">Make a task from this</button>} />,
      { wrapper: ({ children }) => <ExcerptProvider load={loadExcerpt}>{children}</ExcerptProvider> },
    );

    const action = screen.getByRole('button', { name: 'Make a task from this' });
    const list = screen.getByRole('list', { name: 'Sources' });

    expect(action.parentElement).toHaveClass('kiwi-acts');
    expect(list.compareDocumentPosition(action) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });
});
