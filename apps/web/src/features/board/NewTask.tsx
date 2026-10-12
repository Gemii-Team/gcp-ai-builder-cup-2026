import { TaskForm } from '@kiwi/ui';
import { useQueryClient } from '@tanstack/react-query';
import { useApi } from '../../api/client';
import { membersQuery } from '../../api/queries';
import type { ChannelView, TaskView } from '../../api/types';
import { useI18n } from '../../i18n';
import { useCreateTask } from './useCreateTask';

type NewTaskProps = {
  /** The id the header's two New task buttons name in `aria-controls`. */
  id: string;
  channels: readonly ChannelView[];
  /** The Channel filter's value when one is set. */
  filteredChannelId: string | null;
  onCreated: (task: TaskView) => void;
  onCancel: () => void;
};

/**
 * The inline form at the top of the Board's scroller: a task a person adds by hand, with no meeting, message or
 * document behind it. A person acts directly, so the server confirms it at once (see `useCreateTask`): it is a plain
 * card in To do of the Added by hand lane, never a draft. Its Channel starts on the Channel filter, else the first
 * channel; its Owner choices are the members of the selected channel.
 */
export function NewTask({ id, channels, filteredChannelId, onCreated, onCancel }: NewTaskProps) {
  const api = useApi();
  const queryClient = useQueryClient();
  const { plain } = useI18n();
  const create = useCreateTask();
  return (
    <div className="bd-nfw" id={id}>
      <TaskForm
        heading={plain('newTask')}
        subtitle={plain('newTaskSubtitle')}
        initialTitle=""
        channels={channels.map((channel) => ({ id: channel.id, name: channel.name }))}
        channelId={filteredChannelId ?? channels[0]?.id ?? ''}
        membersFor={async (channelId) => {
          const members = await queryClient.fetchQuery({ ...membersQuery(api, channelId), staleTime: 60_000 });
          return members.map((member) => ({ id: member.userId, name: member.name }));
        }}
        onSubmit={async (values) => {
          onCreated(await create(values));
        }}
        onCancel={onCancel}
      />
    </div>
  );
}
