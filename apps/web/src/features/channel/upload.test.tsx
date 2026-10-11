import { Code, ConnectError } from '@connectrpc/connect';
import { create } from '@bufbuild/protobuf';
import {
  CompleteUploadResponseSchema,
  CreateFromRecordingResponseSchema,
  CreateUploadResponseSchema,
  FileStatus,
  FileStatusChangedSchema,
  GetFileResponseSchema,
  MeetingStatus,
  MeetingSummaryReadySchema,
  UploadPurpose,
} from '@kiwi/api-client';
import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useUiStore } from '../../state/ui';
import { file, frame, meeting, message } from '../../testing/fixtures';
import { LOG, openChannel } from '../../testing/openChannel';
import { createStubApi, type StubApi } from '../../testing/stubApi';

beforeEach(() => {
  localStorage.clear();
  useUiStore.setState({ lang: 'en', readSeq: {}, lastChannelId: null, draftsSheetOpen: false });
});

afterEach(() => {
  vi.restoreAllMocks();
});

/** The channel with its first page loaded and the PUT to the signed URL stubbed. */
async function ready(api: StubApi, messages = [message({ seq: 5, body: 'five' })]) {
  const opened = openChannel({ messages }, api);
  await within(await screen.findByRole('log', LOG)).findByText('five');
  const put = vi.fn<typeof fetch>(() => Promise.resolve(new Response(null, { status: 200 })));
  vi.stubGlobal('fetch', put);
  return { ...opened, put };
}

const picker = () => screen.getByLabelText('Attach file', { selector: 'input[type="file"]' });

const created = (headers: Record<string, string> = { 'Content-Type': 'application/pdf' }) =>
  create(CreateUploadResponseSchema, { fileId: 'file-9', uploadUrl: '/api/blob/tok-1', headers });

const completed = (init: Parameters<typeof file>[0]) => create(CompleteUploadResponseSchema, { file: file({ id: 'file-9', ...init }) });

describe('the paperclip', () => {
  it('Composer_AudioFileUploadsAsRecordingAndCreatesMeeting', async () => {
    const api = createStubApi();
    const { user, put } = await ready(api);
    const headers = { 'Content-Type': 'audio/wav', 'X-Kiwi-Upload': 'sig-1' };
    api.files.createUpload.mockResolvedValue(created(headers));
    api.files.completeUpload.mockResolvedValue(completed({ name: 'standup.wav', mime: 'audio/wav', status: FileStatus.UPLOADED }));
    api.meetings.createFromRecording.mockResolvedValue(create(CreateFromRecordingResponseSchema, { meeting: meeting({ id: 'meeting-9', title: 'standup' }) }));
    const recording = new File(['RIFF'], 'standup.wav', { type: 'audio/wav' });

    await user.upload(picker(), recording);

    await waitFor(() => {
      expect(api.meetings.createFromRecording).toHaveBeenCalledOnce();
    });
    expect(api.files.createUpload).toHaveBeenCalledWith({ channelId: 'ch-q4', name: 'standup.wav', mime: 'audio/wav', size: 4n, purpose: UploadPurpose.RECORDING });
    // The bytes go to the signed same-origin URL with exactly the headers it came with, and no credentials.
    expect(put).toHaveBeenCalledWith('/api/blob/tok-1', { method: 'PUT', headers, body: expect.objectContaining({ name: 'standup.wav' }) as File, credentials: 'omit' });
    expect(api.files.completeUpload).toHaveBeenCalledWith({ fileId: 'file-9' });
    expect(api.meetings.createFromRecording).toHaveBeenCalledWith({ channelId: 'ch-q4', fileId: 'file-9', title: 'standup' });
    const order = [api.files.createUpload, put, api.files.completeUpload, api.meetings.createFromRecording].map((fn) => fn.mock.invocationCallOrder[0]);
    expect(order).toEqual([...order].sort((a, b) => (a ?? 0) - (b ?? 0)));
  });

  it('Composer_PdfUploadsAsAttachmentWithoutCreatingMeeting', async () => {
    const api = createStubApi();
    const { user, put } = await ready(api);
    api.files.createUpload.mockResolvedValue(created());
    api.files.completeUpload.mockResolvedValue(completed({ name: 'contract.pdf', mime: 'application/pdf', status: FileStatus.ANALYZING }));

    await user.upload(picker(), new File(['%PDF-1.4'], 'contract.pdf', { type: 'application/pdf' }));

    await waitFor(() => {
      expect(api.files.completeUpload).toHaveBeenCalledOnce();
    });
    expect(api.files.createUpload).toHaveBeenCalledWith({ channelId: 'ch-q4', name: 'contract.pdf', mime: 'application/pdf', size: 8n, purpose: UploadPurpose.ATTACHMENT });
    expect(put).toHaveBeenCalledOnce();
    expect(api.meetings.createFromRecording).not.toHaveBeenCalled();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('opens the file picker from the paperclip', async () => {
    const api = createStubApi();
    const { user } = await ready(api);
    const click = vi.spyOn(picker(), 'click');

    await user.click(screen.getByRole('button', { name: 'Attach file' }));

    expect(click).toHaveBeenCalledOnce();
  });

  it('says why the server refused the type, inline, and sends no bytes', async () => {
    const api = createStubApi();
    const { user, put } = await ready(api);
    api.files.createUpload.mockRejectedValue(new ConnectError('unsupported type', Code.InvalidArgument));

    // The picker's accept list greys a .docx out, but a person can still choose any file: the server decides.
    const docx = new File(['x'], 'notes.docx', { type: 'application/vnd.openxmlformats-officedocument.wordprocessingml.document' });
    fireEvent.change(picker(), { target: { files: [docx] } });

    expect(await screen.findByRole('alert')).toHaveTextContent("Couldn't upload notes.docx: this file type isn't supported.");
    expect(put).not.toHaveBeenCalled();
    expect(api.files.completeUpload).not.toHaveBeenCalled();
    await user.click(within(screen.getByRole('alert')).getByRole('button', { name: 'Close' }));
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('says so when the stored object is refused: a file that failed on completion gets no meeting', async () => {
    const api = createStubApi();
    const { user } = await ready(api);
    api.files.createUpload.mockResolvedValue(created({ 'Content-Type': 'audio/wav' }));
    api.files.completeUpload.mockResolvedValue(completed({ name: 'silence.wav', mime: 'audio/wav', status: FileStatus.FAILED, failureReason: 'empty' }));

    await user.upload(picker(), new File(['x'], 'silence.wav', { type: 'audio/wav' }));

    expect(await screen.findByRole('alert')).toHaveTextContent("Couldn't upload silence.wav: the file is empty.");
    expect(api.meetings.createFromRecording).not.toHaveBeenCalled();
  });

  it('says the file is too large when the storage refuses the bytes', async () => {
    const api = createStubApi();
    const { user, put } = await ready(api);
    api.files.createUpload.mockResolvedValue(created());
    put.mockResolvedValue(new Response(null, { status: 413 }));

    await user.upload(picker(), new File(['x'], 'huge.pdf', { type: 'application/pdf' }));

    expect(await screen.findByRole('alert')).toHaveTextContent("Couldn't upload huge.pdf: the file is too large.");
    expect(api.files.completeUpload).not.toHaveBeenCalled();
  });

  it('says it could not upload when the server cannot be reached', async () => {
    const api = createStubApi();
    const { user } = await ready(api);
    api.files.createUpload.mockRejectedValue(new ConnectError('down', Code.Unavailable));

    await user.upload(picker(), new File(['x'], 'a.pdf', { type: 'application/pdf' }));

    expect(await screen.findByRole('alert')).toHaveTextContent("Couldn't upload a.pdf. Try again.");
  });
});

describe('the slips of a message with files', () => {
  const attached = message({ seq: 6, body: '', files: [{ fileId: 'file-1', name: 'warehouse-slots.csv', mime: 'text/csv', size: 2048n }] });

  it('shows a calm wait while the file is summarized, then an honest failure with Retry, then nothing once it is ready', async () => {
    const api = createStubApi();
    const { client, user } = openChannel({ messages: [attached] }, api);
    api.files.getFile.mockResolvedValue(create(GetFileResponseSchema, { file: file({ status: FileStatus.ANALYZING }) }));
    const log = await screen.findByRole('log', LOG);
    expect(await within(log).findByText('Summarizing file…')).toBeInTheDocument();

    await act(async () => {
      client.emit(
        frame({
          case: 'fileStatusChanged',
          value: create(FileStatusChangedSchema, { fileId: 'file-1', channelId: 'ch-q4', status: FileStatus.FAILED, failureReason: 'unreadable' }),
        }),
      );
      await Promise.resolve();
    });

    expect(await within(log).findByText("Couldn't summarize warehouse-slots.csv: no text could be read from it.")).toBeInTheDocument();
    expect(within(log).queryByText(/^REF /)).not.toBeInTheDocument();
    api.files.retryAnalysis.mockResolvedValue({} as never);
    await user.click(within(log).getByRole('button', { name: 'Retry' }));
    expect(api.files.retryAnalysis).toHaveBeenCalledWith({ fileId: 'file-1' });
    expect(await within(log).findByText('Summarizing file…')).toBeInTheDocument();

    api.files.getFile.mockResolvedValue(create(GetFileResponseSchema, { file: file({ status: FileStatus.READY, ref: 'DOC-0389' }) }));
    await act(async () => {
      client.emit(
        frame({
          case: 'fileStatusChanged',
          value: create(FileStatusChangedSchema, { fileId: 'file-1', channelId: 'ch-q4', status: FileStatus.READY }),
        }),
      );
      await Promise.resolve();
    });

    await waitFor(() => {
      expect(within(log).queryByText('Summarizing file…')).not.toBeInTheDocument();
    });
    expect(within(log).getByText('warehouse-slots.csv')).toBeInTheDocument();
  });

  it('follows a recording uploaded in this session: pending while it is summarized, failed with Retry when the meeting is', async () => {
    const api = createStubApi();
    const { client, user } = openChannel({ messages: [message({ seq: 5, body: 'five' })] }, api);
    const log = await screen.findByRole('log', LOG);
    await within(log).findByText('five');
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(null, { status: 200 }))));
    api.files.createUpload.mockResolvedValue(created({ 'Content-Type': 'audio/wav' }));
    api.files.completeUpload.mockResolvedValue(completed({ name: 'standup.wav', mime: 'audio/wav', status: FileStatus.UPLOADED }));
    api.meetings.createFromRecording.mockResolvedValue(
      create(CreateFromRecordingResponseSchema, { meeting: meeting({ id: 'meeting-9', title: 'standup', status: MeetingStatus.PENDING }) }),
    );
    await user.upload(picker(), new File(['RIFF'], 'standup.wav', { type: 'audio/wav' }));
    await waitFor(() => {
      expect(api.meetings.createFromRecording).toHaveBeenCalled();
    });

    // The server posts the attachment message; the recording's slip is the meeting's, not the file's.
    await act(async () => {
      client.emit(frame({ case: 'messageCreated', value: message({ seq: 6, body: '', files: [{ fileId: 'file-9', name: 'standup.wav', mime: 'audio/wav', size: 4n }] }) }));
      await Promise.resolve();
    });
    expect(await within(log).findByText('Summarizing the recording…')).toBeInTheDocument();
    expect(api.files.getFile).not.toHaveBeenCalled();

    await act(async () => {
      client.emit(
        frame({
          case: 'meetingSummaryReady',
          value: create(MeetingSummaryReadySchema, { meetingId: 'meeting-9', channelId: 'ch-q4', status: MeetingStatus.FAILED, failureReason: 'no_speech' }),
        }),
      );
      await Promise.resolve();
    });

    expect(await within(log).findByText("Couldn't summarize standup: no speech was found in the recording.")).toBeInTheDocument();
    api.meetings.retrySummary.mockResolvedValue({} as never);
    await user.click(within(log).getByRole('button', { name: 'Retry' }));
    expect(api.meetings.retrySummary).toHaveBeenCalledWith({ meetingId: 'meeting-9' });
    expect(await within(log).findByText('Summarizing the recording…')).toBeInTheDocument();
  });
});
