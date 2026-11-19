package main

import (
	"testing"

	"github.com/zakijss/minideck/ui"
)

func TestLetterJump(t *testing.T) {
	var items []ui.Item
	for _, l := range []string{"10cc", "ABBA", "Air", "Beck", "björk", "Cat"} {
		items = append(items, ui.Item{Label: l})
	}
	for _, c := range []struct {
		from int
		fwd  bool
		want int
	}{{0, true, 1}, {1, true, 3}, {2, true, 3}, {3, true, 5}, {5, true, 5},
		{4, false, 1}, {3, false, 1}, {1, false, 0}, {0, false, 0}, {5, false, 3}} {
		if got := letterJump(items, c.from, c.fwd, labelLetter); got != c.want {
			t.Errorf("from %d (fwd %v): %d, want %d", c.from, c.fwd, got, c.want)
		}
	}
	// the server's groups: "The Beatles" filed under B
	ars := []ui.Item{{Label: "Air", Value: Artist{index: "A"}}, {Label: "The Beatles", Value: Artist{index: "B"}},
		{Label: "Beck", Value: Artist{index: "B"}}, {Label: "Cat", Value: Artist{index: "C"}}}
	if got := letterJump(ars, 1, true, func(it *ui.Item) string { return it.Value.(Artist).index }); got != 3 {
		t.Errorf("server groups: %d", got)
	}
}
