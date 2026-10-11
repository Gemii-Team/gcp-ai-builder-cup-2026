import { Code, ConnectError } from '@connectrpc/connect';
import { create } from '@bufbuild/protobuf';
import {
  ConfirmDraftResponseSchema,
  ExcerptSchema,
  GetTranscriptExcerptResponseSchema,
  ListTasksResponseSchema,
  SourceKind,
  TaskState,
} from '@kiwi/api-client';
import { act, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useUiStore } from '../../state/ui';
import { message, task } from '../../testing/fixtures';
import { LOG, openChannel } from '../../testing/openChannel';
import { createStubApi, type StubApi } from '../../testing/stubApi';

beforeEach(() => {
  localStorage.clear();
  useUiStore.setState({ lang: 'en', readSeq: {}, lastChannelId: null, draftsSheetOpen: false });
});

afterEach(() => {
  vi.restoreAllMocks();
});

const meetingSource = { kind: SourceKind.MEETING, id: 'meeting-1', anchor: '21', label: 'Q4 sync', channelId: 'ch-q4', offsetMs: 1307000n };
const DRAFTS = [
  task({ id: 'draft-1', ref: 'TSK-2031', title: 'Send the quotation to ACME', state: TaskState.DRAFT, assigneeId: 'user-nok', source: meetingSource }),
  task({ id: 'draft-2', ref: 'TSK-2032', title: 'Update the onboarding slides', state: TaskState.DRAFT, source: meetingSource }),
];

async function withDrafts(api: StubApi = createStubApi()) {
  const opened = openChannel({ messages: [message({ seq: 1, body: 'hello' })], tasks: DRAFTS }, api);
  await within(await screen.findByRole('log', LOG)).findByText('hello');
  return opened;
}

const bar = () => screen.getByRole('button', { name: /drafts to confirm/ });

describe('the drafts panel', () => {
  it('lists the drafts of the channel with their owners and counts them', async () => {
    await withDrafts();

    const panel = await screen.findByRole('complementary', { name: 'AI drafts' });
    expect(await within(panel).findByText('Send the quotation to ACME')).toBeInTheDocument();
    expect(within(panel).getByText('Update the onboarding slides')).toBeInTheDocument();
    expect(panel).toHaveTextContent('2 to confirm');
    expect(bar()).toHaveTextContent('2 drafts to confirm');
  });

  it('confirms a draft through the server, then seals it into the Confirmed list', async () => {
    const api = createStubApi();
    const { user } = await withDrafts(api);
    api.tasks.confirmDraft.mockResolvedValue(
      create(ConfirmDraftResponseSchema, {
        task: task({ id: 'draft-1', ref: 'TSK-2031', title: 'Send the quotation to ACME', state: TaskState.CONFIRMED, assigneeId: 'user-nok', source: meetingSource }),
      }),
    );
    const panel = within(await screen.findByRole('complementary', { name: 'AI drafts' }));

    await user.click(await panel.findByRole('button', { name: 'Confirm: Send the quotation to ACME' }));

    await waitFor(() => {
      expect(panel.queryByRole('button', { name: 'Confirm: Send the quotation to ACME' })).not.toBeInTheDocument();
    });
    expect(api.tasks.confirmDraft).toHaveBeenCalledWith({ id: 'draft-1' });
    expect(within(panel.getByRole('list', { name: 'Confirmed' })).getByText('Send the quotation to ACME')).toBeInTheDocument();
    expect(panel.getByText(/to confirm/)).toHaveTextContent('1 to confirm');
  });

  it.each([
    ['NotFound', Code.NotFound],
    ['Aborted', Code.Aborted],
  ])('on %s drops the draft that no longer exists, loads the tasks again and leaves it unsealed with the error', async (_name, code) => {
    const api = createStubApi();
    const { user } = await withDrafts(api);
    api.tasks.confirmDraft.mockRejectedValue(new ConnectError('gone', code));
    api.tasks.listTasks.mockResolvedValue(create(ListTasksResponseSchema, { tasks: [DRAFTS[1]] }));
    const panel = within(await screen.findByRole('complementary', { name: 'AI drafts' }));
    const before = api.tasks.listTasks.mock.calls.length;

    await user.click(await panel.findByRole('button', { name: 'Confirm: Send the quotation to ACME' }));

    await waitFor(() => {
      expect(panel.queryByText('Send the quotation to ACME')).not.toBeInTheDocument();
    });
    expect(api.tasks.listTasks.mock.calls.length).toBeGreaterThan(before);
    expect(panel.getByText('Update the onboarding slides')).toBeInTheDocument();
    expect(panel.queryByRole('list', { name: 'Confirmed' })).not.toBeInTheDocument();
  });

  it('keeps a draft a failed confirmation could not reach as it was, with the error', async () => {
    const api = createStubApi();
    const { user } = await withDrafts(api);
    api.tasks.confirmDraft.mockRejectedValue(new ConnectError('down', Code.Unavailable));
    const panel = within(await screen.findByRole('complementary', { name: 'AI drafts' }));

    await user.click(await panel.findByRole('button', { name: 'Confirm: Send the quotation to ACME' }));

    expect(await panel.findByText("Couldn't confirm this draft. Try again.")).toBeInTheDocument();
    expect(panel.getByRole('button', { name: 'Confirm: Send the quotation to ACME' })).toBeInTheDocument();
    expect(panel.queryByRole('list', { name: 'Confirmed' })).not.toBeInTheDocument();
  });
});

describe('the phone drafts sheet', () => {
  it('DraftsSheet_FocusTrapEscapeAndReturnFocus', async () => {
    const { user } = await withDrafts();
    const opener = bar();
    expect(opener).toHaveAttribute('aria-expanded', 'false');

    await user.click(opener);

    const sheet = await screen.findByRole('dialog', { name: 'AI drafts' });
    expect(opener).toHaveAttribute('aria-expanded', 'true');
    expect(opener).toHaveAttribute('aria-controls', sheet.id);
    expect(within(sheet).getByRole('button', { name: 'Close' })).toHaveFocus();
    // The page behind is out of reach and focus cannot leave the sheet.
    expect(screen.queryByRole('log', LOG)).not.toBeInTheDocument();
    await user.tab();
    await user.tab();
    await user.tab();
    expect(sheet).toContainElement(document.activeElement as HTMLElement);

    await user.keyboard('{Escape}');

    await waitFor(() => {
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });
    expect(opener).toHaveFocus();
    expect(opener).toHaveAttribute('aria-expanded', 'false');
    expect(screen.getByRole('log', LOG)).toBeInTheDocument();
  });

  it('closes an open excerpt on Escape before it closes the sheet that holds it', async () => {
    const api = createStubApi();
    const { user } = await withDrafts(api);
    api.meetings.getTranscriptExcerpt.mockResolvedValue(
      create(GetTranscriptExcerptResponseSchema, { excerpt: create(ExcerptSchema, { meetingId: 'meeting-1', title: 'Q4 sync', entries: [{ idx: 21, text: 'Send it Friday', startMs: 1307000n }] }) }),
    );
    await user.click(bar());
    const sheet = within(await screen.findByRole('dialog', { name: 'AI drafts' }));
    await user.click((await sheet.findAllByRole('button', { name: 'Transcript at 0:21:47' }))[0]);
    await sheet.findByRole('group', { name: 'Transcript excerpt at 0:21:47' });

    await user.keyboard('{Escape}');

    await waitFor(() => {
      expect(sheet.queryByRole('group', { name: /excerpt/ })).not.toBeInTheDocument();
    });
    expect(screen.getByRole('dialog', { name: 'AI drafts' })).toBeInTheDocument();
    await user.keyboard('{Escape}');
    await waitFor(() => {
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });
  });

  it('lets Escape cancel a title edit inside the sheet without closing the sheet', async () => {
    const { user } = await withDrafts();
    await user.click(bar());
    const sheet = within(await screen.findByRole('dialog', { name: 'AI drafts' }));

    await user.click(await sheet.findByRole('button', { name: 'Edit: Send the quotation to ACME' }));
    expect(sheet.getByRole('textbox', { name: 'Task title' })).toHaveFocus();
    await user.keyboard('{Escape}');

    expect(sheet.queryByRole('textbox', { name: 'Task title' })).not.toBeInTheDocument();
    expect(screen.getByRole('dialog', { name: 'AI drafts' })).toBeInTheDocument();
  });

  it('closes with its close button and returns focus to the bar', async () => {
    const { user } = await withDrafts();
    await user.click(bar());

    await user.click(within(await screen.findByRole('dialog', { name: 'AI drafts' })).getByRole('button', { name: 'Close' }));

    await waitFor(() => {
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });
    expect(bar()).toHaveFocus();
    await act(async () => Promise.resolve());
  });
});
