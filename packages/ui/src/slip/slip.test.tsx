// @vitest-environment jsdom
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState, type ReactElement } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { CitationMarker } from './CitationMarker';
import { ExcerptProvider, ExcerptRow, type Excerpt, type Source } from './ExcerptContext';
import { FailedSlip } from './FailedSlip';
import { FileChip } from './FileChip';
import { Pending } from './Pending';
import { PendingSlip } from './PendingSlip';
import { PendingSteps } from './PendingSteps';
import { ProofSlip } from './ProofSlip';
import { SourceRow } from './SourceRow';
import { MessageLink, OffsetLink, PageLink } from './SourceLinks';
import type { Track } from './Tracks';

const meeting: Source = {
  kind: 'meeting',
  id: 'meeting-1',
  label: 'Q4 launch weekly sync',
  channelId: 'channel-1',
  offsetMs: 761000,
  entryIdx: 12,
};
const file: Source = {
  kind: 'file',
  id: 'file-1',
  label: 'ACME-contract-v3.pdf',
  channelId: 'channel-1',
  page: 4,
};
const message: Source = {
  kind: 'message',
  id: 'message-1',
  label: 'Arthit K.',
  channelId: 'channel-1',
  at: '2026-10-12T07:02:00Z',
  seq: 41,
};

const tracks: Track[] = [
  { kind: 'decision', text: 'Move the launch to 15 Nov 2026', offsetMs: 761000, entryIdx: 12 },
  { kind: 'decision', text: 'Use ThaiExpress as the main carrier', offsetMs: 1625000, entryIdx: 27 },
  { kind: 'open_question', text: 'Who approves the extra THB 120,000 marketing budget?', offsetMs: 1998000, entryIdx: 33 },
];

const transcript = (offsetMs: number): Excerpt => ({
  kind: 'transcript',
  title: 'Q4 launch weekly sync',
  offsetMs,
  lines: [
    { speaker: 'พิมพ์ชนก สุขใจ', offsetMs, text: 'งั้นเราเลื่อนวันเปิดตัวไปเป็น 15 พฤศจิกายนนะคะ' },
    { speaker: 'อาทิตย์ กิตติ', offsetMs: offsetMs + 17000, text: 'เห็นด้วยครับ ล็อตใหม่เข้าคลังวันที่ 10' },
  ],
});

/** Resolves a transcript excerpt for the clicked offset, like the app's GetExcerpt call. */
const loadTranscript = () => vi.fn((source: Source) => Promise.resolve(transcript(source.offsetMs ?? 0)));

function withExcerpts(ui: ReactElement, load: (source: Source) => Promise<Excerpt> = loadTranscript()) {
  return render(<ExcerptProvider load={load}>{ui}</ExcerptProvider>);
}

function stubReducedMotion(matches: boolean) {
  vi.stubGlobal('matchMedia', (query: string) => ({
    matches: matches && query.includes('prefers-reduced-motion'),
    media: query,
    addEventListener: () => undefined,
    removeEventListener: () => undefined,
  }));
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('OffsetLink', () => {
  it('renders the offset as a button named Transcript at h:mm:ss and toggles aria-expanded', async () => {
    const user = userEvent.setup();
    withExcerpts(
      <ExcerptRow>
        <OffsetLink source={meeting} />
      </ExcerptRow>,
    );
    const link = screen.getByRole('button', { name: 'Transcript at 0:12:41' });
    expect(link).toHaveTextContent('0:12:41');
    expect(link).toHaveAttribute('aria-expanded', 'false');
    expect(link).not.toHaveAttribute('aria-controls');

    await user.click(link);
    expect(link).toHaveAttribute('aria-expanded', 'true');
    const excerpt = await screen.findByRole('group', { name: 'Transcript excerpt at 0:12:41' });
    expect(link).toHaveAttribute('aria-controls', excerpt.id);

    await user.click(link);
    expect(link).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByRole('group', { name: /excerpt/ })).toBeNull();
  });

  it('is set in mono, so a transcript offset never reads like prose', () => {
    withExcerpts(
      <ExcerptRow>
        <OffsetLink source={meeting} />
      </ExcerptRow>,
    );
    expect(screen.getByRole('button', { name: 'Transcript at 0:12:41' })).toHaveClass('font-mono');
  });

  it('TestOffsetLink_NullOffsetRendersNothing', () => {
    const { container } = withExcerpts(
      <ExcerptRow>
        <OffsetLink source={{ ...meeting, offsetMs: null }} />
        <OffsetLink source={{ ...meeting, offsetMs: undefined }} />
      </ExcerptRow>,
    );
    expect(screen.queryByRole('button')).toBeNull();
    expect(screen.queryByText('0:00:00')).toBeNull();
    expect(container.querySelector('.kiwi-ts')).toBeNull();
  });

  it('shows a real zero offset: 0:00:00 is a valid first moment', () => {
    withExcerpts(
      <ExcerptRow>
        <OffsetLink source={{ ...meeting, offsetMs: 0 }} />
      </ExcerptRow>,
    );
    expect(screen.getByRole('button', { name: 'Transcript at 0:00:00' })).toBeInTheDocument();
  });

  it('calls onPress instead of opening an excerpt when the host wants to handle the press', async () => {
    const user = userEvent.setup();
    const onPress = vi.fn();
    const load = loadTranscript();
    withExcerpts(
      <ExcerptRow>
        <OffsetLink source={meeting} onPress={onPress} />
      </ExcerptRow>,
      load,
    );
    const link = screen.getByRole('button', { name: 'Transcript at 0:12:41' });
    expect(link).not.toHaveAttribute('aria-expanded');
    await user.click(link);
    expect(onPress).toHaveBeenCalledWith(meeting);
    expect(load).not.toHaveBeenCalled();
    expect(screen.queryByRole('group')).toBeNull();
  });
});

describe('the transcript excerpt', () => {
  const slip = (
    <ProofSlip
      ref="MTG-1014"
      title="Q4 launch weekly sync"
      meta="Google Meet · 42 min"
      summary="The team moved the launch to 15 Nov 2026."
      tracks={tracks}
      sources={[meeting]}
      generatedAt="2026-10-11T07:31:00Z"
      meetingId="meeting-1"
    />
  );

  it('opens in place under the row that asked for it', async () => {
    const user = userEvent.setup();
    withExcerpts(slip);
    const [first, second] = screen.getAllByRole('button', { name: /^Transcript at/ });
    await user.click(second);
    const excerpt = await screen.findByRole('group', { name: 'Transcript excerpt at 0:27:05' });
    expect(second.closest('li')).toContainElement(excerpt);
    expect(first.closest('li')).not.toContainElement(excerpt);
  });

  it('keeps one excerpt open at a time', async () => {
    const user = userEvent.setup();
    withExcerpts(slip);
    const [first, second] = screen.getAllByRole('button', { name: /^Transcript at/ });
    await user.click(first);
    await screen.findByRole('group', { name: 'Transcript excerpt at 0:12:41' });
    await user.click(second);
    await screen.findByRole('group', { name: 'Transcript excerpt at 0:27:05' });
    expect(screen.getAllByRole('group', { name: /excerpt/ })).toHaveLength(1);
    expect(first).toHaveAttribute('aria-expanded', 'false');
    expect(second).toHaveAttribute('aria-expanded', 'true');
  });

  it('closes on Escape and returns focus to the offset that opened it', async () => {
    const user = userEvent.setup();
    withExcerpts(slip);
    const link = screen.getAllByRole('button', { name: /^Transcript at/ })[0];
    await user.click(link);
    await screen.findByRole('group', { name: 'Transcript excerpt at 0:12:41' });
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('group', { name: /excerpt/ })).toBeNull();
    expect(link).toHaveFocus();
    expect(link).toHaveAttribute('aria-expanded', 'false');
  });

  it('closes with its Close button and returns focus to the offset that opened it', async () => {
    const user = userEvent.setup();
    withExcerpts(slip);
    const link = screen.getAllByRole('button', { name: /^Transcript at/ })[1];
    await user.click(link);
    const excerpt = await screen.findByRole('group', { name: 'Transcript excerpt at 0:27:05' });
    await user.click(within(excerpt).getByRole('button', { name: 'Close' }));
    expect(screen.queryByRole('group', { name: /excerpt/ })).toBeNull();
    expect(link).toHaveFocus();
  });

  it('headlines the transcript with its title and offset and sets each line with its speaker', async () => {
    const user = userEvent.setup();
    withExcerpts(slip);
    await user.click(screen.getAllByRole('button', { name: /^Transcript at/ })[0]);
    const excerpt = await screen.findByRole('group', { name: 'Transcript excerpt at 0:12:41' });
    await within(excerpt).findByText(/Transcript · Q4 launch weekly sync ·/);
    expect(excerpt).toHaveTextContent('Transcript · Q4 launch weekly sync · 0:12:41');
    expect(excerpt).toHaveTextContent('0:12:58');
  });

  it('tags the speaker and the line lang="th" when they are Thai', async () => {
    const user = userEvent.setup();
    withExcerpts(slip);
    await user.click(screen.getAllByRole('button', { name: /^Transcript at/ })[0]);
    const excerpt = await screen.findByRole('group', { name: 'Transcript excerpt at 0:12:41' });
    expect(await within(excerpt).findByText('พิมพ์ชนก สุขใจ')).toHaveAttribute('lang', 'th');
    expect(within(excerpt).getByText('งั้นเราเลื่อนวันเปิดตัวไปเป็น 15 พฤศจิกายนนะคะ')).toHaveAttribute('lang', 'th');
  });

  it('leaves out the speaker and the line offset a recording-only transcript does not have', async () => {
    const user = userEvent.setup();
    const load = vi.fn(() =>
      Promise.resolve<Excerpt>({
        kind: 'transcript',
        title: 'Warehouse call',
        offsetMs: 120000,
        lines: [{ speaker: null, offsetMs: null, text: 'We can take the Bang Na slot.' }],
      }),
    );
    withExcerpts(
      <ExcerptRow>
        <OffsetLink source={{ ...meeting, offsetMs: 120000 }} />
      </ExcerptRow>,
      load,
    );
    await user.click(screen.getByRole('button', { name: 'Transcript at 0:02:00' }));
    const excerpt = await screen.findByRole('group', { name: 'Transcript excerpt at 0:02:00' });
    await within(excerpt).findByText('We can take the Bang Na slot.');
    expect(excerpt).not.toHaveTextContent('0:00:00');
    expect(within(excerpt).getAllByText(/0:02:00/)).toHaveLength(1);
  });

  it('shows the pending row while the excerpt loads and ignores a load that lost to a newer press', async () => {
    const user = userEvent.setup();
    const pending = new Map<number, (excerpt: Excerpt) => void>();
    const load = vi.fn(
      (source: Source) =>
        new Promise<Excerpt>((resolve) => {
          pending.set(source.offsetMs ?? 0, resolve);
        }),
    );
    withExcerpts(slip, load);
    const [first, second] = screen.getAllByRole('button', { name: /^Transcript at/ });
    await user.click(first);
    expect(await screen.findByRole('status')).toHaveTextContent('Loading the excerpt');
    await user.click(second);
    pending.get(761000)?.(transcript(761000));
    pending.get(1625000)?.(transcript(1625000));
    const excerpt = await screen.findByRole('group', { name: 'Transcript excerpt at 0:27:05' });
    await within(excerpt).findByText('0:27:22');
    expect(excerpt).not.toHaveTextContent('0:12:58');
  });

  it('says so, with Close, when the excerpt cannot be loaded', async () => {
    const user = userEvent.setup();
    withExcerpts(
      <ExcerptRow>
        <OffsetLink source={meeting} />
      </ExcerptRow>,
      () => Promise.reject(new Error('unavailable')),
    );
    await user.click(screen.getByRole('button', { name: 'Transcript at 0:12:41' }));
    const excerpt = await screen.findByRole('group', { name: 'Transcript excerpt at 0:12:41' });
    expect(await within(excerpt).findByRole('alert')).toHaveTextContent("Couldn't load this excerpt.");
    await user.click(within(excerpt).getByRole('button', { name: 'Close' }));
    expect(screen.queryByRole('group')).toBeNull();
  });
});

describe('PageLink', () => {
  it('PageLink_OpensDocumentExcerptWithPageOfN', async () => {
    const user = userEvent.setup();
    const load = vi.fn(() =>
      Promise.resolve<Excerpt>({
        kind: 'document',
        file: 'ACME-contract-v3.pdf',
        ref: 'DOC-0371',
        page: 4,
        pages: 18,
        quote: 'The agreement renews automatically for one year unless notice is given 60 days ahead.',
      }),
    );
    withExcerpts(
      <ExcerptRow>
        <PageLink source={file} />
      </ExcerptRow>,
      load,
    );
    const link = screen.getByRole('button', { name: 'ACME-contract-v3.pdf, page 4' });
    expect(link).toHaveTextContent('p. 4');
    await user.click(link);
    const excerpt = await screen.findByRole('group', { name: 'Document excerpt, ACME-contract-v3.pdf page 4' });
    await within(excerpt).findByText(/renews automatically/);
    expect(excerpt).toHaveTextContent('Document · ACME-contract-v3.pdf · p. 4');
    expect(excerpt).toHaveTextContent('Page 4 of 18 · REF DOC-0371');
    expect(load).toHaveBeenCalledWith(file);
  });

  it('leaves out "of N" when the page count is not known', async () => {
    const user = userEvent.setup();
    withExcerpts(
      <ExcerptRow>
        <PageLink source={file} />
      </ExcerptRow>,
      () => Promise.resolve({ kind: 'document', file: 'ACME-contract-v3.pdf', ref: 'DOC-0371', page: 4 }),
    );
    await user.click(screen.getByRole('button', { name: 'ACME-contract-v3.pdf, page 4' }));
    const excerpt = await screen.findByRole('group', { name: /Document excerpt/ });
    await within(excerpt).findByText(/REF DOC-0371/);
    expect(excerpt).toHaveTextContent('Page 4 · REF DOC-0371');
    expect(excerpt).not.toHaveTextContent(' of ');
  });

  it('renders nothing for a file source with no page', () => {
    const { container } = withExcerpts(
      <ExcerptRow>
        <PageLink source={{ ...file, page: undefined }} />
      </ExcerptRow>,
    );
    expect(screen.queryByRole('button')).toBeNull();
    expect(container.querySelector('.kiwi-ts')).toBeNull();
  });
});

describe('MessageLink', () => {
  it('MessageLink_OpensMessageExcerptInPlace', async () => {
    const user = userEvent.setup();
    const load = vi.fn(() =>
      Promise.resolve<Excerpt>({
        kind: 'message',
        channel: 'q4-launch',
        day: 'Mon 12 Oct',
        time: '14:02',
        author: 'Arthit K.',
        text: 'ยืนยันแล้วครับ คลังบางนาว่างช่วงวันที่ 10 ถึง 14 พฤศจิกายน',
      }),
    );
    withExcerpts(
      <div data-testid="source-line">
        From{' '}
        <ExcerptRow>
          <MessageLink source={message} channel="q4-launch" />
        </ExcerptRow>
      </div>,
      load,
    );
    const link = screen.getByRole('button', { name: 'Message in #q4-launch at 14:02' });
    expect(link).toHaveTextContent('#q4-launch');
    expect(link).not.toHaveClass('font-mono');
    await user.click(link);
    expect(link).toHaveAttribute('aria-expanded', 'true');
    const excerpt = await screen.findByRole('group', { name: /Message excerpt/ });
    await within(excerpt).findByText('Arthit K.');
    expect(excerpt).toHaveTextContent('Message · #q4-launch · Mon 12 Oct 14:02');
    expect(within(excerpt).getByText('ยืนยันแล้วครับ คลังบางนาว่างช่วงวันที่ 10 ถึง 14 พฤศจิกายน')).toHaveAttribute('lang', 'th');
    expect(screen.getByTestId('source-line')).toContainElement(excerpt);
    expect(load).toHaveBeenCalledWith(message);
  });

  it('omits the day from the header when the message is from today', async () => {
    const user = userEvent.setup();
    withExcerpts(
      <ExcerptRow>
        <MessageLink source={message} channel="q4-launch" />
      </ExcerptRow>,
      () =>
        Promise.resolve({
          kind: 'message',
          channel: 'q4-launch',
          time: '14:02',
          author: 'Arthit K.',
          text: 'Confirmed.',
        }),
    );
    await user.click(screen.getByRole('button', { name: 'Message in #q4-launch at 14:02' }));
    const excerpt = await screen.findByRole('group', { name: /Message excerpt/ });
    await within(excerpt).findByText('Confirmed.');
    expect(excerpt).toHaveTextContent('Message · #q4-launch · 14:02');
  });
});

describe('ProofSlip', () => {
  const props = {
    ref: 'MTG-1014',
    title: 'Q4 launch weekly sync',
    meta: 'Google Meet · 42 min',
    summary: 'The team moved the launch to 15 Nov 2026.',
    tracks,
    sources: [meeting],
    generatedAt: '2026-10-11T07:31:00Z',
    meetingId: 'meeting-1',
  };

  it('shows its REF, a perforation and the Generated time as a wall clock', () => {
    const { container } = withExcerpts(<ProofSlip {...props} />);
    expect(screen.getByText('REF MTG-1014')).toBeInTheDocument();
    expect(container.querySelector('.kiwi-perf')).not.toBeNull();
    expect(screen.getByText(/^Generated/)).toHaveTextContent('Generated 14:31');
  });

  it('lists its title, meta, summary and sources', () => {
    withExcerpts(<ProofSlip {...props} />);
    expect(screen.getByRole('heading', { name: 'Q4 launch weekly sync' })).toBeInTheDocument();
    expect(screen.getByText('Google Meet · 42 min')).toBeInTheDocument();
    expect(screen.getByText('The team moved the launch to 15 Nov 2026.')).toBeInTheDocument();
    const sources = screen.getByRole('list', { name: 'Sources' });
    expect(within(sources).getByText('Q4 launch weekly sync')).toBeInTheDocument();
  });

  it('puts every track in its own row with a right-hand offset button and tags the label by kind', () => {
    withExcerpts(<ProofSlip {...props} />);
    const rows = screen.getAllByRole('listitem').filter((row) => row.classList.contains('kiwi-track'));
    expect(rows).toHaveLength(3);
    expect(rows[0]).toHaveAttribute('data-kind', 'decision');
    expect(rows[2]).toHaveAttribute('data-kind', 'open_question');
    expect(within(rows[2]).getByText('Open question')).toBeInTheDocument();
    expect(within(rows[0]).getByRole('button', { name: 'Transcript at 0:12:41' })).toBeInTheDocument();
  });

  it('keeps a repeated track label for assistive tech but hides it visually on wide screens', () => {
    withExcerpts(<ProofSlip {...props} />);
    const decisions = screen.getAllByText('Decision');
    expect(decisions).toHaveLength(2);
    expect(decisions[0]).not.toHaveClass('kiwi-dup');
    expect(decisions[1]).toHaveClass('kiwi-dup');
  });

  it('renders no button for a track whose offset is null', () => {
    withExcerpts(
      <ProofSlip
        {...props}
        tracks={[{ kind: 'decision', text: 'Move the launch', offsetMs: null, entryIdx: null }]}
        sources={[]}
      />,
    );
    expect(screen.queryByRole('button')).toBeNull();
    expect(screen.queryByText('0:00:00')).toBeNull();
  });

  it('tags Thai in its title, summary, tracks and sources lang="th"', () => {
    withExcerpts(
      <ProofSlip
        {...props}
        title="ประชุมเปิดตัวไตรมาสสี่"
        summary="ทีมเลื่อนวันเปิดตัวไปเป็นวันที่ 15 พฤศจิกายน"
        tracks={[{ kind: 'decision', text: 'เลื่อนเปิดตัวไป 15 พฤศจิกายน', offsetMs: 761000, entryIdx: 12 }]}
        sources={[{ ...meeting, label: 'ประชุมประจำสัปดาห์' }]}
      />,
    );
    expect(screen.getByText('ประชุมเปิดตัวไตรมาสสี่')).toHaveAttribute('lang', 'th');
    expect(screen.getByText('ทีมเลื่อนวันเปิดตัวไปเป็นวันที่ 15 พฤศจิกายน')).toHaveAttribute('lang', 'th');
    expect(screen.getByText('เลื่อนเปิดตัวไป 15 พฤศจิกายน')).toHaveAttribute('lang', 'th');
    expect(screen.getByText('ประชุมประจำสัปดาห์')).toHaveAttribute('lang', 'th');
  });

  it('sets the drafts-to-confirm footer only when it knows the count', () => {
    const { rerender } = withExcerpts(<ProofSlip {...props} />);
    expect(screen.queryByText(/to confirm/)).toBeNull();
    expect(screen.queryByText(/No drafts waiting/)).toBeNull();
    rerender(
      <ExcerptProvider load={loadTranscript()}>
        <ProofSlip {...props} draftCount={3} />
      </ExcerptProvider>,
    );
    expect(screen.getByText(/drafts to confirm/)).toHaveTextContent('3 drafts to confirm');
    rerender(
      <ExcerptProvider load={loadTranscript()}>
        <ProofSlip {...props} draftCount={1} />
      </ExcerptProvider>,
    );
    expect(screen.getByText(/draft to confirm/)).toHaveTextContent('1 draft to confirm');
    rerender(
      <ExcerptProvider load={loadTranscript()}>
        <ProofSlip {...props} draftCount={0} />
      </ExcerptProvider>,
    );
    expect(screen.getByText('No drafts waiting')).toBeInTheDocument();
  });
});

describe('SourceRow', () => {
  it('shows the marker, the name and the offset of a meeting source', () => {
    withExcerpts(
      <ol>
        <SourceRow n={2} source={meeting} />
      </ol>,
    );
    const row = screen.getByRole('listitem');
    expect(row).toHaveTextContent('[2]');
    expect(row).toHaveTextContent('Q4 launch weekly sync');
    expect(within(row).getByRole('button', { name: 'Transcript at 0:12:41' })).toBeInTheDocument();
  });

  it('shows the page of a file source as a link and the time of a message source as plain mono text', () => {
    withExcerpts(
      <ol>
        <SourceRow n={1} source={file} />
        <SourceRow n={3} source={message} />
      </ol>,
    );
    const [fileRow, messageRow] = screen.getAllByRole('listitem');
    expect(within(fileRow).getByRole('button', { name: 'ACME-contract-v3.pdf, page 4' })).toBeInTheDocument();
    expect(within(messageRow).queryByRole('button')).toBeNull();
    expect(within(messageRow).getByText('14:02')).toHaveClass('font-mono');
  });

  it('has no marker when it is not numbered', () => {
    withExcerpts(
      <ol>
        <SourceRow source={meeting} />
      </ol>,
    );
    expect(screen.getByRole('listitem')).not.toHaveTextContent('[');
  });

  it('hands the press to the host instead of opening the excerpt in place', async () => {
    const user = userEvent.setup();
    const onPress = vi.fn();
    const load = loadTranscript();
    withExcerpts(
      <ol>
        <SourceRow n={2} source={meeting} onPress={onPress} />
      </ol>,
      load,
    );
    await user.click(screen.getByRole('button', { name: 'Transcript at 0:12:41' }));
    expect(onPress).toHaveBeenCalledWith(meeting);
    expect(load).not.toHaveBeenCalled();
    expect(screen.queryByRole('group')).toBeNull();
  });
});

describe('CitationMarker', () => {
  function Answer() {
    const [pressed, setPressed] = useState(false);
    return (
      <ExcerptProvider load={loadTranscript()}>
        <p lang="th">
          สัญญา ACME สิ้นสุดวันที่ 31 ธ.ค. 2569
          <CitationMarker n={1} pressed={pressed} onPress={() => setPressed((on) => !on)} label="ACME-contract-v3.pdf, p. 4" />
        </p>
        <ol>
          <SourceRow n={1} source={file} hit={pressed} />
          <SourceRow n={2} source={meeting} hit={false} />
        </ol>
      </ExcerptProvider>
    );
  }

  it('press sets aria-pressed and marks the matching source row, a second press clears both', async () => {
    const user = userEvent.setup();
    render(<Answer />);
    const marker = screen.getByRole('button', { name: 'Source 1: ACME-contract-v3.pdf, p. 4' });
    const [matching, other] = screen.getAllByRole('listitem');
    expect(marker).toHaveAttribute('aria-pressed', 'false');
    expect(matching).not.toHaveAttribute('data-hit');

    await user.click(marker);
    expect(marker).toHaveAttribute('aria-pressed', 'true');
    expect(matching).toHaveAttribute('data-hit');
    expect(other).not.toHaveAttribute('data-hit');

    await user.click(marker);
    expect(marker).toHaveAttribute('aria-pressed', 'false');
    expect(matching).not.toHaveAttribute('data-hit');
  });

  it('is a Latin marker inside Thai prose, so it carries lang="en" and reads in mono', () => {
    render(<Answer />);
    const marker = screen.getByRole('button', { name: /^Source 1/ });
    expect(marker).toHaveAttribute('lang', 'en');
    expect(marker).toHaveTextContent('[1]');
    expect(marker).toHaveClass('font-mono');
  });
});

describe('FileChip', () => {
  it('shows the file name, its icon and its size', () => {
    const { container } = render(<FileChip name="Onboarding-Q4.pdf" size={14 * 1024} />);
    expect(screen.getByText('Onboarding-Q4.pdf')).toBeInTheDocument();
    expect(screen.getByText('14 KB')).toHaveClass('font-mono');
    expect(container.querySelector('svg[data-icon="file"]')).not.toBeNull();
  });

  it('tags a Thai file name lang="th"', () => {
    render(<FileChip name="สัญญาซื้อขาย.pdf" size={2048} />);
    expect(screen.getByText('สัญญาซื้อขาย')).toHaveAttribute('lang', 'th');
  });
});

describe('Pending', () => {
  it('is a plain sentence with the clock icon and the 96 by 8 pulsing bar', () => {
    const { container } = render(<Pending label="Summarizing file…" />);
    expect(screen.getByRole('status')).toHaveTextContent('Summarizing file…');
    expect(container.querySelector('svg[data-icon="clock"]')).not.toBeNull();
    const bar = container.querySelector('.kiwi-pbar');
    expect(bar).not.toBeNull();
    expect(bar).toHaveClass('kiwi-pulse');
    expect(bar).toHaveAttribute('aria-hidden', 'true');
  });

  it('removes the bar animation class under prefers-reduced-motion', () => {
    stubReducedMotion(true);
    const { container } = render(<Pending label="Summarizing file…" />);
    const bar = container.querySelector('.kiwi-pbar');
    expect(bar).not.toBeNull();
    expect(bar).not.toHaveClass('kiwi-pulse');
  });

  it('keeps the animation when the user has not asked for reduced motion', () => {
    stubReducedMotion(false);
    const { container } = render(<Pending label="Summarizing file…" />);
    expect(container.querySelector('.kiwi-pbar')).toHaveClass('kiwi-pulse');
  });

  it('takes a ready-made label, so a screen can tag the Latin words of its own Thai sentence', () => {
    render(
      <Pending
        label={
          <>
            กำลังเปลี่ยนเส้นทางไปยัง <span lang="en">Google</span>…
          </>
        }
      />,
    );
    expect(screen.getByRole('status')).toHaveTextContent('กำลังเปลี่ยนเส้นทางไปยัง Google…');
    expect(screen.getByText('Google')).toHaveAttribute('lang', 'en');
  });

  it('sits inside a slip as a PendingSlip, without a perforation or a REF (nothing was issued yet)', () => {
    const { container } = render(<PendingSlip label="Summarizing file…" />);
    expect(screen.getByRole('status')).toHaveTextContent('Summarizing file…');
    expect(container.querySelector('.kiwi-slip')).not.toBeNull();
    expect(container.querySelector('.kiwi-perf')).toBeNull();
    expect(screen.queryByText(/REF/)).toBeNull();
  });
});

describe('PendingSteps', () => {
  it('PendingSteps_RetrievingWritingAndNoSpinner', () => {
    const { container, rerender } = render(<PendingSteps phase="retrieving" channelCount={5} />);
    const status = screen.getByRole('status');
    expect(status).toHaveTextContent('Retrieving from 5 channels…');
    expect(status).toHaveTextContent('Writing the answer');
    const [retrieving, writing] = within(status).getAllByRole('listitem');
    expect(retrieving).toHaveAttribute('aria-current', 'step');
    expect(retrieving.querySelector('svg[data-icon="clock"]')).not.toBeNull();
    expect(retrieving.querySelector('.kiwi-pbar')).not.toBeNull();
    expect(writing).not.toHaveAttribute('aria-current');
    expect(writing.querySelector('svg[data-icon="circle"]')).not.toBeNull();
    expect(writing.querySelector('.kiwi-pbar')).toBeNull();

    rerender(<PendingSteps phase="writing" channelCount={5} />);
    const [retrieved, writingNow] = within(screen.getByRole('status')).getAllByRole('listitem');
    expect(retrieved).toHaveTextContent('Retrieved from 5 channels');
    expect(retrieved).not.toHaveTextContent('…');
    expect(retrieved.querySelector('svg[data-icon="check"]')).not.toBeNull();
    expect(retrieved).not.toHaveAttribute('aria-current');
    expect(writingNow).toHaveAttribute('aria-current', 'step');
    expect(writingNow).toHaveTextContent('Writing the answer…');
    expect(writingNow.querySelector('svg[data-icon="clock"]')).not.toBeNull();
    expect(writingNow.querySelector('.kiwi-pbar')).not.toBeNull();

    expect(screen.queryByRole('progressbar')).toBeNull();
    expect(container.querySelector('[class*="spin"]')).toBeNull();
    expect(container.querySelector('[data-icon="spinner"], [data-icon="loader"]')).toBeNull();
  });

  it('counts channels in mono and says channel for one', () => {
    const { container } = render(<PendingSteps phase="retrieving" channelCount={1} />);
    expect(screen.getByRole('status')).toHaveTextContent('Retrieving from 1 channel…');
    expect(container.querySelector('.font-mono')).toHaveTextContent('1');
  });
});

describe('FailedSlip', () => {
  it('states the reason, offers Retry and Failed hh:mm, keeps its perforation and issues no REF', async () => {
    const user = userEvent.setup();
    const onRetry = vi.fn();
    const { container } = render(
      <FailedSlip
        reason="Couldn't summarize warehouse-slots.csv: the model did not answer in time."
        failedAt="2026-10-11T07:31:00Z"
        onRetry={onRetry}
      />,
    );
    expect(screen.getByText(/Couldn't summarize warehouse-slots.csv/)).toBeInTheDocument();
    expect(screen.queryByText(/REF/)).toBeNull();
    expect(screen.getByText(/^Failed/)).toHaveTextContent('Failed 14:31');
    expect(container.querySelector('.kiwi-perf')).not.toBeNull();
    expect(container.querySelector('.kiwi-slip')).toHaveAttribute('data-tone', 'failed');
    await user.click(screen.getByRole('button', { name: 'Retry' }));
    expect(onRetry).toHaveBeenCalledTimes(1);
  });

  it('tags a Thai file name inside the reason lang="th"', () => {
    render(<FailedSlip reason="สรุป สัญญา.pdf ไม่สำเร็จ" failedAt="2026-10-11T07:31:00Z" onRetry={() => undefined} />);
    expect(screen.getByText('สรุป สัญญา.pdf ไม่สำเร็จ')).toHaveAttribute('lang', 'th');
  });
});
