import type { MessageInitShape } from '@bufbuild/protobuf';
import { timestampFromDate } from '@bufbuild/protobuf/wkt';
import { CreateTaskRequestSchema, SourceKind } from '@kiwi/api-client';
import { CreatedTaskCard, dateToDueAt, formatClock, formatDay, LangRuns, Numeral, TaskForm, type CreatedTaskCardProps, type TaskFormValues } from '@kiwi/ui';
import { useQueryClient } from '@tanstack/react-query';
import { useEffect, useRef } from 'react';
import { useApi } from '../../api/client';
import { useChannelTasks } from '../../api/hooks';
import { mapTask } from '../../api/mappers';
import { membersQuery, writeTask } from '../../api/queries';
import type { ChannelView, ChatMessage, TaskView } from '../../api/types';
import { useI18n } from '../../i18n';
import { titleFromMessage } from './makeTaskTitle';

/** `From #q4-launch · Arthit K. · 14:02`, the time in mono: where a task made from a message came from. */
function MessageOrigin({ message, channel, authorName }: { message: ChatMessage; channel: ChannelView; authorName: string }) {
  const { plain } = useI18n();
  return (
    <>
      <LangRuns text={`${plain('fromChannel', { channel: channel.name })} · ${authorName} · `} />
      <Numeral>{formatClock(message.createdAt)}</Numeral>
    </>
  );
}

type MakeTaskProps = {
  /** The id of the form, which the action that opens it names in `aria-controls`. */
  id: string;
  message: ChatMessage;
  channel: ChannelView;
  authorName: string;
  onCreated: (task: TaskView) => void;
  onCancel: () => void;
};

/**
 * The form under a message that makes a task from it: its title is the message's words, its owners are the channel's
 * members, there is no Channel select. A person acts directly, so the task is confirmed at once: the request carries no
 * `state`, and the task the server returns goes into the task caches before its realtime event arrives.
 */
export function MakeTask({ id, message, channel, authorName, onCreated, onCancel }: MakeTaskProps) {
  const api = useApi();
  const queryClient = useQueryClient();
  const { plain } = useI18n();

  async function create(values: TaskFormValues) {
    const request: MessageInitShape<typeof CreateTaskRequestSchema> = {
      channelId: channel.id,
      title: values.title,
      ...(values.ownerId === null ? {} : { assigneeId: values.ownerId }),
      ...(values.dueDate === null ? {} : { dueAt: timestampFromDate(dateToDueAt(values.dueDate)) }),
      source: {
        kind: SourceKind.MESSAGE,
        id: message.id,
        anchor: String(message.seq),
        label: authorName,
        channelId: channel.id,
        at: timestampFromDate(new Date(message.createdAt)),
      },
    };
    const { task } = await api.tasks.createTask(request);
    if (task === undefined) {
      throw new Error('CreateTask answered without a task.');
    }
    const view = mapTask(task);
    writeTask(queryClient, view);
    onCreated(view);
  }

  return (
    <div className="msg-b" id={id}>
      <TaskForm
        heading={plain('makeTaskHeading')}
        subtitle={<MessageOrigin message={message} channel={channel} authorName={authorName} />}
        initialTitle={titleFromMessage(message, (file) => plain('followUpOn', { file }))}
        channelId={channel.id}
        membersFor={async () => {
          const members = await queryClient.fetchQuery({ ...membersQuery(api, channel.id), staleTime: 60_000 });
          return members.map((member) => ({ id: member.userId, name: member.name }));
        }}
        onSubmit={create}
        onCancel={onCancel}
      />
    </div>
  );
}

type MadeTaskProps = {
  task: TaskView;
  message: ChatMessage;
  channel: ChannelView;
  authorName: string;
  ownerName: string | undefined;
  /** This card is the one just made: it takes focus when it appears. */
  focus: boolean;
};

/** The card of a made task. (A component of its own: its `ref` prop is the REF code, which the hooks lint takes for a React ref.) */
function MadeTaskCard({ task, message, channel, authorName, ownerName }: Omit<MadeTaskProps, 'focus'>) {
  const { lang } = useI18n();
  const props: CreatedTaskCardProps = {
    ref: task.ref,
    title: task.title,
    ...(ownerName === undefined ? {} : { ownerName }),
    ...(task.dueAt === null ? {} : { dueLabel: formatDay(task.dueAt, lang) }),
    sourceLabel: <MessageOrigin message={message} channel={channel} authorName={authorName} />,
    boardHref: '/board',
  };
  return <CreatedTaskCard {...props} />;
}

function MadeTask({ focus, ...card }: MadeTaskProps) {
  const root = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (focus) {
      root.current?.querySelector<HTMLElement>('[role="group"]')?.focus();
    }
  }, [focus]);
  return (
    <div className="msg-b" ref={root}>
      <MadeTaskCard {...card} />
    </div>
  );
}

type MadeTasksProps = Omit<MadeTaskProps, 'task' | 'ownerName' | 'focus'> & {
  ownerNameOf: (userId: string) => string | undefined;
  focusTaskId: string | null;
};

/** The cards of the tasks people made from this message: one under it for each, whoever made it and whenever. */
export function MadeTasks({ message, channel, authorName, ownerNameOf, focusTaskId }: MadeTasksProps) {
  const tasks = useChannelTasks(channel.id).data;
  return tasks
    ?.filter((task) => task.state === 'confirmed' && task.createdByKind === 'user' && task.source?.kind === 'message' && task.source.id === message.id)
    .map((task) => (
      <MadeTask
        key={task.id}
        task={task}
        message={message}
        channel={channel}
        authorName={authorName}
        ownerName={task.assigneeId === null ? undefined : ownerNameOf(task.assigneeId)}
        focus={task.id === focusTaskId}
      />
    ));
}
