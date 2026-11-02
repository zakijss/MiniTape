package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A button whose request timed out may have reached the player: sending it
// again would skip two tracks (or pause and resume at once).
func TestRemoteNoRetryAfterTimeout(t *testing.T) {
	var nexts atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/next" {
			nexts.Add(1)
			<-release
		}
		json.NewEncoder(w).Encode(NowPlaying{})
	}))
	defer srv.Close()
	defer close(release)
	var starts atomic.Int32
	r := &remote{c: &http.Client{Timeout: 100 * time.Millisecond}, addr: srv.Listener.Addr().String(),
		start: func() error { starts.Add(1); return nil }}
	r.Next()
	time.Sleep(50 * time.Millisecond)
	if nexts.Load() != 1 || starts.Load() != 0 {
		t.Errorf("/next sent %d times, player started %d times; want 1 and 0", nexts.Load(), starts.Load())
	}
}

// With nothing listening, the player isn't running: start it and send the
// request to it.
func TestRemoteStartsStoppedPlayer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // the player has stopped

	var got struct {
		Queue []Song
		Index int
	}
	var plays atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/play", func(w http.ResponseWriter, r *http.Request) {
		plays.Add(1)
		json.NewDecoder(r.Body).Decode(&got)
	})
	mux.HandleFunc("/state", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(NowPlaying{}) })
	var srv *http.Server
	r := &remote{c: &http.Client{Timeout: time.Second}, addr: addr, start: func() error {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		srv = &http.Server{Handler: mux}
		go srv.Serve(ln)
		return nil
	}}
	r.Play([]Song{{ID: "s1"}, {ID: "s2"}}, 1)
	if srv == nil {
		t.Fatal("the player wasn't started")
	}
	defer srv.Close()
	if plays.Load() != 1 || len(got.Queue) != 2 || got.Index != 1 {
		t.Errorf("/play sent %d times with %+v", plays.Load(), got)
	}
}

// The login read at startup comes late (secrets.enc is slow to open on
// purpose) and must not replace one a /reload set in the meantime.
func TestStartupLoginAfterReload(t *testing.T) {
	sub, lg := &Subsonic{}, &logins{}
	lg.reload(func() { sub.SetLogin("http://new:4533", "new", "pw2") })
	lg.initial(func() { sub.SetLogin("http://old:4533", "old", "pw1") })
	if sub.Base != "http://new:4533" || sub.User != "new" {
		t.Errorf("login %s %s, want the reloaded one", sub.Base, sub.User)
	}

	// Startup first, then a reload: the reload's stays.
	sub, lg = &Subsonic{}, &logins{}
	lg.initial(func() { sub.SetLogin("http://old:4533", "old", "pw1") })
	lg.reload(func() { sub.SetLogin("http://new:4533", "new", "pw2") })
	if sub.Base != "http://new:4533" {
		t.Errorf("login %s, want the reloaded one", sub.Base)
	}

	// Run together (for -race): whichever order, a reload's login wins.
	for range 20 {
		sub, lg = &Subsonic{}, &logins{}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); lg.initial(func() { sub.SetLogin("http://old:4533", "old", "") }) }()
		go func() { defer wg.Done(); lg.reload(func() { sub.SetLogin("http://new:4533", "new", "") }) }()
		wg.Wait()
		if sub.Base != "http://new:4533" {
			t.Fatalf("login %s, want the reloaded one", sub.Base)
		}
	}
}

// A reply cut short must not replace the state: it would show the song with
// its queue half filled in ("UP NEXT" and nothing after it). Only after
// staleAfter failed polls in a row is the player taken as gone.
func TestRemoteKeepsStateOnBadReply(t *testing.T) {
	var mode atomic.Int32 // 0 good, 1 cut short, 2 error
	full := NowPlaying{Has: true, Playing: true, Len: 3,
		Song: Song{ID: "1", Title: "One", Artist: "A"},
		Next: []Song{{ID: "2", Title: "Two", Artist: "B"}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case 1:
			b, _ := json.Marshal(full)
			w.Write(b[:len(b)-40]) // the connection drops mid-reply
		case 2:
			http.Error(w, "busy", http.StatusServiceUnavailable)
		default:
			json.NewEncoder(w).Encode(full)
		}
	}))
	defer srv.Close()
	r := &remote{c: &http.Client{Timeout: time.Second}, addr: srv.Listener.Addr().String()}
	r.fetch()
	check := func(when string) {
		t.Helper()
		r.mu.Lock()
		defer r.mu.Unlock()
		if len(r.last.Next) != 1 || r.last.Next[0].Title != "Two" || r.last.Song.Title != "One" {
			t.Fatalf("%s: state %+v", when, r.last)
		}
	}
	check("good reply")
	mode.Store(1)
	r.fetch()
	check("after a cut-short reply")
	mode.Store(2)
	for range staleAfter - 2 {
		r.fetch()
	}
	check("after a few failed polls")
	r.fetch()
	r.mu.Lock()
	gone := !r.last.Has
	r.mu.Unlock()
	if !gone {
		t.Error("the player wasn't taken as gone after staleAfter failed polls")
	}
	mode.Store(0)
	r.fetch()
	check("once it answers again")
}
