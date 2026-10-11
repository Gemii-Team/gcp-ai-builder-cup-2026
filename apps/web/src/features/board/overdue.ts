import { dueAtToDate } from '@kiwi/ui';
import type { TaskView } from '../../api/types';

/**
 * Whether a task is late: it is not done and its due date lies before today. Both days are Asia/Bangkok calendar days
 * (`YYYY-MM-DD`, which compare as text), so a task due today is not late for the whole of today, whatever the UTC date is.
 */
export function isOverdue(task: Pick<TaskView, 'status' | 'dueAt'>, now: Date): boolean {
  if (task.status === 'done' || task.dueAt === null) {
    return false;
  }
  return dueAtToDate(new Date(task.dueAt)) < dueAtToDate(now);
}
