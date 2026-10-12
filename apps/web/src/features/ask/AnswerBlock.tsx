import { OnBoardSlip } from './OnBoardSlip';
import { Button, DraftSlip, type DraftTask } from '@kiwi/ui';
import { useQuery } from '@tanstack/react-query';
import { useEffect, useRef } from 'react';
import { useApi } from '../../api/client';
import { membersQuery } from '../../api/queries';
import type { ArtifactView, TaskView } from '../../api/types';
import { T, TCount, useI18n } from '../../i18n';
import { AnswerSlip } from './AnswerSlip';
import { useMakeTask } from './useMakeTask';

type AnswerBlockProps = {
  answer: ArtifactView;
  channelNames: ReadonlyMap<string, string>;
  pressed: number | null;
  onPress: (n: number) => void;
  /** The sources of this answer are the ones on show. */
  selected: boolean;
  onSources: () => void;
  /** Says something in the screen's live region. */
  say: (text: string) => void;
};

/** The draft as the handoff slip shows it: the task the tasks service made, and the answer it was made from. */
function draftOf(task: TaskView, answer: ArtifactView): DraftTask {
  const [first] = [...answer.citations].sort((a, b) => a.n - b.n);
  return {
    id: task.id,
    ref: task.ref,
    title: task.title,
    ownerId: task.assigneeId,
    // A draft always has the source it was made from; the first citation's stands in for one the service left out.
    source: task.source ?? first?.source ?? { kind: 'message', id: '', label: '', channelId: task.channelId },
    fromAnswerRef: task.fromRef ?? answer.ref,
  };
}

/**
 * An answer and what a person can do with it: the Sources toggle, and Make a task from this, which drafts a real task
 * through the tasks service and shows it under the answer as a handoff slip. A handoff slip has no Confirm (a task made
 * from an answer is confirmed on the Board, or in the channel's drafts panel): Review on the Board is a link and Discard
 * deletes the draft, after which the button offers to make another. The slip follows the task in the cache: confirmed by
 * someone elsewhere, it gives way to a plain card that says it is on the Board; discarded elsewhere, it is dropped.
 */
export function AnswerBlock({ answer, channelNames, pressed, onPress, selected, onSources, say }: AnswerBlockProps) {
  const { plain } = useI18n();
  const api = useApi();
  const task = useMakeTask(answer);
  const members = useQuery({ ...membersQuery(api, task.draft?.channelId ?? ''), enabled: task.draft?.state === 'draft', staleTime: 60_000 });
  const make = useRef<HTMLButtonElement>(null);
  const slot = useRef<HTMLDivElement>(null);
  /** Where focus goes once the draft has been drawn (its owner select) or removed (the button that made it). */
  const focusNext = useRef<'draft' | 'button' | null>(null);

  useEffect(() => {
    if (focusNext.current === 'draft' && task.draft !== null) {
      focusNext.current = null;
      slot.current?.querySelector<HTMLElement>('select')?.focus();
    } else if (focusNext.current === 'button' && task.draft === null) {
      focusNext.current = null;
      make.current?.focus();
    }
  }, [task.draft]);

  async function draft() {
    focusNext.current = 'draft';
    const made = await task.make();
    if (made === null) {
      focusNext.current = null;
    } else {
      say(plain('askDraftCreated', { title: made.title }));
    }
  }

  async function discard() {
    focusNext.current = 'button';
    if (await task.discard()) {
      say(plain('askDraftDiscarded'));
    } else {
      focusNext.current = null;
    }
  }

  const actions = (
    <>
      <Button size="sm" className="ak-tgl" aria-pressed={selected} aria-label={plain('askSourcesOf', { ref: answer.ref })} onClick={onSources}>
        <TCount k="askSourcesToggle" count={answer.citations.length} />
      </Button>
      {task.available && (
        <Button
          ref={make}
          size="sm"
          disabled={task.draft !== null || task.busy}
          aria-busy={task.busy || undefined}
          onClick={() => {
            void draft();
          }}
        >
          <T k={task.draft === null ? 'askMakeTask' : 'askTaskDrafted'} />
        </Button>
      )}
    </>
  );

  return (
    <>
      <AnswerSlip answer={answer} channelNames={channelNames} pressed={pressed} onPress={onPress} actions={actions} />
      {task.draft !== null && (
        <div ref={slot} className="ak-dr">
          {task.draft.state === 'draft' ? (
            <DraftSlip
              mode="handoff"
              draft={draftOf(task.draft, answer)}
              members={(members.data ?? []).map((member) => ({ id: member.userId, name: member.name }))}
              boardHref="/board"
              onEdit={task.edit}
              onDiscard={() => {
                void discard();
              }}
            />
          ) : (
            <OnBoardSlip task={task.draft} />
          )}
        </div>
      )}
      {task.failure !== null && (
        <p role="alert" className="kiwi-err">
          <T k={task.failure === 'create' ? 'askMakeTaskFailed' : 'askDiscardFailed'} />
        </p>
      )}
    </>
  );
}
