import { describe, expect, it } from 'vitest';
import { isOverdue } from './overdue';

// Midnight Asia/Bangkok is 17:00 UTC of the day before: the UTC date of a due date is not its Bangkok date.
const DUE_MON_12 = '2026-10-11T17:00:00.000Z';
const DUE_TUE_13 = '2026-10-12T17:00:00.000Z';

describe('isOverdue', () => {
  it('isOverdue_BangkokDayBoundary', () => {
    // 00:30 on Tue 13 Oct in Bangkok, still Mon 12 Oct in UTC.
    const justAfterMidnight = new Date('2026-10-12T17:30:00.000Z');
    // 23:30 on Tue 13 Oct in Bangkok.
    const lateTuesday = new Date('2026-10-13T16:30:00.000Z');

    // Due today is not overdue, at either end of the day.
    expect(isOverdue({ status: 'todo', dueAt: DUE_TUE_13 }, justAfterMidnight)).toBe(false);
    expect(isOverdue({ status: 'doing', dueAt: DUE_TUE_13 }, lateTuesday)).toBe(false);
    // Due yesterday is, from the first minute of today in Bangkok (the UTC date has not changed yet).
    expect(isOverdue({ status: 'todo', dueAt: DUE_MON_12 }, justAfterMidnight)).toBe(true);
    expect(isOverdue({ status: 'doing', dueAt: DUE_MON_12 }, lateTuesday)).toBe(true);
    // Due tomorrow is not.
    expect(isOverdue({ status: 'todo', dueAt: '2026-10-13T17:00:00.000Z' }, lateTuesday)).toBe(false);
    // A finished task is never overdue, and a task with no due date cannot be.
    expect(isOverdue({ status: 'done', dueAt: DUE_MON_12 }, lateTuesday)).toBe(false);
    expect(isOverdue({ status: 'todo', dueAt: null }, lateTuesday)).toBe(false);
  });
});
