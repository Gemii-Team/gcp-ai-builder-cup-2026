import {
  Button,
  ExcerptRow,
  formatDay,
  formatOffset,
  Icon,
  LangRuns,
  Menu,
  MenuRadioItem,
  MessageLink,
  OffsetLink,
  PageLink,
  RefCode,
  SealedSlip,
  Slip,
  SlipBody,
  Stub,
  usePrefersReducedMotion,
} from '@kiwi/ui';
import { useEffect, useEffectEvent, useRef, useState, type ReactNode } from 'react';
import type { TaskView } from '../../api/types';
import { T, TSlots, useI18n } from '../../i18n';
import { isOverdue } from './overdue';
import { useMoveTask } from './useMoveTask';
import { STATUS_KEY, STATUSES, type LaneKind, type Status } from './useBoardData';
import { DoneWhen, When } from './When';

const SWATCH: Record<Status, string> = { todo: 'bg-perforation-sage', doing: 'bg-doing-teal', done: 'bg-seal-green' };

/** The seal stamps down over 280ms, 120ms late: the same motion as the drafts panel's confirmation. */
const STAMP = [
  { opacity: 0, transform: 'rotate(-18deg) scale(1.5)' },
  { opacity: 1, transform: 'rotate(-8deg) scale(1)' },
];

/** Who confirmed a draft and when, for the card that lands in To do and stays sealed until it first moves. */
export type Landing = { confirmedBy: string; confirmedAt: string };

export type TaskCardProps = {
  task: TaskView;
  laneKind: LaneKind;
  /** The task's channel, without `#`. */
  channelName: string;
  ownerName: string | undefined;
  /** Who made the task by hand. */
  creatorName: string | undefined;
  now: Date;
  /** Set for the card a person just confirmed and has not moved: it is drawn as the sealed slip. */
  landing: Landing | undefined;
  /** Take focus on the Move button when the card appears (after a move or after New task). */
  focus: boolean;
  onFocused: () => void;
  /** A move has begun: the Board settles a sealed card, announces it and looks after focus. */
  onMoved: (task: TaskView, to: Status) => void;
  onMoveFailed: (task: TaskView) => void;
};

function Meta({ task, laneKind, channelName, ownerName, now }: Pick<TaskCardProps, 'task' | 'laneKind' | 'channelName' | 'ownerName' | 'now'>) {
  const { lang } = useI18n();
  const { dueAt, doneAt } = task;
  let tail: ReactNode;
  if (task.status === 'done') {
    tail = (
      <span className="bd-okl">
        <Icon name="check" size={16} />
        {doneAt === null ? <T k="statusDone" /> : <TSlots k="taskDoneAt" slots={{ when: <DoneWhen iso={doneAt} now={now} /> }} />}
      </span>
    );
  } else if (dueAt !== null && isOverdue(task, now)) {
    // The Overdue Has No Colour Rule: words, a clock icon and weight, never a state colour.
    tail = (
      <span className="bd-late">
        <Icon name="clock" size={16} />
        <T k="taskOverdue" params={{ day: formatDay(dueAt, lang) }} />
      </span>
    );
  } else if (dueAt !== null) {
    tail = (
      <span className="bd-due">
        <LangRuns text={formatDay(dueAt, lang)} />
      </span>
    );
  } else {
    tail = <T k="taskNoDueDate" />;
  }
  // The Added by hand lane has no channel of its own, so its cards name theirs first. A confirmed task with no owner is
  // a settled fact, said in the meta's own voice: never amber (The No Owner Is Neutral Rule).
  const owner = task.assigneeId === null ? <T k="taskNoOwner" /> : ownerName === undefined ? null : <LangRuns text={ownerName} />;
  const parts = [laneKind === 'hand' ? <LangRuns key="channel" text={`#${channelName}`} /> : null, owner, tail].filter((part) => part !== null);
  return (
    <p className="kiwi-d-meta">
      {parts.map((part, index) => (
        <span key={index}>
          {index > 0 && ' · '}
          {part}
        </span>
      ))}
    </p>
  );
}

/** The right side of the stub: where the task came from, with the link that opens it in place. */
function Origin({ task, channelName, creatorName, now }: Pick<TaskCardProps, 'task' | 'channelName' | 'creatorName' | 'now'>) {
  const { source } = task;
  if (source === null) {
    // Added by hand: plain text, no link and no excerpt, because nothing stands behind it.
    const when = <When iso={task.createdAt} now={now} />;
    return creatorName === undefined ? (
      <TSlots k="taskAddedByUnknown" slots={{ when }} />
    ) : (
      <TSlots k="taskAddedBy" slots={{ name: <LangRuns text={creatorName} />, when }} />
    );
  }
  switch (source.kind) {
    case 'meeting':
      return (
        <>
          <T k="taskFromMeeting" />
          {formatOffset(source.offsetMs) !== null && (
            <>
              {' · '}
              <OffsetLink source={source} />
            </>
          )}
        </>
      );
    case 'file':
      return (
        <>
          <LangRuns text={source.label} />
          {source.page !== undefined && (
            <>
              {' · '}
              <PageLink source={source} />
            </>
          )}
        </>
      );
    case 'message': {
      const slots = { channel: <MessageLink source={source} channel={channelName} />, author: <LangRuns text={source.label} /> };
      // A message without a known time reads without one, never with a wrong one.
      return source.at === undefined ? (
        <TSlots k="taskFromMessageUntimed" slots={slots} />
      ) : (
        <TSlots k="taskFromMessage" slots={{ ...slots, when: <When iso={source.at} now={now} /> }} />
      );
    }
  }
}

type MoveControlProps = Pick<TaskCardProps, 'task' | 'focus' | 'onFocused'> & { onMove: (to: Status) => void };

/** The small secondary Move button and the three-row radio menu it opens: the current status is checked. */
function MoveControl({ task, focus, onFocused, onMove }: MoveControlProps) {
  const { plain } = useI18n();
  const button = useRef<HTMLButtonElement>(null);
  const [open, setOpen] = useState(false);

  useEffect(() => {
    if (focus) {
      button.current?.focus();
      onFocused();
    }
  }, [focus, onFocused]);

  return (
    <>
      <Button
        ref={button}
        size="sm"
        className="bd-mv"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label={plain('taskMoveNamed', { code: task.ref, title: task.title })}
        onClick={() => {
          setOpen((was) => !was);
        }}
        onKeyDown={(event) => {
          if (event.key === 'ArrowDown' && !open) {
            event.preventDefault();
            setOpen(true);
          }
        }}
      >
        <T k="taskMove" />
        <Icon name="chevron-down" size={16} />
      </Button>
      <Menu
        label={plain('taskMoveMenu', { code: task.ref })}
        open={open}
        onClose={() => {
          setOpen(false);
        }}
        anchorRef={button}
      >
        {STATUSES.map((status) => (
          <MenuRadioItem
            key={status}
            checked={task.status === status}
            swatch={SWATCH[status]}
            onSelect={() => {
              onMove(status);
            }}
          >
            <T k={STATUS_KEY[status]} />
          </MenuRadioItem>
        ))}
      </Menu>
    </>
  );
}

/**
 * A confirmed task. A plain slip: no perforation, torn edge or shadow. The title, `owner · due` meta, the REF and the
 * source (each of meeting, message and document opens its excerpt in place, one at a time), then the Move button. The card
 * a person has just confirmed is the sealed slip instead, torn and shadowed, until the first time it moves.
 */
export function TaskCard({ task, laneKind, channelName, ownerName, creatorName, now, landing, focus, onFocused, onMoved, onMoveFailed }: TaskCardProps) {
  const { lang } = useI18n();
  const move = useMoveTask(task.id);
  const item = useRef<HTMLLIElement>(null);
  const reduced = usePrefersReducedMotion();
  const landed = landing !== undefined;

  // The sealed card lands in view, and its seal stamps down, as the drafts panel's does.
  const land = useEffectEvent(() => {
    const element = item.current;
    element?.scrollIntoView?.({ block: 'nearest', behavior: 'auto' });
    if (!reduced && typeof element?.animate === 'function') {
      element.querySelector('.kiwi-seal')?.animate(STAMP, { duration: 280, delay: 120, easing: 'cubic-bezier(.16,1,.3,1)', fill: 'backwards' });
    }
  });
  useEffect(() => {
    if (landed) {
      land();
    }
  }, [landed]);

  const control = (
    <MoveControl
      task={task}
      focus={focus}
      onFocused={onFocused}
      onMove={(to) => {
        if (to === task.status) {
          return;
        }
        onMoved(task, to);
        move(to).catch(() => {
          onMoveFailed(task);
        });
      }}
    />
  );

  if (landing !== undefined && task.source !== null) {
    const sealed = {
      ref: task.ref,
      title: task.title,
      source: task.source,
      confirmedBy: landing.confirmedBy,
      confirmedAt: landing.confirmedAt,
      ...(ownerName === undefined ? {} : { ownerName }),
      ...(task.dueAt === null ? {} : { dueLabel: formatDay(task.dueAt, lang) }),
    };
    return (
      <li ref={item} className="bd-card" data-task-id={task.id} data-glide="">
        <SealedSlip {...sealed} actions={<div className="kiwi-acts">{control}</div>} />
      </li>
    );
  }

  return (
    <li ref={item} className="bd-card" data-task-id={task.id} data-glide="">
      <Slip className="bd-tcard">
        <SlipBody>
          <h3 className="kiwi-d-title">
            <LangRuns text={task.title} />
          </h3>
          <Meta task={task} laneKind={laneKind} channelName={channelName} ownerName={ownerName} now={now} />
        </SlipBody>
        <Stub className="bd-tcard-stub">
          <ExcerptRow className="kiwi-row">
            <RefCode code={task.ref} />
            <span className="bd-src">
              <Origin task={task} channelName={channelName} creatorName={creatorName} now={now} />
            </span>
          </ExcerptRow>
          <div className="kiwi-acts">{control}</div>
        </Stub>
      </Slip>
    </li>
  );
}
