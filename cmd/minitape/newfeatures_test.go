package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zakijss/minideck/cache"
	"github.com/zakijss/minideck/ui"
)

// Y on a list asks the server again instead of showing the copy on the
// card, and an album page keeps a single songs line after it.
func TestRefreshSkipsTheCard(t *testing.T) {
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		songs := `{"id":"s1","title":"One","duration":60,"track":1}`
		if asked.Add(1) > 1 { // the album has grown on the server since
			songs += `,{"id":"s2","title":"Two","duration":60,"track":2}`
		}
		w.Write([]byte(`{"subsonic-response":{"status":"ok","album":{"id":"al1","name":"A","song":[` + songs + `]}}}`))
	}))
	defer srv.Close()
	a := testApp(t, srv.URL)
	a.sub.Cache = cache.New(t.TempDir())
	a.stack = []*view{a.home()}
	a.openAlbum(&uiAlbum)
	v := a.top()
	a.waitLoaded(t, v)
	if len(v.list.Items) != 1 {
		t.Fatalf("first load: %d songs", len(v.list.Items))
	}
	time.Sleep(50 * time.Millisecond) // let the card copy settle
	asked.Store(1)
	v.refresh()
	a.waitLoaded(t, v)
	if len(v.list.Items) != 2 {
		t.Fatalf("after refresh: %d songs, want the server's 2", len(v.list.Items))
	}
	if len(v.info) != 2 || !strings.HasPrefix(v.info[1], "2 songs") {
		t.Fatalf("info after refresh: %q", v.info)
	}
	if a.sub.fresh.Load() != 0 {
		t.Fatal("fresh mode left on")
	}
}

// uiAlbum is the album row opened by the refresh test.
var uiAlbum = ui.Item{Value: Album{ID: "al1", Name: "A", Artist: "B"}}

// Seek starts the stream again further in, and past the end moves on.
func TestPlayerSeek(t *testing.T) {
	ff := filepath.Join(t.TempDir(), "ffplay")
	os.WriteFile(ff, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755)
	t.Setenv("MINIDECK_FFPLAY", ff)
	var mu sync.Mutex
	var asked []string
	p := NewPlayer(func(s Song, at int) string {
		mu.Lock()
		asked = append(asked, s.ID+"@"+strconv.Itoa(at))
		mu.Unlock()
		return "x"
	}, nil)
	defer p.Stop()
	p.Play([]Song{{ID: "a", Duration: 100}, {ID: "b", Duration: 100}}, 0)
	p.Seek(30)
	p.Seek(-60) // not before the start
	p.Seek(500) // past the end: the next song
	mu.Lock()
	got := strings.Join(asked, " ")
	mu.Unlock()
	if got != "a@0 a@30 a@0 b@0" {
		t.Fatalf("streams asked for: %s", got)
	}
	if st := p.State(); st.Song.ID != "b" {
		t.Fatalf("playing %s after seeking past the end", st.Song.ID)
	}
}

// The sleep timer's "end of this song" stops there and keeps the place:
// the next song is ready to play, not playing.
func TestPlayerStopsAfterSong(t *testing.T) {
	ff := filepath.Join(t.TempDir(), "ffplay")
	os.WriteFile(ff, []byte("#!/bin/sh\nexec sleep 0.2\n"), 0o755)
	t.Setenv("MINIDECK_FFPLAY", ff)
	old := chrtPath // real-time scheduling needs privileges a test lacks
	chrtPath = ""
	defer func() { chrtPath = old }()
	p := NewPlayer(func(s Song, at int) string { return "x" }, nil)
	defer p.Stop()
	p.Play([]Song{{ID: "a"}, {ID: "b"}}, 0)
	p.StopAfterSong(true)
	if !p.State().SleepEnd {
		t.Fatal("SleepEnd not reported")
	}
	time.Sleep(time.Second)
	st := p.State()
	if st.Playing || st.Song.ID != "b" || st.SleepEnd {
		t.Fatalf("after the song: playing %v, song %s, sleepEnd %v", st.Playing, st.Song.ID, st.SleepEnd)
	}

	// Seeking past the end counts as the song ending.
	p.Play([]Song{{ID: "a", Duration: 100}, {ID: "b"}}, 0)
	p.StopAfterSong(true)
	p.Seek(500)
	if st := p.State(); st.Playing || st.Song.ID != "b" {
		t.Fatalf("after seeking past the end: playing %v, song %s", st.Playing, st.Song.ID)
	}
}

func TestSleepText(t *testing.T) {
	for _, c := range []struct {
		st   NowPlaying
		want string
	}{
		{NowPlaying{}, ""},
		{NowPlaying{SleepLeft: 1}, "SLEEP 1 MIN"},
		{NowPlaying{SleepLeft: 1741}, "SLEEP 30 MIN"},
		{NowPlaying{SleepEnd: true}, "SLEEP AFTER THIS SONG"},
	} {
		if got := sleepText(c.st); got != c.want {
			t.Errorf("sleepText(%+v) = %q, want %q", c.st, got, c.want)
		}
	}
	if songCount(1) != "1 song" || songCount(2) != "2 songs" {
		t.Error("songCount")
	}
}
