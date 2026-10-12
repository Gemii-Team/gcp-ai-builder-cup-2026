import { Code, ConnectError } from '@connectrpc/connect';
import { FileStatus, UploadPurpose } from '@kiwi/api-client';
import type { I18nKey } from '@kiwi/core';
import { useQueryClient } from '@tanstack/react-query';
import { useCallback, useState } from 'react';
import { useApi } from '../../api/client';
import { hasCode } from '../../api/errors';
import { mapFile, mapMeeting } from '../../api/mappers';
import { keys } from '../../api/queries';
import type { MeetingView, RecordingUpload } from '../../api/types';
import { baseName, failureReasonKey, isRecording, isRejectionReason, rejectionReasonOf } from '../../lib/failure';
import { mimeOf } from '../../lib/mime';

/**
 * The sentence an upload that did not go through is told in: `uploadRejectedFor` names the server's reason (the same
 * words a failed file slip uses), `uploadRejected` is a refusal that gave none, `uploadFailed` is a server not reached.
 */
export type UploadRejection = { key: 'uploadRejectedFor'; reason: I18nKey } | { key: 'uploadRejected' | 'uploadFailed' };

export type UploadState = { status: 'idle' } | { status: 'uploading'; name: string } | { status: 'rejected'; name: string; rejection: UploadRejection };

/** A reason the server named, as the sentence that says it; a reason this build does not know is a refusal with no reason. */
function forReason(code: string): UploadRejection {
  return isRejectionReason(code) ? { key: 'uploadRejectedFor', reason: failureReasonKey(code) } : { key: 'uploadRejected' };
}

/**
 * A refusal (`invalid_argument`) names its reason in the shape `docs: upload rejected (<reason>): <detail>`; the reason is
 * read from that shape alone, and the server's English detail is never shown. Any other refusal says only that the
 * server does not accept the file. A failure to reach the server is not a refusal.
 */
function fromError(error: unknown): UploadRejection {
  if (!hasCode(error, Code.InvalidArgument) || !(error instanceof ConnectError)) {
    return { key: 'uploadFailed' };
  }
  const reason = rejectionReasonOf(error.rawMessage);
  return reason === null ? { key: 'uploadRejected' } : forReason(reason);
}

/** Why the storage refused the bytes: the size limit is enforced at the PUT. */
function fromStatus(status: number): UploadRejection {
  return status === 413 ? forReason('too_large') : status === 415 ? forReason('unsupported_type') : { key: 'uploadFailed' };
}

/**
 * Sends a file into a channel. `CreateUpload` returns a signed same-origin URL and the headers it was signed with; the
 * bytes go to that URL with exactly those headers and no credentials (the URL is the authority); `CompleteUpload` then
 * has the server check the stored object. Sound and video are a RECORDING and become a meeting through
 * `CreateFromRecording`; every other file is an ATTACHMENT, which the server posts into the channel and summarizes. A
 * file the server refuses at any step is said so, inline, in the sentence of its reason: nothing is left pending.
 */
export function useUpload(channelId: string) {
  const api = useApi();
  const queryClient = useQueryClient();
  const [state, setState] = useState<UploadState>({ status: 'idle' });

  const upload = useCallback(
    async (file: File) => {
      const reject = (rejection: UploadRejection) => {
        setState({ status: 'rejected', name: file.name, rejection });
      };
      setState({ status: 'uploading', name: file.name });
      const mime = mimeOf(file);
      const recording = isRecording(mime);
      let step: 'create' | 'put' | 'complete' | 'meeting' = 'create';
      try {
        const created = await api.files.createUpload({
          channelId,
          name: file.name,
          mime,
          size: BigInt(file.size),
          purpose: recording ? UploadPurpose.RECORDING : UploadPurpose.ATTACHMENT,
        });
        step = 'put';
        const stored = await fetch(created.uploadUrl, { method: 'PUT', headers: created.headers, body: file, credentials: 'omit' });
        if (!stored.ok) {
          reject(fromStatus(stored.status));
          return;
        }
        step = 'complete';
        const { file: completed } = await api.files.completeUpload({ fileId: created.fileId });
        if (completed === undefined) {
          throw new Error('CompleteUpload answered without a file.');
        }
        queryClient.setQueryData(keys.file(completed.id), mapFile(completed));
        if (completed.status === FileStatus.FAILED) {
          reject(forReason(completed.failureReason));
          return;
        }
        if (recording) {
          step = 'meeting';
          const { meeting } = await api.meetings.createFromRecording({ channelId, fileId: completed.id, title: baseName(file.name) });
          if (meeting === undefined) {
            throw new Error('CreateFromRecording answered without a meeting.');
          }
          queryClient.setQueryData<MeetingView>(keys.meeting(meeting.id), mapMeeting(meeting));
          queryClient.setQueryData<RecordingUpload[]>(keys.recordings(channelId), (uploads = []) => [
            ...uploads,
            { fileId: completed.id, name: file.name, size: file.size, meetingId: meeting.id, at: new Date().toISOString() },
          ]);
          // The answer is the meeting as it was when the call returned. A recording with no speech fails at once, and the event
          // that says so can reach the page first, when the page does not know the meeting yet and so ignores it: the meeting
          // is read again, so the row never waits on a meeting that has already failed.
          void queryClient.invalidateQueries({ queryKey: keys.meeting(meeting.id) });
        }
        setState({ status: 'idle' });
      } catch (error) {
        reject(step === 'create' || step === 'complete' ? fromError(error) : { key: 'uploadFailed' });
      }
    },
    [api, channelId, queryClient],
  );

  const dismiss = useCallback(() => {
    setState({ status: 'idle' });
  }, []);

  return { state, upload, dismiss };
}
