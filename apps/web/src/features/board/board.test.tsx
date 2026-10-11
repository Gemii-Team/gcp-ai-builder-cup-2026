import { Code, ConnectError } from '@connectrpc/connect';
import { create } from '@bufbuild/protobuf';
import { timestampFromDate } from '@bufbuild/protobuf/wkt';
import {
  CreatedByKind,
  ExcerptSchema,
  GetFileResponseSchema,
  GetMeetingResponseSchema,
  GetTranscriptExcerptResponseSchema,
  ListMessagesResponseSchema,
  MoveTaskResponseSchema,
  SourceKind,
  TaskStatus,
} from '@kiwi/api-client';
import { dateToDueAt } from '@kiwi/ui';
import { screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useUiStore } from '../../state/ui';
import { at, channel, file, frame, meeting, message, ME_ID, task } from '../../testing/fixtures';
import { renderApp } from '../../testing/render';
import { createStubApi, seedApi, type StubApi, type World } from '../../testing/stubApi';
import boardCss from './board.css?raw';
import { BoardScreen } from './Board';

const CHANNELS = [channel({ id: 'ch-q4', name: 'q4-launch' }), channel({ id: 'ch-ops', name: 'ops-warehouse' })];

const MEETING = { kind: SourceKind.MEETING, id: 'meeting-1', anchor: '12', label: 'Q4 launch weekly sync', channelId: 'ch-q4', offsetMs: 761000n };
const QUOTE = '4.1 Term. This Agreement ends on 31 December 2026.';
const DOCUMENT = { kind: SourceKind.FILE, id: 'file-1', anchor: 'page:4', label: 'ACME-contract-v3.pdf', channelId: 'ch-q4', page: 4, quote: QUOTE };
const MESSAGE = { kind: SourceKind.MESSAGE, id: 'msg-41', anchor: '41', label: 'Arthit K.', channelId: 'ch-ops', at: timestampFromDate(at('17:05', '2026-10-12')) };

const due = (day: string) => timestampFromDate(dateToDueAt(day));

const ANNOUNCE = task({ id: 't-announce', ref: 'TSK-2030', title: 'Announce the new launch date', assigneeId: ME_ID, dueAt: due('2026-10-14'), position: 'a0', source: MEETING });
const RATE_CARD = task({ id: 't-ratecard', ref: 'TSK-2024', title: 'Get the Q4 rate card', assigneeId: 'user-nok', dueAt: due('2026-10-12'), position: 'a1', source: MEETING });
const RENEW = task({ id: 't-renew', ref: 'TSK-2026', title: 'Decide whether to renew ACME', position: 'a0', source: DOCUMENT });
const FORKLIFT = task({ id: 't-forklift', ref: 'TSK-2028', channelId: 'ch-ops', title: 'Book two extra forklift shifts', assigneeId: 'user-arthit', position: 'a0', source: MESSAGE });
const BRIEFING = task({
  id: 't-hand',
  ref: 'TSK-2036',
  channelId: 'ch-ops',
  title: 'Prepare the warehouse briefing',
  assigneeId: 'user-arthit',
  dueAt: due('2026-10-20'),
  position: 'a0',
  createdById: ME_ID,
  createdByKind: CreatedByKind.USER,
  createdTime: '13:20',
});
const SIGNED = task({ id: 't-signed', ref: 'TSK-2017', title: 'Send the signed contract to Finance', status: TaskStatus.DONE, assigneeId: ME_ID, doneAt: timestampFromDate(at('14:48')), position: 'a0', source: DOCUMENT });
const TASKS = [ANNOUNCE, RATE_CARD, RENEW, FORKLIFT, BRIEFING, SIGNED];

// "Now" is 14:40 on Tue 13 Oct 2026 in Bangkok.
beforeEach(() => {
  localStorage.clear();
  useUiStore.setState({ lang: 'en', readSeq: {}, lastChannelId: null, draftsSheetOpen: false });
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(at('14:40'));
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

function openBoard(world: World = {}, api: StubApi = createStubApi(), initialChannelId: string | null = null) {
  seedApi(api, { channels: CHANNELS, tasks: TASKS, ...world });
  api.meetings.getMeeting.mockResolvedValue(create(GetMeetingResponseSchema, { meeting: meeting({ id: 'meeting-1', ref: 'MTG-1014', startedAt: timestampFromDate(at('13:46')), endedAt: timestampFromDate(at('14:29')) }) }));
  api.files.getFile.mockResolvedValue(create(GetFileResponseSchema, { file: file({ id: 'file-1', name: 'ACME-contract-v3.pdf', ref: 'DOC-0371', pages: 18 }) }));
  return renderApp({ path: '/board', api, routes: [{ path: '/board', element: <BoardScreen initialChannelId={initialChannelId} /> }] });
}

/** The card of a task, by its title. */
const card = (title: string) => {
  const item = screen.getByRole('heading', { level: 3, name: title }).closest('li');
  if (item === null) {
    throw new Error(`"${title}" is not on a card`);
  }
  return within(item);
};

const lane = (name: RegExp | string) => within(screen.getByRole('region', { name }));
const cell = (status: string, laneTitle: string) => within(screen.getByRole('group', { name: `${status} in ${laneTitle}` }));

async function ready() {
  await screen.findByRole('heading', { level: 3, name: 'Announce the new launch date' });
}

describe('the Board: source lanes', () => {
  it('draws the header, the column heads and one lane per source with its meta and REF from GetMeeting and GetFile', async () => {
    const { api } = openBoard();
    await ready();

    expect(screen.getByRole('heading', { level: 1, name: 'Board' })).toBeInTheDocument();
    expect(screen.getByText('Tasks from your 2 channels')).toBeInTheDocument();
    // Ask is a header action, and the 44px icon button phones get: both lead to /ask.
    const asks = within(document.querySelector<HTMLElement>('.chead') ?? document.body).getAllByRole('link', { name: 'Ask' });
    expect(asks.map((link) => link.getAttribute('href'))).toEqual(['/ask', '/ask']);
    // Heads count the confirmed tasks that show: To do 5, Doing 0, Done 1.
    const heads = document.querySelector('.bd-cols');
    expect(heads).toHaveTextContent('To do5Doing0Done1');

    const sync = await screen.findByRole('region', { name: /^Q4 launch weekly sync/ });
    await within(sync).findByText('REF MTG-1014');
    expect(sync).toHaveTextContent('Meeting · Tue 13 Oct · ended 14:29');
    expect(sync).toHaveTextContent('2 tasks');
    const doc = await screen.findByRole('region', { name: /^ACME-contract-v3\.pdf/ });
    await within(doc).findByText('REF DOC-0371');
    expect(doc).toHaveTextContent('Document · 18 pages · #q4-launch');
    const messages = screen.getByRole('region', { name: /^Messages in #ops-warehouse/ });
    expect(messages).toHaveTextContent('Channel · tasks made from a message');
    // A channel's messages have no REF of their own: only the cards' TSK codes appear in the lane, not in its heading.
    expect(within(messages).getByRole('heading', { level: 2 })).not.toHaveTextContent('REF');
    expect(api.meetings.getMeeting).toHaveBeenCalledTimes(1);
  });

  it('AddedByHandLane_IsAlwaysFirstAndCardsShowChannelPrefixAndAddedByNameAndTime', async () => {
    openBoard();
    await ready();

    const regions = screen.getAllByRole('region').filter((region) => region.classList.contains('bd-lane'));
    expect(regions[0]).toHaveAccessibleName(/^Added by hand/);
    const hand = lane(/^Added by hand/);
    expect(hand.getByText('Tasks a person added directly')).toBeInTheDocument();
    expect(hand.queryByText(/^REF (MTG|DOC)/)).not.toBeInTheDocument();

    const briefing = card('Prepare the warehouse briefing');
    // The lane has no channel of its own, so its cards name theirs first.
    expect(briefing.getByText(/#ops-warehouse/).closest('p')).toHaveTextContent('#ops-warehouse · Arthit K. · Tue 20 Oct');
    expect(briefing.getByText('REF TSK-2036')).toBeInTheDocument();
    expect(briefing.getByText(/Added by/).closest('.bd-src')).toHaveTextContent('Added by Pimchanok S. · 13:20');
  });

  it('AddedByHand_SourceIsPlainTextNoLinkNoExcerpt', async () => {
    openBoard();
    await ready();

    const briefing = card('Prepare the warehouse briefing');

    // Only the Move button is a control on it: no offset, page or message link, nothing to open.
    expect(briefing.getAllByRole('button').map((button) => button.getAttribute('aria-label'))).toEqual(['Move TSK-2036: Prepare the warehouse briefing']);
    expect(briefing.queryByRole('link')).not.toBeInTheDocument();
    expect(briefing.getByText(/Added by/).closest('.bd-src')?.querySelector('a, button')).toBeNull();
  });

  it('Board_MessageMadeTaskLandsInItsChannelsMessagesLane', async () => {
    openBoard();
    await ready();

    const messages = lane(/^Messages in #ops-warehouse/);
    expect(messages.getByRole('heading', { level: 3, name: 'Book two extra forklift shifts' })).toBeInTheDocument();
    expect(lane(/^Added by hand/).queryByText('Book two extra forklift shifts')).not.toBeInTheDocument();
    // Cells: To do holds it, Doing and Done say what is missing.
    expect(cell('To do', 'Messages in #ops-warehouse').getByText('Book two extra forklift shifts')).toBeInTheDocument();
    expect(cell('Doing', 'Messages in #ops-warehouse').getByText('Nothing in Doing')).toBeInTheDocument();
    expect(cell('Done', 'Messages in #ops-warehouse').getByText('Nothing done yet')).toBeInTheDocument();
  });
});

describe('the task card', () => {
  it('TaskCard_NoOwnerReadsNeutralNoOwner_NotAmber', async () => {
    openBoard();
    await ready();

    const renew = card('Decide whether to renew ACME');
    const meta = renew.getByText(/No owner/).closest('p');

    expect(meta).toHaveTextContent('No owner · No due date');
    // A draft's missing owner is a warning select; a confirmed task's is a settled fact in the meta's own voice.
    expect(meta?.querySelector('select, svg')).toBeNull();
    expect(meta?.closest('li')?.innerHTML).not.toMatch(/amber|draft-|warn/i);
  });

  it('TaskCard_OverdueHasNoStateColour', async () => {
    openBoard();
    await ready();

    const late = card('Get the Q4 rate card');
    const meta = late.getByText(/Overdue/).closest('.bd-late');

    expect(meta).toHaveTextContent('Overdue · was due Mon 12 Oct');
    // A 16px clock icon, weight 600 in Ledger Ink: no state colour on the card, its date or its lane.
    expect(meta?.querySelector('svg')).toHaveAttribute('width', '16');
    expect(meta?.closest('li')?.innerHTML).not.toMatch(/amber|draft-|returned|\bred\b|warn/i);
    const rule = /\.bd-late \{([^}]*)\}/.exec(boardCss)?.[1] ?? '';
    expect(rule).toContain('font-weight: 600');
    expect(rule).toContain('color: var(--color-ledger-ink)');
    expect(boardCss).not.toMatch(/draft-|returned-/);
    // Due today or tomorrow, or done, is not overdue.
    expect(card('Announce the new launch date').queryByText(/Overdue/)).not.toBeInTheDocument();
    expect(card('Announce the new launch date').getByText('Wed 14 Oct')).toBeInTheDocument();
    expect(card('Send the signed contract to Finance').queryByText(/Overdue/)).not.toBeInTheDocument();
  });

  it('shows a finished task as Done with its time today and its day on another day, newest first', async () => {
    const earlier = task({ id: 't-old', ref: 'TSK-2018', title: 'Review the penalty clause', status: TaskStatus.DONE, assigneeId: ME_ID, doneAt: timestampFromDate(at('10:15', '2026-10-12')), position: 'a9', source: DOCUMENT });
    openBoard({ tasks: [earlier, ...TASKS] });
    await ready();

    expect(card('Send the signed contract to Finance').getByText(/Done/).closest('.bd-okl')).toHaveTextContent('Done 14:48');
    expect(card('Review the penalty clause').getByText(/Done/).closest('.bd-okl')).toHaveTextContent('Done Mon 12 Oct');
    // The Done cell lists every done task of the lane, newest first, whatever the position.
    const done = cell('Done', 'ACME-contract-v3.pdf').getAllByRole('heading', { level: 3 });
    expect(done.map((heading) => heading.textContent)).toEqual(['Send the signed contract to Finance', 'Review the penalty clause']);
  });

  it('MessageSourcedTask_CardReadsFromChannelAuthorTimeAndOpensTheMessage', async () => {
    const api = createStubApi();
    const { user } = openBoard({}, api);
    await ready();
    api.chat.listMessages.mockResolvedValue(create(ListMessagesResponseSchema, { messages: [message({ seq: 41, id: 'msg-41', channelId: 'ch-ops', body: 'ล็อตสินค้าใหม่เข้าคลังวันที่ 10 พ.ย. ต้องจองรถยกเพิ่มอีกสองกะนะครับ' })] }));

    const forklift = card('Book two extra forklift shifts');
    // The day label comes with the time because the message was not sent today.
    expect(forklift.getByText(/^From/).closest('.bd-src')).toHaveTextContent('From #ops-warehouse · Arthit K. · Mon 12 Oct 17:05');
    const link = forklift.getByRole('button', { name: 'Message in #ops-warehouse at 17:05' });
    expect(link).toHaveTextContent('#ops-warehouse');

    await user.click(link);

    const excerpt = await forklift.findByRole('group', { name: 'Message excerpt from Arthit K.' });
    expect(link).toHaveAttribute('aria-expanded', 'true');
    expect(excerpt).toHaveTextContent('Message · #ops-warehouse · Mon 12 Oct 17:05');
    expect(within(excerpt).getByText('ล็อตสินค้าใหม่เข้าคลังวันที่ 10 พ.ย. ต้องจองรถยกเพิ่มอีกสองกะนะครับ')).toHaveAttribute('lang', 'th');
    expect(api.chat.listMessages).toHaveBeenCalledWith({ channelId: 'ch-ops', beforeSeq: 42n, limit: 1 });
  });

  it('TaskCard_MeetingMessageDocumentSourcesOpenExcerptsInPlace', async () => {
    const api = createStubApi();
    const { user } = openBoard({}, api);
    await ready();
    api.meetings.getTranscriptExcerpt.mockResolvedValue(
      create(GetTranscriptExcerptResponseSchema, {
        excerpt: create(ExcerptSchema, { meetingId: 'meeting-1', title: 'Q4 launch weekly sync', entries: [{ idx: 12, speaker: 'พิมพ์ชนก ส.', text: 'งั้นเราเลื่อนวันเปิดตัวไปเป็น 15 พฤศจิกายนนะคะ', startMs: 761000n }] }),
      }),
    );
    api.chat.listMessages.mockResolvedValue(create(ListMessagesResponseSchema, { messages: [message({ seq: 41, id: 'msg-41', channelId: 'ch-ops', body: 'We need two more shifts.' })] }));

    // A meeting: "From meeting · 0:12:41", the offset opens the transcript around its entry, under the REF row.
    const announce = card('Announce the new launch date');
    expect(announce.getByText(/^From meeting/)).toHaveTextContent('From meeting · 0:12:41');
    await user.click(announce.getByRole('button', { name: 'Transcript at 0:12:41' }));
    const transcript = await announce.findByRole('group', { name: 'Transcript excerpt at 0:12:41' });
    expect(api.meetings.getTranscriptExcerpt).toHaveBeenCalledWith({ meetingId: 'meeting-1', entryIdx: 12, radius: 2 });
    expect(within(transcript).getByText(/พิมพ์ชนก/)).toHaveAttribute('lang', 'th');

    // One at a time: opening a message closes the transcript.
    await user.click(card('Book two extra forklift shifts').getByRole('button', { name: /^Message in #ops-warehouse/ }));
    await screen.findByRole('group', { name: /Message excerpt/ });
    expect(screen.queryByRole('group', { name: /Transcript excerpt/ })).not.toBeInTheDocument();

    // A document: the page opens the passage the task carries, with the page count and REF from GetFile.
    await user.click(card('Decide whether to renew ACME').getByRole('button', { name: 'ACME-contract-v3.pdf, page 4' }));
    expect(screen.queryByRole('group', { name: /Message excerpt/ })).not.toBeInTheDocument();
    expect(await screen.findAllByRole('group', { name: /Document excerpt/ })).toHaveLength(1);
  });

  it('DocumentCard_ExcerptShowsQuoteAndPageOfNFromGetFile', async () => {
    const { user } = openBoard();
    await ready();

    const renew = card('Decide whether to renew ACME');
    expect(renew.getByText(/^ACME-contract-v3\.pdf/).closest('.bd-src')).toHaveTextContent('ACME-contract-v3.pdf · p. 4');

    await user.click(renew.getByRole('button', { name: 'ACME-contract-v3.pdf, page 4' }));

    const excerpt = await renew.findByRole('group', { name: 'Document excerpt, ACME-contract-v3.pdf page 4' });
    await waitFor(() => {
      expect(excerpt).toHaveTextContent('Page 4 of 18 · REF DOC-0371');
    });
    expect(excerpt).toHaveTextContent('Document · ACME-contract-v3.pdf · p. 4');
    expect(within(excerpt).getByText(QUOTE)).toBeInTheDocument();
  });

  it('leaves out the passage line when the task has no quote', async () => {
    const unquoted = task({ id: 't-renew', ref: 'TSK-2026', title: 'Decide whether to renew ACME', position: 'a0', source: { ...DOCUMENT, quote: undefined } });
    const { user } = openBoard({ tasks: [unquoted] });
    await screen.findByRole('heading', { level: 3, name: 'Decide whether to renew ACME' });

    await user.click(card('Decide whether to renew ACME').getByRole('button', { name: 'ACME-contract-v3.pdf, page 4' }));

    const excerpt = await screen.findByRole('group', { name: /Document excerpt/ });
    await waitFor(() => {
      expect(excerpt).toHaveTextContent('Page 4 of 18 · REF DOC-0371');
    });
    expect(excerpt.querySelector('.kiwi-ex-q')).toBeNull();
  });
});

describe('the Move menu', () => {
  const MOVE = { name: 'Move TSK-2030: Announce the new launch date' };

  it('MoveMenu_ArrowsHomeEndEscapeAndCurrentChecked', async () => {
    const { user } = openBoard();
    await ready();
    const button = screen.getByRole('button', MOVE);
    expect(button).toHaveAttribute('aria-haspopup', 'menu');
    expect(button).toHaveAttribute('aria-expanded', 'false');

    await user.click(button);

    const menu = screen.getByRole('menu', { name: 'Move TSK-2030 to' });
    expect(button).toHaveAttribute('aria-expanded', 'true');
    const rows = within(menu).getAllByRole('menuitemradio');
    expect(rows.map((row) => row.textContent)).toEqual(['To do', 'Doing', 'Done']);
    // The current status is checked, and has the focus.
    expect(rows.map((row) => row.getAttribute('aria-checked'))).toEqual(['true', 'false', 'false']);
    expect(rows[0]).toHaveFocus();
    await user.keyboard('{ArrowDown}');
    expect(rows[1]).toHaveFocus();
    await user.keyboard('{ArrowUp}{ArrowUp}');
    expect(rows[2]).toHaveFocus();
    await user.keyboard('{Home}');
    expect(rows[0]).toHaveFocus();
    await user.keyboard('{End}');
    expect(rows[2]).toHaveFocus();

    await user.keyboard('{Escape}');

    expect(screen.queryByRole('menu')).not.toBeInTheDocument();
    expect(button).toHaveFocus();
    expect(button).toHaveAttribute('aria-expanded', 'false');
  });

  it('opens with ArrowDown from the Move button', async () => {
    const { user } = openBoard();
    await ready();
    screen.getByRole('button', MOVE).focus();

    await user.keyboard('{ArrowDown}');

    expect(screen.getByRole('menu', { name: 'Move TSK-2030 to' })).toBeInTheDocument();
  });

  it('moves the card to the top of the chosen status of its own lane, says so, and keeps the focus on its Move button', async () => {
    const api = createStubApi();
    const { user } = openBoard({}, api);
    await ready();
    api.tasks.moveTask.mockResolvedValue(create(MoveTaskResponseSchema, { task: { ...ANNOUNCE, status: TaskStatus.DOING, position: 'a0' } }));

    await user.click(screen.getByRole('button', MOVE));
    await user.click(within(screen.getByRole('menu')).getByRole('menuitemradio', { name: 'Doing' }));

    expect(api.tasks.moveTask).toHaveBeenCalledWith({ id: 't-announce', status: TaskStatus.DOING });
    const doing = cell('Doing', 'Q4 launch weekly sync');
    expect(await doing.findByRole('heading', { level: 3, name: 'Announce the new launch date' })).toBeInTheDocument();
    expect(cell('To do', 'Q4 launch weekly sync').queryByText('Announce the new launch date')).not.toBeInTheDocument();
    expect(await screen.findByText('Moved TSK-2030 to Doing.')).toBeInTheDocument();
    await waitFor(() => {
      expect(doing.getByRole('button', MOVE)).toHaveFocus();
    });
  });

  it('puts the card back and says so when the server refuses the move', async () => {
    const api = createStubApi();
    const { user } = openBoard({}, api);
    await ready();
    api.tasks.moveTask.mockRejectedValue(new ConnectError('down', Code.Unavailable));

    await user.click(screen.getByRole('button', MOVE));
    await user.click(within(screen.getByRole('menu')).getByRole('menuitemradio', { name: 'Done' }));

    expect(await screen.findByRole('alert')).toHaveTextContent("Couldn't move TSK-2030. Try again.");
    expect(cell('To do', 'Q4 launch weekly sync').getByText('Announce the new launch date')).toBeInTheDocument();
    expect(cell('Done', 'Q4 launch weekly sync').queryByText('Announce the new launch date')).not.toBeInTheDocument();
  });
});

describe('realtime and filters', () => {
  it('TaskChanged_EventMovesCardBetweenCells', async () => {
    const { client } = openBoard();
    await ready();
    expect(cell('To do', 'Q4 launch weekly sync').getByText('Announce the new launch date')).toBeInTheDocument();

    // Someone else moved it: the real binding writes the changed task into the cache the Board reads.
    client.emit(frame({ case: 'taskChanged', value: { ...ANNOUNCE, status: TaskStatus.DONE, doneAt: timestampFromDate(at('14:41')) } }));

    await waitFor(() => {
      expect(cell('Done', 'Q4 launch weekly sync').getByText('Announce the new launch date')).toBeInTheDocument();
    });
    expect(cell('To do', 'Q4 launch weekly sync').queryByText('Announce the new launch date')).not.toBeInTheDocument();
    expect(card('Announce the new launch date').getByText(/Done/).closest('.bd-okl')).toHaveTextContent('Done 14:41');
    expect(document.querySelector('.bd-cols')).toHaveTextContent('To do4Doing0Done2');
  });

  it('Filters_ChannelAndSourceType', async () => {
    const { user } = openBoard();
    await ready();
    const channelSelect = screen.getByRole('combobox', { name: 'Channel' });
    const sourceSelect = screen.getByRole('combobox', { name: 'Source type' });
    expect(within(channelSelect).getAllByRole('option').map((option) => option.textContent)).toEqual(['All channels', '# q4-launch', '# ops-warehouse']);
    expect(within(sourceSelect).getAllByRole('option').map((option) => option.textContent)).toEqual(['All sources', 'Meetings', 'Messages', 'Documents', 'Added by hand']);

    await user.selectOptions(channelSelect, 'ch-ops');

    expect(screen.getAllByRole('region').filter((region) => region.classList.contains('bd-lane')).map((region) => region.getAttribute('aria-labelledby'))).toHaveLength(2);
    expect(screen.queryByRole('region', { name: /^Q4 launch weekly sync/ })).not.toBeInTheDocument();
    expect(screen.getByText('Book two extra forklift shifts')).toBeInTheDocument();
    expect(screen.getByText('Prepare the warehouse briefing')).toBeInTheDocument();
    expect(document.querySelector('.bd-cols')).toHaveTextContent('To do2Doing0Done0');

    await user.selectOptions(sourceSelect, 'message');

    expect(screen.getByText('Book two extra forklift shifts')).toBeInTheDocument();
    expect(screen.queryByText('Prepare the warehouse briefing')).not.toBeInTheDocument();

    await user.selectOptions(channelSelect, '');
    await user.selectOptions(sourceSelect, 'file');

    expect(screen.getByText('Decide whether to renew ACME')).toBeInTheDocument();
    expect(screen.getByText('Send the signed contract to Finance')).toBeInTheDocument();
    expect(screen.queryByText('Announce the new launch date')).not.toBeInTheDocument();
  });

  it('Filter_AddedByHand_ShowsOnlyTasksWithoutASource', async () => {
    const { user } = openBoard();
    await ready();

    await user.selectOptions(screen.getByRole('combobox', { name: 'Source type' }), 'hand');

    expect(screen.getByText('Prepare the warehouse briefing')).toBeInTheDocument();
    expect(screen.getAllByRole('heading', { level: 3 })).toHaveLength(1);
    expect(screen.getAllByRole('heading', { level: 2 }).map((heading) => heading.textContent).filter((text) => /Added by hand/.test(text))).toHaveLength(1);
  });

  it('Board_NoMatchShowsClearFilters', async () => {
    const { user } = openBoard();
    await ready();

    await user.selectOptions(screen.getByRole('combobox', { name: 'Channel' }), 'ch-ops');
    await user.selectOptions(screen.getByRole('combobox', { name: 'Source type' }), 'meeting');

    expect(screen.getByRole('heading', { level: 3, name: 'No tasks match these filters' })).toBeInTheDocument();
    expect(screen.queryByRole('region', { name: /^Added by hand/ })).not.toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Clear filters' }));

    expect(screen.queryByText('No tasks match these filters')).not.toBeInTheDocument();
    expect(screen.getByRole('combobox', { name: 'Channel' })).toHaveValue('');
    expect(screen.getByRole('combobox', { name: 'Source type' })).toHaveValue('');
    expect(screen.getByText('Announce the new launch date')).toBeInTheDocument();
  });

  it('starts the Channel filter on the channel the route names', async () => {
    openBoard({}, createStubApi(), 'ch-ops');
    await screen.findByRole('heading', { level: 3, name: 'Book two extra forklift shifts' });

    expect(screen.getByRole('combobox', { name: 'Channel' })).toHaveValue('ch-ops');
    expect(screen.queryByText('Announce the new launch date')).not.toBeInTheDocument();
  });

  it('ignores a channel the person is not in', async () => {
    openBoard({}, createStubApi(), 'ch-gone');
    await ready();

    expect(screen.getByRole('combobox', { name: 'Channel' })).toHaveValue('');
  });

  it('says there are no tasks, and not that none match, on an empty board', async () => {
    openBoard({ tasks: [] });

    expect(await screen.findByRole('heading', { level: 3, name: 'No tasks yet' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Clear filters' })).not.toBeInTheDocument();
  });
});
