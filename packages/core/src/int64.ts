const MAX = BigInt(Number.MAX_SAFE_INTEGER);
const MIN = BigInt(Number.MIN_SAFE_INTEGER);

/**
 * The boundary between the generated API types, which carry int64 fields (`seq`, `offset_ms`, `size`) as `bigint`, and
 * the code that compares and displays them as `number`. A value a `number` cannot hold exactly is an error, never a
 * silent rounding.
 *
 * @throws RangeError outside Number.MIN_SAFE_INTEGER..Number.MAX_SAFE_INTEGER, or for a non-integer `number`.
 */
export function toSafeNumber(value: bigint | number): number {
  if (typeof value === 'bigint') {
    if (value > MAX || value < MIN) {
      throw new RangeError(`${String(value)} is outside the safe integer range`);
    }
    return Number(value);
  }
  if (!Number.isSafeInteger(value)) {
    throw new RangeError(`${String(value)} is not a safe integer`);
  }
  return value;
}
