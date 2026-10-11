import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { CitationView } from '../../api/types';
import { AnswerMarkdown } from './AnswerMarkdown';

const CITATIONS: CitationView[] = [
  { n: 1, chunkId: 'c1', quote: 'q1', source: { kind: 'meeting', id: 'meeting-1', label: 'Q4 launch weekly sync', channelId: 'ch-q4', entryIdx: 12, offsetMs: 761000 } },
  { n: 2, chunkId: 'c2', quote: 'q2', source: { kind: 'file', id: 'file-1', label: 'ACME-contract-v3.pdf', channelId: 'ch-q4', page: 4 } },
];

function show(markdown: string, { pressed = null, onPress = vi.fn() }: { pressed?: number | null; onPress?: (n: number) => void } = {}) {
  const view = render(
    <AnswerMarkdown
      markdown={markdown}
      citations={CITATIONS}
      pressed={pressed}
      onPress={onPress}
      labelOf={(citation) => citation.source.label}
    />,
  );
  return { ...view, onPress };
}

afterEach(() => {
  Reflect.deleteProperty(window, 'pwned');
});

describe('AnswerMarkdown', () => {
  it('AnswerMarkdown_StripsScriptAndRawHtml', () => {
    const { container } = show(
      [
        'Safe sentence.[1]',
        '<script>window.pwned = true</script>',
        'Hello <img src="x" onerror="window.pwned = true"> there <b onclick="window.pwned = true">bold</b>.',
        '<iframe src="https://evil.example"></iframe>',
      ].join('\n\n'),
    );

    expect(container.querySelector('script, img, iframe, b')).toBeNull();
    for (const element of container.querySelectorAll('*')) {
      expect([...element.attributes].map((attribute) => attribute.name).filter((name) => name.startsWith('on'))).toEqual([]);
    }
    expect(container).not.toHaveTextContent('<script');
    expect(container).not.toHaveTextContent('window.pwned');
    expect('pwned' in window).toBe(false);
    // What the model wrote as words is kept.
    expect(container).toHaveTextContent('Safe sentence.[1]');
    expect(container).toHaveTextContent('Hello there');
  });

  it('keeps a link only when it is http(s) or mailto, opens it safely, and shows any other link as its text', () => {
    const { container } = show(
      '[docs](https://example.com/a) [mail](mailto:a@example.com) [bad](javascript:alert(1)) [data](data:text/html;base64,PHNjcmlwdD4=) ![pixel](https://evil.example/p.png)',
    );

    const links = container.querySelectorAll('a');
    expect([...links].map((link) => link.getAttribute('href'))).toEqual(['https://example.com/a', 'mailto:a@example.com']);
    for (const link of links) {
      expect(link).toHaveAttribute('rel', 'noopener noreferrer');
      expect(link).toHaveAttribute('target', '_blank');
    }
    expect(container).toHaveTextContent('bad');
    expect(container).toHaveTextContent('data');
    // A picture is never fetched on a model's say-so: its description stands in.
    expect(container.querySelector('img')).toBeNull();
    expect(container).toHaveTextContent('pixel');
  });

  it('turns every heading the model wrote into a bold line, so the page has one outline', () => {
    const { container } = show('# Big\n\n###### Small\n\nBody');

    expect(container.querySelector('h1, h2, h3, h4, h5, h6')).toBeNull();
    expect([...container.querySelectorAll('.am-heading')].map((heading) => heading.textContent)).toEqual(['Big', 'Small']);
  });

  it('AnswerMarkdown_CitationMarkersBecomeButtonsWithLangEn', async () => {
    const user = userEvent.setup();
    const { container, onPress } = show('**Decided:** the launch moved.[1] Also see the contract [2].\n\n- A list item cites [2]\n- Made-up [9] stays text and `[1]` in code too');

    const markers = screen.getAllByRole('button');
    expect(markers.map((marker) => marker.textContent)).toEqual(['[1]', '[2]', '[2]']);
    for (const marker of markers) {
      expect(marker).toHaveAttribute('lang', 'en');
      expect(marker).toHaveAttribute('aria-pressed', 'false');
    }
    expect(markers[0]).toHaveAccessibleName('Source 1: Q4 launch weekly sync');
    expect(markers[1]).toHaveAccessibleName('Source 2: ACME-contract-v3.pdf');
    // A marker inside bold text and one inside a list item are buttons too; `[9]` has no source and `[1]` in code is code.
    expect(within(container.querySelector('li') ?? container).getByRole('button')).toHaveTextContent('[2]');
    expect(container).toHaveTextContent('Made-up [9] stays text');
    expect(container.querySelector('code')).toHaveTextContent('[1]');
    expect(container.querySelector('code button')).toBeNull();

    await user.click(markers[1]);
    expect(onPress).toHaveBeenCalledExactlyOnceWith(2);
  });

  it('marks the pressed citation, and only that one', () => {
    show('One.[1] Two.[2]', { pressed: 2 });

    expect(screen.getByRole('button', { name: /^Source 1/ })).toHaveAttribute('aria-pressed', 'false');
    expect(screen.getByRole('button', { name: /^Source 2/ })).toHaveAttribute('aria-pressed', 'true');
  });

  it('AnswerMarkdown_ThaiAnswerCarriesLangTh', () => {
    const { container } = show('สัญญา ACME ฉบับ v3 สิ้นสุดวันที่ 31 ธ.ค. 2569 และต่ออายุอัตโนมัติ[1] ค่าปรับคือ 2% ต่อวัน[2]');

    // Every Thai run, with the Latin words and digits between its Thai characters, is `lang="th"`; the markers are `lang="en"`.
    const runs = [...container.querySelectorAll('[lang="th"]')].map((run) => run.textContent);
    expect(runs).toEqual(['สัญญา ACME ฉบับ v3 สิ้นสุดวันที่ 31 ธ.ค. 2569 และต่ออายุอัตโนมัติ', 'ค่าปรับคือ 2% ต่อวัน']);
    expect(screen.getAllByRole('button').map((marker) => marker.getAttribute('lang'))).toEqual(['en', 'en']);
    const walker = document.createTreeWalker(container, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
      if (/[\u0E00-\u0E7F]/.test(node.textContent ?? '')) {
        expect(node.parentElement?.closest('[lang]')?.getAttribute('lang')).toBe('th');
      }
    }
  });

  it('tags nothing for an English answer', () => {
    const { container } = show('The launch moved to 15 Nov.[1]');

    expect(container.querySelector('[lang="th"]')).toBeNull();
  });
});
