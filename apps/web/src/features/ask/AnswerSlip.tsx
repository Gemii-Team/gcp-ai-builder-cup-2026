import { ProofSlip } from '@kiwi/ui';
import type { ReactNode } from 'react';
import type { ArtifactView } from '../../api/types';
import { useI18n } from '../../i18n';
import { Markdown } from '../markdown/Markdown';
import { displaySource, markerLabel } from './sources';

type AnswerSlipProps = {
  answer: ArtifactView;
  channelNames: ReadonlyMap<string, string>;
  /** The citation whose marker is pressed: its marker fills and its row takes Citation Wash. */
  pressed: number | null;
  onPress: (n: number) => void;
  /** Sources and Make a task, in the stub. */
  actions?: ReactNode;
};

/**
 * An answer: a proof slip with no title whose body is the model's markdown (sanitised, its `[n]` markers pressable) and
 * whose stub lists one row per cited source: marker and name at the left, offset, page or time at the right. Pressing a
 * marker, or a row's offset or page, is a citation press, not an excerpt: the matching source slip is the excerpt.
 */
export function AnswerSlip({ answer, channelNames, pressed, onPress, actions }: AnswerSlipProps) {
  const { plain } = useI18n();
  const cited = [...answer.citations].sort((a, b) => a.n - b.n);
  // The REF code is a prop named `ref`: the hooks lint takes it for a React ref when it is read beside the rest of the props.
  const common = { ref: answer.ref, generatedAt: answer.generatedAt };
  return (
    <ProofSlip
      {...common}
      summary={
        <Markdown
          markdown={answer.bodyMarkdown}
          cited={{ citations: cited, pressed, onPress, labelOf: (citation) => markerLabel(displaySource(citation, channelNames, plain), plain) }}
        />
      }
      sources={cited.map((citation) => displaySource(citation, channelNames, plain))}
      sourceNumbers={cited.map((citation) => citation.n)}
      hitNumber={pressed}
      onSourcePress={(_, index) => {
        const citation = cited[index];
        if (citation !== undefined) {
          onPress(citation.n);
        }
      }}
      {...(actions === undefined ? {} : { actions })}
    />
  );
}
