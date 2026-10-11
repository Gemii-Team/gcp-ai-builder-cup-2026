import { Code } from '@connectrpc/connect';
import type { MessageInitShape } from '@bufbuild/protobuf';
import { timestampFromDate } from '@bufbuild/protobuf/wkt';
import { CreateTaskRequestSchema, SourceKind, SourceSchema, TaskState } from '@kiwi/api-client';
import type { DraftPatch, Source } from '@kiwi/ui';
import { useQueryClient } from '@tanstack/react-query';
import { useRef, useState } from 'react';
import { useApi } from '../../api/client';
import { editDraft } from '../../api/confirmDraft';
import { hasCode } from '../../api/errors';
import { mapTask } from '../../api/mappers';
import { removeTask, writeTask } from '../../api/queries';
import type { ArtifactView, CitationView, TaskView } from '../../api/types';
import { clipRunes } from '../../lib/text';

/** The longest title of a task made from an answer, in runes. */
const MAX_TITLE_RUNES = 120;

/** The longest quote a task's source may carry, in runes: the tasks service refuses a longer one. */
const MAX_QUOTE_RUNES = 500;

/** The end of a sentence: a full stop, `!` or `?` followed by a space or the end. Never after a Thai letter, where a full stop abbreviates (`ธ.ค.`). */
const SENTENCE_END = /(?<![\u0E00-\u0E7F])[.!?。](?=\s|$)/;

/**
 * What a task made from an answer is called to begin with: the answer's first sentence, as plain words (no citation
 * markers, no markdown), clipped to 120 runes. Thai has no full stops between sentences, so a Thai answer's title is its
 * first line.
 */
export function answerTitle(markdown: string): string {
  const line =
    markdown
      .replace(/!?\[([^\]]*)\]\([^)]*\)/g, '$1')
      .replace(/\s*\[\d+\]/g, '')
      .replace(/^\s{0,3}(?:#{1,6}\s+|[-*+]\s+|\d+[.)]\s+|>\s?)/gm, '')
      .replace(/\*{1,3}|~~|`/g, '')
      .split('\n')
      .map((candidate) => candidate.trim())
      .find((candidate) => candidate !== '') ?? '';
  const end = SENTENCE_END.exec(line);
  return clipRunes(end === null ? line : line.slice(0, end.index + 1), MAX_TITLE_RUNES);
}

const SOURCE_KINDS = { meeting: SourceKind.MEETING, file: SourceKind.FILE, message: SourceKind.MESSAGE } as const;

/** The anchor the tasks service stores for a source: a meeting's entry index, a file's `page:<n>`, a message's seq. */
function anchorOf(source: Source): string {
  switch (source.kind) {
    case 'meeting':
      return source.entryIdx == null ? '' : String(source.entryIdx);
    case 'file':
      return source.page === undefined ? '' : `page:${String(source.page)}`;
    case 'message':
      return source.seq === undefined ? '' : String(source.seq);
  }
}

/** A citation's source as the source of a task, with the cited passage as its quote (at most 500 runes). */
function taskSourceOf({ source, quote }: CitationView): MessageInitShape<typeof SourceSchema> {
  return {
    kind: SOURCE_KINDS[source.kind],
    id: source.id,
    anchor: anchorOf(source),
    label: source.label,
    channelId: source.channelId,
    // Only a message source has a time of its own.
    ...(source.kind === 'message' && source.at !== undefined ? { at: timestampFromDate(new Date(source.at)) } : {}),
    ...(source.offsetMs == null ? {} : { offsetMs: BigInt(source.offsetMs) }),
    ...(source.page === undefined ? {} : { page: source.page }),
    ...(quote === '' ? {} : { quote: clipRunes(quote, MAX_QUOTE_RUNES) }),
  };
}

/**
 * Make a task from an answer. The task is a REAL draft in the tasks service, never something a tab remembers: its channel
 * is the channel of the answer's first citation, its source is that citation's source with the cited passage as its quote,
 * and `from_ref` is the answer's REF. It enters the task caches through the one `writeTask`, so the Board's drafts band
 * shows it. A draft made here is never confirmed here: that is the Board's, or the channel's drafts panel's, to do.
 */
export function useMakeTask(answer: ArtifactView) {
  const api = useApi();
  const queryClient = useQueryClient();
  const [draft, setDraft] = useState<TaskView | null>(null);
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<'create' | 'discard' | null>(null);
  const making = useRef(false);
  const first = [...answer.citations].sort((a, b) => a.n - b.n)[0];
  /** An answer with no citation, or none whose source names its channel, has nothing for a task to stand on. */
  const available = first !== undefined && first.source.channelId !== '';

  /** Drafts the task; resolves with it, or null when it could not be made (`failure` says so). */
  async function make(): Promise<TaskView | null> {
    // `busy` is state, which a second press in the same tick has not seen yet: the ref is what stops it.
    if (first === undefined || !available || making.current || draft !== null) {
      return null;
    }
    making.current = true;
    setBusy(true);
    setFailure(null);
    try {
      const request: MessageInitShape<typeof CreateTaskRequestSchema> = {
        channelId: first.source.channelId,
        title: answerTitle(answer.bodyMarkdown) || first.source.label,
        state: TaskState.DRAFT,
        source: taskSourceOf(first),
        fromRef: answer.ref,
      };
      const { task } = await api.tasks.createTask(request);
      if (task === undefined) {
        throw new Error('CreateTask answered without a task.');
      }
      const view = mapTask(task);
      writeTask(queryClient, view);
      setDraft(view);
      return view;
    } catch {
      setFailure('create');
      return null;
    } finally {
      making.current = false;
      setBusy(false);
    }
  }

  /** Deletes the draft, which frees the button to make another. A draft that is already gone counts as discarded. */
  async function discard(): Promise<void> {
    if (draft === null) {
      return;
    }
    setFailure(null);
    try {
      await api.tasks.discardDraft({ id: draft.id });
    } catch (error) {
      if (!hasCode(error, Code.NotFound)) {
        setFailure('discard');
        return;
      }
    }
    removeTask(queryClient, draft);
    setDraft(null);
  }

  /** Saves the owner (or title) of the draft: a rejection reaches the draft slip, which says so. */
  async function edit(patch: DraftPatch): Promise<void> {
    if (draft === null) {
      return;
    }
    await editDraft(api, queryClient, draft.id, patch);
    setDraft((current) =>
      current === null
        ? current
        : {
            ...current,
            ...(patch.title === undefined ? {} : { title: patch.title }),
            ...(patch.ownerId === undefined ? {} : { assigneeId: patch.ownerId }),
          },
    );
  }

  return { draft, available, busy, failure, make, discard, edit };
}
