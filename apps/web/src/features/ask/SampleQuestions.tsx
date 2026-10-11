import { Button } from '@kiwi/ui';
import { T } from '../../i18n';
import { SAMPLE_QUESTIONS } from './samples';

/** One sample question in English and one in Thai. Pressing one fills the field and sends nothing. */
export function SampleQuestions({ onPick }: { onPick: (question: string) => void }) {
  return (
    <div className="ak-samples">
      <span>
        <T k="askSamples" />
      </span>
      {SAMPLE_QUESTIONS.map((sample) => (
        <Button
          key={sample.text}
          size="sm"
          className="ak-sample"
          {...(sample.lang === 'th' ? { lang: 'th' } : {})}
          onClick={() => {
            onPick(sample.text);
          }}
        >
          {sample.text}
        </Button>
      ))}
    </div>
  );
}
