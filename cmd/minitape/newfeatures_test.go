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
