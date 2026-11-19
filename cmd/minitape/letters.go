package main

import (
	"unicode"
	"unicode/utf8"

	"github.com/zakijss/minideck/ui"
)

// labelLetter is the group of a row in an A-to-Z list: its first letter,
// in capitals, or "#" for a digit or a sign.
func labelLetter(it *ui.Item) string {
	r, _ := utf8.DecodeRuneInString(it.Label)
	if !unicode.IsLetter(r) {
		return "#"
	}
	return string(unicode.ToUpper(r))
}

// letterJump is the row Left (fwd false) or Right goes to from sel: the
// first of the previous group, or of the next. Groups are runs of rows
// with the same letter, in the list's own order, so a server that files
// "The Beatles" under B is followed as it is.
func letterJump(items []ui.Item, sel int, fwd bool, letter func(*ui.Item) string) int {
	if sel < 0 || sel >= len(items) {
		return max(0, min(sel, len(items)-1))
	}
	k := func(i int) string { return letter(&items[i]) }
	start := func(i int) int { // the first row of i's group
		for i > 0 && k(i-1) == k(i) {
			i--
		}
		return i
	}
	if fwd {
		for i := sel + 1; i < len(items); i++ {
			if k(i) != k(sel) {
				return i
			}
		}
		return sel
	}
	if s := start(sel); s > 0 {
		return start(s - 1)
	}
	return 0
}
