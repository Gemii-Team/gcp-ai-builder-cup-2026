import type { I18nKey } from '@kiwi/core';

/** What a failed file or meeting says, as the sentence fragment that follows `Couldn't summarize {name}:`. */
export function failureReasonKey(code: string): I18nKey {
  switch (code) {
    case 'too_large':
      return 'reasonTooLarge';
    case 'unsupported_type':
      return 'reasonUnsupportedType';
    case 'empty':
      return 'reasonEmpty';
    case 'unreadable':
    case 'no_text':
      return 'reasonUnreadable';
    case 'no_speech':
      return 'reasonNoSpeech';
    case 'transcript_too_long':
      return 'reasonTranscriptTooLong';
    case 'model_unavailable':
      return 'reasonModelUnavailable';
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
