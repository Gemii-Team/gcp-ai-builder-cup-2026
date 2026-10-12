import { afterEach, describe, expect, it, vi } from 'vitest';
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

  it('writes Thai in the Gregorian year, with the design\'s abbreviated weekday and month', () => {
    expect(dayLabel('2026-10-13T07:02:00Z', 'th', now).text).toBe('อ., 13 ต.ค. 2026');
  });

  // The weekday and month abbreviations are tables, not what the runtime's CLDR writes: Node 24.21 (CLDR 48) writes the Thai
  // weekday in full (`อังคาร`), and en-GB writes September as `Sept` on every Node. These hold under every ICU.
  it.each([
    ['2026-10-11T05:00:00Z', 'Sun, 11 Oct 2026', 'อา., 11 ต.ค. 2026'],
    ['2026-10-12T05:00:00Z', 'Mon, 12 Oct 2026', 'จ., 12 ต.ค. 2026'],
    ['2026-10-13T05:00:00Z', 'Tue, 13 Oct 2026', 'อ., 13 ต.ค. 2026'],
    ['2026-10-14T05:00:00Z', 'Wed, 14 Oct 2026', 'พ., 14 ต.ค. 2026'],
    ['2026-10-15T05:00:00Z', 'Thu, 15 Oct 2026', 'พฤ., 15 ต.ค. 2026'],
    ['2026-10-16T05:00:00Z', 'Fri, 16 Oct 2026', 'ศ., 16 ต.ค. 2026'],
    ['2026-10-17T05:00:00Z', 'Sat, 17 Oct 2026', 'ส., 17 ต.ค. 2026'],
    ['2026-01-15T05:00:00Z', 'Thu, 15 Jan 2026', 'พฤ., 15 ม.ค. 2026'],
    ['2026-02-15T05:00:00Z', 'Sun, 15 Feb 2026', 'อา., 15 ก.พ. 2026'],
    ['2026-03-15T05:00:00Z', 'Sun, 15 Mar 2026', 'อา., 15 มี.ค. 2026'],
    ['2026-04-15T05:00:00Z', 'Wed, 15 Apr 2026', 'พ., 15 เม.ย. 2026'],
    ['2026-05-15T05:00:00Z', 'Fri, 15 May 2026', 'ศ., 15 พ.ค. 2026'],
    ['2026-06-15T05:00:00Z', 'Mon, 15 Jun 2026', 'จ., 15 มิ.ย. 2026'],
    ['2026-07-15T05:00:00Z', 'Wed, 15 Jul 2026', 'พ., 15 ก.ค. 2026'],
    ['2026-08-15T05:00:00Z', 'Sat, 15 Aug 2026', 'ส., 15 ส.ค. 2026'],
    ['2026-09-15T05:00:00Z', 'Tue, 15 Sep 2026', 'อ., 15 ก.ย. 2026'],
    ['2026-11-15T05:00:00Z', 'Sun, 15 Nov 2026', 'อา., 15 พ.ย. 2026'],
    ['2026-12-15T05:00:00Z', 'Tue, 15 Dec 2026', 'อ., 15 ธ.ค. 2026'],
  ])('writes every weekday and month: %s is %s in English and %s in Thai', (iso, en, th) => {
    expect(dayLabel(iso, 'en', now).text).toBe(en);
    expect(dayLabel(iso, 'th', now).text).toBe(th);
  });

  it('is the Bangkok day, so 17:00Z is already the next day there, across a month, a year and a leap day', () => {
    expect(dayLabel('2026-10-16T16:59:59Z', 'en', now).text).toBe('Fri, 16 Oct 2026');
    expect(dayLabel('2026-10-16T17:00:00Z', 'en', now).text).toBe('Sat, 17 Oct 2026');
    expect(dayLabel('2026-10-16T17:30:00Z', 'th', now).text).toBe('ส., 17 ต.ค. 2026');
    expect(dayLabel('2026-08-31T17:30:00Z', 'en', now).text).toBe('Tue, 1 Sep 2026');
    expect(dayLabel('2026-12-31T16:59:59Z', 'th', now).text).toBe('พฤ., 31 ธ.ค. 2026');
    expect(dayLabel('2026-12-31T17:00:00Z', 'en', now).text).toBe('Fri, 1 Jan 2027');
    expect(dayLabel('2026-12-31T17:00:00Z', 'th', now).text).toBe('ศ., 1 ม.ค. 2027');
    expect(dayLabel('2028-02-28T17:00:00Z', 'en', now).text).toBe('Tue, 29 Feb 2028');
    expect(dayLabel('2028-02-29T17:00:00Z', 'th', now).text).toBe('พ., 1 มี.ค. 2028');
  });

  describe('under a runtime whose CLDR names every weekday and month another way', () => {
    afterEach(() => {
      vi.restoreAllMocks();
    });

    it.each([
      ['en', 'Tue, 1 Sep 2026'],
      ['th', 'อ., 1 ก.ย. 2026'],
    ] as const)('writes the %s label as %s all the same', (lang, expected) => {
      // What a newer (or older) ICU does to the names `Intl` writes, simulated: every name part is replaced, numbers are left.
      const formatToParts = Object.getOwnPropertyDescriptor(Intl.DateTimeFormat.prototype, 'formatToParts')?.value as (
        this: Intl.DateTimeFormat,
        date?: Date | number,
      ) => Intl.DateTimeFormatPart[];
      vi.spyOn(Intl.DateTimeFormat.prototype, 'formatToParts').mockImplementation(function (this: Intl.DateTimeFormat, date?: Date | number) {
        return formatToParts.call(this, date).map((part) => {
          if (part.type === 'weekday') {
            return { ...part, value: 'WEEKDAY-OF-ANOTHER-CLDR' };
          }
          if (part.type === 'month' && !/^\d+$/.test(part.value)) {
            return { ...part, value: 'MONTH-OF-ANOTHER-CLDR' };
          }
          return part;
        });
      });

      expect(dayLabel('2026-09-01T05:00:00Z', lang, now).text).toBe(expected);
    });
  });
});
