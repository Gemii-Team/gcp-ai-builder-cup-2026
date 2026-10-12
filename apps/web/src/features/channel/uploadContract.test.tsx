import { Code, ConnectError } from '@connectrpc/connect';
import { create, toJson } from '@bufbuild/protobuf';
import {
  CompleteUploadResponseSchema,
  CreateUploadResponseSchema,
  FileStatus,
  FileStatusChangedSchema,
  GetFileResponseSchema,
  UploadPurpose,
} from '@kiwi/api-client';
import { act, fireEvent, screen, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createApi } from '../../api/client';
import { useUiStore } from '../../state/ui';
import { file, frame, message } from '../../testing/fixtures';
import { LOG, openChannel } from '../../testing/openChannel';
import { createStubApi, type StubApi } from '../../testing/stubApi';

// The contract between the server and the web for an upload the server refuses. The refusals are the files the Go test
// `TestContract_UploadRefusals_MatchTheFixturesTheWebReads` (backend/internal/app) compares with the bytes the composed
// server writes: `CreateUpload` and `CompleteUpload` answer `invalid_argument` with the message `docs: upload rejected
// (<reason>): <sentence>` (<reason> one of five) or, for a refusal with no reason of its own, only `invalid`. The file row
// is marked failed with the same reason, which `file_status_changed` and `GetFile` carry too. Recordings are PCM16 WAV only.

beforeEach(() => {
  localStorage.clear();
  useUiStore.setState({ lang: 'en', readSeq: {}, lastChannelId: null, draftsSheetOpen: false });
});

afterEach(() => {
  vi.restoreAllMocks();
});

/** The five reasons, with the sentence each says in English and in Thai. A failed file slip says the same for the same reason. */
const REASONS = [
  ['empty', 'the file is empty.', 'ไฟล์ว่างเปล่า'],
  ['too_large', 'the file is too large.', 'ไฟล์มีขนาดใหญ่เกินไป'],
  ['unsupported_type', "this file type isn't supported.", 'ไม่รองรับไฟล์ประเภทนี้'],
  ['unsupported_audio', 'recordings must be WAV (PCM16) files.', 'การบันทึกเสียงต้องเป็นไฟล์ WAV (PCM16) เท่านั้น'],
  ['size_mismatch', "the file's size changed while it was uploading. Try again.", 'ขนาดไฟล์เปลี่ยนไประหว่างอัปโหลด ลองอีกครั้ง'],
] as const;

/** A refusal as a hand-typed message, for the answers the server does not send today: a reason a newer server may add. */
const rejection = (reason: string) => new ConnectError(`docs: upload rejected (${reason}): the server's own English detail`, Code.InvalidArgument);

/** Chooses a file the way the picker does, past its accept list: the server decides what it takes. */
function choose(chosen: File) {
  fireEvent.change(screen.getByLabelText(/^(Attach file|แนบไฟล์)$/, { selector: 'input[type="file"]' }), { target: { files: [chosen] } });
}

/** The channel open, the storage willing, and `CreateUpload` answering with the signed URL. */
async function ready(api: StubApi) {
  const opened = openChannel({ messages: [message({ seq: 5, body: 'five' })] }, api);
  const thai = useUiStore.getState().lang === 'th';
  await within(await screen.findByRole('log', { name: thai ? 'ข้อความใน #q4-launch' : LOG.name })).findByText('five');
  const put = vi.fn<typeof fetch>(() => Promise.resolve(new Response(null, { status: 200 })));
  vi.stubGlobal('fetch', put);
  api.files.createUpload.mockResolvedValue(create(CreateUploadResponseSchema, { fileId: 'file-9', uploadUrl: '/api/blob/tok-1', headers: { 'Content-Type': 'x/y' } }));
  return { ...opened, put };
}

type Fixture = { httpStatus: number; contentType: string; body: { code: string; message: string } };

const files = import.meta.glob<string>('../../../../../packages/api-client/testdata/upload-errors/*.json', { query: '?raw', import: 'default', eager: true });
const fixtures = Object.entries(files).map(([path, text]) => ({
  name: path.slice(path.lastIndexOf('/') + 1).replace(/\.json$/, ''),
  fixture: JSON.parse(text) as Fixture,
}));

/** What the person picks for each fixture and is told: the reason (a REASONS row), or null for a refusal that names none. */
const OUTCOME: Record<string, { pick: () => File; reason: (typeof REASONS)[number][0] | null }> = {
  create_empty: { pick: () => new File([], 'notes.txt', { type: 'text/plain' }), reason: 'empty' },
  create_too_large: { pick: () => new File(['x'], 'big.pdf', { type: 'application/pdf' }), reason: 'too_large' },
  create_unsupported_type: {
    pick: () => new File(['x'], 'plan.docx', { type: 'application/vnd.openxmlformats-officedocument.wordprocessingml.document' }),
    reason: 'unsupported_type',
  },
  create_unsupported_audio: { pick: () => new File(['ID3'], 'standup.mp3', { type: 'audio/mpeg' }), reason: 'unsupported_audio' },
  create_no_reason: { pick: () => new File(['x'], 'notes.txt', { type: 'text/plain' }), reason: null },
  complete_empty: { pick: () => new File([], 'empty.txt', { type: 'text/plain' }), reason: 'empty' },
  complete_too_large: { pick: () => new File(['x'], 'grew.txt', { type: 'text/plain' }), reason: 'too_large' },
  complete_unsupported_type: { pick: () => new File(['x'], 'notes.pdf', { type: 'application/pdf' }), reason: 'unsupported_type' },
  complete_unsupported_audio: { pick: () => new File(['ID3'], 'standup.wav', { type: 'audio/wav' }), reason: 'unsupported_audio' },
  complete_size_mismatch: { pick: () => new File(['x'], 'changes.txt', { type: 'text/plain' }), reason: 'size_mismatch' },
};

/**
 * The channel screen over the real connect-web transport and the real generated FileService client, whose `fetch` answers
 * with `fixture` at the step the fixture is named for (`create_` or `complete_`) and as a willing server everywhere else.
 */
async function openWithAnswer(name: string, { httpStatus, contentType, body }: Fixture) {
  const refusedStep = name.startsWith('create_') ? 'CreateUpload' : 'CompleteUpload';
  const created = toJson(CreateUploadResponseSchema, create(CreateUploadResponseSchema, { fileId: 'file-9', uploadUrl: '/api/blob/tok-1', headers: { 'Content-Type': 'x/y' } }));
  const fetchStub = vi.fn<typeof fetch>((input, init) => {
    const target = typeof input === 'string' ? input : '';
    if (init?.method === 'PUT') {
      return Promise.resolve(new Response(null, { status: 200 }));
    }
    if (target.endsWith(`/kiwi.files.v1.FileService/${refusedStep}`)) {
      return Promise.resolve(new Response(JSON.stringify(body), { status: httpStatus, headers: { 'Content-Type': contentType } }));
    }
    if (target.endsWith('/kiwi.files.v1.FileService/CreateUpload')) {
      return Promise.resolve(new Response(JSON.stringify(created), { status: 200, headers: { 'Content-Type': 'application/json' } }));
    }
    return Promise.reject(new Error(`Unexpected request to ${target}`));
  });
  vi.stubGlobal('fetch', fetchStub);
  const real = createApi();
  const api = createStubApi();
  api.files.createUpload.mockImplementation((request, options) => real.files.createUpload(request, options));
  api.files.completeUpload.mockImplementation((request, options) => real.files.completeUpload(request, options));
  const opened = openChannel({ messages: [message({ seq: 5, body: 'five' })] }, api);
  const thai = useUiStore.getState().lang === 'th';
  await within(await screen.findByRole('log', { name: thai ? 'ข้อความใน #q4-launch' : LOG.name })).findByText('five');
  return { ...opened, api, fetchStub, refusedStep };
}

describe('a rejected upload says its reason: the answers the server writes', () => {
  it('has an expectation for every fixture, and a fixture for every expectation', () => {
    const names = fixtures.map(({ name }) => name);
    expect(names.filter((name) => !Object.hasOwn(OUTCOME, name)), 'fixtures with no expectation in OUTCOME').toEqual([]);
    expect(Object.keys(OUTCOME).filter((name) => !names.includes(name)), 'expectations with no fixture').toEqual([]);
  });

  it('has an answer for every reason and both steps', () => {
    const names = fixtures.map(({ name }) => name);
    for (const [reason] of REASONS) {
      expect(
        names.some((name) => name.startsWith('complete_') && OUTCOME[name]?.reason === reason),
        `a CompleteUpload fixture for ${reason}`,
      ).toBe(true);
    }
    expect(names.some((name) => name.startsWith('create_') && OUTCOME[name]?.reason === null)).toBe(true);
  });

  describe.each([
    ['en', "Couldn't upload {name}: {reason}", "Couldn't upload {name}: the server doesn't accept this file."],
    ['th', 'อัปโหลด {name} ไม่ได้: {reason}', 'อัปโหลด {name} ไม่ได้: เซิร์ฟเวอร์ไม่รับไฟล์นี้'],
  ] as const)('in %s', (lang, sentence, noReason) => {
    for (const { name, fixture } of fixtures) {
      it(`${name}: ${Object.hasOwn(OUTCOME, name) ? String(OUTCOME[name]?.reason) : 'no expectation'}`, async () => {
        const outcome = OUTCOME[name];
        if (outcome === undefined) {
          throw new Error(`the fixture ${name} has no expectation in OUTCOME: say what the person must be told for it`);
        }
        useUiStore.getState().setLang(lang);
        const { api, fetchStub, refusedStep } = await openWithAnswer(name, fixture);
        const chosen = outcome.pick();

        choose(chosen);

        const alert = await screen.findByRole('alert');
        const row = REASONS.find(([reason]) => reason === outcome.reason);
        const said = row === undefined ? noReason.replace('{name}', chosen.name) : sentence.replace('{name}', chosen.name).replace('{reason}', row[lang === 'en' ? 1 : 2]);
        expect(alert).toHaveTextContent(said);
        // The server's own sentence is for logs and people reading the wire: the page says it in its own words.
        const detail = fixture.body.message.split('): ')[1];
        if (detail !== undefined) {
          expect(alert).not.toHaveTextContent(detail);
        }
        if (outcome.reason === 'unsupported_audio' && lang === 'th') {
          expect(within(alert).getByText('WAV')).toHaveAttribute('lang', 'en');
          expect(within(alert).getByText('PCM16')).toHaveAttribute('lang', 'en');
        }
        expect(screen.getAllByRole('alert')).toHaveLength(1);
        // The calls that were made: a refusal at CreateUpload ends there, a refusal at CompleteUpload is after the PUT.
        const requested = fetchStub.mock.calls.map(([input, init]) => (init?.method === 'PUT' ? 'PUT' : (typeof input === 'string' ? input : '').split('/').pop()));
        expect(requested).toEqual(refusedStep === 'CreateUpload' ? ['CreateUpload'] : ['CreateUpload', 'PUT', 'CompleteUpload']);
        expect(api.files.createUpload).toHaveBeenCalledWith(
          expect.objectContaining({ name: chosen.name, purpose: chosen.type.startsWith('audio/') ? UploadPurpose.RECORDING : UploadPurpose.ATTACHMENT }),
        );
        expect(api.meetings.createFromRecording).not.toHaveBeenCalled();
      });
    }
  });
});

// Answers this build reads as a refusal that gives no reason, or as no refusal at all. They are typed out here because the server does not send them today.
describe("an answer that is not one of the server's reasons", () => {
  it('an invalid_argument with any other message says only that the server does not accept the file, whatever words it uses', async () => {
    const api = createStubApi();
    await ready(api);
    // Wording that a classifier over the message would take for a reason: it is not the contract's shape.
    api.files.completeUpload.mockRejectedValue(new ConnectError('upload rejected (empty): unsupported file type, too large', Code.InvalidArgument));

    choose(new File(['x'], 'a.pdf', { type: 'application/pdf' }));

    expect(await screen.findByRole('alert')).toHaveTextContent("Couldn't upload a.pdf: the server doesn't accept this file.");
  });

  it('an invalid_argument that names a reason this build does not know says only that the server does not accept the file', async () => {
    const api = createStubApi();
    await ready(api);
    api.files.completeUpload.mockRejectedValue(rejection('virus_found'));

    choose(new File(['x'], 'a.pdf', { type: 'application/pdf' }));

    expect(await screen.findByRole('alert')).toHaveTextContent("Couldn't upload a.pdf: the server doesn't accept this file.");
  });

  it('an error that is not a refusal is a server that could not be reached, whatever its message says', async () => {
    const api = createStubApi();
    await ready(api);
    api.files.completeUpload.mockRejectedValue(new ConnectError('docs: upload rejected (empty): detail', Code.Unavailable));

    choose(new File(['x'], 'a.pdf', { type: 'application/pdf' }));

    expect(await screen.findByRole('alert')).toHaveTextContent("Couldn't upload a.pdf. Try again.");
  });
});

/** What a failed file slip says for a reason, whole: the English and the Thai sentence of the same reason as the inline notice has them. */
const SLIP = {
  en: { name: 'Messages in #q4-launch', sentence: (reason: string) => `Couldn't summarize warehouse-slots.csv: ${reason}`, pending: 'Summarizing file…' },
  th: { name: 'ข้อความใน #q4-launch', sentence: (reason: string) => `สรุป warehouse-slots.csv ไม่ได้: ${reason}`, pending: 'กำลังสรุปไฟล์…' },
} as const;

/** The channel with a message carrying a CSV, whose file the server reports as analyzing. */
async function withFile(lang: 'en' | 'th') {
  useUiStore.getState().setLang(lang);
  const api = createStubApi();
  const opened = openChannel(
    { messages: [message({ seq: 6, body: '', files: [{ fileId: 'file-1', name: 'warehouse-slots.csv', mime: 'text/csv', size: 2048n }] })] },
    api,
  );
  api.files.getFile.mockResolvedValue(create(GetFileResponseSchema, { file: file({ status: FileStatus.ANALYZING }) }));
  const log = await screen.findByRole('log', { name: SLIP[lang].name });
  await within(log).findByText((_, element) => element?.matches('[role="status"]') === true && element.textContent === SLIP[lang].pending);
  return { ...opened, api, log };
}

/** The text of the failed slip, whole, once it shows: what the person reads and a screen reader announces. */
async function failureText(log: HTMLElement): Promise<{ text: string; element: HTMLElement }> {
  const element = await vi.waitFor(() => {
    const found = log.querySelector<HTMLElement>('.kiwi-fmsg');
    if (found === null) {
      throw new Error('no failed slip yet');
    }
    return found;
  });
  return { text: element.textContent, element };
}

describe.each(['en', 'th'] as const)('a failed file slip says the same reason, in %s', (lang) => {
  /** The reason's sentence in this language, as the inline rejection of the same reason says it. */
  const said = (english: string, thai: string) => SLIP[lang].sentence(lang === 'en' ? english : thai);

  /** The Latin words a Thai sentence marks carry `lang="en"` wherever the sentence is shown. */
  const expectMarked = (reason: string, element: HTMLElement) => {
    if (reason === 'unsupported_audio' && lang === 'th') {
      expect(within(element).getByText('WAV')).toHaveAttribute('lang', 'en');
      expect(within(element).getByText('PCM16')).toHaveAttribute('lang', 'en');
    }
  };

  it.each(REASONS)('file_status_changed FAILED with %s', async (reason, english, thai) => {
    const { client, log } = await withFile(lang);

    await act(async () => {
      client.emit(
        frame({
          case: 'fileStatusChanged',
          value: create(FileStatusChangedSchema, { fileId: 'file-1', channelId: 'ch-q4', status: FileStatus.FAILED, failureReason: reason }),
        }),
      );
      await Promise.resolve();
    });

    const { text, element } = await failureText(log);
    expect(text).toBe(said(english, thai));
    expect(within(log).queryByText(/^REF /)).not.toBeInTheDocument();
    expectMarked(reason, element);
  });

  it.each(REASONS)('GetFile of a failed file says %s with no event needed', async (reason, english, thai) => {
    useUiStore.getState().setLang(lang);
    const api = createStubApi();
    openChannel(
      { messages: [message({ seq: 6, body: '', files: [{ fileId: 'file-1', name: 'warehouse-slots.csv', mime: 'text/csv', size: 2048n }] })] },
      api,
    );
    api.files.getFile.mockResolvedValue(create(GetFileResponseSchema, { file: file({ status: FileStatus.FAILED, failureReason: reason }) }));
    const log = await screen.findByRole('log', { name: SLIP[lang].name });

    const { text, element } = await failureText(log);

    expect(text).toBe(said(english, thai));
    expectMarked(reason, element);
  });
});

describe('the MIME type of a file the browser gave none', () => {
  /** What `CreateUpload` was asked for, after the file is chosen and the upload goes through to its end. */
  async function chosen(chosenFile: File) {
    const api = createStubApi();
    await ready(api);
    api.files.completeUpload.mockResolvedValue(create(CompleteUploadResponseSchema, { file: file({ id: 'file-9', status: FileStatus.ANALYZING }) }));
    choose(chosenFile);
    await vi.waitFor(() => {
      expect(api.files.createUpload).toHaveBeenCalledOnce();
    });
    return api.files.createUpload.mock.calls[0]?.[0];
  }

  it.each([
    ['notes.md', 'text/markdown', UploadPurpose.ATTACHMENT],
    ['table.csv', 'text/csv', UploadPurpose.ATTACHMENT],
    ['Contract.PDF', 'application/pdf', UploadPurpose.ATTACHMENT],
    ['photo.jpeg', 'image/jpeg', UploadPurpose.ATTACHMENT],
    ['standup.wav', 'audio/wav', UploadPurpose.RECORDING],
  ])('infers %s as %s', async (name, mime, purpose) => {
    const request = await chosen(new File(['x'], name, { type: '' }));

    expect(request).toMatchObject({ name, mime, purpose });
  });

  it('sends any other file as the browser gave it, even with none', async () => {
    expect(await chosen(new File(['x'], 'archive.zip', { type: '' }))).toMatchObject({ mime: '', purpose: UploadPurpose.ATTACHMENT });
  });

  it('keeps the type the browser gave over what the extension says', async () => {
    expect(await chosen(new File(['x'], 'notes.md', { type: 'text/plain' }))).toMatchObject({ mime: 'text/plain' });
  });
});
