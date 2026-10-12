package meetings

import (
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"golang.org/x/text/unicode/norm"

	chatv1 "kiwi/gen/kiwi/chat/v1"
)

// ResolveOwner finds the member of members a hint such as "Somchai" or "สมชาย ใจดี"
// names. The model only extracts what the transcript says; this is the code that decides
// who that is, and it decides only when there is no doubt: a hint that matches no member,
// or more than one (two people called Somchai), gives nil, and the task is left without
// an owner for a person to fill in.
//
// A member matches when every name token of the hint, folded to lower case and without
// Latin accents, is one of the member's own name tokens. Thai marks are letters of a Thai
// name and are kept. members must be the members of the one channel the task goes to, so
// no one outside it can be named.
func ResolveOwner(hint string, members []*chatv1.Member) (userID *uuid.UUID) {
	want := nameTokens(hint)
	if len(want) == 0 {
		return nil
	}
	var found *uuid.UUID
	for _, m := range members {
		id, err := uuid.Parse(m.GetUserId())
		if err != nil || !containsAll(nameTokens(m.GetName()), want) {
			continue
		}
		if found != nil && *found != id {
			return nil
		}
		found = &id
	}
	return found
}

// containsAll reports whether every token of want is in have.
func containsAll(have, want []string) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			if h == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// nameTokens splits a name into its folded words. Letters, digits and combining marks
// make a word; anything else separates two. Folding is lower case and, for scripts other
// than Thai, the removal of accents (José is jose); a Thai vowel or tone mark changes the
// name and stays.
func nameTokens(name string) []string {
	folded := strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Mn, r) && !unicode.Is(unicode.Thai, r) {
			return -1
		}
		return unicode.ToLower(r)
	}, norm.NFD.String(name))
	return strings.FieldsFunc(folded, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.IsMark(r)
	})
}

// weekdays are the day codes of the due_hint grammar.
var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// ResolveDue turns a due_hint into the moment a task is due: the start of a day in
// Asia/Bangkok, which is how the board reads a due date. The model normalises what the
// transcript said into one of
//
//	2026-10-16    that date
//	+3d           three days after the meeting (+1d to +999d)
//	next:fri      the first Friday after the meeting (mon, tue, wed, thu, fri, sat or sun)
//	eow           the end of the working week: the meeting day if it is a Friday, else the
//	              next Friday
//	none          no due date
//
// and the days are counted from meetingEnd's date in Bangkok, not in UTC: a meeting that
// ended at 18:30 UTC ended on the next day, 01:30, in Bangkok. Anything else, a date
// that does not exist or a phrase such as "soon", is nil: free text is never parsed.
//
// A date is also nil when it falls outside the window of a task that someone could mean:
// from the year before the meeting to ten years after it, in Bangkok. 0001-01-01 is no
// due date and cannot be stored as one, and 2569-10-16 is a Buddhist Era year that
// slipped through the model's conversion: either would reach the board as a draft with a
// wrong date, and a wrong guess must be an empty field.
func ResolveDue(hint string, meetingEnd time.Time) *time.Time {
	hint = strings.ToLower(strings.TrimSpace(hint))
	y, m, d := meetingEnd.In(bangkok).Date()
	due := dueDay(hint, y, m, d)
	if due == nil || due.Year() < y-1 || due.Year() > y+10 {
		return nil
	}
	return due
}

// dueDay resolves hint against the meeting day y-m-d, in Bangkok, without judging the
// year it lands in.
func dueDay(hint string, y int, m time.Month, d int) *time.Time {
	day := func(offset int) *time.Time {
		t := time.Date(y, m, d+offset, 0, 0, 0, 0, bangkok)
		return &t
	}

	switch {
	case hint == "", hint == "none":
		return nil
	case hint == "eow":
		wd := time.Date(y, m, d, 0, 0, 0, 0, bangkok).Weekday()
		return day((int(time.Friday-wd) + 7) % 7)
	case strings.HasPrefix(hint, "next:"):
		target, ok := weekdays[strings.TrimPrefix(hint, "next:")]
		if !ok {
			return nil
		}
		wd := time.Date(y, m, d, 0, 0, 0, 0, bangkok).Weekday()
		// Strictly after the meeting day: the same weekday is a week away.
		ahead := (int(target-wd)+6)%7 + 1
		return day(ahead)
	case strings.HasPrefix(hint, "+") && strings.HasSuffix(hint, "d"):
		digits := hint[1 : len(hint)-1]
		if len(digits) < 1 || len(digits) > 3 || strings.TrimLeft(digits, "0123456789") != "" {
			return nil
		}
		n, err := strconv.Atoi(digits)
		if err != nil {
			return nil
		}
		return day(n)
	}
	if t, err := time.ParseInLocation(time.DateOnly, hint, bangkok); err == nil {
		return &t
	}
	return nil
}
