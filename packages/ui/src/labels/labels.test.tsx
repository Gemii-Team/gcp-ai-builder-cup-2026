// @vitest-environment jsdom
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { ReactElement } from 'react';
import { describe, expect, it } from 'vitest';
import { Seal } from '../marks';
import { CitationMarker } from '../slip/CitationMarker';
import { ExcerptProvider, ExcerptRow, type Excerpt, type Source } from '../slip/ExcerptContext';
import { FailedSlip } from '../slip/FailedSlip';
import { FileChip } from '../slip/FileChip';
import { PendingSlip } from '../slip/PendingSlip';
import { PendingSteps } from '../slip/PendingSteps';
import { ProofSlip } from '../slip/ProofSlip';
import { OffsetLink, PageLink } from '../slip/SourceLinks';
import { SourceRow } from '../slip/SourceRow';
import { en } from './en';
import { LabelsProvider, useLabels, type Labels, type UiLang } from './index';
import { th } from './th';

type Table = Record<string, string | { one: string; other: string }>;

/** Every string of a table by key path: a plural key contributes `key.one` and `key.other`. */
function entries(table: Table): [string, string][] {
  return Object.entries(table).flatMap(([key, value]) =>
    typeof value === 'string'
      ? ([[key, value]] as [string, string][])
      : ([[`${key}.one`, value.one], [`${key}.other`, value.other]] as [string, string][]),
  );
}

const placeholders = (text: string) => [...text.matchAll(/\{(\w+)\}/g)].map((match) => match[1]).sort();

describe('UiLabels', () => {
  it('UiLabels_ThaiHasEveryEnglishKey', () => {
    const english = entries(en);
    const thai = entries(th);
    expect(thai.map(([key]) => key).sort()).toEqual(english.map(([key]) => key).sort());
    for (const [key, value] of [...english, ...thai]) {
      expect(value.trim(), key).not.toBe('');
    }
  });

  it('gives each Thai string the same {placeholders} as its English source, so no parameter is dropped', () => {
    const thai = new Map(entries(th));
    for (const [key, value] of entries(en)) {
      expect(placeholders(thai.get(key) ?? ''), key).toEqual(placeholders(value));
    }
  });

  it('writes Thai that is Thai: every Thai string has a Thai character, and every <en> marker is closed', () => {
    for (const [key, value] of entries(th)) {
      expect(value, key).toMatch(/[\u0E00-\u0E7F]/);
      expect(value.match(/<en>/g)?.length ?? 0, key).toBe(value.match(/<\/en>/g)?.length ?? 0);
    }
  });

  it('has English plural forms and no Thai ones: Thai repeats one text for both', () => {
    expect(en.draftsToConfirm.one).not.toBe(en.draftsToConfirm.other);
    expect(th.draftsToConfirm.one).toBe(th.draftsToConfirm.other);
    expect(th.retrieving.one).toBe(th.retrieving.other);
  });
});

/** The English fragments of every fixed label: what a Thai screen must never show. */
const ENGLISH_FRAGMENTS = [
  ...new Set(
    entries(en)
      .flatMap(([, value]) => value.split(/\{\w+\}/))
      .map((fragment) => fragment.replace(/[·#,…]/g, ' ').trim())
      .filter((fragment) => /[A-Za-z]{3,}/.test(fragment)),
  ),
];

/** Everything a person or a screen reader can read from the markup: text and every accessible name. */
function readable(root: HTMLElement): string {
  const names = [...root.querySelectorAll('[aria-label], [title]')].flatMap((element) => [
    element.getAttribute('aria-label') ?? '',
    element.getAttribute('title') ?? '',
  ]);
  return [root.textContent, ...names].join('\n');
}

const transcript = (offsetMs: number): Excerpt => ({
  kind: 'transcript',
  title: 'ประชุมประจำสัปดาห์',
  offsetMs,
  lines: [{ speaker: 'พิมพ์ชนก สุขใจ', offsetMs, text: 'งั้นเราเลื่อนวันเปิดตัวไปเป็นวันที่สิบห้า' }],
});

const meeting: Source = { kind: 'meeting', id: 'm1', label: 'ประชุมประจำสัปดาห์', channelId: 'c1', offsetMs: 761000, entryIdx: 12 };
const file: Source = { kind: 'file', id: 'f1', label: 'สัญญาซื้อขาย.pdf', channelId: 'c1', page: 4 };

function inThai(ui: ReactElement, load = (source: Source) => Promise.resolve<Excerpt>(transcript(source.offsetMs ?? 0))) {
  return render(
    <LabelsProvider lang="th">
      <ExcerptProvider load={load}>{ui}</ExcerptProvider>
    </LabelsProvider>,
  );
}

describe('under LabelsProvider lang="th"', () => {
  it('ProofSlip shows no English label: Generated, tracks, sources, drafts footer and offset names are Thai', () => {
    const { container } = inThai(
      <ProofSlip
        ref="MTG-1014"
        title="ประชุมไตรมาสสี่"
        summary="ทีมเลื่อนวันเปิดตัวไปเป็นวันที่สิบห้า"
        tracks={[
          { kind: 'decision', text: 'เลื่อนเปิดตัวไปวันที่สิบห้า', offsetMs: 761000, entryIdx: 12 },
          { kind: 'decision', text: 'ใช้ขนส่งเจ้าเดิมต่อไป', offsetMs: 1625000, entryIdx: 27 },
          { kind: 'open_question', text: 'ใครอนุมัติงบการตลาดเพิ่ม', offsetMs: 1998000, entryIdx: 33 },
        ]}
        sources={[meeting]}
        generatedAt="2026-10-11T07:31:00Z"
        draftCount={3}
        meetingId="m1"
      />,
    );
    expect(screen.getByText('REF MTG-1014')).toBeInTheDocument();
    expect(screen.getByText(/สร้างเมื่อ/)).toHaveAttribute('lang', 'th');
    expect(container).toHaveTextContent('สร้างเมื่อ 14:31');
    expect(screen.getAllByText('ข้อตัดสินใจ')).toHaveLength(2);
    expect(screen.getByText('คำถามที่ค้างอยู่')).toHaveAttribute('lang', 'th');
    expect(container).toHaveTextContent('3 ฉบับร่างรอยืนยัน');
    expect(screen.getByRole('list', { name: 'แหล่งที่มา' })).toBeInTheDocument();
    expect(screen.getAllByRole('button', { name: 'บทถอดเสียงที่ 0:12:41' }).length).toBeGreaterThan(0);
    for (const fragment of ENGLISH_FRAGMENTS) {
      expect(readable(container), fragment).not.toContain(fragment);
    }
  });

  it('ProofSlip says there are no drafts in Thai when the count is zero', () => {
    const { container } = inThai(
      <ProofSlip ref="MTG-1014" summary="ทีมสรุปแล้ว" sources={[]} generatedAt="2026-10-11T07:31:00Z" draftCount={0} />,
    );
    expect(screen.getByText('ไม่มีฉบับร่างที่รออยู่')).toHaveAttribute('lang', 'th');
    for (const fragment of ENGLISH_FRAGMENTS) {
      expect(readable(container), fragment).not.toContain(fragment);
    }
  });

  it('FailedSlip shows Retry and Failed hh:mm in Thai and still issues no REF', () => {
    const { container } = inThai(
      <FailedSlip reason="สรุปไฟล์ไม่สำเร็จ โมเดลไม่ตอบกลับทันเวลา" failedAt="2026-10-11T07:31:00Z" onRetry={() => undefined} />,
    );
    expect(screen.getByRole('button', { name: 'ลองอีกครั้ง' })).toBeInTheDocument();
    expect(screen.getByText(/ล้มเหลวเมื่อ/)).toHaveAttribute('lang', 'th');
    expect(container).toHaveTextContent('ล้มเหลวเมื่อ 14:31');
    expect(screen.queryByText(/REF/)).toBeNull();
    for (const fragment of ENGLISH_FRAGMENTS) {
      expect(readable(container), fragment).not.toContain(fragment);
    }
  });

  it('PendingSlip adds no English of its own around the caller\'s label', () => {
    const { container } = inThai(<PendingSlip label="กำลังสรุปไฟล์…" />);
    expect(screen.getByRole('status')).toHaveTextContent('กำลังสรุปไฟล์…');
    for (const fragment of ENGLISH_FRAGMENTS) {
      expect(readable(container), fragment).not.toContain(fragment);
    }
  });

  it('PendingSteps names the progress and its steps in Thai, with one noun for every count', () => {
    const { container, rerender } = inThai(<PendingSteps phase="retrieving" channelCount={1} />);
    const thai = (phase: 'retrieving' | 'writing', channelCount: number) => (
      <LabelsProvider lang="th">
        <PendingSteps phase={phase} channelCount={channelCount} />
      </LabelsProvider>
    );
    expect(screen.getByRole('status', { name: 'ความคืบหน้าของคำตอบ' })).toHaveTextContent('กำลังค้นหาจาก 1 ช่องสนทนา…');
    rerender(thai('retrieving', 5));
    expect(screen.getByRole('status')).toHaveTextContent('กำลังค้นหาจาก 5 ช่องสนทนา…');
    rerender(thai('writing', 5));
    expect(screen.getByRole('status')).toHaveTextContent('ค้นหาจาก 5 ช่องสนทนาแล้ว');
    expect(screen.getByRole('status')).toHaveTextContent('กำลังเขียนคำตอบ…');
    for (const fragment of ENGLISH_FRAGMENTS) {
      expect(readable(container), fragment).not.toContain(fragment);
    }
  });

  it('OffsetLink is named in Thai and opens an excerpt whose group, header and Close are Thai', async () => {
    const user = userEvent.setup();
    const { container } = inThai(
      <ExcerptRow>
        <OffsetLink source={meeting} />
      </ExcerptRow>,
    );
    const link = screen.getByRole('button', { name: 'บทถอดเสียงที่ 0:12:41' });
    expect(link).toHaveTextContent('0:12:41');
    await user.click(link);
    const excerpt = await screen.findByRole('group', { name: 'ข้อความตัดตอนจากบทถอดเสียงที่ 0:12:41' });
    await within(excerpt).findByText('พิมพ์ชนก สุขใจ');
    expect(excerpt).toHaveTextContent('บทถอดเสียง · ประชุมประจำสัปดาห์ · 0:12:41');
    await user.click(within(excerpt).getByRole('button', { name: 'ปิด' }));
    expect(screen.queryByRole('group')).toBeNull();
    for (const fragment of ENGLISH_FRAGMENTS) {
      expect(readable(container), fragment).not.toContain(fragment);
    }
  });

  it('PageLink and its document excerpt read in Thai, with the REF code left literal', async () => {
    const user = userEvent.setup();
    inThai(
      <ExcerptRow>
        <PageLink source={file} />
      </ExcerptRow>,
      () =>
        Promise.resolve<Excerpt>({
          kind: 'document',
          file: 'สัญญาซื้อขาย.pdf',
          ref: 'DOC-0371',
          page: 4,
          pages: 18,
          quote: 'สัญญาต่ออายุอัตโนมัติหนึ่งปี',
        }),
    );
    const link = screen.getByRole('button', { name: 'สัญญาซื้อขาย.pdf หน้า 4' });
    expect(link).toHaveTextContent('หน้า 4');
    await user.click(link);
    const excerpt = await screen.findByRole('group', { name: 'ข้อความตัดตอนจากเอกสาร สัญญาซื้อขาย.pdf หน้า 4' });
    await within(excerpt).findByText('สัญญาต่ออายุอัตโนมัติหนึ่งปี');
    expect(excerpt).toHaveTextContent('เอกสาร · สัญญาซื้อขาย.pdf · หน้า 4');
    expect(excerpt).toHaveTextContent('หน้า 4 จาก 18 · REF DOC-0371');
    expect(within(excerpt).getByText('REF DOC-0371')).toHaveAttribute('lang', 'en');
  });

  it('SourceRow and CitationMarker name their source in Thai', () => {
    const { container } = inThai(
      <>
        <CitationMarker n={1} pressed={false} onPress={() => undefined} label="สัญญาซื้อขาย.pdf หน้า 4" />
        <CitationMarker n={2} pressed={false} onPress={() => undefined} />
        <ol>
          <SourceRow n={1} source={file} />
        </ol>
      </>,
    );
    expect(screen.getByRole('button', { name: 'แหล่งที่มา 1: สัญญาซื้อขาย.pdf หน้า 4' })).toHaveAttribute('lang', 'en');
    expect(screen.getByRole('button', { name: 'แหล่งที่มา 2' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'สัญญาซื้อขาย.pdf หน้า 4' })).toHaveTextContent('หน้า 4');
    for (const fragment of ENGLISH_FRAGMENTS) {
      expect(readable(container), fragment).not.toContain(fragment);
    }
  });

  it('FileChip adds no label of its own and keeps its Thai name tagged', () => {
    const { container } = inThai(<FileChip name="สัญญาซื้อขาย.pdf" size={2048} />);
    expect(screen.getByText('สัญญาซื้อขาย')).toHaveAttribute('lang', 'th');
    for (const fragment of ENGLISH_FRAGMENTS) {
      expect(readable(container), fragment).not.toContain(fragment);
    }
  });

  it('the seal is stamped in Thai', () => {
    inThai(<Seal />);
    const seal = screen.getByRole('img', { name: 'ยืนยันแล้ว' });
    expect(seal).toHaveTextContent('ยืนยันแล้ว');
    expect(seal).not.toHaveTextContent('CONFIRMED');
  });

  it('tags a Latin marker inside a Thai label lang="en" and the Thai around it lang="th"', () => {
    render(
      <LabelsProvider lang="th" labels={{ generated: 'สร้างโดย <en>K.I.W.I.</en> เมื่อ {time}' }}>
        <ExcerptProvider load={() => Promise.reject(new Error('unused'))}>
          <ProofSlip ref="MTG-1014" summary="ทีมสรุปแล้ว" sources={[]} generatedAt="2026-10-11T07:31:00Z" />
        </ExcerptProvider>
      </LabelsProvider>,
    );
    expect(screen.getByText('K.I.W.I.')).toHaveAttribute('lang', 'en');
    expect(screen.getByText('สร้างโดย')).toHaveAttribute('lang', 'th');
    expect(screen.getByText('เมื่อ')).toHaveAttribute('lang', 'th');
    expect(screen.getByText('สร้างโดย').parentElement).toHaveTextContent('สร้างโดย K.I.W.I. เมื่อ 14:31');
  });
});

describe('without a provider, or under lang="en"', () => {
  const slip = (
    <ExcerptProvider load={() => Promise.reject(new Error('unused'))}>
      <ProofSlip
        ref="MTG-1014"
        title="Q4 launch weekly sync"
        summary="The team moved the launch."
        tracks={[{ kind: 'open_question', text: 'Who approves?', offsetMs: 761000, entryIdx: 12 }]}
        sources={[{ kind: 'meeting', id: 'm1', label: 'Q4 launch weekly sync', channelId: 'c1', offsetMs: 761000, entryIdx: 12 }]}
        generatedAt="2026-10-11T07:31:00Z"
        draftCount={3}
        meetingId="m1"
      />
      <FailedSlip reason="Couldn't summarize." failedAt="2026-10-11T07:31:00Z" onRetry={() => undefined} />
      <PendingSteps phase="retrieving" channelCount={5} />
    </ExcerptProvider>
  );

  it('renders English with the P0 strings', () => {
    const { container } = render(slip);
    expect(container).toHaveTextContent('Generated 14:31');
    expect(container).toHaveTextContent('Open question');
    expect(container).toHaveTextContent('3 drafts to confirm');
    expect(container).toHaveTextContent('Failed 14:31');
    expect(container).toHaveTextContent('Retrieving from 5 channels…');
    expect(screen.getAllByRole('button', { name: 'Transcript at 0:12:41' }).length).toBeGreaterThan(0);
    expect(screen.getByRole('list', { name: 'Sources' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Retry' })).toBeInTheDocument();
    expect(screen.getByRole('status', { name: 'Answer progress' })).toBeInTheDocument();
  });

  it('renders the same markup under an explicit English provider as without one', () => {
    const bare = render(slip);
    const plain = bare.container.innerHTML;
    bare.unmount();
    const explicit = render(<LabelsProvider lang="en">{slip}</LabelsProvider>);
    expect(explicit.container.innerHTML).toBe(plain);
  });

  it('takes a partial override: an explicit label wins over the table', () => {
    render(
      <LabelsProvider lang="en" labels={{ retry: 'Try again' }}>
        <FailedSlip reason="Couldn't summarize." failedAt="2026-10-11T07:31:00Z" onRetry={() => undefined} />
      </LabelsProvider>,
    );
    expect(screen.getByRole('button', { name: 'Try again' })).toBeInTheDocument();
    expect(screen.getByText(/^Failed/)).toHaveTextContent('Failed 14:31');
  });

  it('keeps an explicit CitationMarker label ahead of the generic Source n name', () => {
    render(<CitationMarker n={2} pressed={false} onPress={() => undefined} label="Q4 sync" />);
    expect(screen.getByRole('button', { name: 'Source 2: Q4 sync' })).toBeInTheDocument();
  });

  it('stamps the seal CONFIRMED and names it Confirmed', () => {
    render(<Seal />);
    expect(screen.getByRole('img', { name: 'Confirmed' })).toHaveTextContent('CONFIRMED');
  });
});

describe('useLabels', () => {
  function Probe({ lang, run }: { lang: UiLang; run: (labels: Labels) => string }) {
    return (
      <LabelsProvider lang={lang}>
        <Inner run={run} />
      </LabelsProvider>
    );
  }
  function Inner({ run }: { run: (labels: Labels) => string }) {
    return <p data-testid="out">{run(useLabels())}</p>;
  }

  it('fills {placeholders} and picks the English plural by Intl.PluralRules', () => {
    render(<Probe lang="en" run={(labels) => [labels.text('draftsToConfirm', { count: 1 }), labels.text('draftsToConfirm', { count: 0 }), labels.text('draftsToConfirm', { count: 7 })].join(' | ')} />);
    expect(screen.getByTestId('out')).toHaveTextContent('1 draft to confirm | 0 drafts to confirm | 7 drafts to confirm');
  });

  it('has no plural split in Thai', () => {
    render(<Probe lang="th" run={(labels) => [labels.text('draftsToConfirm', { count: 1 }), labels.text('draftsToConfirm', { count: 7 })].join(' | ')} />);
    expect(screen.getByTestId('out')).toHaveTextContent('1 ฉบับร่างรอยืนยัน | 7 ฉบับร่างรอยืนยัน');
  });

  it('strips <en> markers from plain text, which is what an aria-label can hold', () => {
    render(
      <LabelsProvider lang="th" labels={{ sources: 'แหล่งที่มา <en>AI</en>' }}>
        <Inner run={(labels) => labels.text('sources')} />
      </LabelsProvider>,
    );
    expect(screen.getByTestId('out')).toHaveTextContent('แหล่งที่มา AI');
    expect(screen.getByTestId('out')).not.toHaveTextContent('<en>');
  });

  it('refuses a label whose placeholder has no value, rather than showing {time}', () => {
    expect(() => render(<Probe lang="en" run={(labels) => labels.text('generated')} />)).toThrow(/time/);
  });
});
