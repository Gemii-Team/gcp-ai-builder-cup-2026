import { ConnectError, Code } from '@connectrpc/connect';
import { create } from '@bufbuild/protobuf';
import {
  ArtifactKind,
  ListMessagesResponseSchema,
  type SendMessageResponse,
  MembershipChange,
  MembershipChangedSchema,
  MentionKind,
  SendMessageResponseSchema,
  SourceKind,
  TrackKind,
  type SendMessageRequest,
} from '@kiwi/api-client';
import { ExcerptProvider, type Excerpt } from '@kiwi/ui';
import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useUiStore } from '../../state/ui';
import { agentMessage, artifact, channel, frame, message, ME_ID } from '../../testing/fixtures';
import { createStubApi, seedApi, type StubApi, type World } from '../../testing/stubApi';
import { renderApp, renderPlain } from '../../testing/render';
import { mapMessage } from '../../api/mappers';
import { ChannelScreen } from './ChannelScreen';
import { Composer, type MentionTarget } from './Composer';
import { MessageRow } from './MessageRow';

const LOG = { name: 'Messages in #q4-launch' };
const PEOPLE: MentionTarget[] = [
  { kind: 'agent', id: 'assistant', name: 'assistant' },
  { kind: 'user', id: 'user-arthit', name: 'Arthit K.' },
  { kind: 'user', id: 'user-nok', name: 'Nok' },
];

beforeEach(() => {
  localStorage.clear();
  useUiStore.setState({ lang: 'en', readSeq: {}, lastChannelId: null, draftsSheetOpen: false });
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.useRealTimers();
});

/** The channel screen in the shell, as the `/c/$channelId` route mounts it. `messages` are newest first, as the server sends them. */
function openChannel(world: World = {}, api: StubApi = createStubApi()) {
  seedApi(api, { channels: [channel({ id: 'ch-q4', name: 'q4-launch', lastSeq: 5n })], ...world });
  return renderApp({ path: '/c/ch-q4', api, routes: [{ path: '/c/$channelId', element: <ChannelScreen channelId="ch-q4" /> }] });
}

const sentRequest = (api: StubApi, call = 0): SendMessageRequest => {
  const request = api.chat.sendMessage.mock.calls[call]?.[0];
  if (request === undefined) {
    throw new Error('SendMessage was not called');
  }
  return request as SendMessageRequest;
};

const noExcerpt = () => Promise.reject(new Error('no excerpt expected'));

describe('MessageRow', () => {
  it('MessageRow_ThaiBodyCarriesLangTh', () => {
    renderPlain(
      <ExcerptProvider load={noExcerpt}>
        <MessageRow
          message={mapMessage(message({ seq: 1, body: 'ยืนยันแล้วครับ คลังบางนาว่างช่วง 10–14 พ.ย. ok' }))}
          authorName="อาทิตย์"
        />
      </ExcerptProvider>,
    );

    const body = screen.getByText(/ยืนยันแล้วครับ/);
    expect(body).toHaveAttribute('lang', 'th');
    expect(body).toHaveTextContent('ยืนยันแล้วครับ คลังบางนาว่างช่วง 10–14 พ.ย');
    expect(screen.getByText('อาทิตย์')).toHaveAttribute('lang', 'th');
  });

  it('leaves an English body untagged and marks a mention', () => {
    renderPlain(
      <ExcerptProvider load={noExcerpt}>
        <MessageRow
          message={mapMessage(message({ seq: 2, body: 'Hello @assistant please check', mentions: [{ kind: MentionKind.AGENT, id: 'assistant', label: 'assistant' }] }))}
          authorName="Arthit K."
        />
      </ExcerptProvider>,
    );

    expect(screen.getByText('@assistant')).toHaveClass('at');
    expect(screen.getByText(/Hello/).closest('[lang="th"]')).toBeNull();
  });

  it('MessageRow_ArtifactRendersProofSlipWithMark', async () => {
    const summary = artifact({
      kind: ArtifactKind.MEETING_SUMMARY,
      ref: 'MTG-1014',
      title: 'Q4 launch weekly sync',
      meta: 'Google Meet · 42 min',
      bodyMarkdown: 'The team moved the launch to 15 Nov 2026.',
      meetingId: 'meeting-1',
      tracks: [
        { kind: TrackKind.DECISION, text: 'Move the launch to 15 Nov 2026', offsetMs: 761000n, entryIdx: 12 },
        { kind: TrackKind.OPEN_QUESTION, text: 'Who approves the budget?' },
      ],
      sources: [{ kind: SourceKind.MEETING, id: 'meeting-1', anchor: '12', label: 'Q4 launch weekly sync', channelId: 'ch-q4', offsetMs: 761000n }],
    });
    const load = vi.fn(() =>
      Promise.resolve<Excerpt>({ kind: 'transcript', title: 'Q4 launch weekly sync', offsetMs: 761000, lines: [{ speaker: 'Pim', offsetMs: 761000, text: 'Move it' }] }),
    );
    const { user } = renderPlain(
      <ExcerptProvider load={load}>
        <MessageRow message={mapMessage(agentMessage({ seq: 3, time: '14:31', artifact: summary }))} authorName="" draftCount={3} />
      </ExcerptProvider>,
    );

    // The AI mark stands in the avatar; the author line names K.I.W.I. and the kind of slip.
    const row = screen.getByRole('article');
    expect(row.querySelector('svg[viewBox="0 0 16 16"]')).toBeInTheDocument();
    expect(within(row).getByText('K.I.W.I.')).toBeInTheDocument();
    expect(within(row).getByText('meeting summary')).toBeInTheDocument();
    expect(within(row).getByText('REF MTG-1014')).toBeInTheDocument();
    expect(within(row).getByRole('heading', { name: 'Q4 launch weekly sync' })).toBeInTheDocument();
    expect(within(row).getByText(/drafts to confirm/)).toHaveTextContent('3 drafts to confirm');
    // Never a human's row: no Make a task action here, and the time is the wall clock.
    expect(within(row).getByText('14:31', { selector: 'time' })).toBeInTheDocument();

    // The decision's offset opens the transcript moment; the untimed question has no offset to open.
    const offsets = within(row).getAllByRole('button', { name: 'Transcript at 0:12:41' });
    expect(offsets.length).toBeGreaterThan(0);
    await user.click(offsets[0]);
    expect(await within(row).findByRole('group', { name: 'Transcript excerpt at 0:12:41' })).toBeInTheDocument();
    expect(load).toHaveBeenCalledWith(expect.objectContaining({ kind: 'meeting', id: 'meeting-1', entryIdx: 12 }));
  });

  it('draws no offset for the tracks of a summary that names no meeting: no link opens nothing', () => {
    const summary = artifact({
      kind: ArtifactKind.MEETING_SUMMARY,
      ref: 'MTG-1020',
      title: 'Standup',
      tracks: [{ kind: TrackKind.DECISION, text: 'Ship on Friday', offsetMs: 5000n, entryIdx: 1 }],
    });
    renderPlain(
      <ExcerptProvider load={noExcerpt}>
        <MessageRow message={mapMessage(agentMessage({ seq: 4, artifact: summary }))} authorName="" />
      </ExcerptProvider>,
    );

    expect(screen.queryByRole('button', { name: /Transcript at/ })).not.toBeInTheDocument();
    expect(screen.getByText('Ship on Friday')).toBeInTheDocument();
  });

  it('labels the assistant by a constant name and puts a cited answer in a slip whose markers press their source row', async () => {
    const answer = artifact({
      kind: ArtifactKind.ANSWER,
      ref: 'ANS-2207',
      bodyMarkdown: 'The contract ends on 31 Dec 2026.[1] Late delivery is 2% a day.[2]',
      citations: [
        { n: 1, chunkId: 'c1', quote: 'term', source: { kind: SourceKind.FILE, id: 'file-1', anchor: 'page:4', label: 'ACME-contract-v3.pdf', channelId: 'ch-q4', page: 4 } },
        { n: 2, chunkId: 'c2', quote: 'late', source: { kind: SourceKind.MEETING, id: 'meeting-1', anchor: '18', label: 'Q4 sync', channelId: 'ch-q4', offsetMs: 1102000n } },
      ],
    });
    const { user } = renderPlain(
      <ExcerptProvider load={noExcerpt}>
        <MessageRow message={mapMessage(agentMessage({ seq: 5, time: '14:40', artifact: answer }))} authorName="" />
      </ExcerptProvider>,
    );

    const row = screen.getByRole('article');
    expect(within(row).getByText('@assistant')).toBeInTheDocument();
    expect(within(row).getByText('REF ANS-2207')).toBeInTheDocument();
    const marker = within(row).getByRole('button', { name: 'Source 1: ACME-contract-v3.pdf, p. 4' });
    expect(marker).toHaveAttribute('aria-pressed', 'false');

    await user.click(marker);

    expect(marker).toHaveAttribute('aria-pressed', 'true');
    expect(within(row).getByText('ACME-contract-v3.pdf').closest('li')).toHaveAttribute('data-hit');
    await user.click(marker);
    expect(marker).toHaveAttribute('aria-pressed', 'false');
  });

  it('shows a plain agent reply with the assistant label and no slip', () => {
    renderPlain(
      <ExcerptProvider load={noExcerpt}>
        <MessageRow message={mapMessage(agentMessage({ seq: 6, body: 'Sure, drafting now.' }))} authorName="" />
      </ExcerptProvider>,
    );

    expect(screen.getByText('@assistant')).toBeInTheDocument();
    expect(screen.getByText('Sure, drafting now.')).toBeInTheDocument();
    expect(screen.queryByText(/REF /)).not.toBeInTheDocument();
  });

  // What the model writes in a slip is markdown: a list is a list, bold is bold, a link is a safe link. The file summary
  // of the first real run showed its bullets inline in one paragraph; each kind of slip is held to the same renderer.
  const MODEL_MARKDOWN = 'Key points:\n\n- first **bold** point\n- second point with `code`\n\nSee [the docs](https://example.com/a) and ![x](https://evil.example/p.png).';
  const KINDS: { name: string; kind: ArtifactKind; extra?: Parameters<typeof artifact>[0] }[] = [
    { name: 'file summary', kind: ArtifactKind.DOC_SUMMARY },
    { name: 'meeting summary', kind: ArtifactKind.MEETING_SUMMARY, extra: { meetingId: 'meeting-1', title: 'Q4 launch weekly sync' } },
    { name: 'channel summary', kind: ArtifactKind.CHANNEL_SUMMARY, extra: { title: 'q4-launch' } },
    { name: 'agent answer', kind: ArtifactKind.ANSWER },
    { name: 'not-found answer', kind: ArtifactKind.NOT_FOUND },
  ];
  for (const { name, kind, extra } of KINDS) {
    it(`MessageRow_${name.replaceAll(/\W+/g, '')}_RendersTheModelsMarkdown`, () => {
      renderPlain(
        <ExcerptProvider load={noExcerpt}>
          <MessageRow message={mapMessage(agentMessage({ seq: 7, artifact: artifact({ kind, ref: 'DOC-1001', bodyMarkdown: MODEL_MARKDOWN, ...extra }) }))} authorName="" />
        </ExcerptProvider>,
      );

      const row = screen.getByRole('article');
      expect(within(row).getAllByRole('listitem').map((item) => item.textContent)).toEqual(['first bold point', 'second point with code']);
      expect(within(row).getByText('bold').tagName).toBe('STRONG');
      expect(within(row).getByText('code').tagName).toBe('CODE');
      const link = within(row).getByRole('link', { name: 'the docs' });
      expect(link).toHaveAttribute('href', 'https://example.com/a');
      expect(link).toHaveAttribute('target', '_blank');
      expect(link).toHaveAttribute('rel', 'noopener noreferrer');
      // No picture is ever fetched on a model's say-so: the description stands in for it.
      expect(row.querySelector('img')).toBeNull();
      expect(row).toHaveTextContent('and x.');
      // None of the markup the model wrote is left as characters.
      expect(row).not.toHaveTextContent('**');
      expect(row).not.toHaveTextContent('- first');
    });
  }
});

describe('Composer', () => {
  it('Composer_EnterDuringImeCompositionDoesNotSend', async () => {
    const onSend = vi.fn(() => Promise.resolve());
    const { user } = renderPlain(<Composer channelName="q4-launch" people={PEOPLE} canSend onSend={onSend} />);
    const input = screen.getByRole('combobox', { name: 'Message #q4-launch' });
    await user.type(input, 'สวัสดี');

    // The Enter that commits a Thai IME composition arrives with `isComposing` set (and keyCode 229 in Safari, after
    // compositionend): it picks the candidate, it does not send the half-typed message.
    fireEvent.keyDown(input, { key: 'Enter', code: 'Enter', isComposing: true, keyCode: 229 });
    fireEvent.keyDown(input, { key: 'Enter', code: 'Enter', isComposing: false, keyCode: 229 });
    expect(onSend).not.toHaveBeenCalled();
    expect(input).toHaveValue('สวัสดี');

    await user.keyboard('{Enter}');

    expect(onSend).toHaveBeenCalledOnce();
    expect(onSend).toHaveBeenCalledWith('สวัสดี', []);
    expect(input).toHaveValue('');
  });

  it('does not close the mention list on an Escape that belongs to the IME', async () => {
    const { user } = renderPlain(<Composer channelName="q4-launch" people={PEOPLE} canSend onSend={() => Promise.resolve()} />);
    const input = screen.getByRole('combobox');
    await user.type(input, '@');
    expect(screen.getByRole('listbox')).toBeInTheDocument();

    fireEvent.keyDown(input, { key: 'Escape', code: 'Escape', isComposing: true, keyCode: 229 });
    expect(screen.getByRole('listbox')).toBeInTheDocument();

    await user.keyboard('{Escape}');
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
    expect(input).toHaveValue('@');
  });

  it('keeps Send disabled until there is text, and sends what was typed trimmed', async () => {
    const onSend = vi.fn(() => Promise.resolve());
    const { user } = renderPlain(<Composer channelName="q4-launch" people={PEOPLE} canSend onSend={onSend} />);
    const send = screen.getByRole('button', { name: 'Send' });
    expect(send).toBeDisabled();

    await user.type(screen.getByRole('combobox'), '  hello team ');
    expect(send).toBeEnabled();
    await user.click(send);

    expect(onSend).toHaveBeenCalledWith('hello team', []);
  });

  it('cannot send while the stream has not loaded, and keeps what was typed', async () => {
    const { user } = renderPlain(<Composer channelName="q4-launch" people={PEOPLE} canSend={false} onSend={() => Promise.resolve()} />);

    await user.type(screen.getByRole('combobox'), 'hello');

    expect(screen.getByRole('button', { name: 'Send' })).toBeDisabled();
  });

  it('names the mentions it sends: the agent by its constant label, a member by id', async () => {
    const onSend = vi.fn(() => Promise.resolve());
    const { user } = renderPlain(<Composer channelName="q4-launch" people={PEOPLE} canSend onSend={onSend} />);
    const input = screen.getByRole('combobox');

    await user.type(input, 'Thanks @Arthit K. and @assistant check');
    await user.keyboard('{Enter}');

    expect(onSend).toHaveBeenCalledWith('Thanks @Arthit K. and @assistant check', [
      { kind: 'user', id: 'user-arthit', label: 'Arthit K.' },
      { kind: 'agent', id: 'assistant', label: 'assistant' },
    ]);
  });
});

describe('MentionList', () => {
  it('MentionList_AtOpensListboxArrowsEnterEscape', async () => {
    const { user } = renderPlain(<Composer channelName="q4-launch" people={PEOPLE} canSend onSend={() => Promise.resolve()} />);
    const input = screen.getByRole('combobox', { name: 'Message #q4-launch' });
    expect(input).toHaveAttribute('aria-expanded', 'false');

    await user.type(input, 'hi @');

    // `@assistant` leads the list, in bold, with what it is; the members follow.
    const list = screen.getByRole('listbox', { name: 'Mention an agent or member' });
    expect(input).toHaveAttribute('aria-expanded', 'true');
    const options = within(list).getAllByRole('option');
    expect(options.map((option) => option.textContent)).toEqual(['@assistantAI assistant · online', 'A' + 'Arthit K.', 'N' + 'Nok']);
    expect(within(options[0]).getByText('@assistant').tagName).toBe('B');
    expect(options[0]).toHaveAttribute('aria-selected', 'true');
    expect(input).toHaveAttribute('aria-activedescendant', (options[0]).id);

    await user.keyboard('{ArrowDown}');
    expect(within(list).getAllByRole('option')[1]).toHaveAttribute('aria-selected', 'true');
    await user.keyboard('{ArrowUp}{ArrowUp}');
    expect(within(list).getAllByRole('option')[2]).toHaveAttribute('aria-selected', 'true');

    await user.keyboard('{Enter}');
    expect(input).toHaveValue('hi @Nok ');
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument();

    await user.type(input, '@no');
    expect(within(screen.getByRole('listbox')).getAllByRole('option')).toHaveLength(1);
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
    expect(input).toHaveValue('hi @Nok @no');
  });

  it('closes when no name matches and when the caret leaves the mention', async () => {
    const { user } = renderPlain(<Composer channelName="q4-launch" people={PEOPLE} canSend onSend={() => Promise.resolve()} />);
    const input = screen.getByRole('combobox');

    await user.type(input, '@zzz');
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument();

    await user.clear(input);
    await user.type(input, '@no');
    expect(screen.getByRole('listbox')).toBeInTheDocument();
    await user.type(input, ' now');
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
  });

  it('picks with the pointer without losing focus', async () => {
    const { user } = renderPlain(<Composer channelName="q4-launch" people={PEOPLE} canSend onSend={() => Promise.resolve()} />);
    const input = screen.getByRole('combobox');
    await user.type(input, '@');

    await user.click(within(screen.getByRole('listbox')).getByRole('option', { name: /assistant/ }));

    expect(input).toHaveValue('@assistant ');
    expect(input).toHaveFocus();
  });
});

describe('the stream', () => {
  it('shows the latest page oldest first, with a day divider and the wall-clock time of each message', async () => {
    vi.useFakeTimers({ toFake: ['Date'] });
    vi.setSystemTime(new Date('2026-10-13T08:00:00Z'));
    openChannel({
      messages: [
        message({ seq: 3, time: '14:48', body: 'third' }),
        message({ seq: 2, time: '14:40', body: 'second', authorId: ME_ID }),
        message({ seq: 1, time: '14:02', body: 'first' }),
      ],
    });

    const log = await screen.findByRole('log', LOG);
    expect(await within(log).findByText('first')).toBeInTheDocument();

    const rows = within(log).getAllByRole('article');
    expect(rows.map((row) => /first|second|third/.exec(row.textContent)?.[0])).toEqual(['first', 'second', 'third']);
    expect(within(rows[0]).getByText('Arthit K.')).toBeInTheDocument();
    expect(within(rows[1]).getByText('Pimchanok S.')).toBeInTheDocument();
    expect(within(rows[0]).getByText('14:02', { selector: 'time' })).toBeInTheDocument();
    expect(within(log).getByText('Today · Tue, 13 Oct 2026')).toBeInTheDocument();
  });

  it('loads the latest 50 and prepends the older page on Load earlier', async () => {
    const page = (from: number, to: number) => Array.from({ length: to - from + 1 }, (_, index) => message({ seq: to - index, body: `m${String(to - index)}` }));
    const api = createStubApi();
    openChannel({ messages: page(51, 100) }, api);
    const log = await screen.findByRole('log', LOG);
    expect(await within(log).findByText('m100')).toBeInTheDocument();
    expect(api.chat.listMessages).toHaveBeenCalledWith({ channelId: 'ch-q4', beforeSeq: 0n, limit: 50 }, expect.anything());
    // The page before it is the last one: it holds fewer than 50 messages.
    api.chat.listMessages.mockResolvedValue(create(ListMessagesResponseSchema, { messages: page(2, 50) }));

    fireEvent.click(within(log).getByRole('button', { name: 'Load earlier' }));

    expect(await within(log).findByText('m2')).toBeInTheDocument();
    expect(api.chat.listMessages).toHaveBeenLastCalledWith({ channelId: 'ch-q4', beforeSeq: 51n, limit: 50 }, expect.anything());
    const texts = within(log).getAllByRole('article').map((row) => row.textContent);
    expect(texts).toHaveLength(99);
    expect(texts[0]).toContain('m2');
    expect(texts.at(-1)).toContain('m100');
    expect(within(log).queryByRole('button', { name: 'Load earlier' })).not.toBeInTheDocument();
  });

  it('offers no Load earlier when the latest page is short', async () => {
    openChannel({ messages: [message({ seq: 1, body: 'only' })] });
    const log = await screen.findByRole('log', LOG);
    await within(log).findByText('only');

    expect(within(log).queryByRole('button', { name: 'Load earlier' })).not.toBeInTheDocument();
  });

  it('says so, with Retry, when the messages cannot be loaded', async () => {
    const api = createStubApi();
    openChannel({}, api);
    api.chat.listMessages.mockRejectedValueOnce(new ConnectError('down', Code.Unavailable)).mockResolvedValue({ messages: [message({ seq: 1, body: 'back' })] } as never);

    expect(await screen.findByText("Couldn't load the messages.")).toBeInTheDocument();
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
      await Promise.resolve();
    });

    expect(await screen.findByText('back')).toBeInTheDocument();
  });

  it('adds a message that arrives over the socket to the end of the stream, once', async () => {
    const { client } = openChannel({ messages: [message({ seq: 5, body: 'five' })] });
    const log = await screen.findByRole('log', LOG);
    await within(log).findByText('five');
    const frameOf = (seq: number) => frame({ case: 'messageCreated', value: message({ seq, body: `live ${String(seq)}` }) });

    await act(async () => {
      client.emit(frameOf(6));
      client.emit(frameOf(6));
      await Promise.resolve();
    });

    expect(await within(log).findByText('live 6')).toBeInTheDocument();
    expect(within(log).getAllByText('live 6')).toHaveLength(1);
  });

  it('marks the channel read at its head while it is open, so the rail shows no pill for it', async () => {
    const { client } = openChannel({ messages: [message({ seq: 5, body: 'five' })] });
    const log = await screen.findByRole('log', LOG);
    await within(log).findByText('five');

    await act(async () => {
      client.emit(frame({ case: 'messageCreated', value: message({ seq: 6, body: 'six' }) }));
      await Promise.resolve();
    });

    await within(log).findByText('six');
    await waitFor(() => {
      expect(useUiStore.getState().readSeq['ch-q4']).toBe(6);
    });
    expect(useUiStore.getState().lastChannelId).toBe('ch-q4');
    expect(screen.queryByLabelText(/unread/)).not.toBeInTheDocument();
  });

  it('says the channel is not available once the person is removed from it', async () => {
    const { client } = openChannel({ messages: [message({ seq: 1, body: 'hello' })] });
    await within(await screen.findByRole('log', LOG)).findByText('hello');

    await act(async () => {
      client.emit(
        frame({ case: 'membershipChanged', value: create(MembershipChangedSchema, { channelId: 'ch-q4', userId: ME_ID, change: MembershipChange.REMOVED }) }),
      );
      await Promise.resolve();
    });

    expect(await screen.findByText("This channel isn't available to you.")).toBeInTheDocument();
    expect(screen.queryByRole('log', LOG)).not.toBeInTheDocument();
  });
});

describe('the header', () => {
  it('names the channel and its members, with Ask as the primary link and Board as a secondary one', async () => {
    openChannel();

    const heading = await screen.findByRole('heading', { level: 1 });
    expect(heading).toHaveTextContent('# q4-launch');
    expect(await screen.findByText('3 members')).toBeInTheDocument();
    const header = within(heading.closest('header') as HTMLElement);
    expect(header.getAllByRole('link', { name: 'Ask' })[0]).toHaveAttribute('href', '/ask?channel=ch-q4');
    expect(header.getByRole('link', { name: 'Board' })).toHaveAttribute('href', '/board?channel=ch-q4');
  });
});

describe('sending', () => {
  /** The message the server stored for a send, with the seq it was given. */
  const storedMessage = (request: SendMessageRequest, seq = 6) =>
    message({ seq, id: `msg-${String(seq)}`, authorId: ME_ID, body: request.body, clientMsgId: request.clientMsgId, time: '15:00' });
  /** The server's answer to a send. */
  const stored = (request: SendMessageRequest, seq = 6) => create(SendMessageResponseSchema, { message: storedMessage(request, seq) });

  it('OptimisticSend_ReconcilesByClientMsgIdNoDuplicate', async () => {
    const api = createStubApi();
    const { client, user } = openChannel({ messages: [message({ seq: 5, body: 'five' })] }, api);
    const log = await screen.findByRole('log', LOG);
    await within(log).findByText('five');
    const answer = Promise.withResolvers<SendMessageResponse>();
    api.chat.sendMessage.mockReturnValue(answer.promise);

    await user.type(screen.getByRole('combobox', { name: 'Message #q4-launch' }), 'Hello team');
    await user.keyboard('{Enter}');

    // At once, before the server has answered: the message is in the stream, marked as being sent.
    expect(await within(log).findByText('Hello team')).toBeInTheDocument();
    expect(within(log).getByText('Sending…')).toBeInTheDocument();
    const request = sentRequest(api);
    expect(request.clientMsgId).toMatch(/^[0-9a-f-]{36}$/);
    expect(request.channelId).toBe('ch-q4');

    // The gateway's frame for it arrives first, carrying the same client_msg_id: the unsent row becomes the stored message.
    await act(async () => {
      client.emit(frame({ case: 'messageCreated', value: storedMessage(request) }));
      await Promise.resolve();
    });
    await waitFor(() => {
      expect(within(log).queryByText('Sending…')).not.toBeInTheDocument();
    });
    expect(within(log).getAllByText('Hello team')).toHaveLength(1);

    // Then the send's own answer lands: still one.
    await act(async () => {
      answer.resolve(stored(request));
      await Promise.resolve();
    });
    expect(within(log).getAllByText('Hello team')).toHaveLength(1);
    expect(within(log).getAllByRole('article')).toHaveLength(2);
  });

  it('shows the message the server stored in place of the unsent one when the answer comes first', async () => {
    const api = createStubApi();
    const { user } = openChannel({ messages: [message({ seq: 5, body: 'five' })] }, api);
    const log = await screen.findByRole('log', LOG);
    await within(log).findByText('five');
    api.chat.sendMessage.mockImplementation((request) => Promise.resolve(stored(request as SendMessageRequest)));

    await user.type(screen.getByRole('combobox'), 'Hello team');
    await user.click(screen.getByRole('button', { name: 'Send' }));

    await waitFor(() => {
      expect(within(log).queryByText('Sending…')).not.toBeInTheDocument();
    });
    expect(within(log).getAllByText('Hello team')).toHaveLength(1);
  });

  it('keeps a message the server refused as Not sent, with Retry that sends the same message again', async () => {
    const api = createStubApi();
    const { user } = openChannel({ messages: [message({ seq: 5, body: 'five' })] }, api);
    const log = await screen.findByRole('log', LOG);
    await within(log).findByText('five');
    api.chat.sendMessage.mockRejectedValueOnce(new ConnectError('too long', Code.InvalidArgument));

    await user.type(screen.getByRole('combobox'), 'a message');
    await user.keyboard('{Enter}');

    expect(await within(log).findByText('Not sent')).toBeInTheDocument();
    const first = sentRequest(api);
    api.chat.sendMessage.mockImplementation((request) => Promise.resolve(stored(request as SendMessageRequest)));

    await user.click(within(log).getByRole('button', { name: 'Retry sending' }));

    await waitFor(() => {
      expect(within(log).queryByText('Not sent')).not.toBeInTheDocument();
    });
    expect(sentRequest(api, 1).clientMsgId).toBe(first.clientMsgId);
    expect(within(log).getAllByText('a message')).toHaveLength(1);
  });

  it('queues a message the network refused and sends it once, in order, when the connection is back', async () => {
    const api = createStubApi();
    const { client, user } = openChannel({ messages: [message({ seq: 5, body: 'five' })] }, api);
    const log = await screen.findByRole('log', LOG);
    await within(log).findByText('five');
    api.chat.sendMessage.mockRejectedValue(new ConnectError('offline', Code.Unavailable));

    const input = screen.getByRole('combobox');
    await user.type(input, 'first');
    await user.keyboard('{Enter}');
    await user.type(input, 'second');
    await user.keyboard('{Enter}');

    expect(await within(log).findAllByText('Waiting for the connection')).toHaveLength(2);
    expect(api.chat.sendMessage).toHaveBeenCalledTimes(2);
    api.chat.sendMessage.mockImplementation((request) => Promise.resolve(stored(request as SendMessageRequest, request.body === 'first' ? 6 : 7)));

    await act(async () => {
      client.open();
      await Promise.resolve();
    });

    await waitFor(() => {
      expect(within(log).queryByText('Waiting for the connection')).not.toBeInTheDocument();
    });
    expect(api.chat.sendMessage.mock.calls.slice(2).map(([request]) => (request as SendMessageRequest).body)).toEqual(['first', 'second']);
    expect(within(log).getAllByText('first')).toHaveLength(1);
    expect(within(log).getAllByText('second')).toHaveLength(1);
  });

  it('sends the mentions of a message with it', async () => {
    const api = createStubApi();
    const { user } = openChannel({ messages: [message({ seq: 5, body: 'five' })] }, api);
    await within(await screen.findByRole('log', LOG)).findByText('five');
    api.chat.sendMessage.mockImplementation((request) => Promise.resolve(stored(request as SendMessageRequest)));
    await screen.findByText('3 members');

    await user.type(screen.getByRole('combobox'), '@ass');
    await user.keyboard('{Enter}');
    await user.type(screen.getByRole('combobox'), 'what is due?');
    await user.keyboard('{Enter}');

    await waitFor(() => {
      expect(api.chat.sendMessage).toHaveBeenCalledOnce();
    });
    const request = sentRequest(api);
    expect(request.body).toBe('@assistant what is due?');
    expect(request.mentions.map((m) => [m.kind, m.id, m.label])).toEqual([[MentionKind.AGENT, 'assistant', 'assistant']]);
  });
});

