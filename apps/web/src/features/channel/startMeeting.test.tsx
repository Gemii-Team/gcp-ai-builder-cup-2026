import { create } from '@bufbuild/protobuf';
import { BinaryWriter, WireType } from '@bufbuild/protobuf/wire';
import { Code, ConnectError } from '@connectrpc/connect';
import { StartMeetingResponseSchema } from '@kiwi/api-client';
import { screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { hardNavigate } from '../../navigation';
import { useUiStore } from '../../state/ui';
import { meeting } from '../../testing/fixtures';
import { openChannel } from '../../testing/openChannel';
import { createStubApi } from '../../testing/stubApi';

beforeEach(() => {
  localStorage.clear();
  useUiStore.setState({ lang: 'en', readSeq: {}, lastChannelId: null, draftsSheetOpen: false });
});

afterEach(() => {
  vi.restoreAllMocks();
});

const MEET = 'https://meet.google.com/abc-defg-hij';

/** A `google.rpc.ErrorInfo` of the domain `kiwi` in a Connect error, as the API answers a failed Meet call. */
function meetError(code: Code, message: string, reason: string, cause: string): ConnectError {
  const entry = new BinaryWriter().tag(1, WireType.LengthDelimited).string('cause').tag(2, WireType.LengthDelimited).string(cause).finish();
  const value = new BinaryWriter()
    .tag(1, WireType.LengthDelimited)
    .string(reason)
    .tag(2, WireType.LengthDelimited)
    .string('kiwi')
    .tag(3, WireType.LengthDelimited)
    .bytes(entry)
    .finish();
  const error = new ConnectError(message, code);
  error.details.push({ type: 'google.rpc.ErrorInfo', value });
  return error;
}

const started = (transcriptionAutomatic: boolean) => create(StartMeetingResponseSchema, { meeting: meeting({ meetUri: MEET }), transcriptionAutomatic });
const startButton = () => screen.findByRole('button', { name: 'Start meeting' });

/** The notice region that holds `text`: a status for a meeting that started, an alert for one that did not. */
async function noticeWith(text: string | RegExp, role: 'status' | 'alert'): Promise<HTMLElement> {
  const region = (await screen.findByText(text)).closest<HTMLElement>(`[role="${role}"]`);
  if (region === null) {
    throw new Error(`the text ${String(text)} is not inside a ${role} region`);
  }
  return region;
}

const MANUAL =
  'Meeting started. Transcription is not automatic for your Google account: turn it on in Meet (Activities, then Transcripts) so a summary can follow. You can also upload a recording.';
const MANUAL_START = /Transcription is not automatic/;

describe('Start meeting: transcription notice', () => {
  it('StartMeeting_TranscriptionNotAutomatic_ShowsTheNoticeAndStillOpensMeet', async () => {
    const api = createStubApi();
    const { user } = openChannel({}, api);
    vi.spyOn(hardNavigate, 'open').mockImplementation(() => undefined);
    api.meetings.startMeeting.mockResolvedValue(started(false));

    await user.click(await startButton());

    const notice = await noticeWith(MANUAL, 'status');
    expect(notice).toHaveTextContent(MANUAL);
    expect(hardNavigate.open).toHaveBeenCalledWith(MEET);
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('StartMeeting_TranscriptionAutomatic_ShowsNoNotice', async () => {
    const api = createStubApi();
    const { user } = openChannel({}, api);
    vi.spyOn(hardNavigate, 'open').mockImplementation(() => undefined);
    api.meetings.startMeeting.mockResolvedValue(started(true));

    await user.click(await startButton());

    await waitFor(() => {
      expect(hardNavigate.open).toHaveBeenCalledWith(MEET);
    });
    expect(screen.queryByText(MANUAL_START)).not.toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('clears the notice when the next meeting starts with automatic transcription', async () => {
    const api = createStubApi();
    const { user } = openChannel({}, api);
    vi.spyOn(hardNavigate, 'open').mockImplementation(() => undefined);
    api.meetings.startMeeting.mockResolvedValueOnce(started(false)).mockResolvedValueOnce(started(true));

    await user.click(await startButton());
    await noticeWith(MANUAL, 'status');
    await user.click(await startButton());

    await waitFor(() => {
      expect(screen.queryByText(MANUAL_START)).not.toBeInTheDocument();
    });
  });

  it('says it in Thai, with the Latin product words in en runs', async () => {
    useUiStore.setState({ lang: 'th' });
    const api = createStubApi();
    const { user } = openChannel({}, api);
    vi.spyOn(hardNavigate, 'open').mockImplementation(() => undefined);
    api.meetings.startMeeting.mockResolvedValue(started(false));

    await user.click(await screen.findByRole('button', { name: 'เริ่มประชุม' }));

    const notice = await noticeWith(/เริ่มการประชุมแล้ว/, 'status');
    expect(notice).toHaveTextContent('เริ่มการประชุมแล้ว');
    expect(notice).not.toHaveTextContent('Transcription is not automatic');
    const latin = Array.from(notice.querySelectorAll('[lang="en"]')).map((run) => run.textContent);
    expect(latin).toEqual(expect.arrayContaining(['Google', 'Meet']));
  });
});

describe('Start meeting: failures of Meet', () => {
  const causes: { name: string; error: ConnectError; en: string; th: string }[] = [
    {
      name: 'meet access refused',
      error: meetError(Code.FailedPrecondition, 'meet access refused', 'MEET_ACCESS', 'access_denied'),
      en: "Google refused K.I.W.I.'s access to Meet for your account. Check the permission in your Google account, then try again.",
      th: 'ไม่อนุญาตให้',
    },
    {
      name: 'api disabled',
      error: meetError(Code.FailedPrecondition, 'meet unavailable', 'MEET_UNAVAILABLE', 'api_disabled'),
      en: "Meet isn't enabled on this K.I.W.I. server. Ask whoever runs it to turn it on.",
      th: 'ยังไม่ได้เปิดใช้',
    },
    {
      name: 'rate limited',
      error: meetError(Code.ResourceExhausted, 'meet unavailable', 'MEET_UNAVAILABLE', 'rate_limited'),
      en: 'Google is limiting Meet requests right now. Wait a minute, then try again.',
      th: 'กำลังจำกัดจำนวนคำขอ',
    },
    {
      name: 'unavailable',
      error: meetError(Code.Unavailable, 'meet unavailable', 'MEET_UNAVAILABLE', 'unavailable'),
      en: "Meet isn't answering right now. Try again in a moment.",
      th: 'ยังไม่ตอบสนอง',
    },
    {
      name: 'refused',
      error: meetError(Code.FailedPrecondition, 'meet unavailable', 'MEET_UNAVAILABLE', 'refused'),
      en: 'Google refused to create the meeting. Try again, and tell whoever runs K.I.W.I. if it keeps happening.',
      th: 'ปฏิเสธการสร้างการประชุม',
    },
  ];

  for (const cause of causes) {
    it(`StartMeeting_${cause.name}_SaysWhyInsteadOfTryAgain`, async () => {
      const api = createStubApi();
      const { user } = openChannel({}, api);
      vi.spyOn(hardNavigate, 'to').mockImplementation(() => undefined);
      vi.spyOn(hardNavigate, 'open').mockImplementation(() => undefined);
      api.meetings.startMeeting.mockRejectedValue(cause.error);

      await user.click(await startButton());

      const notice = await noticeWith(cause.en, 'alert');
      expect(notice).toHaveTextContent(cause.en);
      expect(notice).not.toHaveTextContent("Couldn't start the meeting");
      // A FailedPrecondition that names a Meet failure is not a request for consent.
      expect(hardNavigate.to).not.toHaveBeenCalled();
      expect(hardNavigate.open).not.toHaveBeenCalled();
    });

    it(`says ${cause.name} in Thai`, async () => {
      useUiStore.setState({ lang: 'th' });
      const api = createStubApi();
      const { user } = openChannel({}, api);
      api.meetings.startMeeting.mockRejectedValue(cause.error);

      await user.click(await screen.findByRole('button', { name: 'เริ่มประชุม' }));

      const notice = await noticeWith(new RegExp(cause.th), 'alert');
      expect(notice).toHaveTextContent(cause.th);
      expect(notice).not.toHaveTextContent('เริ่มการประชุมไม่ได้ ลองอีกครั้ง');
    });
  }

  it('keeps the generic message for an error it does not know, and for a Meet failure with a cause it does not know', async () => {
    const api = createStubApi();
    const { user } = openChannel({}, api);
    api.meetings.startMeeting
      .mockRejectedValueOnce(new ConnectError('boom', Code.Internal))
      .mockRejectedValueOnce(meetError(Code.Unavailable, 'meet unavailable', 'MEET_UNAVAILABLE', 'something_new'));

    await user.click(await startButton());
    expect(await screen.findByRole('alert')).toHaveTextContent("Couldn't start the meeting. Try again.");
    await user.click(await startButton());

    await waitFor(() => {
      expect(api.meetings.startMeeting).toHaveBeenCalledTimes(2);
    });
    expect(await screen.findByRole('alert')).toHaveTextContent("Couldn't start the meeting. Try again.");
  });

  it('still sends the person to the consent flow when the API asks for consent', async () => {
    const api = createStubApi();
    const { user } = openChannel({}, api);
    vi.spyOn(hardNavigate, 'to').mockImplementation(() => undefined);
    const consent = new ConnectError('consent required', Code.FailedPrecondition);
    // ErrorInfo{reason: CONSENT_REQUIRED, domain: kiwi}: no Meet reason in it.
    const value = new BinaryWriter().tag(1, WireType.LengthDelimited).string('CONSENT_REQUIRED').tag(2, WireType.LengthDelimited).string('kiwi').finish();
    consent.details.push({ type: 'google.rpc.ErrorInfo', value });
    api.meetings.startMeeting.mockRejectedValue(consent);

    await user.click(await startButton());

    await waitFor(() => {
      expect(hardNavigate.to).toHaveBeenCalledWith('/api/auth/meet-consent?return_to=%2Fc%2Fch-q4');
    });
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });
});
