import { create } from '@bufbuild/protobuf';
import { timestampFromDate } from '@bufbuild/protobuf/wkt';
import { AgentRunState, ArtifactKind, GetRunResponseSchema, RunStatus, SourceKind, StartRunResponseSchema } from '@kiwi/api-client';
import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import { beforeEach, describe, expect, it } from 'vitest';
import { useUiStore } from '../../state/ui';
import { artifact, at, channel, run, runFrame } from '../../testing/fixtures';
import { renderApp } from '../../testing/render';
import { createStubApi, seedApi, type StubApi } from '../../testing/stubApi';
import { stubViewport } from '../../testing/viewport';
import { AskScreen } from './AskScreen';
import { SAMPLE_QUESTIONS } from './samples';

beforeEach(() => {
  localStorage.clear();
  useUiStore.setState({ lang: 'en', readSeq: {}, lastChannelId: null, draftsSheetOpen: false });
});

const CHANNELS = [channel({ id: 'ch-q4', name: 'q4-launch' }), channel({ id: 'ch-ops', name: 'ops-warehouse' })];
const QUESTION = 'What did we decide about the Q4 launch date?';

const ANSWER = artifact({
  kind: ArtifactKind.ANSWER,
  ref: 'ANS-2209',
  bodyMarkdown: '**Decided:** the launch moved to 15 Nov 2026.[1] The contract ends on 31 Dec.[2] Arthit confirmed the warehouse is free.[3]',
  scopeChannelIds: ['ch-q4', 'ch-ops'],
  generatedAt: timestampFromDate(at('14:51')),
  citations: [
    { n: 1, chunkId: 'c1', quote: 'งั้นเราเลื่อนวันเปิดตัวไปเป็น 15 พฤศจิกายนนะคะ', source: { kind: SourceKind.MEETING, id: 'meeting-1', anchor: '12', label: 'Q4 launch weekly sync', channelId: 'ch-q4', offsetMs: 761000n } },
    { n: 2, chunkId: 'c2', quote: '4.1 Term. This Agreement ends on 31 December 2026.', source: { kind: SourceKind.FILE, id: 'file-1', anchor: 'page:4', label: 'ACME-contract-v3.pdf', channelId: 'ch-q4', page: 4 } },
    { n: 3, chunkId: 'c3', quote: 'ยืนยันแล้วครับ คลังบางนาว่างช่วง 10–14 พ.ย.', source: { kind: SourceKind.MESSAGE, id: 'msg-41', anchor: '41', label: 'Arthit K.', channelId: 'ch-ops', at: timestampFromDate(at('14:02')) } },
  ],
});

const NOT_FOUND = artifact({
  kind: ArtifactKind.NOT_FOUND,
  ref: 'ANS-2210',
  scopeChannelIds: ['ch-q4', 'ch-ops'],
  generatedAt: timestampFromDate(at('14:55')),
});

/** The Ask screen in the signed-in shell, on a stubbed API with two channels and a fake realtime client. */
function openAsk({ api = createStubApi(), channelId = null, extra = null }: { api?: StubApi; channelId?: string | null; extra?: React.ReactNode } = {}) {
  seedApi(api, { channels: CHANNELS });
  const rendered = renderApp({ path: '/ask', api, routes: [{ path: '/ask', element: <><AskScreen initialChannelId={channelId} />{extra}</> }] });
  return { ...rendered, field: () => screen.getByRole('textbox', { name: 'Ask a question, in English or Thai' }), askButton: () => screen.getByRole('button', { name: 'Ask' }) };
}

/** Types a question and sends it; the stubbed run starts as queued. */
async function askQuestion(opened: ReturnType<typeof openAsk>, question = QUESTION, runId = 'run-1') {
  opened.api.agents.startRun.mockResolvedValueOnce(create(StartRunResponseSchema, { run: run({ id: runId, question }) }));
  await opened.user.type(opened.field(), `${question}{Enter}`);
  await screen.findByRole('status', { name: 'Answer progress' });
  await act(async () => {});
}

const emit = (opened: ReturnType<typeof openAsk>, init: Parameters<typeof runFrame>[0]) => {
  act(() => {
    opened.client.emit(runFrame(init));
  });
};

describe('the command field', () => {
  it('CommandField_DisabledUntilTextWithReason_SlashFocusesEscapeClearsNotWhileComposing', async () => {
    const opened = openAsk({ extra: <input aria-label="Another field" /> });
    const { api, user, field, askButton } = opened;
    await screen.findByText(/Searching/);

    // Ask is disabled until there is text, and says why to assistive tech.
    expect(askButton()).toBeDisabled();
    expect(askButton()).toHaveAccessibleDescription('Type a question first.');
    expect(field()).toHaveAccessibleDescription("Searching 2 channels you're in");
    await user.type(field(), '  ');
    expect(askButton()).toBeDisabled();
    await user.type(field(), 'x');
    expect(askButton()).toBeEnabled();
    expect(askButton()).not.toHaveAccessibleDescription('Type a question first.');

    // Escape clears the field; with nothing to clear it leaves the field.
    await user.keyboard('{Escape}');
    expect(field()).toHaveValue('');
    expect(field()).toHaveFocus();
    await user.keyboard('{Escape}');
    expect(field()).not.toHaveFocus();

    // `/` focuses the field from anywhere that is not a field, and is an ordinary character in one.
    await user.keyboard('/');
    expect(field()).toHaveFocus();
    expect(field()).toHaveValue('');
    await user.click(screen.getByRole('textbox', { name: 'Another field' }));
    await user.keyboard('/');
    expect(screen.getByRole('textbox', { name: 'Another field' })).toHaveValue('/');
    expect(field()).not.toHaveFocus();

    // The Enter that commits an IME candidate sends nothing, and the Escape that dismisses one clears nothing.
    await user.click(field());
    await user.type(field(), 'ถาม');
    fireEvent.keyDown(field(), { key: 'Enter', isComposing: true });
    fireEvent.keyDown(field(), { key: 'Enter', keyCode: 229 });
    fireEvent.keyDown(field(), { key: 'Escape', isComposing: true });
    fireEvent.keyDown(field(), { key: 'Escape', keyCode: 229 });
    expect(api.agents.startRun).not.toHaveBeenCalled();
    expect(field()).toHaveValue('ถาม');

    // Once composing is over, Enter asks and the field empties.
    api.agents.startRun.mockResolvedValue(create(StartRunResponseSchema, { run: run() }));
    await user.keyboard('{Enter}');
    expect(api.agents.startRun).toHaveBeenCalledOnce();
    expect(api.agents.startRun.mock.calls[0]?.[0]).toEqual({ agent: 'assistant', question: 'ถาม' });
    expect(field()).toHaveValue('');
    expect(await screen.findByRole('status', { name: 'Answer progress' })).toBeInTheDocument();
  });

  it('asks with the Ask button too, and sends the question trimmed', async () => {
    const opened = openAsk();
    await screen.findByText(/Searching/);
    opened.api.agents.startRun.mockResolvedValue(create(StartRunResponseSchema, { run: run() }));
    await opened.user.type(opened.field(), `  ${QUESTION}  `);

    await opened.user.click(opened.askButton());

    expect(opened.api.agents.startRun.mock.calls[0]?.[0]).toEqual({ agent: 'assistant', question: QUESTION });
  });

  it('wears the Ask label, the search placeholder and the `/` key hint on a desktop', async () => {
    const { field } = openAsk();
    await screen.findByText(/Searching/);

    expect(field()).toHaveAttribute('placeholder', 'Ask about your channels, meetings, files and tasks');
    expect(screen.getByText('/', { selector: 'kbd' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Ask' })).toHaveTextContent('Ask');
  });
});

describe('the scope line', () => {
  it('says how many channels are searched, opens to the channels and the rule, and lists the languages at the right', async () => {
    const { user } = openAsk();

    const toggle = await screen.findByRole('button', { name: /^Searching 2 channels you're in/ });
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    expect(screen.getByText('ภาษาไทย')).toHaveAttribute('lang', 'th');
    expect(screen.queryByRole('list', { name: 'Channels searched' })).not.toBeInTheDocument();

    await user.click(toggle);

    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    const chips = within(screen.getByRole('list', { name: 'Channels searched' })).getAllByRole('listitem');
    expect(chips.map((chip) => chip.textContent)).toEqual(['# q4-launch', '# ops-warehouse']);
    expect(screen.getByText('Only channels you belong to: their messages, meeting transcripts, files and tasks.')).toBeVisible();
  });
});

describe('sample questions', () => {
  it('SampleQuestions_FillTheFieldWithoutSending', async () => {
    const { api, user, field } = openAsk();
    await screen.findByText(/Searching/);

    const [english, thai] = SAMPLE_QUESTIONS;
    const thaiButton = screen.getByRole('button', { name: thai.text });
    expect(screen.getByText('Sample questions')).toBeInTheDocument();
    expect(thaiButton).toHaveAttribute('lang', 'th');
    expect(screen.getByRole('button', { name: english.text })).not.toHaveAttribute('lang');

    await user.click(screen.getByRole('button', { name: english.text }));
    expect(field()).toHaveValue(english.text);
    expect(field()).toHaveFocus();
    await user.click(thaiButton);
    expect(field()).toHaveValue(thai.text);
    expect(field()).toHaveFocus();

    // Neither asked anything: the person reads the question and sends it.
    expect(api.agents.startRun).not.toHaveBeenCalled();
    expect(screen.queryByRole('article')).not.toBeInTheDocument();
  });

  it('moves to the top of the thread on a phone', async () => {
    stubViewport(390);
    const { field } = openAsk();
    await screen.findByText(/Searching/);

    const thread = screen.getByRole('region', { name: 'Thread' });
    expect(within(thread).getByText('Sample questions')).toBeInTheDocument();
    expect(field()).toHaveAttribute('placeholder', 'Ask about your work');
  });
});

describe('a question and its run', () => {
  it('PendingSteps_RetrievingThenWritingThenSlip', async () => {
    const opened = openAsk();
    await screen.findByText(/Searching/);
    await askQuestion(opened);

    // Retrieving until the first tool progress or text, with no spinner and no streamed words.
    const steps = screen.getByRole('status', { name: 'Answer progress' });
    expect(steps).toHaveTextContent('Retrieving from 2 channels…');
    expect(within(steps).getAllByRole('listitem')[0]).toHaveAttribute('aria-current', 'step');

    emit(opened, { state: AgentRunState.STREAMING, textDelta: 'The launch moved to' });
    expect(steps).toHaveTextContent('Retrieved from 2 channels');
    expect(steps).toHaveTextContent('Writing the answer…');
    expect(within(steps).getAllByRole('listitem')[1]).toHaveAttribute('aria-current', 'step');
    expect(screen.queryByText(/The launch moved to/)).not.toBeInTheDocument();

    // The slip arrives only with the validated answer.
    emit(opened, { state: AgentRunState.DONE, answer: ANSWER });

    expect(screen.queryByRole('status', { name: 'Answer progress' })).not.toBeInTheDocument();
    const article = screen.getByRole('article');
    expect(within(article).getByText('REF ANS-2209')).toBeInTheDocument();
    expect(within(article).getByText('answer in English')).toBeInTheDocument();
    expect(within(article).getByRole('heading', { level: 2, name: QUESTION })).toBeInTheDocument();
    expect(await screen.findByText('Answer ready, ANS-2209: 3 sources.')).toBeInTheDocument();
  });

  it('shows the thread newest first, each question with its own answer', async () => {
    const opened = openAsk();
    await screen.findByText(/Searching/);
    await askQuestion(opened, 'First question', 'run-1');
    emit(opened, { runId: 'run-1', state: AgentRunState.DONE, answer: ANSWER });
    await askQuestion(opened, 'Second question', 'run-2');

    const articles = screen.getAllByRole('article');
    expect(articles.map((article) => within(article).getByRole('heading', { level: 2 }).textContent)).toEqual(['Second question', 'First question']);
    expect(within(articles[0]).getByRole('status', { name: 'Answer progress' })).toBeInTheDocument();
    expect(within(articles[1]).getByText('REF ANS-2209')).toBeInTheDocument();
  });

  it('tags a Thai question and a Thai answer, and calls the answer Thai', async () => {
    const opened = openAsk();
    await screen.findByText(/Searching/);
    await askQuestion(opened, 'สัญญา ACME หมดอายุเมื่อไหร่');
    emit(opened, {
      state: AgentRunState.DONE,
      answer: artifact({ ...ANSWER, bodyMarkdown: 'สัญญา ACME สิ้นสุดวันที่ 31 ธ.ค. 2569[1]' }),
    });

    const article = screen.getByRole('article');
    expect(within(article).getByRole('heading', { level: 2 })).toContainHTML('lang="th"');
    expect(within(article).getByText('answer in Thai')).toBeInTheDocument();
    expect(within(article).getByText(/สัญญา ACME สิ้นสุด/)).toHaveAttribute('lang', 'th');
  });

  it('Ask_Answer_RendersTheModelsMarkdown: a list is a list, a picture is its description and a line break is kept', async () => {
    const opened = openAsk();
    await screen.findByText(/Searching/);
    await askQuestion(opened);
    emit(opened, {
      state: AgentRunState.DONE,
      answer: artifact({ ...ANSWER, bodyMarkdown: 'The launch moved.[1]\nThe contract is unchanged.\n\n- **Date:** 15 Nov 2026\n- Owner: Pim\n\n![chart](https://evil.example/c.png)' }),
    });

    const article = screen.getByRole('article');
    const body = article.querySelector('.md');
    expect(body?.querySelectorAll('ul > li')).toHaveLength(2);
    expect(within(article).getByText('Date:').tagName).toBe('STRONG');
    expect(body?.querySelector('p br')).not.toBeNull();
    expect(article.querySelector('img')).toBeNull();
    expect(body).toHaveTextContent('chart');
    // The marker still presses its source.
    expect(within(article).getByRole('button', { name: /^Source 1/ })).toBeInTheDocument();
  });

  it('NotFoundSlip_IsNotAFailure_KeepsPerforationIssuesRefAndListsSearchedChannels', async () => {
    const opened = openAsk();
    await screen.findByText(/Searching/);
    await askQuestion(opened);
    emit(opened, { state: AgentRunState.DONE, answer: NOT_FOUND });

    const article = screen.getByRole('article');
    // A proof slip, not a failure: no Returned family, a REF, and its perforation kept.
    expect(article.querySelector('[data-tone="failed"], .kiwi-fmsg')).toBeNull();
    expect(article.querySelector('[class*="returned"], [class*="fail"]')).toBeNull();
    expect(within(article).queryByRole('button', { name: 'Retry' })).not.toBeInTheDocument();
    expect(article.querySelector('.kiwi-perf')).not.toBeNull();
    expect(within(article).getByText('REF ANS-2210')).toBeInTheDocument();
    expect(within(article).getByText('Not found in channels you can access')).toBeInTheDocument();
    expect(within(article).getByText('no answer found')).toBeInTheDocument();
    expect(within(article).getByRole('heading', { level: 3, name: 'What you can do next' })).toBeInTheDocument();
    expect(within(article).getByText(/found nothing that answers this/)).toHaveTextContent('Searched messages, meeting transcripts, files and tasks in the 2 channels');
    // Its proof of scope: the channels that were searched, one chip each, named from the channels the person is in.
    expect(within(article).getByText("Searched only channels you're in")).toBeInTheDocument();
    const chips = within(within(article).getByRole('list', { name: 'Channels searched' })).getAllByRole('listitem');
    expect(chips.map((chip) => chip.textContent)).toEqual(['# q4-launch', '# ops-warehouse']);
    expect(article).toHaveTextContent('Searched 14:55');
    expect(await screen.findByText('Not found in channels you can access. Next steps are listed below the answer.', { selector: '[role="status"]' })).toBeInTheDocument();
  });

  it('shows a run that ended with a channel summary as the channel summary slip: its REF and the messages it rests on, never the failure', async () => {
    const opened = openAsk();
    await screen.findByText(/Searching/);
    await askQuestion(opened);
    emit(opened, {
      state: AgentRunState.DONE,
      answer: artifact({
        kind: ArtifactKind.CHANNEL_SUMMARY,
        ref: 'ANS-2212',
        title: QUESTION,
        bodyMarkdown: 'The launch moves to **Friday**.',
        scopeChannelIds: ['ch-q4'],
        generatedAt: timestampFromDate(at('14:57')),
        sources: [
          { kind: SourceKind.MESSAGE, id: 'msg-41', anchor: '41', label: 'Arthit K.', channelId: 'ch-q4', at: timestampFromDate(at('14:02')) },
          { kind: SourceKind.MESSAGE, id: 'msg-42', anchor: '42', label: 'Pimchanok S.', channelId: 'ch-q4', at: timestampFromDate(at('14:09')) },
        ],
      }),
    });

    const article = screen.getByRole('article');
    expect(article.querySelector('[data-tone="failed"]')).toBeNull();
    expect(article).not.toHaveTextContent('something went wrong');
    expect(within(article).queryByRole('button', { name: 'Retry' })).not.toBeInTheDocument();
    expect(within(article).getByText('REF ANS-2212')).toBeInTheDocument();
    expect(within(article).getByText('Friday').tagName).toBe('STRONG');
    expect(within(article).getByText('channel summary')).toBeInTheDocument();
    expect(within(article).getAllByRole('listitem').map((item) => item.textContent)).toEqual([expect.stringContaining('Arthit K.'), expect.stringContaining('Pimchanok S.')]);
    expect(article).toHaveTextContent('Generated 14:57');
    expect(await screen.findByText('Answer ready, ANS-2212: 2 sources.', { selector: '[role="status"]' })).toBeInTheDocument();
  });

  it('says in words why a finished run with nothing to show has no answer, not that something went wrong', async () => {
    const opened = openAsk();
    await screen.findByText(/Searching/);
    opened.api.agents.getRun.mockResolvedValue(
      create(GetRunResponseSchema, { run: run({ status: RunStatus.DONE, answer: artifact({ kind: ArtifactKind.UNSPECIFIED, bodyMarkdown: 'plain text of an older run' }) }) }),
    );
    await askQuestion(opened);

    emit(opened, { state: AgentRunState.DONE });

    const article = screen.getByRole('article');
    expect(await within(article).findByText("Couldn't answer this question: no answer with a source came back. Try again.")).toBeInTheDocument();
    expect(article).not.toHaveTextContent('something went wrong');
    expect(article).not.toHaveTextContent('plain text of an older run');
    expect(within(article).getByRole('button', { name: 'Retry' })).toBeInTheDocument();
  });

  it('names a searched channel the person has since left only by what it is, and never by its name', async () => {
    const opened = openAsk();
    await screen.findByText(/Searching/);
    await askQuestion(opened);
    emit(opened, { state: AgentRunState.DONE, answer: artifact({ ...NOT_FOUND, scopeChannelIds: ['ch-q4', 'ch-gone'] }) });

    const chips = within(within(screen.getByRole('article')).getByRole('list', { name: 'Channels searched' })).getAllByRole('listitem');
    expect(chips.map((chip) => chip.textContent)).toEqual(['# q4-launch', "a channel you're no longer in"]);
  });

  it('puts the question back in the field, selected, with Edit question, and offers the team a channel', async () => {
    const opened = openAsk();
    await screen.findByText(/Searching/);
    await askQuestion(opened);
    emit(opened, { state: AgentRunState.DONE, answer: NOT_FOUND });

    await opened.user.click(screen.getByRole('button', { name: 'Edit question' }));

    expect(opened.field()).toHaveValue(QUESTION);
    expect(opened.field()).toHaveFocus();
    expect((opened.field() as HTMLInputElement).selectionStart).toBe(0);
    expect((opened.field() as HTMLInputElement).selectionEnd).toBe(QUESTION.length);
    expect(screen.getByRole('link', { name: 'Ask the team in a channel' })).toHaveAttribute('href', '/');
  });

  it('shows a failed run as a failed slip with the reason, no REF and Retry, which starts a new run for the same question', async () => {
    const opened = openAsk();
    await screen.findByText(/Searching/);
    opened.api.agents.getRun.mockResolvedValue(create(GetRunResponseSchema, { run: run({ status: RunStatus.FAILED, failureReason: 'model_unavailable' }) }));
    await askQuestion(opened);

    emit(opened, { state: AgentRunState.FAILED });

    const article = screen.getByRole('article');
    expect(await within(article).findByText("Couldn't answer this question: the AI service was unavailable. Try again.")).toBeInTheDocument();
    expect(article.querySelector('[data-tone="failed"]')).not.toBeNull();
    expect(within(article).queryByText(/REF/)).not.toBeInTheDocument();
    expect(article).toHaveTextContent(/Failed \d\d:\d\d/);

    opened.api.agents.startRun.mockResolvedValueOnce(create(StartRunResponseSchema, { run: run({ id: 'run-2' }) }));
    await opened.user.click(within(article).getByRole('button', { name: 'Retry' }));

    expect(opened.api.agents.startRun).toHaveBeenCalledTimes(2);
    expect(opened.api.agents.startRun.mock.calls[1]?.[0]).toEqual({ agent: 'assistant', question: QUESTION });
    expect(await within(article).findByRole('status', { name: 'Answer progress' })).toBeInTheDocument();
    expect(screen.getAllByRole('article')).toHaveLength(1);
    emit(opened, { runId: 'run-2', state: AgentRunState.DONE, answer: ANSWER });
    expect(within(article).getByText('REF ANS-2209')).toBeInTheDocument();
  });

  it('says a Retry that failed again in the same slip', async () => {
    const opened = openAsk();
    await screen.findByText(/Searching/);
    await waitFor(() => {
      expect(opened.api.chat.listChannels).toHaveBeenCalled();
    });
    opened.api.agents.startRun.mockRejectedValue(new Error('offline'));
    await opened.user.type(opened.field(), `${QUESTION}{Enter}`);

    expect(await screen.findByText("Couldn't answer this question: K.I.W.I. couldn't be reached.")).toBeInTheDocument();
  });
});
