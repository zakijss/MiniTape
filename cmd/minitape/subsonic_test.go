package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A server that refuses token logins (Subsonic error 41) gets the legacy
// hex password instead, and the request then succeeds.
func TestLegacyAuthFallback(t *testing.T) {
	var sawToken, sawLegacy bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		resp := map[string]any{"status": "ok", "version": "1.16.1"}
		switch {
		case q.Get("t") != "":
			sawToken = true
			resp = map[string]any{"status": "failed", "error": map[string]any{"code": 41, "message": "no tokens"}}
		case q.Get("p") == "enc:707721": // hex("pw!")
			sawLegacy = true
		default:
			resp = map[string]any{"status": "failed", "error": map[string]any{"code": 40, "message": "bad"}}
		}
		json.NewEncoder(w).Encode(map[string]any{"subsonic-response": resp})
	}))
	defer srv.Close()
	c := &Subsonic{Base: srv.URL, User: "u", Password: "pw!", HTTP: srv.Client()}
	if err := c.Ping(); err != nil {
		t.Fatal(err)
	}
	if !sawToken || !sawLegacy || !c.legacy {
		t.Fatalf("token=%v legacy=%v", sawToken, sawLegacy)
	}
}

// Broken or unexpected replies come back as errors (or empty lists), never
// a panic.
func TestMalformedReplies(t *testing.T) {
	for _, body := range []string{
		"", "not json", "null", "[]",
		`{"subsonic-response": null}`,
		`{"subsonic-response": {"status": "ok"}}`,
		`{"subsonic-response": {"status": "failed"}}`,
		`{"subsonic-response": {"status": "failed", "error": {"code": 40}}}`,
		`{"subsonic-response": {"status": "ok", "album": {"song": "x"}}}`,
		`{"subsonic-response": {"status": "ok", "artists": {"index": [{"artist": null}]}}}`,
		`{"subsonic-response": {"status": "ok", "lyricsList": {"structuredLyrics": [{"synced": true, "line": []}]}}}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(body))
		}))
		c := &Subsonic{Base: srv.URL, User: "u", Password: "p", HTTP: srv.Client()}
		c.Ping()
		c.AlbumList("newest", 10)
		c.Album("a")
		c.Artists()
		c.Artist("a")
		c.Playlists()
		c.Playlist("p")
		c.RandomSongs(5)
		c.Search("q", 5)
		c.Starred()
		c.Lyrics(Song{ID: "s", Artist: "a", Title: "t"})
		if _, err := c.CoverArt("c", 64); err == nil {
			t.Errorf("cover from %q: no error", body)
		}
		srv.Close()
	}
}

// A wrong login reads as a sentence for the screen.
func TestWrongLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"subsonic-response": {"status": "failed", "error": {"code": 40, "message": "Wrong username or password"}}}`))
	}))
	defer srv.Close()
	c := &Subsonic{Base: srv.URL, User: "u", Password: "p", HTTP: srv.Client()}
	if err := c.Ping(); err == nil || err.Error() != "Wrong username or password." {
		t.Errorf("Ping: %v", err)
	}
}

// Timed lyrics prefer a synced set, apply its offset, and the current line
// follows the playing time.
func TestLyrics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"subsonic-response": {"status": "ok", "lyricsList": {"structuredLyrics": [
			{"synced": false, "line": [{"value": "plain"}]},
			{"synced": true, "offset": 500, "line": [{"start": 1000, "value": " one "}, {"start": 3000, "value": "two"}]}
		]}}}`))
	}))
	defer srv.Close()
	c := &Subsonic{Base: srv.URL, User: "u", Password: "p", HTTP: srv.Client()}
	lines, synced, err := c.Lyrics(Song{ID: "s"})
	if err != nil || !synced || len(lines) != 2 {
		t.Fatalf("lines %v synced %v err %v", lines, synced, err)
	}
	if lines[0].Text != "one" || lines[0].At != 500*time.Millisecond || lines[1].At != 2500*time.Millisecond {
		t.Errorf("lines %+v", lines)
	}
	for _, c := range []struct {
		at   time.Duration
		want int
	}{{0, -1}, {500 * time.Millisecond, 0}, {2 * time.Second, 0}, {time.Hour, 1}} {
		if got := currentLine(lines, c.at); got != c.want {
			t.Errorf("currentLine at %v = %d, want %d", c.at, got, c.want)
		}
	}
	if got := currentLine(nil, time.Second); got != -1 {
		t.Errorf("currentLine with no lines = %d", got)
	}
}
