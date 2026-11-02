package main

import (
	"strconv"
	"strings"
	"testing"
)

// The audioserver is boosted while music plays, put back when it stops, and
// followed when Onion starts a new one; nothing is done on ticks without a
// change.
func TestAudioBoost(t *testing.T) {
	pid := 100
	var calls []string
	b := &audioBoost{
		pid: func() int { return pid },
		set: func(p, policy, prio int) error {
			calls = append(calls, "set "+strconv.Itoa(p)+" "+strconv.Itoa(policy)+" "+strconv.Itoa(prio))
			return nil
		},
		nice: func(p, n int) error {
			calls = append(calls, "nice "+strconv.Itoa(p))
			return nil
		},
	}
	step := func(playing bool, want ...string) {
		t.Helper()
		calls = nil
		b.update(playing)
		if strings.Join(calls, ", ") != strings.Join(want, ", ") {
			t.Errorf("update(%v): %q, want %q", playing, calls, want)
		}
	}
	step(false)
	step(true, "set 100 1 20")
	step(true) // still playing: nothing
	step(false, "set 100 0 0", "nice 100")
	step(false)
	step(true, "set 100 1 20")
	pid = 200 // Onion restarted the audioserver
	step(true, "set 100 0 0", "nice 100", "set 200 1 20")
	pid = 0 // and it's gone
	step(true, "set 200 0 0", "nice 200")
	step(true)
}

// Where real-time scheduling isn't allowed, the audioserver gets a raised
// priority instead.
func TestAudioBoostFallback(t *testing.T) {
	var niced []int
	b := &audioBoost{
		pid:  func() int { return 7 },
		set:  func(int, int, int) error { return errNotAllowed },
		nice: func(_, n int) error { niced = append(niced, n); return nil },
	}
	b.update(true)
	b.update(false)
	if len(niced) != 2 || niced[0] != -10 || niced[1] != 0 {
		t.Errorf("nice levels %v, want [-10 0]", niced)
	}
}

type notAllowed struct{}

func (notAllowed) Error() string { return "operation not permitted" }

var errNotAllowed error = notAllowed{}

func TestFfplayCommand(t *testing.T) {
	old := chrtPath
	defer func() { chrtPath = old }()
	chrtPath = "/usr/bin/chrt"
	c := ffplayCommand("/x/ffplay", "-nodisp", "u")
	if got := strings.Join(c.Args, " "); got != "/usr/bin/chrt -f 20 /x/ffplay -nodisp u" {
		t.Errorf("with chrt: %q", got)
	}
	chrtPath = ""
	if got := strings.Join(ffplayCommand("/x/ffplay", "a").Args, " "); got != "/x/ffplay a" {
		t.Errorf("without chrt: %q", got)
	}
}
