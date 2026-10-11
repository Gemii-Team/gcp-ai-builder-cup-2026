import { Code, ConnectError } from '@connectrpc/connect';
import { ListTasksResponseSchema } from '@kiwi/api-client';
import { keys } from '../../api/queries';
import { create } from '@bufbuild/protobuf';
import { timestampFromDate } from '@bufbuild/protobuf/wkt';
import {
  AgentRunState,
  ArtifactKind,
  CreateTaskResponseSchema,
  DiscardDraftResponseSchema,
  GetFileResponseSchema,
  GetMeetingResponseSchema,
  GetTranscriptExcerptResponseSchema,
  SourceKind,
  StartRunResponseSchema,
  TaskState,
  UpdateTaskResponseSchema,
} from '@kiwi/api-client';
import { act, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useUiStore } from '../../state/ui';
import { artifact, at, channel, file, frame, meeting, run, runFrame, task } from '../../testing/fixtures';
import { renderApp } from '../../testing/render';
import { createStubApi, seedApi } from '../../testing/stubApi';
import { stubViewport } from '../../testing/viewport';
import { AskScreen } from './AskScreen';

beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
  useUiStore.setState({ lang: 'en', readSeq: {}, lastChannelId: null, draftsSheetOpen: false });
});

afterEach(() => {
  vi.useRealTimers();
  Reflect.deleteProperty(Element.prototype, 'scrollIntoView');
});

const CHANNELS = [channel({ id: 'ch-q4', name: 'q4-launch' }), channel({ id: 'ch-ops', name: 'ops-warehouse' })];
const MEETING = { kind: SourceKind.MEETING, id: 'meeting-1', anchor: '12', label: 'Q4 launch weekly sync', channelId: 'ch-q4', offsetMs: 761000n };

const ANSWER = artifact({
  kind: ArtifactKind.ANSWER,
  ref: 'ANS-2209',
  bodyMarkdown: '**Decided:** the launch moved to 15 Nov 2026.[1] The contract ends on 31 Dec.[2] Arthit confirmed the warehouse is free.[3]',
  scopeChannelIds: ['ch-q4', 'ch-ops'],
  generatedAt: timestampFromDate(at('14:51')),
  citations: [
    { n: 1, chunkId: 'c1', quote: 'งั้นเราเลื่อนวันเปิดตัวไปเป็น 15 พฤศจิกายนนะคะ', source: MEETING },
    { n: 2, chunkId: 'c2', quote: '4.1 Term. This Agreement ends on 31 December 2026.', source: { kind: SourceKind.FILE, id: 'file-1', anchor: 'page:4', label: 'ACME-contract-v3.pdf', channelId: 'ch-q4', page: 4 } },
    { n: 3, chunkId: 'c3', quote: 'ยืนยันแล้วครับ คลังบางนาว่าง', source: { kind: SourceKind.MESSAGE, id: 'msg-41', anchor: '41', label: 'Arthit K.', channelId: 'ch-ops', at: timestampFromDate(at('14:02')) } },
  ],
});

const OTHER = artifact({
  kind: ArtifactKind.ANSWER,
  ref: 'ANS-2211',
  bodyMarkdown: 'The late fee is 2% a day.[1]',
  scopeChannelIds: ['ch-q4'],
  citations: [{ n: 1, chunkId: 'c9', quote: '4.2 Late delivery. 2% a day.', source: { kind: SourceKind.FILE, id: 'file-1', anchor: 'page:4', label: 'ACME-contract-v3.pdf', channelId: 'ch-q4', page: 4 } }],
});

const DRAFT = task({
  id: 'draft-1',
  ref: 'TSK-2036',
  title: 'Decided: the launch moved to 15 Nov 2026.',
  state: TaskState.DRAFT,
  fromRef: 'ANS-2209',
  source: { ...MEETING, quote: 'งั้นเราเลื่อนวันเปิดตัวไปเป็น 15 พฤศจิกายนนะคะ' },
});

function open() {
  const api = seedApi(createStubApi(), { channels: CHANNELS });
  api.meetings.getMeeting.mockResolvedValue(create(GetMeetingResponseSchema, { meeting: meeting({ id: 'meeting-1', ref: 'MTG-1014', startedAt: timestampFromDate(at('14:00')) }) }));
  api.files.getFile.mockResolvedValue(create(GetFileResponseSchema, { file: file({ id: 'file-1', name: 'ACME-contract-v3.pdf', ref: 'DOC-0371', pages: 18 }) }));
  api.meetings.getTranscriptExcerpt.mockResolvedValue(
    create(GetTranscriptExcerptResponseSchema, {
      excerpt: { title: 'Q4 launch weekly sync', entries: [{ speaker: 'พิมพ์ชนก ส.', startMs: 761000n, text: 'งั้นเราเลื่อนวันเปิดตัวไปเป็น 15 พฤศจิกายนนะคะ' }] },
    }),
  );
  const rendered = renderApp({ path: '/ask', api, routes: [{ path: '/ask', element: <AskScreen initialChannelId={null} /> }] });
  return { ...rendered, field: () => screen.getByRole('textbox', { name: 'Ask a question, in English or Thai' }) };
}

/** Asks `question`, then answers it with `answer` through the progress frame. */
async function answered(opened: ReturnType<typeof open>, question: string, runId: string, answer = ANSWER) {
  await screen.findByText(/Searching/);
  opened.api.agents.startRun.mockResolvedValueOnce(create(StartRunResponseSchema, { run: run({ id: runId, question }) }));
  await opened.user.type(opened.field(), `${question}{Enter}`);
  await waitFor(() => {
    expect(opened.api.agents.startRun.mock.calls.length).toBeGreaterThan(0);
  });
  await act(() => Promise.resolve());
  act(() => {
    opened.client.emit(runFrame({ runId, state: AgentRunState.DONE, answer }));
  });
}

const panel = () => screen.getByRole('complementary', { name: 'Sources' });

describe('the Sources panel', () => {
  it('SourcesPanel_SwapsToTheSelectedAnswersSources', async () => {
    const opened = open();
    await answered(opened, 'What did we decide?', 'run-1');

    // The answer that has just arrived is the one on show: heading, count, REF and question, one slip per citation.
    expect(within(panel()).getByRole('heading', { level: 2, name: 'Sources' })).toBeInTheDocument();
    expect(panel()).toHaveTextContent('3 cited');
    expect(panel()).toHaveTextContent('ANS-2209 What did we decide?');
    const slips = within(within(panel()).getByRole('list', { name: 'Cited sources' })).getAllByRole('listitem');
    expect(slips.map((slip) => slip.getAttribute('data-n'))).toEqual(['1', '2', '3']);
    expect(screen.getByRole('button', { name: 'Sources of ANS-2209' })).toHaveAttribute('aria-pressed', 'true');

    // A second answer takes the panel; pressing the first answer's Sources button swaps it back.
    await answered(opened, 'What is the late fee?', 'run-2', OTHER);
    expect(panel()).toHaveTextContent('1 cited');
    expect(panel()).toHaveTextContent('ANS-2211 What is the late fee?');
    expect(screen.getByRole('button', { name: 'Sources of ANS-2211' })).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByRole('button', { name: 'Sources of ANS-2209' })).toHaveAttribute('aria-pressed', 'false');

    await opened.user.click(screen.getByRole('button', { name: 'Sources of ANS-2209' }));

    expect(panel()).toHaveTextContent('3 cited');
    expect(panel()).toHaveTextContent('ANS-2209 What did we decide?');
    expect(await screen.findByText('Showing sources for ANS-2209.', { selector: '[role="status"]' })).toBeInTheDocument();
  });

  // A message is dated unless it was sent today, so the clock is frozen: the fixture message is from 13 Oct (Bangkok).
  it.each([
    ['on the next day', '2026-10-14', 'Message in #ops-warehouse · Tue 13 Oct'],
    ['on the same day', '2026-10-13', 'Message in #ops-warehouse · today'],
  ])('shows each cited source as a slip with no perforation: a transcript, a document and a message (read %s)', async (_, day, messageKind) => {
    vi.useFakeTimers({ toFake: ['Date'] });
    vi.setSystemTime(at('14:40', day));
    const opened = open();
    await answered(opened, 'What did we decide?', 'run-1');

    const [meetingSlip, documentSlip, messageSlip] = within(panel()).getAllByRole('listitem');
    // A meeting: its title and offset, the date from GetMeeting, the transcript lines, and a stub with its REF.
    await within(meetingSlip).findByText('REF MTG-1014');
    expect(meetingSlip).toHaveTextContent('[1]Q4 launch weekly sync0:12:41');
    expect(meetingSlip).toHaveTextContent('Meeting · Tue 13 Oct · transcript');
    expect(await within(meetingSlip).findByText(/พิมพ์ชนก/)).toBeInTheDocument();
    expect(meetingSlip).toHaveTextContent('Transcript');
    // A document: the page and its count from GetFile, the quoted passage, and a stub with its REF.
    await within(documentSlip).findByText('REF DOC-0371');
    expect(documentSlip).toHaveTextContent('Document · page 4 of 18');
    expect(documentSlip).toHaveTextContent('4.1 Term. This Agreement ends on 31 December 2026.');
    // A message: named from its channel, its time, the quote; no stub, since it is not an issued output.
    expect(messageSlip).toHaveTextContent('Arthit K. in #ops-warehouse14:02');
    expect(messageSlip).toHaveTextContent(messageKind);
    expect(messageSlip).not.toHaveTextContent('REF');
    expect(panel().querySelector('.kiwi-perf')).toBeNull();
  });

  it('says what it is for before any question has been answered', async () => {
    open();

    expect(await screen.findByText('The sources of an answer appear here, with the passage each claim rests on.')).toBeInTheDocument();
  });
});

describe('a citation press', () => {
  it('CitationPress_HighlightsSourceSlipAnnouncesAndOpensSheetOnPhone', async () => {
    stubViewport(390);
    const scrollIntoView = vi.fn();
    Element.prototype.scrollIntoView = scrollIntoView;
    const opened = open();
    await answered(opened, 'What did we decide?', 'run-1');
    const article = screen.getByRole('article');
    // On a phone the panel is a sheet that is shut until a citation or Sources opens it.
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    const marker = within(article).getAllByRole('button', { name: /^Source 2/ })[0];

    await opened.user.click(marker);

    // The sheet opens on the pressed source: its marker fills, its slip is tinted and in view, and it is announced.
    const sheet = await screen.findByRole('dialog', { name: 'Sources' });
    expect(within(sheet).getByRole('button', { name: 'Close' })).toBeInTheDocument();
    expect(sheet.querySelector('[data-n="2"] .ak-sp')).toHaveAttribute('data-hit');
    expect(sheet.querySelector('[data-n="1"] .ak-sp')).not.toHaveAttribute('data-hit');
    expect(scrollIntoView).toHaveBeenCalledWith({ block: 'nearest', behavior: 'smooth' });
    expect(await screen.findByText('Source 2: ACME-contract-v3.pdf, p. 4.', { selector: '[role="status"]' })).toBeInTheDocument();

    // Closing returns focus to the marker that opened it, which the screen holds by reference.
    await opened.user.click(within(sheet).getByRole('button', { name: 'Close' }));
    await waitFor(() => {
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });
    expect(marker).toHaveFocus();
    expect(marker).toHaveAttribute('aria-pressed', 'true');

    // Pressing it again lets it go, and opens nothing.
    await opened.user.click(marker);
    expect(marker).toHaveAttribute('aria-pressed', 'false');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('fills the marker, tints the slip and its row beside the answer on a wide screen, and a row offset does the same', async () => {
    const opened = open();
    await answered(opened, 'What did we decide?', 'run-1');
    const article = screen.getByRole('article');

    await opened.user.click(within(article).getAllByRole('button', { name: /^Source 1/ })[0]);

    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(panel().querySelector('[data-n="1"] .ak-sp')).toHaveAttribute('data-hit');
    expect(article.querySelector('.kiwi-src[data-hit]')).toHaveTextContent('Q4 launch weekly sync');

    // The offset of a row presses its citation (it does not open an excerpt in place): [3] tints the message slip.
    await opened.user.click(within(article).getByRole('button', { name: 'ACME-contract-v3.pdf, page 4' }));
    expect(panel().querySelector('[data-n="2"] .ak-sp')).toHaveAttribute('data-hit');
    expect(panel().querySelector('[data-n="1"] .ak-sp')).not.toHaveAttribute('data-hit');
    expect(article.querySelector('[role="group"]')).toBeNull();
  });
});

describe('Make a task from this', () => {
  it('MakeTask_HandoffSlipHasNoConfirmAndDiscardCallsDiscardDraft', async () => {
    const opened = open();
    opened.api.tasks.createTask.mockResolvedValue(create(CreateTaskResponseSchema, { task: DRAFT }));
    opened.api.tasks.updateTask.mockResolvedValue(create(UpdateTaskResponseSchema, { task: { ...DRAFT, assigneeId: 'user-nok' } }));
    opened.api.tasks.discardDraft.mockResolvedValue(create(DiscardDraftResponseSchema));
    // The server's task list holds the draft once it is made: the slip is read back from it.
    opened.api.tasks.listTasks.mockResolvedValue(create(ListTasksResponseSchema, { tasks: [DRAFT] }));
    await answered(opened, 'What did we decide?', 'run-1');

    await opened.user.click(screen.getByRole('button', { name: 'Make a task from this' }));

    // A real draft through the tasks service, from the first citation, and nothing kept in the tab.
    expect(opened.api.tasks.createTask.mock.calls[0]?.[0]).toMatchObject({ channelId: 'ch-q4', state: TaskState.DRAFT, fromRef: 'ANS-2209', source: { id: 'meeting-1', anchor: '12' } });
    expect(sessionStorage).toHaveLength(0);
    // The handoff slip: a Draft tag, its REF and answer, the note and Review as a link: no Confirm, no Edit.
    const article = screen.getByRole('article');
    const slip = within(article).getByRole('heading', { level: 3, name: 'Decided: the launch moved to 15 Nov 2026.' }).closest('.kiwi-draft') as HTMLElement;
    expect(slip).toHaveTextContent('Draft');
    expect(slip).toHaveTextContent('REF TSK-2036');
    expect(slip).toHaveTextContent('From answer ANS-2209');
    expect(slip).toHaveTextContent('Stays a draft until a person confirms it on the Board.');
    expect(within(slip).getByRole('link', { name: 'Review on the Board' })).toHaveAttribute('href', '/board');
    expect(within(slip).queryByRole('button', { name: /^Confirm/ })).not.toBeInTheDocument();
    expect(within(slip).queryByRole('button', { name: /^Edit/ })).not.toBeInTheDocument();
    // The button now reads Task drafted and is off; the owner select has focus and the draft was announced.
    expect(within(article).getByRole('button', { name: 'Task drafted' })).toBeDisabled();
    const owner = within(slip).getByRole('combobox', { name: 'Owner' });
    expect(owner).toHaveFocus();
    expect(await screen.findByText(/^Draft task created: Decided/, { selector: '[role="status"]' })).toBeInTheDocument();

    // The owner is chosen through UpdateTask.
    await opened.user.selectOptions(owner, 'Nok');
    expect(opened.api.tasks.updateTask.mock.calls[0]?.[0]).toEqual({ id: 'draft-1', assigneeId: 'user-nok' });

    // Discard calls DiscardDraft, removes the slip and gives the button back, with focus.
    await opened.user.click(within(slip).getByRole('button', { name: 'Discard' }));

    expect(opened.api.tasks.discardDraft.mock.calls[0]?.[0]).toEqual({ id: 'draft-1' });
    expect(within(article).queryByRole('heading', { level: 3 })).not.toBeInTheDocument();
    const again = within(article).getByRole('button', { name: 'Make a task from this' });
    expect(again).toBeEnabled();
    await waitFor(() => {
      expect(again).toHaveFocus();
    });
    expect(await screen.findByText('Draft discarded.', { selector: '[role="status"]' })).toBeInTheDocument();
  });

  it('says so, in the answer, when the draft could not be made', async () => {
    const opened = open();
    opened.api.tasks.createTask.mockRejectedValue(new Error('down'));
    await answered(opened, 'What did we decide?', 'run-1');

    await opened.user.click(screen.getByRole('button', { name: 'Make a task from this' }));

    expect(await screen.findByRole('alert')).toHaveTextContent("Couldn't draft the task. Try again.");
    expect(screen.getByRole('button', { name: 'Make a task from this' })).toBeEnabled();
  });
});

describe('the handoff slip follows the task', () => {
  /** An answered question whose draft has been made; the workspace's task list is the server's, which holds the draft. */
  async function drafted() {
    const opened = open();
    opened.api.tasks.createTask.mockResolvedValue(create(CreateTaskResponseSchema, { task: DRAFT }));
    opened.api.tasks.listTasks.mockResolvedValue(create(ListTasksResponseSchema, { tasks: [DRAFT] }));
    await answered(opened, 'What did we decide?', 'run-1');
    await opened.user.click(screen.getByRole('button', { name: 'Make a task from this' }));
    const slip = (await screen.findByRole('heading', { level: 3, name: DRAFT.title })).closest('.kiwi-draft');
    expect(slip).not.toBeNull();
    await waitFor(() => {
      expect(opened.api.tasks.listTasks).toHaveBeenCalled();
    });
    return opened;
  }

  it('becomes an On the Board card, with a Review link and no Draft wording, Confirm or Discard, once the task is confirmed elsewhere', async () => {
    const opened = await drafted();
    const article = screen.getByRole('article');

    // A `task_changed` frame through the real bindToQuery writes the confirmed task into the cache.
    act(() => {
      opened.client.emit(frame({ case: 'taskChanged', value: { ...DRAFT, state: TaskState.CONFIRMED, assigneeId: 'user-nok' } }));
    });

    const card = await within(article).findByRole('group', { name: 'Task TSK-2036' });
    expect(card).toHaveTextContent('Decided: the launch moved to 15 Nov 2026.');
    expect(card).toHaveTextContent('On the Board · TSK-2036');
    expect(within(card).getByRole('link', { name: 'Review on the Board' })).toHaveAttribute('href', '/board');
    expect(card.querySelector('.kiwi-perf, .kiwi-tag')).toBeNull();
    expect(article).not.toHaveTextContent('Stays a draft');
    expect(within(article).queryByRole('button', { name: /^(Discard|Confirm|Edit)/ })).not.toBeInTheDocument();
    expect(within(article).getByRole('button', { name: 'Task drafted' })).toBeDisabled();
  });

  it('is dropped, and the button comes back, once the task is gone from the list', async () => {
    const opened = await drafted();

    act(() => {
      opened.queryClient.setQueryData(keys.tasks(), []);
    });

    await waitFor(() => {
      expect(screen.queryByRole('heading', { level: 3, name: DRAFT.title })).not.toBeInTheDocument();
    });
    expect(screen.getByRole('button', { name: 'Make a task from this' })).toBeEnabled();
  });

  it('says a failed Discard, and drops the slip without an error when the task is already confirmed (Aborted) or gone (NotFound)', async () => {
    const opened = await drafted();
    const discard = () => within(screen.getByRole('article')).getByRole('button', { name: 'Discard' });
    opened.api.tasks.discardDraft.mockRejectedValueOnce(new ConnectError('down', Code.Unavailable));

    await opened.user.click(discard());
    expect(await screen.findByRole('alert')).toHaveTextContent("Couldn't discard the draft. Try again.");
    expect(screen.getByRole('heading', { level: 3, name: DRAFT.title })).toBeInTheDocument();

    opened.api.tasks.discardDraft.mockRejectedValueOnce(new ConnectError('only drafts can be discarded', Code.Aborted));
    await opened.user.click(discard());
    await waitFor(() => {
      expect(screen.queryByRole('heading', { level: 3, name: DRAFT.title })).not.toBeInTheDocument();
    });
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Make a task from this' })).toBeEnabled();
  });
});
