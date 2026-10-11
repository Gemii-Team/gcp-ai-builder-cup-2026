import { createI18n } from '@kiwi/core';
import { describe, expect, it } from 'vitest';
import { failureReasonKey, isRecording, isRejectionReason, REJECTION_REASONS, rejectionReasonOf, baseName } from './failure';

describe('rejectionReasonOf', () => {
  it.each(REJECTION_REASONS)('reads %s from the exact shape `docs: upload rejected (<reason>): <detail>`', (reason) => {
    expect(rejectionReasonOf(`docs: upload rejected (${reason}): the detail`)).toBe(reason);
  });

  it('does not read the detail, whatever it says, parentheses included', () => {
    expect(rejectionReasonOf('docs: upload rejected (empty): file is too large (too_large) and unsupported_type')).toBe('empty');
  });

  it('is anchored: a message that only contains the shape, or has a prefix, names no reason', () => {
    expect(rejectionReasonOf('[invalid_argument] docs: upload rejected (empty): x')).toBeNull();
    expect(rejectionReasonOf('error: docs: upload rejected (empty): x')).toBeNull();
    expect(rejectionReasonOf(' docs: upload rejected (empty): x')).toBeNull();
  });

  it('names no reason for any other shape or wording', () => {
    for (const message of [
      '',
      'unsupported type',
      'upload rejected (empty): x',
      'docs: upload rejected empty: x',
      'docs: upload rejected (): x',
      'docs: upload rejected (empty) x',
      'docs: the file is empty',
    ]) {
      expect(rejectionReasonOf(message), message).toBeNull();
    }
  });

  it('names no reason this build does not know', () => {
    expect(rejectionReasonOf('docs: upload rejected (virus_found): x')).toBeNull();
    expect(isRejectionReason('virus_found')).toBe(false);
    expect(isRejectionReason('unsupported_audio')).toBe(true);
  });
});

describe('failureReasonKey', () => {
  it('has its own sentence for each of the five rejection reasons and for each other reason a job gives', () => {
    const codes = [...REJECTION_REASONS, 'unreadable', 'no_text', 'no_speech', 'transcript_too_long', 'model_unavailable'];
    const keys = codes.map(failureReasonKey);

    expect(keys).not.toContain('reasonUnknown');
    // `unreadable` and `no_text` share one sentence; every other code has a sentence of its own.
    expect(new Set(keys).size).toBe(codes.length - 1);
  });

  it('falls back to the unknown sentence for a code it has not heard of', () => {
    expect(failureReasonKey('something_new')).toBe('reasonUnknown');
  });

  it.each(['en', 'th'] as const)('has a %s sentence for every rejection reason, and the Thai names WAV and PCM16 as Latin', (lang) => {
    const { plain, t } = createI18n(lang);
    const sentences = REJECTION_REASONS.map((reason) => plain(failureReasonKey(reason)));

    expect(new Set(sentences).size).toBe(REJECTION_REASONS.length);
    for (const sentence of sentences) {
      expect(sentence.trim()).not.toBe('');
    }
    if (lang === 'th') {
      expect(t(failureReasonKey('unsupported_audio'))).toContain('<en>WAV</en>');
      expect(t(failureReasonKey('unsupported_audio'))).toContain('<en>PCM16</en>');
    }
  });
});

describe('isRecording and baseName', () => {
  it('takes sound and video for a recording and nothing else', () => {
    expect(['audio/wav', 'audio/mpeg', 'video/mp4'].map(isRecording)).toEqual([true, true, true]);
    expect(['application/pdf', 'text/csv', ''].map(isRecording)).toEqual([false, false, false]);
  });

  it('names a meeting by the file name without its extension', () => {
    expect(baseName('standup.wav')).toBe('standup');
    expect(baseName('q4.launch.sync.wav')).toBe('q4.launch.sync');
    expect(baseName('noextension')).toBe('noextension');
  });
});
