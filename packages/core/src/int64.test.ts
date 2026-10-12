import { describe, expect, it } from 'vitest';
import { toSafeNumber } from './int64';

describe('toSafeNumber', () => {
  it('converts a bigint inside the safe-integer range', () => {
    expect(toSafeNumber(0n)).toBe(0);
    expect(toSafeNumber(1234n)).toBe(1234);
    expect(toSafeNumber(-42n)).toBe(-42);
    expect(toSafeNumber(BigInt(Number.MAX_SAFE_INTEGER))).toBe(Number.MAX_SAFE_INTEGER);
    expect(toSafeNumber(BigInt(Number.MIN_SAFE_INTEGER))).toBe(Number.MIN_SAFE_INTEGER);
  });

  it('passes a safe integer number through', () => {
    expect(toSafeNumber(7)).toBe(7);
    expect(toSafeNumber(Number.MAX_SAFE_INTEGER)).toBe(Number.MAX_SAFE_INTEGER);
  });

  it('throws RangeError for a bigint outside the safe-integer range, instead of rounding it', () => {
    expect(() => toSafeNumber(BigInt(Number.MAX_SAFE_INTEGER) + 1n)).toThrow(RangeError);
    expect(() => toSafeNumber(BigInt(Number.MIN_SAFE_INTEGER) - 1n)).toThrow(RangeError);
    expect(() => toSafeNumber(9223372036854775807n)).toThrow(RangeError);
  });

  it('throws RangeError for a number that is not a safe integer', () => {
    expect(() => toSafeNumber(Number.MAX_SAFE_INTEGER + 1)).toThrow(RangeError);
    expect(() => toSafeNumber(1.5)).toThrow(RangeError);
    expect(() => toSafeNumber(Number.NaN)).toThrow(RangeError);
    expect(() => toSafeNumber(Number.POSITIVE_INFINITY)).toThrow(RangeError);
  });
});
