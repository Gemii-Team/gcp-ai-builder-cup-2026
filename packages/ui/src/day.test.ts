import { afterEach, describe, expect, it, vi } from 'vitest';
import { formatDay } from './format';

// The weekday and month abbreviations are the design's, written out here in full and by hand. The runtime's CLDR data is
// not trusted for them: Node 22 (ICU 76, CLDR 46) writes the Thai weekday as `ศ.`, Node 24.21 (ICU 78, CLDR 48) as `ศุกร์`,
// and en-GB writes September as `Sept`. The expectations below hold under every ICU, so a run on any Node proves it.

const WEEK = [
  // 2026-10-11 is a Sunday.
  ['2026-10-11', 'Sun 11 Oct', 'อา. 11 ต.ค.'],
  ['2026-10-12', 'Mon 12 Oct', 'จ. 12 ต.ค.'],
  ['2026-10-13', 'Tue 13 Oct', 'อ. 13 ต.ค.'],
  ['2026-10-14', 'Wed 14 Oct', 'พ. 14 ต.ค.'],
  ['2026-10-15', 'Thu 15 Oct', 'พฤ. 15 ต.ค.'],
  ['2026-10-16', 'Fri 16 Oct', 'ศ. 16 ต.ค.'],
  ['2026-10-17', 'Sat 17 Oct', 'ส. 17 ต.ค.'],
] as const;

// The 15th of every month of 2026, with the weekday it falls on.
const YEAR = [
  ['2026-01-15', 'Thu 15 Jan', 'พฤ. 15 ม.ค.'],
  ['2026-02-15', 'Sun 15 Feb', 'อา. 15 ก.พ.'],
  ['2026-03-15', 'Sun 15 Mar', 'อา. 15 มี.ค.'],
  ['2026-04-15', 'Wed 15 Apr', 'พ. 15 เม.ย.'],
  ['2026-05-15', 'Fri 15 May', 'ศ. 15 พ.ค.'],
  ['2026-06-15', 'Mon 15 Jun', 'จ. 15 มิ.ย.'],
  ['2026-07-15', 'Wed 15 Jul', 'พ. 15 ก.ค.'],
  ['2026-08-15', 'Sat 15 Aug', 'ส. 15 ส.ค.'],
  ['2026-09-15', 'Tue 15 Sep', 'อ. 15 ก.ย.'],
  ['2026-10-15', 'Thu 15 Oct', 'พฤ. 15 ต.ค.'],
  ['2026-11-15', 'Sun 15 Nov', 'อา. 15 พ.ย.'],
  ['2026-12-15', 'Tue 15 Dec', 'อ. 15 ธ.ค.'],
] as const;

describe('formatDay: weekday and month abbreviations come from tables, not from the runtime', () => {
  it.each(WEEK)('writes every weekday: %s is %s in English and %s in Thai', (day, en, th) => {
    expect(formatDay(day, 'en')).toBe(en);
    expect(formatDay(day, 'th')).toBe(th);
  });

  it.each(YEAR)('writes every month: %s is %s in English and %s in Thai (September is Sep, never Sept)', (day, en, th) => {
    expect(formatDay(day, 'en')).toBe(en);
    expect(formatDay(day, 'th')).toBe(th);
  });

  it('reads the Bangkok calendar day of an instant: 2026-10-16T17:00:00Z is already Saturday the 17th there', () => {
    expect(formatDay('2026-10-16T16:59:59Z', 'en')).toBe('Fri 16 Oct');
    expect(formatDay('2026-10-16T16:59:59Z', 'th')).toBe('ศ. 16 ต.ค.');
    expect(formatDay('2026-10-16T17:00:00Z', 'en')).toBe('Sat 17 Oct');
    expect(formatDay('2026-10-16T17:00:00Z', 'th')).toBe('ส. 17 ต.ค.');
    expect(formatDay('2026-10-16T17:30:00Z', 'en')).toBe('Sat 17 Oct');
    expect(formatDay(new Date('2026-10-16T17:30:00Z'), 'th')).toBe('ส. 17 ต.ค.');
  });

  it('crosses a month, a year and a leap day on the weekday that follows', () => {
    expect(formatDay('2026-12-31T16:59:59Z', 'en')).toBe('Thu 31 Dec');
    expect(formatDay('2026-12-31T17:00:00Z', 'en')).toBe('Fri 1 Jan');
    expect(formatDay('2026-12-31T17:00:00Z', 'th')).toBe('ศ. 1 ม.ค.');
    expect(formatDay('2028-02-28T17:00:00Z', 'en')).toBe('Tue 29 Feb');
    expect(formatDay('2028-02-28T17:00:00Z', 'th')).toBe('อ. 29 ก.พ.');
    expect(formatDay('2028-02-29T17:00:00Z', 'en')).toBe('Wed 1 Mar');
    expect(formatDay('2028-02-29T17:00:00Z', 'th')).toBe('พ. 1 มี.ค.');
  });

  it('reads an instant in the zone it is given, and a date-only string in none', () => {
    expect(formatDay('2026-10-16T06:00:00Z', 'en', 'America/Los_Angeles')).toBe('Thu 15 Oct');
    expect(formatDay('2026-10-16T06:00:00Z', 'th', 'America/Los_Angeles')).toBe('พฤ. 15 ต.ค.');
    expect(formatDay('2026-10-16', 'th', 'Pacific/Kiritimati')).toBe('ศ. 16 ต.ค.');
  });

  describe('under a runtime whose CLDR names every weekday and month another way', () => {
    afterEach(() => {
      vi.restoreAllMocks();
    });

    // What a newer (or older) ICU does to the names `Intl` writes, simulated: every name part is replaced, numbers are left.
    // A label built from such a name changes with the browser; one built from the tables cannot.
    function otherCldr() {
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
    }

    it.each([
      ['2026-10-16', 'en', 'Fri 16 Oct'],
      ['2026-10-16', 'th', 'ศ. 16 ต.ค.'],
      ['2026-09-01', 'en', 'Tue 1 Sep'],
      ['2026-09-01', 'th', 'อ. 1 ก.ย.'],
      ['2026-10-16T17:30:00Z', 'en', 'Sat 17 Oct'],
      ['2026-10-16T17:30:00Z', 'th', 'ส. 17 ต.ค.'],
    ] as const)('writes %s in %s as %s all the same', (day, lang, expected) => {
      otherCldr();

      expect(formatDay(day, lang)).toBe(expected);
    });
  });
});
