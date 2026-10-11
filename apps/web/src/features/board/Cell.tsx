import type { ReactNode } from 'react';
import type { TaskView } from '../../api/types';
import { T, useI18n } from '../../i18n';
import { STATUS_KEY } from './useBoardData';
import type { Status } from './useBoardData';

const EMPTY = { todo: 'cellEmptyTodo', doing: 'cellEmptyDoing', done: 'cellEmptyDone' } as const;

type CellProps = {
  status: Status;
  /** The lane's title as text, which names the cell: `To do in Q4 launch weekly sync`. */
  laneTitle: string;
  tasks: readonly TaskView[];
  renderCard: (task: TaskView) => ReactNode;
};

/** One status of one lane: its cards 12px apart, or the dashed empty-cell outline saying what is missing. */
export function Cell({ status, laneTitle, tasks, renderCard }: CellProps) {
  const { plain } = useI18n();
  return (
    <div className="bd-cell" data-status={status} role="group" aria-label={plain('cellLabel', { status: plain(STATUS_KEY[status]), lane: laneTitle })}>
      {tasks.length > 0 ? (
        <ul>{tasks.map((task) => renderCard(task))}</ul>
      ) : (
        <p className="bd-cell-empty">
          <T k={EMPTY[status]} />
        </p>
      )}
    </div>
  );
}
