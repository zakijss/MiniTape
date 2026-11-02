package main

import (
	"image"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zakijss/minideck/ui"
)

// testApp is an app drawing to nowhere, talking to the server at base.
func testApp(t *testing.T, base string) *app {
	t.Helper()
	t.Setenv("MINIDECK_UI_NULL", "1")
	scr, err := ui.Open()
	if err != nil {
		t.Fatal(err)
	}
	a := &app{scr: scr, covers: map[coverKey]image.Image{}, pending: map[coverKey]bool{}, failed: map[coverKey]time.Time{},
		backdrops: map[string]*image.RGBA{}, fetchSem: make(chan struct{}, 3), cfg: &Settings{},
		lyr: map[string]*songLyrics{}, poke: func() {}, done: make(chan func(), 8),
		player: &stillPlayer{}}
	a.sub = &Subsonic{Base: base, User: "u", Password: "p", HTTP: http.DefaultClient}
	return a
}

// waitLoaded runs the event loop's part that matters here, drawing and
// applying background results, until v has loaded.
func (a *app) waitLoaded(t *testing.T, v *view) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for v.loading {
		a.draw()
		select {
		case apply := <-a.done:
			apply()
		case <-deadline:
			t.Fatal("still loading")
		}
	}
	a.draw()
}

// Lists load in the background while the screen keeps drawing them; with
// -race this fails if a loader writes to a view the event loop reads.
func TestLoadWhileDrawing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body string
		switch {
		case strings.HasSuffix(r.URL.Path, "/getAlbumList2"):
			body = `"albumList2":{"album":[{"id":"al1","name":"Night Signals","artist":"The Static Gardens","year":2007}]}`
		case strings.HasSuffix(r.URL.Path, "/getAlbum"):
			body = `"album":{"id":"al1","name":"Night Signals","song":[{"id":"s1","title":"Signal Fires","duration":214,"track":1},{"id":"s2","title":"Low Tide Radio","duration":187,"track":2}]}`
		case strings.HasSuffix(r.URL.Path, "/getRandomSongs"):
			body = `"randomSongs":{"song":[{"id":"s1","title":"Signal Fires"},{"id":"s2","title":"Low Tide Radio"}]}`
		case strings.HasSuffix(r.URL.Path, "/getArtists"):
			body = `"artists":{"index":[{"artist":[{"id":"ar1","name":"The Static Gardens","albumCount":1}]}]}`
		}
		time.Sleep(20 * time.Millisecond) // let the screen draw while loading
		w.Write([]byte(`{"subsonic-response":{"status":"ok",` + body + `}}`))
	}))
	defer srv.Close()
	a := testApp(t, srv.URL)
	a.stack = []*view{a.home()}

	v := a.albumList("Recently added", "newest")
	a.push(v)
	a.waitLoaded(t, v)
	if len(v.list.Items) != 1 || v.sub != "1 albums" || v.err != "" {
		t.Fatalf("album list: %d items, sub %q, err %q", len(v.list.Items), v.sub, v.err)
	}

	// The album page: its year and artist are set after songList returns,
	// and the songs' line must still come out right.
	a.openAlbum(&v.list.Items[0])
	al := a.top()
	a.waitLoaded(t, al)
	if len(al.songs) != 2 || len(al.info) != 2 || al.info[0] != "The Static Gardens" || al.info[1] != "2007 · 2 songs · 7 min" {
		t.Fatalf("album page: songs %d, info %q", len(al.songs), al.info)
	}

	ar := a.artists()
	a.push(ar)
	a.waitLoaded(t, ar)
	if len(ar.list.Items) != 1 || ar.sub != "1 artists" {
		t.Fatalf("artists: %d items, sub %q", len(ar.list.Items), ar.sub)
	}

	a.shuffleAll()
	sh := a.top()
	a.waitLoaded(t, sh)
	if !a.np || a.top() == sh {
		t.Fatalf("shuffle: now playing %v, view still shown %v", a.np, a.top() == sh)
	}
}

// A failed lyrics fetch stays on screen instead of being retried on every
// redraw, until the back-off passes or the user asks again (Y twice).
func TestLyricsFailureBacksOff(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	a := testApp(t, srv.URL)
	redraw := make(chan struct{}, 1)
	a.poke = func() {
		select {
		case redraw <- struct{}{}:
		default:
		}
	}
	song := Song{ID: "s1", Title: "Signal Fires", Artist: "The Static Gardens", Duration: 214}
	a.player = &stillPlayer{NowPlaying{Song: song, Has: true, Playing: true}}
	a.np, a.lyricsOn = true, true
	a.stack = []*view{{title: "MiniTape"}}

	// The event loop: every finished fetch pokes a redraw, which asks for
	// the lyrics again.
	loop := func(n int) {
		for range n {
			a.draw()
			select {
			case <-redraw:
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	loop(10)
	l := a.lyricsFor(song, false)
	a.lyrMu.Lock()
	loading, errText := l.loading, l.err
	a.lyrMu.Unlock()
	if loading || !strings.Contains(errText, "503") {
		t.Fatalf("loading %v, error %q: want the error shown", loading, errText)
	}
	// One fetch asks two endpoints (structured lyrics, then plain).
	if n := hits.Load(); n != 2 {
		t.Fatalf("%d requests after one failure, want 2", n)
	}

	// Y twice: lyrics off, then on again.
	a.handle(ui.Event{Button: ui.Y, Pressed: true})
	a.handle(ui.Event{Button: ui.Y, Pressed: true})
	loop(10)
	if n := hits.Load(); n != 4 {
		t.Fatalf("%d requests after asking again, want 4", n)
	}

	// After the back-off it tries again by itself, once.
	a.lyrMu.Lock()
	a.lyr[song.ID].failed = time.Now().Add(-lyricsRetry)
	a.lyrMu.Unlock()
	loop(10)
	if n := hits.Load(); n != 6 {
		t.Fatalf("%d requests after the back-off, want 6", n)
	}
}

// Covers in the pixel style are redrawn at the size they are drawn, once;
// switching to smooth shows the cover as fetched, at once.
func TestShownCoverStyles(t *testing.T) {
	cover := image.NewRGBA(image.Rect(0, 0, largePx, largePx))
	for i := range cover.Pix {
		cover.Pix[i] = uint8(i * 7)
	}
	a := &app{cfg: &Settings{}}
	p := a.shown(cover, 188)
	if p == image.Image(cover) || p.Bounds().Dx() != 188 || p.Bounds().Dy() != 188 {
		t.Fatalf("pixel cover %v", p.Bounds())
	}
	if a.shown(cover, 188) != p {
		t.Error("redrawn again instead of kept")
	}
	if a.shown(cover, 232) == p {
		t.Error("one size used for another")
	}
	a.cfg.CoverStyle = "smooth"
	if a.shown(cover, 188) != image.Image(cover) {
		t.Error("smooth style still pixelated")
	}
	if a.shown(nil, 188) != nil {
		t.Error("no cover gave a picture")
	}
}

// The thumbnail size follows ui.List's rows: 66 px, stretched to fill.
func TestListThumbSize(t *testing.T) {
	for _, c := range []struct{ h, n, want int }{
		{300, 2, 44},  // fewer items than rows: natural height
		{300, 20, 53}, // 4 rows of 75
		{264, 20, 44}, // 4 rows of 66
	} {
		if got := listThumbSize(c.h, c.n); got != c.want {
			t.Errorf("listThumbSize(%d, %d) = %d, want %d", c.h, c.n, got, c.want)
		}
	}
}

func TestTrackLabelIsolatesTitle(t *testing.T) {
	if got := trackLabel(3, "سلام"); got != "3  \u2068سلام\u2069" {
		t.Errorf("trackLabel = %q", got)
	}
}
