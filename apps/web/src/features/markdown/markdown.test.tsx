import { cleanup, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { CitationView } from '../../api/types';
import { Markdown } from './Markdown';

const CITATIONS: CitationView[] = [
  { n: 1, chunkId: 'c1', quote: 'q1', source: { kind: 'meeting', id: 'meeting-1', label: 'Q4 launch weekly sync', channelId: 'ch-q4', entryIdx: 12, offsetMs: 761000 } },
  { n: 2, chunkId: 'c2', quote: 'q2', source: { kind: 'file', id: 'file-1', label: 'ACME-contract-v3.pdf', channelId: 'ch-q4', page: 4 } },
];

function show(markdown: string, { pressed = null, onPress = vi.fn() }: { pressed?: number | null; onPress?: (n: number) => void } = {}) {
  const view = render(<Markdown markdown={markdown} cited={{ citations: CITATIONS, pressed, onPress, labelOf: (citation) => citation.source.label }} />);
  return { ...view, onPress };
}

afterEach(() => {
  Reflect.deleteProperty(window, 'pwned');
});

// The one renderer of every slip that shows model text (the Ask answer, the file and meeting summaries, the agent's answers
// and channel summaries). The test names that start with AnswerMarkdown_ are those of the Ask answer's first renderer.
describe('Markdown', () => {
  // Raw HTML is inert text: the model's `Result<T>` or `<name>` stays visible, and nothing it writes becomes an element.
  it('AnswerMarkdown_RendersRawHtmlAsText', () => {
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
    expect('pwned' in window).toBe(false);
    // What the model wrote is on the page as the characters it wrote.
    expect(container).toHaveTextContent('Safe sentence.[1]');
    expect(container).toHaveTextContent('<script>window.pwned = true</script>');
    expect(container).toHaveTextContent('Hello <img src="x" onerror="window.pwned = true"> there <b onclick="window.pwned = true">bold</b>.');
    expect(container).toHaveTextContent('<iframe src="https://evil.example"></iframe>');
  });

  it('Markdown_AngleBracketWords_StayVisible', () => {
    const { container } = show('The type is Result<T>, the section is <TBD>, and the file is <name>.\n\n- a generic `List<T>` and a bare <T> in a list');

    expect(container.querySelector('p')).toHaveTextContent('The type is Result<T>, the section is <TBD>, and the file is <name>.');
    expect(container.querySelector('li')).toHaveTextContent('a generic List<T> and a bare <T> in a list');
    expect(container.querySelector('T, TBD, name')).toBeNull();
  });

  it('keeps a link only when it is http(s), opens it safely, and shows any other link as its text', () => {
    const { container } = show(
      '[docs](https://example.com/a) [mail](mailto:a@example.com) [bad](javascript:alert(1)) [data](data:text/html;base64,PHNjcmlwdD4=) ![pixel](https://evil.example/p.png)',
    );

    const links = container.querySelectorAll('a');
    expect([...links].map((link) => link.getAttribute('href'))).toEqual(['https://example.com/a']);
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
    expect([...container.querySelectorAll('.md-heading')].map((heading) => heading.textContent)).toEqual(['Big', 'Small']);
  });

  it('Markdown_List_RendersAListWithItsItems', () => {
    const { container } = show('Key points:\n\n- first\n- second\n- third\n\n1. one\n2. two');

    const [bullets, numbered] = [...container.querySelectorAll('ul, ol')];
    expect(bullets.tagName).toBe('UL');
    expect([...bullets.querySelectorAll(':scope > li')].map((item) => item.textContent)).toEqual(['first', 'second', 'third']);
    expect(numbered.tagName).toBe('OL');
    expect([...numbered.querySelectorAll(':scope > li')].map((item) => item.textContent)).toEqual(['one', 'two']);
    // The dashes were the markup, not the words.
    expect(container).not.toHaveTextContent('- first');
  });

  it('Markdown_Emphasis_RendersBoldItalicAndInlineCode', () => {
    const { container } = show('A **bold** word, an *italic* one and `inline()` code.');

    expect(container.querySelector('strong')).toHaveTextContent('bold');
    expect(container.querySelector('em')).toHaveTextContent('italic');
    expect(container.querySelector('code')).toHaveTextContent('inline()');
    expect(container).not.toHaveTextContent('**');
  });

  // The newline that sits between the elements of a list item is structure, not a line break the model wrote: only a
  // newline inside the words of a line is turned into a `<br>`.
  for (const [name, markdown, items] of [
    ['a loose list', '- a\n\n- b', ['a', 'b']],
    ['a loose numbered list', '1. a\n\n2. b', ['a', 'b']],
    ['a nested list', '- a\n  - b\n- c', ['ab', 'c']],
    ['a flat tight list', '- a\n- b\n- c', ['a', 'b', 'c']],
    ['a loose nested list', '- a\n\n  - b\n\n- c', ['ab', 'c']],
  ] as const) {
    it(`Markdown_${name.replaceAll(' ', '')}_HasNoBreakInItsItems`, () => {
      const { container } = show(markdown);

      expect(container.querySelectorAll('li br')).toHaveLength(0);
      const list = container.querySelector('ul, ol');
      expect([...(list?.querySelectorAll(':scope > li') ?? [])].map((item) => item.textContent.replaceAll(/\s+/g, ''))).toEqual([...items]);
      for (const item of container.querySelectorAll('li')) {
        expect(item.firstChild?.nodeName, 'the first node of an item').not.toBe('BR');
        expect(item.lastChild?.nodeName, 'the last node of an item').not.toBe('BR');
      }
    });
  }

  it('Markdown_ARealBreakInsideAParagraph_IsStillABreakBetweenTheWords', () => {
    for (const markdown of ['a\nb', 'a  \nb', 'a\\\nb']) {
      const { container } = show(markdown);

      expect(container.querySelector('p')?.innerHTML, markdown).toBe('a<br>b');
      cleanup();
    }
  });

  it('Markdown_ARealBreakInsideAListItem_IsStillABreak', () => {
    const { container } = show('- first line\n  second line\n- other');

    const [first, other] = [...container.querySelectorAll('li')];
    expect(first.innerHTML).toBe('first line<br>second line');
    expect(other.querySelector('br')).toBeNull();
  });

  it('Markdown_LineBreaks_Survive', () => {
    const { container } = show('First line\nSecond line\n\nNew paragraph\\\nafter a hard break');

    const [first, second] = [...container.querySelectorAll('p')];
    // HTML folds a newline into a space, so each line break the model wrote is a <br>.
    expect(first.querySelectorAll('br')).toHaveLength(1);
    expect(first.innerHTML).toBe('First line<br>Second line');
    expect(second.querySelectorAll('br')).toHaveLength(1);
    expect(second).toHaveTextContent('New paragraphafter a hard break');
  });

  it('Markdown_Code_KeepsItsOwnNewlinesWithoutBreakTags', () => {
    const { container } = show('```\nline one\nline two\n```');

    expect(container.querySelector('pre code')?.textContent).toBe('line one\nline two\n');
    expect(container.querySelector('pre br')).toBeNull();
  });

  it('Markdown_Image_RendersItsDescriptionAndStartsNoRequest', () => {
    const fetchStub = vi.fn<typeof fetch>(() => Promise.reject(new Error('no request expected')));
    vi.stubGlobal('fetch', fetchStub);
    const imageSrc = vi.spyOn(HTMLImageElement.prototype, 'src', 'set');
    try {
      const { container } = show('Before ![A chart of the quarter](https://evil.example/pixel.png?q=secret) after, and a second ![](https://evil.example/2.png).');

      expect(container.querySelector('img')).toBeNull();
      expect(container).toHaveTextContent('Before A chart of the quarter after, and a second .');
      expect(fetchStub).not.toHaveBeenCalled();
      expect(imageSrc).not.toHaveBeenCalled();
    } finally {
      vi.unstubAllGlobals();
      imageSrc.mockRestore();
    }
  });

  it('Markdown_ScriptAndJavascriptLink_AreNeitherElementsNorLinks', () => {
    const { container } = show('<script>window.pwned = true</script>\n\n[click](javascript:window.pwned=true)\n\n<a href="javascript:window.pwned=true">raw</a>');

    expect(container.querySelector('script, a')).toBeNull();
    expect(container).toHaveTextContent('click');
    expect(container).toHaveTextContent('<script>window.pwned = true</script>');
    expect(container).toHaveTextContent('<a href="javascript:window.pwned=true">raw</a>');
    expect('pwned' in window).toBe(false);
  });

  it('says nothing of citations when it is given none: [n] stays text', () => {
    const { container } = render(<Markdown markdown="A summary.[1]" />);

    expect(screen.queryByRole('button')).not.toBeInTheDocument();
    expect(container).toHaveTextContent('A summary.[1]');
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
