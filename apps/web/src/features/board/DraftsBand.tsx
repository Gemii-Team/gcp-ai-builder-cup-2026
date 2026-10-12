import { formatDay, DraftsPanel, type BoardCounts, type DraftTask, type SealedTask } from '@kiwi/ui';
import { useQueries, useQueryClient } from '@tanstack/react-query';
import { useApi } from '../../api/client';
import { confirmDraft, editDraft } from '../../api/confirmDraft';
import { membersQuery } from '../../api/queries';
import type { TaskView } from '../../api/types';
import { useI18n } from '../../i18n';

type DraftsBandProps = {
  /** The AI's drafts of every channel the person is in: a filter never hides one. */
  drafts: readonly TaskView[];
  /** What was confirmed a moment ago, for the phone sheet's `Confirmed just now` list. */
  sealed: readonly SealedTask[];
  counts: BoardCounts;
  /** The server confirmed this draft: the Board lands its sealed card in To do of its lane. */
  onConfirmed: (task: TaskView) => void;
};

/**
 * The AI drafts band: the drafts panel laid across the top of the scroller (and, below 1024px, the same panel as the
 * bottom sheet). Its drafts belong to different channels, and a draft can only be given to a member of its own, so each
 * slip's owner select lists the members of that slip's channel. Confirming awaits the server first (see `confirmDraft`).
 */
export function DraftsBand({ drafts, sealed, counts, onConfirmed }: DraftsBandProps) {
  const api = useApi();
  const queryClient = useQueryClient();
  const { lang } = useI18n();
  const channelIds = [...new Set(drafts.map((draft) => draft.channelId))];
  const members = useQueries({ queries: channelIds.map((channelId) => membersQuery(api, channelId)) });

  const panelDrafts: DraftTask[] = drafts.flatMap((draft) => {
    if (draft.source === null) {
      return [];
    }
    const roster = members[channelIds.indexOf(draft.channelId)]?.data;
    return [
      {
        id: draft.id,
        ref: draft.ref,
        title: draft.title,
        ownerId: draft.assigneeId,
        source: draft.source,
        ...(draft.dueAt === null ? {} : { dueLabel: formatDay(draft.dueAt, lang) }),
        ...(draft.fromRef === null ? {} : { fromAnswerRef: draft.fromRef }),
        ...(roster === undefined ? {} : { members: roster.map((member) => ({ id: member.userId, name: member.name })) }),
      },
    ];
  });

  return (
    <DraftsPanel
      variant="band"
      drafts={panelDrafts}
      sealed={sealed}
      members={[]}
      counts={counts}
      onConfirm={async (id) => {
        const draft = drafts.find((candidate) => candidate.id === id);
        if (draft !== undefined) {
          onConfirmed(await confirmDraft(api, queryClient, draft));
        }
      }}
      onEdit={(id, patch) => editDraft(api, queryClient, id, patch)}
    />
  );
}
