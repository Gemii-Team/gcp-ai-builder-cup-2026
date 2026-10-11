package tasks

import (
	"fmt"
	"strings"

	"kiwi/internal/platform/errs"
)

// digits are the base-62 digits of a position, in ascending byte order: the order
// the database sorts the "C"-collated position column by.
const digits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// smallestInteger is the one integer part no key may be, because nothing sorts
// before it.
var smallestInteger = "A" + strings.Repeat(digits[:1], 26)

// PositionBetween returns a position that sorts strictly between a and b by byte
// order, so a card can be placed between two others by writing only its own row.
// An empty a or b is an open end: PositionBetween("", "") is the first key of an
// empty lane, PositionBetween(last, "") appends and PositionBetween("", first)
// prepends.
//
// A position is a base-62 integer part, whose first character says how long it is
// ("a" is two characters, "b" three, ...; "Z" is two, "Y" three, ...), followed by
// a fraction that never ends in a zero. Appends and prepends step the integer part,
// so a lane filled from either end keeps short keys; a card dropped between two
// others takes the midpoint of their fractions. This is the fractional indexing of
// David Greenspan, kept compatible with the "fractional-indexing" packages.
//
// It panics when a or b is not a position it returned or when a does not sort
// before b: callers read both from the database, and positionBetween reports
// the same conditions as errors.
func PositionBetween(a, b string) string {
	key, err := positionBetween(a, b)
	if err != nil {
		panic("tasks: PositionBetween: " + err.Error())
	}
	return key
}

// positionBetween is PositionBetween returning errors: a key that is not valid, or
// a that does not sort before b, is errs.ErrInvalid.
func positionBetween(a, b string) (string, error) {
	for _, k := range []string{a, b} {
		if k != "" {
			if err := validatePosition(k); err != nil {
				return "", err
			}
		}
	}
	if a != "" && b != "" && a >= b {
		return "", fmt.Errorf("position %q does not sort before %q: %w", a, b, errs.ErrInvalid)
	}

	switch {
	case a == "" && b == "":
		return "a" + digits[:1], nil
	case a == "":
		ib, fb := splitPosition(b)
		if ib == smallestInteger {
			return ib + midpoint("", fb), nil
		}
		if ib < b {
			return ib, nil
		}
		prev, ok := decrementInteger(ib)
		if !ok {
			return "", fmt.Errorf("no position sorts before %q", b)
		}
		return prev, nil
	case b == "":
		ia, fa := splitPosition(a)
		if next, ok := incrementInteger(ia); ok {
			return next, nil
		}
		return ia + midpoint(fa, ""), nil
	}

	ia, fa := splitPosition(a)
	ib, fb := splitPosition(b)
	if ia == ib {
		return ia + midpoint(fa, fb), nil
	}
	next, ok := incrementInteger(ia)
	if !ok {
		return "", fmt.Errorf("no position sorts after %q", a)
	}
	if next < b {
		return next, nil
	}
	return ia + midpoint(fa, ""), nil
}

// midpoint returns a fraction (digits, no trailing zero) strictly between the
// fractions a and b. An empty a is 0 and an empty b is 1; a sorts before b.
func midpoint(a, b string) string {
	if b != "" {
		// Keep the digits both fractions share and take the midpoint of the rest.
		n := 0
		for n < len(b) && digitAt(a, n) == b[n] {
			n++
		}
		if n > 0 {
			return b[:n] + midpoint(tail(a, n), b[n:])
		}
	}

	lo, hi := 0, len(digits)
	if a != "" {
		lo = strings.IndexByte(digits, a[0])
	}
	if b != "" {
		hi = strings.IndexByte(digits, b[0])
	}
	if hi-lo > 1 {
		return string(digits[(lo+hi+1)/2]) // rounds half up
	}
	// The first digits are neighbours.
	if len(b) > 1 {
		return b[:1]
	}
	return string(digits[lo]) + midpoint(tail(a, 1), "")
}

// digitAt is the n-th digit of fraction s, which reads as zeros past its end.
func digitAt(s string, n int) byte {
	if n < len(s) {
		return s[n]
	}
	return digits[0]
}

// tail is s without its first n bytes, empty when s is shorter.
func tail(s string, n int) string {
	if n >= len(s) {
		return ""
	}
	return s[n:]
}

// integerLength is the length of the integer part a key starting with head has.
func integerLength(head byte) (int, bool) {
	switch {
	case head >= 'a' && head <= 'z':
		return int(head-'a') + 2, true
	case head >= 'A' && head <= 'Z':
		return int('Z'-head) + 2, true
	}
	return 0, false
}

// splitPosition cuts a valid key into its integer part and its fraction.
func splitPosition(key string) (integer, fraction string) {
	n, _ := integerLength(key[0])
	return key[:n], key[n:]
}

func validatePosition(key string) error {
	n, ok := integerLength(key[0])
	switch {
	case !ok || n > len(key):
		return fmt.Errorf("position %q has no integer part: %w", key, errs.ErrInvalid)
	case key == smallestInteger:
		return fmt.Errorf("position %q is the one integer nothing sorts before: %w", key, errs.ErrInvalid)
	case strings.ContainsFunc(key, func(r rune) bool { return !strings.ContainsRune(digits, r) }):
		return fmt.Errorf("position %q has a character that is not a base-62 digit: %w", key, errs.ErrInvalid)
	case len(key) > n && key[len(key)-1] == digits[0]:
		return fmt.Errorf("position %q ends in a zero: %w", key, errs.ErrInvalid)
	}
	return nil
}

// incrementInteger returns the integer part after x, or false when x is the last.
func incrementInteger(x string) (string, bool) {
	head, digs := x[0], []byte(x[1:])
	carry := true
	for i := len(digs) - 1; carry && i >= 0; i-- {
		d := strings.IndexByte(digits, digs[i]) + 1
		if d == len(digits) {
			digs[i] = digits[0]
		} else {
			digs[i] = digits[d]
			carry = false
		}
	}
	if !carry {
		return string(head) + string(digs), true
	}
	switch head {
	case 'Z':
		return "a" + digits[:1], true
	case 'z':
		return "", false
	}
	head++
	if head > 'a' {
		digs = append(digs, digits[0])
	} else {
		digs = digs[:len(digs)-1]
	}
	return string(head) + string(digs), true
}

// decrementInteger returns the integer part before x, or false when x is the first.
func decrementInteger(x string) (string, bool) {
	last := digits[len(digits)-1]
	head, digs := x[0], []byte(x[1:])
	borrow := true
	for i := len(digs) - 1; borrow && i >= 0; i-- {
		d := strings.IndexByte(digits, digs[i]) - 1
		if d == -1 {
			digs[i] = last
		} else {
			digs[i] = digits[d]
			borrow = false
		}
	}
	if !borrow {
		return string(head) + string(digs), true
	}
	switch head {
	case 'a':
		return "Z" + string(last), true
	case 'A':
		return "", false
	}
	head--
	if head < 'Z' {
		digs = append(digs, last)
	} else {
		digs = digs[:len(digs)-1]
	}
	return string(head) + string(digs), true
}
