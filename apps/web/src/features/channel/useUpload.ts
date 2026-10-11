import { Code, ConnectError } from '@connectrpc/connect';
import { FileStatus, UploadPurpose } from '@kiwi/api-client';
import type { I18nKey } from '@kiwi/core';
import { useQueryClient } from '@tanstack/react-query';
import { useCallback, useState } from 'react';
import { useApi } from '../../api/client';
import { hasCode } from '../../api/errors';
import { mapFile, mapMeeting } from '../../api/mappers';
import { keys } from '../../api/queries';
import type { MeetingView } from '../../api/types';
import { baseName, isRecording } from '../../lib/failure';

/** The sentence an upload that did not go through is told in. */
export type UploadRejection = Extract<I18nKey, 'uploadUnsupported' | 'uploadTooLarge' | 'uploadEmpty' | 'uploadRejected' | 'uploadFailed'>;

export type UploadState = { status: 'idle' } | { status: 'uploading'; name: string } | { status: 'rejected'; name: string; reason: UploadRejection };

/** The server's short code for a file it refused, as the sentence of the rejection. */
function fromCode(code: string): UploadRejection {
  switch (code) {
    case 'too_large':
      return 'uploadTooLarge';
    case 'unsupported_type':
      return 'uploadUnsupported';
    case 'empty':
      return 'uploadEmpty';
    default:
      return 'uploadRejected';
  }
}

/** A refusal (`ErrInvalid`) says what was wrong in its message; one that does not say is simply refused. A failure to reach the server is not a refusal. */
function fromError(error: unknown): UploadRejection {
  if (!hasCode(error, Code.InvalidArgument) || !(error instanceof ConnectError)) {
    return 'uploadFailed';
  }
  if (/type|mime|unsupported/i.test(error.rawMessage)) {
    return 'uploadUnsupported';
  }
  if (/large|size|limit/i.test(error.rawMessage)) {
    return 'uploadTooLarge';
  }
  return /empty/i.test(error.rawMessage) ? 'uploadEmpty' : 'uploadRejected';
}

/** Why the storage refused the bytes: the size limit is enforced at the PUT. */
function fromStatus(status: number): UploadRejection {
  return status === 413 ? 'uploadTooLarge' : status === 415 ? 'uploadUnsupported' : 'uploadFailed';
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
      const reject = (reason: UploadRejection) => {
        setState({ status: 'rejected', name: file.name, reason });
      };
      setState({ status: 'uploading', name: file.name });
      const recording = isRecording(file.type);
      let step: 'create' | 'put' | 'complete' | 'meeting' = 'create';
      try {
        const created = await api.files.createUpload({
          channelId,
          name: file.name,
          mime: file.type,
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
          reject(fromCode(completed.failureReason));
          return;
        }
        if (recording) {
          step = 'meeting';
          const { meeting } = await api.meetings.createFromRecording({ channelId, fileId: completed.id, title: baseName(file.name) });
          if (meeting === undefined) {
            throw new Error('CreateFromRecording answered without a meeting.');
          }
          queryClient.setQueryData<MeetingView>(keys.meeting(meeting.id), mapMeeting(meeting));
          queryClient.setQueryData(keys.recording(completed.id), meeting.id);
        }
        setState({ status: 'idle' });
      } catch (error) {
        reject(step === 'create' || step === 'complete' ? fromError(error) : 'uploadFailed');
      }
    },
    [api, channelId, queryClient],
  );

  const dismiss = useCallback(() => {
    setState({ status: 'idle' });
  }, []);

  return { state, upload, dismiss };
}
