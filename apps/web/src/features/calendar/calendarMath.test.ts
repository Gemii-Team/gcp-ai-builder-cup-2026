import { describe, expect, it } from 'vitest';
import type { CalendarEventView } from '../../api/types';
import { addDays, addMonths, eventsOn, monthGrid, place, sameDay, startOfWeek, weekDays } from './calendarMath';

function event(id: string, startAt: string, orgWide = false): CalendarEventView {
  return { id, channelId: 'c', ref: `EVT-${id}`, title: id, startAt, endAt: null, meetUri: '', htmlLink: '', status: 'scheduled', orgWide };
}

describe('calendarMath', () => {
  it('monthGrid covers the month in six Sunday-first weeks', () => {
    const grid = monthGrid(new Date(2026, 9, 15)); // October 2026
    expect(grid).toHaveLength(42);
    expect(grid[0].getDay()).toBe(0); // starts on a Sunday
    // October 2026 starts on a Thursday, so the grid starts on Sun Sep 27.
    expect(sameDay(grid[0], new Date(2026, 8, 27))).toBe(true);
    expect(grid.some((day) => sameDay(day, new Date(2026, 9, 1)))).toBe(true);
    expect(grid.some((day) => sameDay(day, new Date(2026, 9, 31)))).toBe(true);
  });

  it('weekDays returns the seven days of the cursor week, Sunday-first', () => {
    const days = weekDays(new Date(2026, 9, 14)); // a Wednesday
    expect(days).toHaveLength(7);
    expect(sameDay(days[0], startOfWeek(new Date(2026, 9, 14)))).toBe(true);
    expect(days[0].getDay()).toBe(0);
    expect(days[6].getDay()).toBe(6);
  });

  it('addMonths lands on the first of the target month and addDays steps by day', () => {
    expect(sameDay(addMonths(new Date(2026, 9, 20), 1), new Date(2026, 10, 1))).toBe(true);
    expect(sameDay(addDays(new Date(2026, 9, 31), 1), new Date(2026, 10, 1))).toBe(true);
  });

  it('place defaults a missing end to 30 minutes after the start', () => {
    const p = place(event('a', '2026-10-14T09:00:00.000Z'));
    expect(p.end.getTime() - p.start.getTime()).toBe(30 * 60 * 1000);
  });

  it('eventsOn keeps a day’s events, soonest first', () => {
    const a = place(event('a', new Date(2026, 9, 14, 15, 0).toISOString()));
    const b = place(event('b', new Date(2026, 9, 14, 9, 0).toISOString()));
    const other = place(event('c', new Date(2026, 9, 15, 9, 0).toISOString()));
    const got = eventsOn([a, b, other], new Date(2026, 9, 14));
    expect(got.map((p) => p.event.id)).toEqual(['b', 'a']);
  });
});
