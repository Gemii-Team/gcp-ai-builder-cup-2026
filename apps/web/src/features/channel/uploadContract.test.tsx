import { Code, ConnectError } from '@connectrpc/connect';
import { create } from '@bufbuild/protobuf';
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
import { useUiStore } from '../../state/ui';
import { file, frame, message } from '../../testing/fixtures';
import { LOG, openChannel } from '../../testing/openChannel';
import { createStubApi, type StubApi } from '../../testing/stubApi';

// The backend's contract for an upload the server rejects (L18): `CompleteUpload` answers `invalid_argument` with the
// message `docs: upload rejected (<reason>): <detail>`, <reason> one of five; the file row is marked failed with that
// reason, which `file_status_changed` and `GetFile` carry too. Recordings are PCM16 WAV only.

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

describe('a rejected upload says its reason', () => {
  it.each(REASONS)('CompleteUpload rejects with %s: the inline sentence is the reason, in English, and never the server detail', async (reason, english) => {
    const api = createStubApi();
    await ready(api);
    api.files.completeUpload.mockRejectedValue(rejection(reason));

    choose(new File(['x'], 'standup.pdf', { type: 'application/pdf' }));

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent(`Couldn't upload standup.pdf: ${english}`);
    expect(alert).not.toHaveTextContent("the server's own English detail");
    expect(api.meetings.createFromRecording).not.toHaveBeenCalled();
  });

  it.each(REASONS)('CompleteUpload rejects with %s: the same reason in Thai, with its Latin words tagged lang="en"', async (reason, _english, thai) => {
    useUiStore.getState().setLang('th');
    const api = createStubApi();
    await ready(api);
    api.files.completeUpload.mockRejectedValue(rejection(reason));

    choose(new File(['x'], 'standup.pdf', { type: 'application/pdf' }));

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent(`อัปโหลด standup.pdf ไม่ได้: ${thai}`);
    if (reason === 'unsupported_audio') {
      expect(within(alert).getByText('WAV')).toHaveAttribute('lang', 'en');
      expect(within(alert).getByText('PCM16')).toHaveAttribute('lang', 'en');
    }
  });

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

  it('an mp3 recording is a RECORDING, and its rejection says recordings must be WAV (PCM16) files', async () => {
    const api = createStubApi();
    await ready(api);
    api.files.completeUpload.mockRejectedValue(rejection('unsupported_audio'));

    choose(new File(['ID3'], 'standup.mp3', { type: 'audio/mpeg' }));

    expect(await screen.findByRole('alert')).toHaveTextContent("Couldn't upload standup.mp3: recordings must be WAV (PCM16) files.");
    expect(api.files.createUpload).toHaveBeenCalledWith(expect.objectContaining({ mime: 'audio/mpeg', purpose: UploadPurpose.RECORDING }));
    expect(api.meetings.createFromRecording).not.toHaveBeenCalled();
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
