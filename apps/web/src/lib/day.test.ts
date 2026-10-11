import { describe, expect, it } from 'vitest';
import { dayKey, dayLabel } from './day';

describe('dayKey', () => {
  it('is the Bangkok calendar day, so a message at 01:30 there is on the new day while it is still the old one in UTC', () => {
    expect(dayKey('2026-10-12T18:30:00Z')).toBe('2026-10-13');
    expect(dayKey('2026-10-13T16:59:00Z')).toBe('2026-10-13');
    expect(dayKey('2026-10-13T17:00:00Z')).toBe('2026-10-14');
  });
});

describe('dayLabel', () => {
  const now = new Date('2026-10-13T08:00:00Z');

  it('writes the weekday, the day, the month and the year in English', () => {
    expect(dayLabel('2026-10-13T07:02:00Z', 'en', now)).toEqual({ text: 'Tue, 13 Oct 2026', today: true });
  });

  it('says whether the day is the current one in Bangkok, and only then', () => {
    expect(dayLabel('2026-10-12T07:02:00Z', 'en', now).today).toBe(false);
    expect(dayLabel('2026-10-13T20:00:00Z', 'en', now).today).toBe(false);
  });

  it('writes Thai in the Gregorian year', () => {
    const { text } = dayLabel('2026-10-13T07:02:00Z', 'th', now);

    expect(text).toContain('13');
    expect(text).toContain('2026');
    expect(text).toMatch(/[\u0E00-\u0E7F]/);
  });
});
