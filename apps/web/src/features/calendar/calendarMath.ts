import type { CalendarEventView } from '../../api/types';

/** Which calendar view is shown. */
export type CalView = 'month' | 'week' | 'day';

/** An event placed on the grid: its parsed local start and end, kept beside the view model it came from. */
export type PlacedEvent = {
  event: CalendarEventView;
  start: Date;
  end: Date;
};

/** Weeks start on Sunday, as most calendars this app's people read do. */
export const WEEK_STARTS_ON = 0;

/** Minutes past midnight for the grid's day; 24 hours of rows. */
export const HOURS = Array.from({ length: 24 }, (_, hour) => hour);

/** Midnight of the day `d` falls in, in local time. */
export function startOfDay(d: Date): Date {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate());
}

export function addDays(d: Date, n: number): Date {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate() + n);
}

/** The first of the month `n` months from `d`. */
export function addMonths(d: Date, n: number): Date {
  return new Date(d.getFullYear(), d.getMonth() + n, 1);
}

export function sameDay(a: Date, b: Date): boolean {
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
}

export function sameMonth(a: Date, b: Date): boolean {
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth();
}

/** The Sunday on or before `d` (midnight). */
export function startOfWeek(d: Date): Date {
  const day = startOfDay(d);
  const back = (day.getDay() - WEEK_STARTS_ON + 7) % 7;
  return addDays(day, -back);
}

/** The 42 days (six weeks) that cover the month `cursor` is in, Sunday-first. */
export function monthGrid(cursor: Date): Date[] {
  const first = new Date(cursor.getFullYear(), cursor.getMonth(), 1);
  const start = startOfWeek(first);
  return Array.from({ length: 42 }, (_, i) => addDays(start, i));
}

/** The seven days of the week `cursor` is in, Sunday-first. */
export function weekDays(cursor: Date): Date[] {
  const start = startOfWeek(cursor);
  return Array.from({ length: 7 }, (_, i) => addDays(start, i));
}

/** Minutes from the day's midnight; clamped to [0, 1440]. */
export function minutesOfDay(d: Date): number {
  return Math.max(0, Math.min(24 * 60, d.getHours() * 60 + d.getMinutes()));
}

/** Parses an event's instants once, defaulting a missing end to 30 minutes after the start. */
export function place(event: CalendarEventView): PlacedEvent {
  const start = new Date(event.startAt);
  const end = event.endAt !== null ? new Date(event.endAt) : new Date(start.getTime() + 30 * 60 * 1000);
  return { event, start, end };
}

/** The events whose local start day is `day`, soonest first. An event is shown on the day it begins. */
export function eventsOn(placed: readonly PlacedEvent[], day: Date): PlacedEvent[] {
  return placed.filter((p) => sameDay(p.start, day)).sort((a, b) => a.start.getTime() - b.start.getTime());
}
