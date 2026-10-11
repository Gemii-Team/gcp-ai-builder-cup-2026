import { Code, ConnectError } from '@connectrpc/connect';
import { create } from '@bufbuild/protobuf';
import { timestampFromDate } from '@bufbuild/protobuf/wkt';
import {
  ConfirmDraftResponseSchema,
  CreateTaskResponseSchema,
  ExcerptSchema,
  GetTranscriptExcerptResponseSchema,
  ListMembersResponseSchema,
  ListTasksResponseSchema,
  MoveTaskResponseSchema,
  TaskState,
  TaskStatus,
  UpdateTaskResponseSchema,
  type CreateTaskRequest,
} from '@kiwi/api-client';
import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { ANNOUNCE, card, cell, due, endBoardDay, MEETING, openBoard, ready, startBoardDay, TASKS } from '../../testing/boardWorld';
import { member, ME_ID, MEMBERS, task } from '../../testing/fixtures';
import { createStubApi } from '../../testing/stubApi';
import { stubViewport } from '../../testing/viewport';
import screenCss from '../shell/screen.css?raw';

beforeEach(startBoardDay);
afterEach(endBoardDay);

const QUOTATION = task({
  id: 'draft-a',
  ref: 'TSK-2031',
  title: 'Send the quotation to ACME',
  state: TaskState.DRAFT,
  assigneeId: 'user-nok',
  dueAt: due('2026-10-16'),
  position: 'a5',
  source: { ...MEETING, anchor: '21', offsetMs: 1307000n },
});
const SLIDES = task({ id: 'draft-b', ref: 'TSK-2032', title: 'Update the onboarding slides', state: TaskState.DRAFT, position: 'a6', source: { ...MEETING, anchor: '30', offsetMs: 1802000n } });
const WITH_DRAFTS = [...TASKS, QUOTATION, SLIDES];

const confirmedQuotation = () => create(ConfirmDraftResponseSchema, { task: { ...QUOTATION, state: TaskState.CONFIRMED, position: 'Zz' } });
const band = () => within(screen.getByRole('complementary', { name: 'AI drafts' }));
const SYNC = 'Q4 launch weekly sync';

describe('the AI drafts band', () => {
  it('Board_DraftsOnlyInBandNeverInLanes', async () => {
    const { user } = openBoard({ tasks: WITH_DRAFTS });
    await ready();

    const drafts = band();
    expect(drafts.getByText('Send the quotation to ACME')).toBeInTheDocument();
    expect(drafts.getByText('Update the onboarding slides')).toBeInTheDocument();
    expect(drafts.getByText('Nothing here takes effect until you confirm it.')).toBeInTheDocument();
    expect(document.querySelector('.kiwi-pc')).toHaveTextContent('2 to confirm');
    // Never in a lane or a count: the heads still count only confirmed tasks.
    const lanes = within(document.querySelector<HTMLElement>('.bd-lanes') ?? document.body);
    expect(lanes.queryByText('Send the quotation to ACME')).not.toBeInTheDocument();
    expect(lanes.queryByText('Update the onboarding slides')).not.toBeInTheDocument();
    expect(document.querySelector('.bd-cols')).toHaveTextContent('To do5Doing0Done1');
    // The band comes before the column heads and the lanes in the scroller.
    const order = [...(document.querySelector('.bd-scroller')?.children ?? [])].map((child) => child.className || child.tagName);
    expect(order.indexOf('kiwi-panel')).toBeLessThan(order.indexOf('bd-cols'));

    // A filter never hides a draft.
    await user.selectOptions(screen.getByRole('combobox', { name: 'Channel' }), 'ch-ops');
    expect(band().getByText('Send the quotation to ACME')).toBeInTheDocument();
    expect(document.querySelector('.kiwi-pc')).toHaveTextContent('2 to confirm');
  });

  it('offers each draft the members of its own channel', async () => {
    const api = createStubApi();
    const ops = task({ id: 'draft-ops', ref: 'TSK-2040', title: 'Print the pick lists', channelId: 'ch-ops', state: TaskState.DRAFT, position: 'a7', source: { ...MEETING, id: 'meeting-9', channelId: 'ch-ops' } });
    openBoard({ tasks: [...WITH_DRAFTS, ops] }, api);
    api.chat.listMembers.mockImplementation(({ channelId }) =>
      Promise.resolve(create(ListMembersResponseSchema, { members: channelId === 'ch-ops' ? [member({ userId: 'user-arthit', name: 'Arthit K.' })] : MEMBERS })),
    );
    await ready();

    const slip = (title: string) => within(screen.getByRole('heading', { level: 3, name: title }).closest('li') ?? document.body);
    await waitFor(() => {
      expect(slip('Print the pick lists').getAllByRole('option').map((option) => option.textContent)).toEqual(['Choose an owner', 'Arthit K.']);
    });
    expect(slip('Send the quotation to ACME').getAllByRole('option').map((option) => option.textContent)).toEqual(['Choose an owner', 'Pimchanok S.', 'Arthit K.', 'Nok']);
  });

  it('saves an owner chosen on a draft and lets it be confirmed', async () => {
    const api = createStubApi();
    const { user } = openBoard({ tasks: WITH_DRAFTS }, api);
    await ready();
    api.tasks.updateTask.mockResolvedValue(create(UpdateTaskResponseSchema, { task: { ...SLIDES, assigneeId: 'user-arthit' } }));
    const slides = within(screen.getByRole('heading', { level: 3, name: 'Update the onboarding slides' }).closest('li') ?? document.body);
    expect(slides.getByRole('button', { name: 'Confirm: Update the onboarding slides' })).toBeDisabled();

    await waitFor(() => {
      expect(slides.getAllByRole('option')).toHaveLength(4);
    });
    await user.selectOptions(slides.getByRole('combobox', { name: 'Owner' }), 'user-arthit');

    expect(api.tasks.updateTask).toHaveBeenCalledWith({ id: 'draft-b', assigneeId: 'user-arthit' });
    await waitFor(() => {
      expect(slides.getByRole('button', { name: 'Confirm: Update the onboarding slides' })).toBeEnabled();
    });
  });

  it('Board_ConfirmingDraftLandsTornCardInToDoOfItsLaneAndClearsHidingFilters', async () => {
    const api = createStubApi();
    const { user } = openBoard({ tasks: WITH_DRAFTS }, api, 'ch-ops');
    await screen.findByRole('heading', { level: 3, name: 'Book two extra forklift shifts' });
    // #ops-warehouse is filtered: the draft's lane is not on screen.
    expect(screen.queryByRole('region', { name: new RegExp(`^${SYNC}`) })).not.toBeInTheDocument();
    api.tasks.confirmDraft.mockResolvedValue(confirmedQuotation());

    await user.click(band().getByRole('button', { name: 'Confirm: Send the quotation to ACME' }));

    expect(api.tasks.confirmDraft).toHaveBeenCalledWith({ id: 'draft-a' });
    // The filters cleared so it can be seen, and the draft left the band for its own lane.
    const todo = await waitFor(() => cell('To do', SYNC));
    expect(screen.getByRole('combobox', { name: 'Channel' })).toHaveValue('');
    expect(band().queryByText('Send the quotation to ACME')).not.toBeInTheDocument();
    // At the top of To do: the torn, sealed slip, with its Move button and no perforation.
    const first = todo.getAllByRole('heading', { level: 3 })[0];
    expect(first).toHaveTextContent('Send the quotation to ACME');
    const slip = first?.closest('li');
    expect(slip?.querySelector('.torn .kiwi-sealed')).not.toBeNull();
    expect(slip?.querySelector('.kiwi-seal')).not.toBeNull();
    expect(slip?.querySelector('.kiwi-perf')).toBeNull();
    expect(within(slip ?? document.body).getByRole('button', { name: 'Move TSK-2031: Send the quotation to ACME' })).toBeInTheDocument();
    expect(within(slip ?? document.body).getByText('Pimchanok S.', { exact: false })).toBeInTheDocument();
    expect(await screen.findByText(`Added to To do in ${SYNC}. Filters cleared so you can see it.`)).toBeInTheDocument();
    expect(document.querySelector('.kiwi-pc')).toHaveTextContent('1 to confirm');
  });

  it('Board_JustConfirmedCardSettlesToPlainCardOnFirstMove', async () => {
    const api = createStubApi();
    const { user } = openBoard({ tasks: WITH_DRAFTS }, api);
    await ready();
    api.tasks.confirmDraft.mockResolvedValue(confirmedQuotation());
    await user.click(band().getByRole('button', { name: 'Confirm: Send the quotation to ACME' }));
    await waitFor(() => {
      expect(cell('To do', SYNC).getAllByRole('heading', { level: 3 })[0]).toHaveTextContent('Send the quotation to ACME');
    });
    expect(card('Send the quotation to ACME').getByRole('button', { name: /^Move TSK-2031/ }).closest('li')?.querySelector('.torn')).not.toBeNull();
    api.tasks.moveTask.mockResolvedValue(create(MoveTaskResponseSchema, { task: { ...QUOTATION, state: TaskState.CONFIRMED, status: TaskStatus.DOING, position: 'a0' } }));

    await user.click(card('Send the quotation to ACME').getByRole('button', { name: /^Move TSK-2031/ }));
    await user.click(within(screen.getByRole('menu')).getByRole('menuitemradio', { name: 'Doing' }));

    const settled = (await within(screen.getByRole('group', { name: `Doing in ${SYNC}` })).findByRole('heading', { level: 3, name: 'Send the quotation to ACME' })).closest('li');
    expect(settled?.querySelector('.torn')).toBeNull();
    expect(settled?.querySelector('.kiwi-seal')).toBeNull();
    expect(settled?.querySelector('.kiwi-perf')).toBeNull();
    expect(settled?.querySelector('.kiwi-slip')).toHaveClass('bd-tcard');

    // Moved back to To do it is still a plain card: the seal belongs to the moment of confirming only.
    api.tasks.moveTask.mockResolvedValue(create(MoveTaskResponseSchema, { task: { ...QUOTATION, state: TaskState.CONFIRMED, status: TaskStatus.TODO, position: 'a0' } }));
    await user.click(within(settled ?? document.body).getByRole('button', { name: /^Move TSK-2031/ }));
    await user.click(within(screen.getByRole('menu')).getByRole('menuitemradio', { name: 'To do' }));
    await waitFor(() => {
      const back = cell('To do', SYNC).getByRole('heading', { level: 3, name: 'Send the quotation to ACME' }).closest('li');
      expect(back?.querySelector('.kiwi-slip')).toHaveClass('bd-tcard');
      expect(back?.querySelector('.torn, .kiwi-seal')).toBeNull();
    });
  });

  it.each([
    ['NotFound', Code.NotFound],
    ['Aborted', Code.Aborted],
  ])('on %s drops the vanished draft, loads the tasks again and seals nothing', async (_name, code) => {
    const api = createStubApi();
    const { user } = openBoard({ tasks: WITH_DRAFTS }, api);
    await ready();
    api.tasks.confirmDraft.mockRejectedValue(new ConnectError('gone', code));
    api.tasks.listTasks.mockResolvedValue(create(ListTasksResponseSchema, { tasks: [...TASKS, SLIDES] }));

    await user.click(band().getByRole('button', { name: 'Confirm: Send the quotation to ACME' }));

    await waitFor(() => {
      expect(band().queryByText('Send the quotation to ACME')).not.toBeInTheDocument();
    });
    expect(band().getByText('Update the onboarding slides')).toBeInTheDocument();
    expect(within(document.querySelector<HTMLElement>('.bd-lanes') ?? document.body).queryByText('Send the quotation to ACME')).not.toBeInTheDocument();
    expect(document.querySelector('.torn')).toBeNull();
  });

  it('keeps a draft a failed confirmation could not reach, with the error', async () => {
    const api = createStubApi();
    const { user } = openBoard({ tasks: WITH_DRAFTS }, api);
    await ready();
    api.tasks.confirmDraft.mockRejectedValue(new ConnectError('down', Code.Unavailable));

    await user.click(band().getByRole('button', { name: 'Confirm: Send the quotation to ACME' }));

    expect(await band().findByText("Couldn't confirm this draft. Try again.")).toBeInTheDocument();
    expect(band().getByRole('button', { name: 'Confirm: Send the quotation to ACME' })).toBeInTheDocument();
    expect(document.querySelector('.torn')).toBeNull();
  });
});

describe('New task', () => {
  const buttons = () => screen.getAllByRole('button', { name: 'New task' });
  const form = () => screen.getByRole('form', { name: 'New task' });
  const CREATED = () =>
    create(CreateTaskResponseSchema, {
      task: task({ id: 't-new', ref: 'TSK-2037', channelId: 'ch-ops', title: 'Print the pick lists', assigneeId: 'user-arthit', dueAt: due('2026-10-20'), position: 'a9', createdById: ME_ID, createdTime: '14:40' }),
    });

  it('opens one form at the top of the scroller from either header button, focuses Title and announces it', async () => {
    const { user } = openBoard({ tasks: WITH_DRAFTS });
    await ready();
    // The header's text button and the phones' 44px icon button: both report `aria-expanded` and name the form.
    expect(buttons()).toHaveLength(2);
    expect(buttons().map((button) => button.getAttribute('aria-expanded'))).toEqual(['false', 'false']);

    await user.click(buttons()[0]);

    expect(form().closest('.bd-nfw')?.parentElement?.firstElementChild).toBe(form().closest('.bd-nfw'));
    expect(within(form()).getByRole('heading', { name: 'New task' })).toBeInTheDocument();
    expect(within(form()).getByText('Added by hand: no meeting, message or document behind it.')).toBeInTheDocument();
    expect(within(form()).getByRole('textbox', { name: 'Title' })).toHaveFocus();
    expect(buttons().map((button) => button.getAttribute('aria-expanded'))).toEqual(['true', 'true']);
    expect(buttons().map((button) => button.getAttribute('aria-controls'))).toEqual([form().closest('.bd-nfw')?.id, form().closest('.bd-nfw')?.id]);
    expect(await screen.findByText('New task form opened.')).toBeInTheDocument();
    expect(screen.getAllByRole('form')).toHaveLength(1);

    await user.click(buttons()[1]);

    expect(screen.queryByRole('form', { name: 'New task' })).not.toBeInTheDocument();
    expect(buttons().map((button) => button.getAttribute('aria-expanded'))).toEqual(['false', 'false']);
  });

  it('NewTask_EmptyTitleShowsErrorAndKeepsTheFormOpen', async () => {
    const api = createStubApi();
    const { user } = openBoard({}, api);
    await ready();
    await user.click(buttons()[0]);

    await user.click(within(form()).getByRole('button', { name: 'Create task' }));

    expect(await within(form()).findByText("Title can't be empty.")).toBeInTheDocument();
    expect(within(form()).getByRole('textbox', { name: 'Title' })).toHaveAttribute('aria-invalid', 'true');
    expect(api.tasks.createTask).not.toHaveBeenCalled();
    expect(form()).toBeInTheDocument();
  });

  it('NewTask_DefaultChannelFollowsTheChannelFilter', async () => {
    const { user } = openBoard({}, createStubApi(), 'ch-ops');
    await screen.findByRole('heading', { level: 3, name: 'Book two extra forklift shifts' });

    await user.click(buttons()[0]);

    expect(within(form()).getByRole('combobox', { name: 'Channel' })).toHaveValue('ch-ops');
  });

  it('starts the Channel on the first channel when no filter is set, and lists member channels', async () => {
    const { user } = openBoard();
    await ready();

    await user.click(buttons()[0]);

    const channel = within(form()).getByRole('combobox', { name: 'Channel' });
    expect(channel).toHaveValue('ch-q4');
    expect(within(channel).getAllByRole('option').map((option) => option.textContent)).toEqual(['# q4-launch', '# ops-warehouse']);
  });

  it('NewTask_OwnerOptionsAreTheSelectedChannelsMembers', async () => {
    const api = createStubApi();
    const { user } = openBoard({}, api);
    api.chat.listMembers.mockImplementation(({ channelId }) =>
      Promise.resolve(create(ListMembersResponseSchema, { members: channelId === 'ch-ops' ? [member({ userId: 'user-arthit', name: 'Arthit K.' })] : MEMBERS })),
    );
    await ready();
    await user.click(buttons()[0]);
    const owner = within(form()).getByRole('combobox', { name: 'Owner' });
    await waitFor(() => {
      expect(within(owner).getAllByRole('option').map((option) => option.textContent)).toEqual(['No owner', 'Pimchanok S.', 'Arthit K.', 'Nok']);
    });

    await user.selectOptions(within(form()).getByRole('combobox', { name: 'Channel' }), 'ch-ops');

    await waitFor(() => {
      expect(within(owner).getAllByRole('option').map((option) => option.textContent)).toEqual(['No owner', 'Arthit K.']);
    });
  });

  it('NewTask_Create_CallsCreateTaskWithoutSourceOrState', async () => {
    const api = createStubApi();
    const { user } = openBoard({}, api);
    await ready();
    api.tasks.createTask.mockResolvedValue(CREATED());
    await user.click(buttons()[0]);
    await user.type(within(form()).getByRole('textbox', { name: 'Title' }), 'Print the pick lists');
    await user.selectOptions(within(form()).getByRole('combobox', { name: 'Channel' }), 'ch-ops');
    await waitFor(() => {
      expect(within(within(form()).getByRole('combobox', { name: 'Owner' })).getAllByRole('option').length).toBeGreaterThan(1);
    });
    await user.selectOptions(within(form()).getByRole('combobox', { name: 'Owner' }), 'user-arthit');
    const [day] = within(within(form()).getByRole('group', { name: 'Due date' })).getAllByRole('spinbutton');
    await user.click(day);
    await user.keyboard('20102026');

    await user.click(within(form()).getByRole('button', { name: 'Create task' }));

    await waitFor(() => {
      expect(api.tasks.createTask).toHaveBeenCalledOnce();
    });
    const request = api.tasks.createTask.mock.calls[0]?.[0] as CreateTaskRequest;
    // Confirmed at once, never a draft: no `state`, no `source`; due at midnight in Bangkok.
    expect(request).not.toHaveProperty('state');
    expect(request).not.toHaveProperty('source');
    expect(request).toEqual({ channelId: 'ch-ops', title: 'Print the pick lists', assigneeId: 'user-arthit', dueAt: timestampFromDate(new Date('2026-10-20T00:00:00+07:00')) });
  });

  it('NewTask_LandsInToDoOfAddedByHandLaneAndFocusesMove', async () => {
    const api = createStubApi();
    const { user } = openBoard({}, api);
    await ready();
    api.tasks.createTask.mockResolvedValue(CREATED());
    await user.click(buttons()[0]);
    await user.type(within(form()).getByRole('textbox', { name: 'Title' }), 'Print the pick lists');

    await user.click(within(form()).getByRole('button', { name: 'Create task' }));

    // In To do of the Added by hand lane: a plain card with its REF, `Added by` source and the channel first in its meta.
    const todo = await waitFor(() => cell('To do', 'Added by hand'));
    const created = within(await todo.findByRole('heading', { level: 3, name: 'Print the pick lists' }).then((heading) => heading.closest('li') ?? document.body));
    expect(created.getByText('REF TSK-2037')).toBeInTheDocument();
    expect(created.getByText(/Added by/).closest('.bd-src')).toHaveTextContent('Added by Pimchanok S. · 14:40');
    expect(created.getByText(/#ops-warehouse/).closest('p')).toHaveTextContent('#ops-warehouse · Arthit K. · Tue 20 Oct');
    expect(created.getByRole('button', { name: 'Move TSK-2037: Print the pick lists' })).toHaveFocus();
    expect(screen.queryByRole('form', { name: 'New task' })).not.toBeInTheDocument();
    expect(await screen.findByText('Task TSK-2037 created in Added by hand, To do.')).toBeInTheDocument();
    // Plain, not a draft and not sealed.
    expect(created.getByRole('button', { name: /^Move/ }).closest('li')?.querySelector('.torn, .kiwi-seal, .kiwi-perf, .kiwi-tag')).toBeNull();
    expect(within(screen.getByRole('region', { name: /^Added by hand/ })).getByText('Prepare the warehouse briefing')).toBeInTheDocument();
  });

  it('NewTask_ClearsFiltersThatWouldHideIt', async () => {
    const api = createStubApi();
    const { user } = openBoard({}, api, 'ch-q4');
    await ready();
    api.tasks.createTask.mockResolvedValue(CREATED());
    await user.click(buttons()[0]);
    await user.type(within(form()).getByRole('textbox', { name: 'Title' }), 'Print the pick lists');
    await user.selectOptions(within(form()).getByRole('combobox', { name: 'Channel' }), 'ch-ops');

    await user.click(within(form()).getByRole('button', { name: 'Create task' }));

    expect(await screen.findByText('Task TSK-2037 created in Added by hand, To do. Filters cleared so you can see it.')).toBeInTheDocument();
    expect(screen.getByRole('combobox', { name: 'Channel' })).toHaveValue('');
    expect(screen.getByRole('heading', { level: 3, name: 'Print the pick lists' })).toBeInTheDocument();
  });

  it('keeps the form open with the error when the request is rejected', async () => {
    const api = createStubApi();
    const { user } = openBoard({}, api);
    await ready();
    api.tasks.createTask.mockRejectedValue(new ConnectError('down', Code.Unavailable));
    await user.click(buttons()[0]);
    await user.type(within(form()).getByRole('textbox', { name: 'Title' }), 'Print the pick lists');

    await user.click(within(form()).getByRole('button', { name: 'Create task' }));

    expect(await within(form()).findByText("Couldn't create the task. Try again.")).toBeInTheDocument();
    expect(within(form()).getByRole('textbox', { name: 'Title' })).toHaveValue('Print the pick lists');
  });

  it.each([
    ['the header button', 0],
    ['the phones icon button', 1],
  ])('NewTask_EscapeCancelsAndRestoresFocusToTheOpener (%s)', async (_name, which) => {
    const { user } = openBoard();
    await ready();
    const opener = buttons()[which];
    await user.click(opener);
    expect(within(form()).getByRole('textbox', { name: 'Title' })).toHaveFocus();

    await user.keyboard('{Escape}');

    expect(screen.queryByRole('form', { name: 'New task' })).not.toBeInTheDocument();
    expect(opener).toHaveFocus();
    expect(opener).toHaveAttribute('aria-expanded', 'false');
  });

  it('Cancel closes the form and returns focus to the button that opened it', async () => {
    const { user } = openBoard();
    await ready();
    const opener = buttons()[0];
    await user.click(opener);

    await user.click(within(form()).getByRole('button', { name: 'Cancel' }));

    expect(screen.queryByRole('form', { name: 'New task' })).not.toBeInTheDocument();
    expect(opener).toHaveFocus();
  });

  it('is not offered to somebody who is in no channel', async () => {
    openBoard({ channels: [], tasks: [] });

    expect(await screen.findByRole('heading', { level: 3, name: 'No tasks yet' })).toBeInTheDocument();
    expect(buttons().every((button) => button.hasAttribute('disabled'))).toBe(true);
  });
});

describe('on a phone (390px)', () => {
  const MENU_BAR = /drafts to confirm/;

  it('Phone_SegmentedStatusShowsOneStatusPerLane', async () => {
    stubViewport(390);
    const { user } = openBoard();
    await ready();

    // The column heads give way to the three-way status control, with the counts.
    expect(document.querySelector('.bd-cols')).toBeNull();
    const status = within(screen.getByRole('group', { name: 'Status' }));
    expect(status.getAllByRole('button').map((button) => button.textContent)).toEqual(['To do 5', 'Doing 0', 'Done 1']);
    expect(status.getByRole('button', { name: /^To do/ })).toHaveAttribute('aria-pressed', 'true');
    // A lane shows one status: only To do cells, and every lane here has a task to do.
    expect(screen.queryAllByRole('group', { name: new RegExp(`^(Doing|Done) in `) })).toHaveLength(0);
    expect(screen.getAllByRole('group', { name: /^To do in / })).toHaveLength(4);

    await user.click(status.getByRole('button', { name: /^Done/ }));

    expect(status.getByRole('button', { name: /^Done/ })).toHaveAttribute('aria-pressed', 'true');
    // Only the lane that has a done task is drawn.
    expect(screen.getAllByRole('group', { name: /^Done in / })).toHaveLength(1);
    expect(screen.getByRole('group', { name: 'Done in ACME-contract-v3.pdf' })).toBeInTheDocument();
    expect(screen.queryByRole('region', { name: /^Added by hand/ })).not.toBeInTheDocument();

    await user.click(status.getByRole('button', { name: /^Doing/ }));

    expect(screen.getByText('Nothing in Doing')).toBeInTheDocument();
    expect(screen.queryAllByRole('region', { name: /^(Added|Q4|ACME|Messages)/ })).toHaveLength(0);
  });

  it('Phone_NewTaskIsA44pxIconButtonNamedNewTask', async () => {
    stubViewport(390);
    openBoard();
    await ready();

    const icon = screen.getAllByRole('button', { name: 'New task' }).find((button) => button.classList.contains('ask-ic'));
    expect(icon).toBeDefined();
    // The header's phone rules (jsdom lays nothing out, so the stylesheet itself is read): a 44px square, the text actions hidden.
    const phone = /@media \(max-width: 1023px\) \{([\s\S]*?)\n\}\n/.exec(screenCss)?.[1] ?? '';
    expect(phone).toMatch(/\.chead \.back,\s*\.chead \.ask-ic \{[^}]*display: inline-grid;[^}]*width: 44px;[^}]*height: 44px;/);
    expect(phone).toMatch(/\.chead \.acts \{[^}]*display: none;/);
    // Ask is the second 44px icon button, after New task.
    expect([...document.querySelectorAll('.chead .ask-ic')].map((element) => element.getAttribute('aria-label'))).toEqual(['New task', 'Ask']);
  });

  it('keeps the filter selects behind one Filters button, with the count of those that are set', async () => {
    stubViewport(390);
    const { user } = openBoard();
    await ready();
    const toggle = screen.getByRole('button', { name: /^Filters/ });
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByRole('combobox', { name: 'Channel' })).not.toBeInTheDocument();

    await user.click(toggle);

    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    await user.selectOptions(screen.getByRole('combobox', { name: 'Channel' }), 'ch-ops');
    expect(screen.getByRole('button', { name: /^Filters/ })).toHaveTextContent(/^Filters\s?\(1\)$/);
    expect(screen.getByRole('combobox', { name: 'Source type' })).toBeInTheDocument();
  });

  it('puts the drafts in a bottom sheet opened from the drafts bar, and lands a confirmed card in To do', async () => {
    stubViewport(390);
    const api = createStubApi();
    const { user } = openBoard({ tasks: WITH_DRAFTS }, api);
    await ready();
    expect(screen.queryByRole('complementary', { name: 'AI drafts' })).not.toBeInTheDocument();
    const bar = screen.getByRole('button', { name: MENU_BAR });
    expect(bar).toHaveTextContent('2 drafts to confirm');
    await user.click(bar);
    const sheet = within(await screen.findByRole('dialog', { name: 'AI drafts' }));
    expect(bar).toHaveAttribute('aria-expanded', 'true');
    api.tasks.confirmDraft.mockResolvedValue(confirmedQuotation());

    await user.click(sheet.getByRole('button', { name: 'Confirm: Send the quotation to ACME' }));

    // The sheet lists it under Confirmed just now with `Added to To do`, where Move would be.
    const justNow = await sheet.findByRole('list', { name: 'Confirmed just now' });
    expect(justNow).toHaveTextContent('Added to To do');
    expect(within(justNow).queryByRole('button', { name: /^Move/ })).not.toBeInTheDocument();
    // The page behind a modal sheet is out of reach for assistive tech, hence `hidden`.
    expect(screen.getByRole('button', { name: /^To do/, hidden: true })).toHaveAttribute('aria-pressed', 'true');
    expect(await screen.findByText(`Added to To do in ${SYNC}.`)).toBeInTheDocument();
  });

  it('returns focus to the bar through its ref when the sheet closes, even though the bar was never focused', async () => {
    stubViewport(390);
    const { user } = openBoard({ tasks: WITH_DRAFTS });
    await ready();
    expect(document.body).toHaveFocus();
    const bar = screen.getByRole('button', { name: MENU_BAR });

    // `fireEvent` does not focus the button, so the sheet's opener is not the page's active element when it opens.
    fireEvent.click(bar);
    await screen.findByRole('dialog', { name: 'AI drafts' });
    await user.keyboard('{Escape}');

    await waitFor(() => {
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });
    expect(bar).toHaveFocus();
  });

  it('closes an open excerpt on Escape before it closes the sheet that holds it', async () => {
    stubViewport(390);
    const api = createStubApi();
    const { user } = openBoard({ tasks: WITH_DRAFTS }, api);
    await ready();
    api.meetings.getTranscriptExcerpt.mockResolvedValue(
      create(GetTranscriptExcerptResponseSchema, { excerpt: create(ExcerptSchema, { meetingId: 'meeting-1', title: SYNC, entries: [{ idx: 21, text: 'Send it Friday', startMs: 1307000n }] }) }),
    );
    await user.click(screen.getByRole('button', { name: MENU_BAR }));
    const sheet = within(await screen.findByRole('dialog', { name: 'AI drafts' }));
    await user.click(sheet.getByRole('button', { name: 'Transcript at 0:21:47' }));
    await sheet.findByRole('group', { name: 'Transcript excerpt at 0:21:47' });

    await user.keyboard('{Escape}');

    await waitFor(() => {
      expect(sheet.queryByRole('group', { name: /excerpt/ })).not.toBeInTheDocument();
    });
    expect(screen.getByRole('dialog', { name: 'AI drafts' })).toBeInTheDocument();
  });

  it('moves a card, and puts focus on the status control the card has left', async () => {
    stubViewport(390);
    const api = createStubApi();
    const { user } = openBoard({}, api);
    await ready();
    api.tasks.moveTask.mockResolvedValue(create(MoveTaskResponseSchema, { task: { ...ANNOUNCE, status: TaskStatus.DOING, position: 'a0' } }));

    await user.click(card('Announce the new launch date').getByRole('button', { name: /^Move TSK-2030/ }));
    await user.click(within(screen.getByRole('menu')).getByRole('menuitemradio', { name: 'Doing' }));

    expect(await screen.findByText('Moved TSK-2030 to Doing.')).toBeInTheDocument();
    expect(screen.queryByText('Announce the new launch date')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: /^To do/ })).toHaveFocus();
    expect(screen.getByRole('button', { name: /^Doing/ })).toHaveTextContent('Doing 1');
  });
});
