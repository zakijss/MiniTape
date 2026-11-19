package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zakijss/minideck/ui"
)

// fakeFFplay makes the player run script instead of ffplay.
func fakeFFplay(t *testing.T, script string) {
	ff := filepath.Join(t.TempDir(), "ffplay")
	os.WriteFile(ff, []byte("#!/bin/sh\n"+script+"\n"), 0o755)
	t.Setenv("MINIDECK_FFPLAY", ff)
	old := chrtPath
	chrtPath = ""
	t.Cleanup(func() { chrtPath = old })
}

func ids(q []Song) string {
	var s []string
	for _, x := range q {
		s = append(s, x.ID)
	}
	return strings.Join(s, " ")
}

// Songs are removed and moved in the queue by place and id; the playing
// one stays playing and can't be removed.
func TestPlayerQueueEdits(t *testing.T) {
	fakeFFplay(t, "exec sleep 30")
	p := NewPlayer(func(s Song, at int) string { return "x" }, nil)
	defer p.Stop()
	p.Play([]Song{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}, 1)
	v := p.State().Queue
	p.Remove(1, "b") // playing
	p.Remove(0, "x") // not the song there
	if q, pos := p.Queue(); ids(q) != "a b c d" || pos != 1 || p.State().Queue != v {
		t.Fatalf("refused removes changed it: %s %d", ids(q), pos)
	}
	p.Remove(0, "a")
	p.Move(2, -1, "d")
	p.Move(0, 1, "b") // the playing song, moved down
	if q, pos := p.Queue(); ids(q) != "d b c" || pos != 1 || p.State().Song.ID != "b" || p.State().Queue == v {
		t.Fatalf("after edits: %s %d", ids(q), pos)
	}
	p.Move(2, 1, "c") // past the end
	p.Jump(2, "c")
	if st := p.State(); st.Song.ID != "c" || !st.Playing {
		t.Fatalf("jump: %+v", st)
	}
	// seeking back to the start isn't a new play to scrobble
	plays := p.State().Plays
	p.Seek(-100)
	if p.State().Plays != plays {
		t.Fatal("seeking to 0:00 counted a new play")
	}
}

// A restored queue waits, paused at its place; A plays on from there.
func TestPlayerRestore(t *testing.T) {
	dir := t.TempDir()
	fakeFFplay(t, "echo \"$@\" > "+filepath.Join(dir, "args")+"\nexec sleep 30")
	p := NewPlayer(func(s Song, at int) string { return "x" }, nil)
	p.root = func() string { return "/m" }
	defer p.Stop()
	p.Restore([]Song{{ID: "/m/a.mp3", Path: "/m/a.mp3"}, {ID: "/m/b.mp3", Path: "/m/b.mp3"}}, 1, 42*time.Second, repeatAll)
	st := p.State()
	if !st.Has || st.Playing || st.Pos != 1 || st.Elapsed != 42*time.Second || st.Repeat != repeatAll {
		t.Fatalf("restored: %+v", st)
	}
	p.Toggle()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if b, _ := os.ReadFile(filepath.Join(dir, "args")); strings.Contains(string(b), "-ss 42 file:/m/b.mp3") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("didn't play on from 0:42")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !p.State().Playing {
		t.Fatal("not playing")
	}
}

// The queue and place are saved and read back; songs that can't play now
// are dropped, the place moving on to the next one kept.
func TestResume(t *testing.T) {
	dir, card := t.TempDir(), t.TempDir()
	f := func(name string, there bool) Song {
		p := filepath.Join(card, name)
		if there {
			os.WriteFile(p, []byte("x"), 0o644)
		}
		return Song{ID: p, Path: p, Title: name}
	}
	outside := filepath.Join(t.TempDir(), "x.mp3") // a file, but not in the music folder
	os.WriteFile(outside, []byte("x"), 0o644)
	q := []Song{{ID: "srv1"}, f("1.mp3", true), f("2.mp3", false), {ID: outside, Path: outside}, f("3.mp3", true)}
	save := func(pos int, at time.Duration) {
		p := NewPlayer(nil, nil)
		p.Restore(q, pos, at, repeatOne)
		(&resumer{dir: dir, qver: -1}).save(p, p.State(), false, "http://a u")
	}
	// on a song that's still there
	save(1, 40*time.Second)
	got, pos, at, rep := loadResume(dir, "http://a u", card)
	if ids(got) != ids([]Song{q[0], q[1], q[4]}) || pos != 1 || at != 40*time.Second || rep != repeatOne {
		t.Fatalf("same server: %s %d %v %d", ids(got), pos, at, rep)
	}
	// on the song whose file is gone, with another server set up
	save(2, 40*time.Second)
	got, pos, at, _ = loadResume(dir, "http://b u", card)
	if ids(got) != ids([]Song{q[1], q[4]}) || pos != 1 || at != 0 {
		t.Fatalf("other server: %s %d %v", ids(got), pos, at)
	}
	// nothing changed, nothing written
	os.Remove(filepath.Join(dir, savedPlaceFile))
	p := NewPlayer(nil, nil)
	p.Restore(q, 2, 40*time.Second, repeatOne)
	r := &resumer{dir: dir, qver: p.State().Queue, place: savedPlace{2, q[2].ID, 40, repeatOne}}
	r.save(p, p.State(), false, "")
	if _, err := os.Stat(filepath.Join(dir, savedPlaceFile)); err == nil {
		t.Fatal("wrote the place again")
	}
	// a stopped player saves an empty queue: nothing to restore
	p.Stop()
	r.save(p, p.State(), false, "")
	if got, _, _, _ := loadResume(dir, "", card); len(got) != 0 {
		t.Fatalf("after stop: %s", ids(got))
	}
}

// queuePlayer is a stillPlayer with a queue, noting the changes asked of
// it.
type queuePlayer struct {
	stillPlayer
	q   []Song
	ops []string
}

func (p *queuePlayer) Queue() ([]Song, int, error) { return p.q, p.st.Pos, nil }
func (p *queuePlayer) Jump(i int, id string)       { p.ops = append(p.ops, fmt.Sprint("jump ", i, id)) }
func (p *queuePlayer) Remove(i int, id string)     { p.ops = append(p.ops, fmt.Sprint("remove ", i, id)) }
func (p *queuePlayer) Move(i, d int, id string) {
	p.ops = append(p.ops, fmt.Sprint("move ", i, d, id))
}

// Down on Now Playing lists the queue on the current song; X, L2/R2 and A
// remove, move and play the selected one; B goes back to Now Playing.
func TestQueueView(t *testing.T) {
	a := testApp(t, "http://127.0.0.1:1")
	q := []Song{{ID: "a", Title: "A", Duration: 60}, {ID: "b", Title: "B"}, {ID: "c", Title: "C"}}
	pl := &queuePlayer{stillPlayer{NowPlaying{Song: q[1], Has: true, Playing: true, Pos: 1, Queue: 3}}, q, nil}
	a.player = pl
	a.stack = []*view{{title: "MiniTape"}}
	a.np = true
	a.handle(ui.Event{Button: ui.Down, Pressed: true})
	v := a.top()
	if a.np || v.title != "Queue" {
		t.Fatalf("Down didn't open the queue: np %v, %q", a.np, v.title)
	}
	a.waitLoaded(t, v)
	if len(v.list.Items) != 3 || v.list.Sel != 1 || v.list.Items[1].Detail != "playing" || v.list.Items[0].Detail != "1:00" {
		t.Fatalf("rows: sel %d %+v", v.list.Sel, v.list.Items)
	}
	for _, b := range []ui.Button{ui.X, ui.R2, ui.A} {
		a.handle(ui.Event{Button: b, Pressed: true})
	}
	if got := strings.Join(pl.ops, ","); got != "remove 1b,move 1 1b,jump 2c" || !a.np || len(a.stack) != 1 {
		t.Fatalf("ops %q, np %v, %d views", got, a.np, len(a.stack))
	}
	a.handle(ui.Event{Button: ui.Down, Pressed: true})
	a.handle(ui.Event{Button: ui.B, Pressed: true})
	if !a.np || len(a.stack) != 1 {
		t.Fatal("B didn't go back to Now Playing")
	}
}

// A server's song keeps its "path" (where the file is on the server) out
// of Song.Path, and the player plays from the card only inside the music
// folder.
func TestServerPathIsNotLocal(t *testing.T) {
	var s Song
	if err := json.Unmarshal([]byte(`{"id":"s1","path":"-i /etc/passwd","title":"x"}`), &s); err != nil || s.Path != "" {
		t.Fatalf("decoded %+v, %v", s, err)
	}
	for _, c := range []struct {
		path string
		ok   bool
	}{{"/m/a/b.mp3", true}, {"/m/../etc/x.mp3", false}, {"/mx/a.mp3", false}, {"m/a.mp3", false}, {"-y", false}, {"http://h/a.mp3", false}} {
		if _, ok := inside(c.path, "/m"); ok != c.ok {
			t.Errorf("inside(%q) = %v", c.path, ok)
		}
	}
	dir := t.TempDir()
	fakeFFplay(t, "echo \"$@\" >> "+filepath.Join(dir, "args")+"\nexec sleep 30")
	p := NewPlayer(func(s Song, at int) string { return "x" }, nil)
	p.root = func() string { return "/m" }
	defer p.Stop()
	p.Play([]Song{{ID: "/etc/passwd", Path: "/etc/passwd"}}, 0)
	if st := p.State(); st.Playing || st.Err == "" {
		t.Fatalf("played a file outside the music folder: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, "args")); err == nil {
		t.Fatal("ffplay started")
	}
}
