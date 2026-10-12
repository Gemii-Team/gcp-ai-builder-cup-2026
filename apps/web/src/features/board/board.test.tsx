import { Code, ConnectError } from '@connectrpc/connect';
import { create } from '@bufbuild/protobuf';
import { timestampFromDate } from '@bufbuild/protobuf/wkt';
import { ExcerptSchema, GetTranscriptExcerptResponseSchema, ListMessagesResponseSchema, MoveTaskResponseSchema, TaskStatus } from '@kiwi/api-client';
import { screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { ANNOUNCE, card, cell, DOCUMENT, endBoardDay, lane, openBoard, QUOTE, ready, startBoardDay, TASKS } from '../../testing/boardWorld';
import { at, frame, message, ME_ID, task } from '../../testing/fixtures';
import { createStubApi } from '../../testing/stubApi';
import boardCss from './board.css?raw';

beforeEach(startBoardDay);
afterEach(endBoardDay);

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
    expect(within(document.querySelector<HTMLElement>('.bd-lanes') ?? document.body).getAllByRole('heading', { level: 3 })).toHaveLength(1);
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
