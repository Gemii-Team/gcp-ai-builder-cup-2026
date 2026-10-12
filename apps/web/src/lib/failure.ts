import type { I18nKey } from '@kiwi/core';
import { webReason } from './serverReasons';

/**
 * The reasons the server gives for a file it will not take: the `<reason>` of `docs: upload rejected (<reason>): <detail>`
 * (the Connect error that both `CreateUpload` and `CompleteUpload` answer with) and the `failure_reason` of a failed
 * file (`file_status_changed`, `GetFile`). The same words, so a rejected upload and a failed file slip say the same
 * thing for the same reason.
 */
export const REJECTION_REASONS = ['empty', 'too_large', 'unsupported_type', 'unsupported_audio', 'size_mismatch'] as const;

export type RejectionReason = (typeof REJECTION_REASONS)[number];

export function isRejectionReason(code: string): code is RejectionReason {
  return (REJECTION_REASONS as readonly string[]).includes(code);
}

/** The whole shape, anchored at the start: nothing is read from the server's English `<detail>` or from any other wording. */
const REJECTED = /^docs: upload rejected \((\w+)\):/;

/** The reason of a rejected upload, from the raw message of its `invalid_argument` error; null for any other message. */
export function rejectionReasonOf(message: string): RejectionReason | null {
  const code = REJECTED.exec(message)?.[1];
  return code !== undefined && isRejectionReason(code) ? code : null;
}

/** What a failed file or meeting says, as the sentence fragment that follows `Couldn't summarize {name}:`. */
export function failureReasonKey(code: string): I18nKey {
  switch (webReason(code)) {
    case 'too_large':
      return 'reasonTooLarge';
    case 'unsupported_type':
      return 'reasonUnsupportedType';
    case 'unsupported_audio':
      return 'reasonUnsupportedAudio';
    case 'size_mismatch':
      return 'reasonSizeMismatch';
    case 'empty':
      return 'reasonEmpty';
    case 'unsupported_by_provider':
      return 'reasonUnsupportedByProvider';
    case 'unreadable':
    case 'no_text_layer':
      return 'reasonUnreadable';
    case 'no_speech':
      return 'reasonNoSpeech';
    case 'transcript_too_long':
      return 'reasonTranscriptTooLong';
    case 'model_unavailable':
      return 'reasonModelUnavailable';
    // The two an Ask run can fail with before the server has said anything: the request never arrived, or no answer came in time.
    case 'not_reached':
      return 'reasonNotReached';
    case 'timed_out':
      return 'reasonTimedOut';
    // An Ask run that is over but carries no answer to show: an artifact of no kind, as an older build stored a model's plain reply.
    case 'no_answer':
      return 'reasonNoAnswer';
    default:
      return 'reasonUnknown';
  }
}

/** The recording MIME types: sound and video are a meeting to summarize; every other file is an attachment. */
export function isRecording(mime: string): boolean {
  return mime.startsWith('audio/') || mime.startsWith('video/');
}

/** A file name without its extension, which names the meeting a recording becomes. */
export function baseName(name: string): string {
  return /^(.+)\.[^./\\]+$/.exec(name)?.[1] ?? name;
}
