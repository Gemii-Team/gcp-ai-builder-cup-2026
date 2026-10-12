import { Code, ConnectError } from '@connectrpc/connect';
import { create } from '@bufbuild/protobuf';
import { timestampFromDate } from '@bufbuild/protobuf/wkt';
import {
  ArtifactKind,
  CreateTaskResponseSchema,
  MentionKind,
  SourceKind,
  StartMeetingResponseSchema,
  type CreateTaskRequest,
} from '@kiwi/api-client';
import { screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { keys } from '../../api/queries';
import type { TaskView } from '../../api/types';
import { hardNavigate } from '../../navigation';
import { useUiStore } from '../../state/ui';
import { agentMessage, artifact, at, meeting, message, task } from '../../testing/fixtures';
import { LOG, openChannel } from '../../testing/openChannel';
import { createStubApi, type StubApi } from '../../testing/stubApi';

beforeEach(() => {
  localStorage.clear();
  useUiStore.setState({ lang: 'en', readSeq: {}, lastChannelId: null, draftsSheetOpen: false });
});

afterEach(() => {
  vi.restoreAllMocks();
});

const ACTION = /^Make a task from/;
const titleField = () => screen.getByRole('textbox', { name: 'Title' });
const runes = (text: string) => Array.from(text).length;

describe('MessageAction', () => {
  it('MessageAction_OnlyOnHumanMessages_NeverOnAgentMessagesOrSlips', async () => {
    const summary = artifact({ kind: ArtifactKind.MEETING_SUMMARY, ref: 'MTG-1014', title: 'Q4 launch weekly sync', bodyMarkdown: 'Moved.' });
    openChannel({
      messages: [
        message({ seq: 4, time: '14:10', authorId: 'user-nok', body: '', files: [{ fileId: 'file-1', name: 'warehouse-slots.csv', mime: 'text/csv', size: 2048n }] }),
        agentMessage({ seq: 3, time: '14:31', artifact: summary }),
        agentMessage({ seq: 2, time: '14:05', body: 'Sure, drafting now.' }),
        message({ seq: 1, time: '14:02', body: 'Hello team' }),
      ],
    });
    const log = await screen.findByRole('log', LOG);
    await within(log).findByText('Hello team');

    const actions = within(log).getAllByRole('button', { name: ACTION });

    expect(actions.map((action) => action.getAttribute('aria-label'))).toEqual([
      "Make a task from Arthit K.'s message at 14:02",
      "Make a task from Nok's message at 14:10",
    ]);
    for (const row of within(log).getAllByRole('article')) {
      const human = within(row).queryByText('@assistant') === null && within(row).queryByText('K.I.W.I.') === null;
      expect(within(row).queryAllByRole('button', { name: ACTION })).toHaveLength(human ? 1 : 0);
    }
    expect(actions[0]).toHaveAttribute('aria-expanded', 'false');
    expect(actions[0]).toHaveAttribute('title', 'Make a task');
    expect(within(actions[0]).getByText('Make a task')).toBeInTheDocument();
  });
});

describe('MakeTask', () => {
  it('MakeTask_PrefillsTitleFromMessageClippedTo120Runes_AndFollowUpOnFileForFileOnlyMessages', async () => {
    const long = `${'ก'.repeat(130)} tail`;
    const { user } = openChannel({
      messages: [
        message({ seq: 4, time: '14:10', body: '', files: [{ fileId: 'file-1', name: 'warehouse-slots.csv', mime: 'text/csv', size: 2048n }] }),
        message({ seq: 3, time: '14:08', body: '@Nok please   update the slides', mentions: [{ kind: MentionKind.USER, id: 'user-nok', label: 'Nok' }] }),
        message({ seq: 2, time: '14:05', body: '@assistant   what is   due?', mentions: [{ kind: MentionKind.AGENT, id: 'assistant', label: 'assistant' }] }),
        message({ seq: 1, time: '14:02', body: long }),
      ],
    });
    const log = await screen.findByRole('log', LOG);
    await within(log).findByText(/ก{20}/);
    const open = async (time: string) => {
      await user.click(within(log).getByRole('button', { name: new RegExp(`at ${time}$`) }));
      return titleField();
    };

    const clipped = await open('14:02');
    expect(runes((clipped as HTMLInputElement).value)).toBe(120);
    expect((clipped as HTMLInputElement).value).toBe(`${'ก'.repeat(119)}…`);
    expect(clipped).toHaveFocus();
    expect(screen.getByRole('form', { name: 'Make a task from this message' })).toHaveTextContent('From #q4-launch · Arthit K. · 14:02');
    expect(clipped).toHaveAttribute('lang', 'th');

    expect(((await open('14:05')) as HTMLInputElement).value).toBe('what is due?');
    expect(screen.getAllByRole('form')).toHaveLength(1);
    expect(((await open('14:08')) as HTMLInputElement).value).toBe('please update the slides');
    expect(((await open('14:10')) as HTMLInputElement).value).toBe('Follow up on warehouse-slots.csv');

    // Escape closes the form and returns focus to the action that opened it.
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('form')).not.toBeInTheDocument();
    expect(within(log).getByRole('button', { name: /at 14:10$/ })).toHaveFocus();
  });

  it('MakeTask_TitleClipCountsCodePointsAndNeverSplitsAnEmojiOrAThaiCluster', async () => {
    const emojis = '😀'.repeat(125);
    const thai = `${'ก'.repeat(118)}น้ and the rest of the message`;
    const { user } = openChannel({
      messages: [message({ seq: 2, time: '14:08', body: thai }), message({ seq: 1, time: '14:02', body: emojis })],
    });
    const log = await screen.findByRole('log', LOG);
    await within(log).findByText(emojis);

    await user.click(within(log).getByRole('button', { name: /at 14:02$/ }));
    const astral = (titleField() as HTMLInputElement).value;
    // 125 astral runes are 250 UTF-16 units: counted in runes the title is 119 emoji and the ellipsis, 120 in all.
    expect(astral).toBe(`${'😀'.repeat(119)}…`);
    expect(runes(astral)).toBe(120);
    expect(astral).not.toMatch(/[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/);

    await user.click(within(log).getByRole('button', { name: /at 14:08$/ }));
    // The consonant and its tone mark are one cluster: it does not fit in 119 runes, so the title ends before it.
    expect((titleField() as HTMLInputElement).value).toBe(`${'ก'.repeat(118)}…`);
  });

  const CREATED = (init: Parameters<typeof task>[0] = {}) =>
    create(CreateTaskResponseSchema, {
      task: task({
        id: 'task-new',
        ref: 'TSK-2035',
        title: 'Send the quotation to ACME',
        source: { kind: SourceKind.MESSAGE, id: 'msg-1', anchor: '1', label: 'Arthit K.', channelId: 'ch-q4' },
        ...init,
      }),
    });

  /** The channel with one human message, its form open. */
  async function withForm(api: StubApi = createStubApi()) {
    const opened = openChannel({ messages: [message({ seq: 1, time: '14:02', body: 'Send the quotation to ACME' })] }, api);
    const log = await screen.findByRole('log', LOG);
    await within(log).findByText('Send the quotation to ACME');
    await opened.user.click(within(log).getByRole('button', { name: ACTION }));
    await screen.findByRole('textbox', { name: 'Title' });
    return { ...opened, log };
  }

  it('MakeTask_Submit_CallsCreateTaskConfirmedWithMessageSource', async () => {
    const api = createStubApi();
    const { user } = await withForm(api);
    api.tasks.createTask.mockResolvedValue(CREATED());
    await waitFor(() => {
      expect(within(screen.getByRole('combobox', { name: 'Owner' })).getAllByRole('option')).toHaveLength(4);
    });
    await user.selectOptions(screen.getByRole('combobox', { name: 'Owner' }), 'Nok');
    const [day] = within(screen.getByRole('group', { name: 'Due date' })).getAllByRole('spinbutton');
    await user.click(day);
    await user.keyboard('16102026');

    await user.click(screen.getByRole('button', { name: 'Create task' }));

    await waitFor(() => {
      expect(api.tasks.createTask).toHaveBeenCalledOnce();
    });
    const request = api.tasks.createTask.mock.calls[0]?.[0] as CreateTaskRequest;
    // Confirmed at once, never a draft: no `state`; due at midnight in Bangkok; the message as the source.
    expect(request).not.toHaveProperty('state');
    expect(request).toEqual({
      channelId: 'ch-q4',
      title: 'Send the quotation to ACME',
      assigneeId: 'user-nok',
      dueAt: timestampFromDate(new Date('2026-10-16T00:00:00+07:00')),
      source: { kind: SourceKind.MESSAGE, id: 'msg-1', anchor: '1', label: 'Arthit K.', channelId: 'ch-q4', at: timestampFromDate(at('14:02')) },
    });
  });

  it('asks for no owner and no due date when none was chosen', async () => {
    const api = createStubApi();
    const { user } = await withForm(api);
    api.tasks.createTask.mockResolvedValue(CREATED());

    await user.click(screen.getByRole('button', { name: 'Create task' }));

    await waitFor(() => {
      expect(api.tasks.createTask).toHaveBeenCalledOnce();
    });
    const request = api.tasks.createTask.mock.calls[0]?.[0] as CreateTaskRequest;
    expect(request).not.toHaveProperty('assigneeId');
    expect(request).not.toHaveProperty('dueAt');
    expect(request).not.toHaveProperty('state');
  });

  it('MakeTask_ShowsCreatedCardWithRefAndViewOnBoardAndMovesFocus', async () => {
    const api = createStubApi();
    const { user, log, queryClient } = await withForm(api);
    queryClient.setQueryData<TaskView[]>(keys.tasks(), []);
    api.tasks.createTask.mockResolvedValue(CREATED());

    await user.click(screen.getByRole('button', { name: 'Create task' }));

    const card = await within(log).findByRole('group', { name: 'Task TSK-2035' });
    expect(screen.queryByRole('form')).not.toBeInTheDocument();
    expect(card).toHaveFocus();
    expect(card).toHaveTextContent('REF TSK-2035');
    expect(card).toHaveTextContent('From #q4-launch · Arthit K. · 14:02');
    expect(within(card).getByRole('link', { name: 'View on Board' })).toHaveAttribute('href', '/board');
    // A task a person made is confirmed at once: no draft look, no seal, no perforation, no torn edge.
    expect(card.closest('.torn')).toBeNull();
    expect(card.querySelector('.kiwi-perf, .kiwi-tag, .kiwi-seal')).toBeNull();
    expect(within(card).queryByText('Draft')).not.toBeInTheDocument();
    expect(await screen.findByText('Task TSK-2035 created: Send the quotation to ACME. It is on the Board under To do.')).toBeInTheDocument();
    // The task is in both caches without waiting for the realtime event.
    expect(queryClient.getQueryData<TaskView[]>(keys.tasks())?.map((t) => t.id)).toEqual(['task-new']);
    expect(queryClient.getQueryData<TaskView[]>(keys.channelTasks('ch-q4'))?.map((t) => t.id)).toEqual(['task-new']);
  });

  it('MakeTask_ServerRejection_KeepsTheFormOpenWithTheError', async () => {
    const api = createStubApi();
    const { user, log } = await withForm(api);
    api.tasks.createTask.mockRejectedValue(new ConnectError('not a member', Code.PermissionDenied));

    await user.click(screen.getByRole('button', { name: 'Create task' }));

    expect(await screen.findByText("Couldn't create the task. Try again.")).toBeInTheDocument();
    expect(screen.getByRole('form', { name: 'Make a task from this message' })).toBeInTheDocument();
    expect(within(log).queryByRole('group', { name: /^Task / })).not.toBeInTheDocument();
  });

  it('shows the card of a task already made from the message, to anyone who opens the channel', async () => {
    openChannel({
      messages: [message({ seq: 1, time: '14:02', body: 'Send the quotation to ACME' })],
      tasks: [task({ id: 'task-7', ref: 'TSK-2031', source: { kind: SourceKind.MESSAGE, id: 'msg-1', anchor: '1', label: 'Arthit K.', channelId: 'ch-q4' } })],
    });
    const log = await screen.findByRole('log', LOG);

    expect(await within(log).findByRole('group', { name: 'Task TSK-2031' })).not.toHaveFocus();
  });
});

describe('Start meeting', () => {
  it('StartMeeting_ConsentRequiredRedirects', async () => {
    const api = createStubApi();
    const { user } = openChannel({}, api);
    vi.spyOn(hardNavigate, 'to').mockImplementation(() => undefined);
    api.meetings.startMeeting.mockRejectedValue(new ConnectError('Meet access has not been granted', Code.FailedPrecondition));

    await user.click(await screen.findByRole('button', { name: 'Start meeting' }));

    await waitFor(() => {
      expect(hardNavigate.to).toHaveBeenCalledWith('/api/auth/meet-consent?return_to=%2Fc%2Fch-q4');
    });
    expect(api.meetings.startMeeting).toHaveBeenCalledWith({ channelId: 'ch-q4' });
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('opens the Meet link of the meeting it started, as the signed-in account', async () => {
    const api = createStubApi();
    const { user } = openChannel({}, api);
    vi.spyOn(hardNavigate, 'open').mockImplementation(() => undefined);
    api.meetings.startMeeting.mockResolvedValue(create(StartMeetingResponseSchema, { meeting: meeting({ meetUri: 'https://meet.google.com/abc-defg-hij' }) }));

    await user.click(await screen.findByRole('button', { name: 'Start meeting' }));

    await waitFor(() => {
      expect(hardNavigate.open).toHaveBeenCalledWith('https://meet.google.com/abc-defg-hij?authuser=pim%40siamriverside.co.th');
    });
  });

  it('says so when the meeting could not be started', async () => {
    const api = createStubApi();
    const { user } = openChannel({}, api);
    vi.spyOn(hardNavigate, 'to').mockImplementation(() => undefined);
    api.meetings.startMeeting.mockRejectedValue(new ConnectError('down', Code.Unavailable));

    await user.click(await screen.findByRole('button', { name: 'Start meeting' }));

    expect(await screen.findByRole('alert')).toHaveTextContent("Couldn't start the meeting. Try again.");
    expect(hardNavigate.to).not.toHaveBeenCalled();
  });
});
