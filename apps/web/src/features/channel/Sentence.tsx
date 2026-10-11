import type { I18nKey, I18nParams } from '@kiwi/core';
import { LangRuns, Rich } from '@kiwi/ui';
import { useI18n } from '../../i18n';

/** Stands where the person's own words go while the sentence around them is rendered (a control character no sentence contains). */
const SLOT = '\u0001';

type SentenceProps = {
  k: I18nKey;
  /** The person's own words (a file name, a meeting title): their Thai runs are tagged `lang="th"`. */
  text: string;
  /** The placeholder of `k` that takes `text`. */
  slot?: string;
  /** The other placeholders; a `reason` is another key, whose sentence is filled in. */
  reason?: I18nKey;
  params?: I18nParams;
};

/**
 * A screen sentence that names a file or a meeting. Its Latin words inside Thai prose (`WAV`, `PCM16`) are tagged
 * `lang="en"` by `Rich`, and the person's own words, the file name, are rendered by `LangRuns`. The inline upload notice
 * and the failed-file slip both say a reason through this one component, so the same reason reads, and is tagged, alike.
 */
export function Sentence({ k, text, slot = 'name', reason, params }: SentenceProps) {
  const { t } = useI18n();
  const sentence = t(k, { ...params, [slot]: SLOT, ...(reason === undefined ? {} : { reason: t(reason) }) });
  const [before = '', after = ''] = sentence.split(SLOT);
  return (
    <>
      <Rich text={before} />
      <LangRuns text={text} />
      <Rich text={after} />
    </>
  );
}
