package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/zakijss/minideck/procs"
	"github.com/zakijss/minideck/secrets"
	"github.com/zakijss/minideck/tnet"
	"github.com/zakijss/minideck/ui"
)

// The player runs as a background service ("minitape daemon") so music keeps
// playing after MiniTape's screen is closed, in the Onion menu and in other
// apps. It pauses while a game runs and resumes after. MiniTape's screen talks
// to it over HTTP on 127.0.0.1.
const daemonAddr = "127.0.0.1:45211"

// nowFile holds what's playing, for other apps' tickers (see ui.Now).
const nowFile = "/tmp/minideck-now.json"

func runDaemon(dir string) {
	// Audio comes first: this process (overlay drawing, HTTP) runs at low
	// priority; ffplay and the audioserver get real-time scheduling while
	// music plays (see realtime.go).
	syscall.Setpriority(syscall.PRIO_PROCESS, 0, 10)
	// It stays running next to games: keep the heap small.
	debug.SetGCPercent(50)
	// Listen first: the screen is waiting for the player. Opening the
	// password (PBKDF2, deliberately slow) happens after, and requests wait
	// for it.
	cfg := loadSettings(dir)
	var cfgMu sync.Mutex // cfg is replaced on /reload while streams start
	sub := &Subsonic{Base: cfg.URL, User: cfg.User, HTTP: tnet.Client(20 * time.Second)}
	ready := make(chan struct{})
	first := cfg
	var lg logins
	// Last.fm and ListenBrainz: listens that couldn't be sent last time
	// are read now and sent once the logins are open.
	sc := NewScrobbler(filepath.Join(dir, scrobbleQueueFile))
	scHTTP := scrobbleClient()
	go func() {
		sec, err := secrets.Load(filepath.Join(dir, "secrets.enc"))
		if err != nil {
			log.Printf("secrets: %v", err)
		}
		lg.initial(func() {
			sub.SetLogin(first.URL, first.User, sec[first.Secret])
			sc.Configure(scrobbleServices(sec, scHTTP), false)
		})
		close(ready)
	}()
	rl, err := startRelay(func(id string, at, kbps int) string {
		<-ready
		cfgMu.Lock()
		format := cfg.Format
		cfgMu.Unlock()
		return sub.StreamURL(id, format, kbps, at)
	}, func() int {
		cfgMu.Lock()
		defer cfgMu.Unlock()
		return cfg.bitRate()
	}, func() string {
		cfgMu.Lock()
		defer cfgMu.Unlock()
		return cfg.Format
	}, filepath.Join(dir, "streams"), func() int64 {
		cfgMu.Lock()
		defer cfgMu.Unlock()
		return cfg.songCache()
	})
	if err != nil {
		log.Fatal(err)
	}
	pl := NewPlayer(rl.URL, nil)
	pl.prefetch = rl.Prefetch

	mux := http.NewServeMux()
	var lastAsked atomic.Int64 // when the screen last asked for the state
	lastAsked.Store(time.Now().Unix())
	// sleepAt is when the sleep timer pauses the music (Unix seconds, 0 off)
	var sleepAt atomic.Int64
	mux.HandleFunc("/state", func(w http.ResponseWriter, r *http.Request) {
		lastAsked.Store(time.Now().Unix())
		st := pl.State()
		st.Kbps = rl.playingRate()
		if at := sleepAt.Load(); at > 0 {
			st.SleepLeft = max(1, int(at-time.Now().Unix()))
		}
		json.NewEncoder(w).Encode(st)
	})
	mux.HandleFunc("/play", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Queue []Song
			Index int
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		pl.Play(req.Queue, req.Index)
	})
	// /play-song?q=...: play the best match from the start, then the rest of
	// its album (or random songs, if it's the album's last).
	mux.HandleFunc("/play-song", func(w http.ResponseWriter, r *http.Request) {
		<-ready
		songs, err := sub.Search(r.URL.Query().Get("q"), 10)
		if err != nil || len(songs) == 0 {
			http.Error(w, "no match", http.StatusNotFound)
			return
		}
		song := songs[0]
		queue, idx := []Song{song}, 0
		if song.AlbumID != "" {
			if al, err := sub.Album(song.AlbumID); err != nil {
				log.Printf("play-song: album %s: %v", song.AlbumID, err)
			} else {
				for i, x := range al.Song {
					// match by id, or by title and track (some servers give
					// search results different ids from album listings)
					if x.ID == song.ID || (x.Title == song.Title && x.Track == song.Track) {
						queue, idx = al.Song, i
						break
					}
				}
			}
		}
		if idx == len(queue)-1 {
			// nothing after it on the album: carry on with random songs
			if more, err := sub.RandomSongs(50); err == nil {
				for _, x := range more {
					if x.ID != song.ID {
						queue = append(queue, x)
					}
				}
			}
		}
		pl.Play(queue, idx)
		json.NewEncoder(w).Encode(song)
	})
	mux.HandleFunc("/toggle", func(w http.ResponseWriter, r *http.Request) { pl.Toggle() })
	mux.HandleFunc("/next", func(w http.ResponseWriter, r *http.Request) { pl.Next() })
	mux.HandleFunc("/prev", func(w http.ResponseWriter, r *http.Request) { pl.Prev() })
	mux.HandleFunc("/stop", func(w http.ResponseWriter, r *http.Request) { pl.Stop() })
	// /sleep?min=30 pauses in 30 minutes; min=-1 stops at the end of the
	// song; min=0 turns the timer off.
	mux.HandleFunc("/sleep", func(w http.ResponseWriter, r *http.Request) {
		m, _ := strconv.Atoi(r.URL.Query().Get("min"))
		sleepAt.Store(0)
		pl.StopAfterSong(m < 0)
		if m > 0 {
			sleepAt.Store(time.Now().Add(time.Duration(m) * time.Minute).Unix())
		}
	})
	// /seek?d=-10: seconds back (negative) or forward
	mux.HandleFunc("/seek", func(w http.ResponseWriter, r *http.Request) {
		d, _ := strconv.Atoi(r.URL.Query().Get("d"))
		pl.Seek(d)
	})
	mux.HandleFunc("/reload", func(w http.ResponseWriter, r *http.Request) {
		// settings changed: pick up the new server and login
		lg.reload(func() {
			c := loadSettings(dir)
			s2, err := secrets.Load(filepath.Join(dir, "secrets.enc"))
			if err != nil {
				log.Printf("secrets: %v", err)
			}
			cfgMu.Lock()
			cfg = c
			cfgMu.Unlock()
			rl.prune() // the limit may be lower now
			sub.SetLogin(c.URL, c.User, s2[c.Secret])
			// An unreadable secrets.enc would look like every service
			// turned off, and drop their queued listens.
			if err == nil {
				sc.Configure(scrobbleServices(s2, scHTTP), true)
			}
		})
	})
	mux.HandleFunc("/scrobble-flush", func(w http.ResponseWriter, r *http.Request) { sc.Flush() })
	quit := make(chan struct{})
	var quitOnce sync.Once
	stop := func() { quitOnce.Do(func() { close(quit) }) }
	mux.HandleFunc("/quit", func(w http.ResponseWriter, r *http.Request) { stop() })
	// Ended from Tasks (SIGTERM) or the session going away: stop the same
	// way as /quit, so ffplay stops and /tmp/stay_awake and the ticker file
	// are cleaned up.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() { <-sigs; stop() }()
	srv := &http.Server{Addr: daemonAddr, Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err) // another daemon already owns the port
		}
	}()
	log.Printf("listening on %s", daemonAddr)

	ov, err := ui.OpenOverlay()
	if err != nil {
		log.Printf("no overlay: %v", err)
	}
	cfgMu.Lock()
	overlayOn := cfg.menuTicker()
	cfgMu.Unlock()

	tick := time.NewTicker(time.Second)
	frame := ui.TickerClock() // one step of the strip per frame
	// Checked once a second, not every frame: scanning /proc is not free.
	menuUp, inGame, shown := false, false, false
	keptAwake := false // whether /tmp/stay_awake is ours
	menuPid := 0
	// While the strip shows over the menu, a fast check puts it back the
	// moment the menu draws over it (see Overlay.Guard).
	var guard *time.Ticker
	var guardC <-chan time.Time
	setGuard := func(on bool) {
		switch {
		case on && guard == nil:
			guard = time.NewTicker(8 * time.Millisecond)
			guardC = guard.C
		case !on && guard != nil:
			guard.Stop()
			guard, guardC = nil, nil
		}
	}
	boost := newAudioBoost()
	defer func() {
		pl.Stop()
		boost.update(false)
		rl.close()
		os.Remove(nowFile)
		if keptAwake {
			os.Remove("/tmp/stay_awake")
		}
	}()
	// Scrobbling: "now playing" when a track starts, a play once half of it
	// (or 4 minutes) has played, as Last.fm counts them. The listen keeps
	// the time the song started, which is what Last.fm and ListenBrainz
	// file it under.
	scrobbled, counted := "", ""
	var cur Listen
	for {
		select {
		case <-quit:
			srv.Close()
			return
		case <-tick.C:
			menuPid = procs.Pid("MainUI")
			menuUp, inGame = menuPid > 0, procs.GameRunning()
			// Games: suspend, then resume afterwards.
			if inGame {
				pl.Suspend()
			} else {
				pl.Resume()
			}
			if at := sleepAt.Load(); at > 0 && time.Now().Unix() >= at {
				sleepAt.Store(0)
				log.Printf("sleep timer: pausing")
				pl.Pause()
			}
			st := pl.State()
			playing := st.Has && st.Playing && !st.Paused && !st.Suspended
			boost.update(playing)
			cfgMu.Lock()
			overlayOn = cfg.menuTicker() // Settings may have changed it
			toServer := cfg.serverScrobble()
			cfgMu.Unlock()
			if playing {
				key := fmt.Sprintf("%d/%s", st.Pos, st.Song.ID)
				if key != scrobbled {
					scrobbled = key
					cur = listenOf(st.Song, time.Now().Add(-st.Elapsed))
					if toServer {
						go sub.Scrobble(st.Song.ID, false)
					}
					sc.NowPlaying(cur)
				}
				half := time.Duration(st.Song.Duration) * time.Second / 2
				if key != counted && st.Song.Duration > 30 && st.Elapsed >= min(half, 4*time.Minute) {
					counted = key
					if toServer {
						go sub.Scrobble(st.Song.ID, true)
					}
					sc.Scrobble(cur)
				}
			}
			sc.Tick(playing)
			writeNow(st)
			// Keep the device awake while music plays. Only remove the flag
			// if this process set it: something else may want the device
			// awake too.
			if st.Has && st.Playing && !st.Paused {
				if _, err := os.Stat("/tmp/stay_awake"); err != nil {
					os.WriteFile("/tmp/stay_awake", nil, 0644)
					keptAwake = true
				}
			} else if keptAwake {
				os.Remove("/tmp/stay_awake")
				keptAwake = false
			}
			screenOpen := time.Since(time.Unix(lastAsked.Load(), 0)) < 30*time.Second
			if pl.IdleFor() > 10*time.Minute && !screenOpen {
				log.Printf("idle for 10 minutes: exiting")
				srv.Close()
				return
			}
		case <-frame.C:
			// Turned off while showing: one more pass takes it down.
			if ov == nil || (!overlayOn && !shown) {
				continue
			}
			st := pl.State()
			// Notice the menu closing (a game or app starting) within a
			// frame, not a second later.
			if menuUp {
				if !procs.Exists(menuPid) {
					menuUp = false
				}
			}
			// Only over the Onion menu: MiniTape and MiniRack draw the ticker
			// themselves, and games get the screen to themselves.
			show := overlayOn && st.Has && st.Playing && !st.Paused && menuUp && !inGame
			if show && !shown {
				ov.Begin() // waits for the status icons, then fades in
			}
			shown = show
			if show {
				ov.Draw(tickerText(st.Song))
			}
			setGuard(show)
		case <-guardC:
			ov.Guard()
		}
	}
}

func tickerText(s Song) string {
	parts := []string{s.Title}
	if s.Artist != "" {
		parts = append(parts, s.Artist)
	}
	if s.Album != "" {
		parts = append(parts, s.Album)
	}
	return strings.Join(parts, "   ·   ")
}

// writeNow updates the now-playing file other apps read for their ticker.
// It lives in RAM (/tmp) and is rewritten every second, so a failed write
// only means a missing ticker until the next one.
func writeNow(st NowPlaying) {
	if !st.Has || !st.Playing || st.Suspended {
		os.Remove(nowFile)
		return
	}
	b, err := json.Marshal(ui.Now{Text: tickerText(st.Song), Paused: st.Paused})
	if err != nil {
		return
	}
	if old, _ := os.ReadFile(nowFile); !bytes.Equal(old, b) {
		if os.WriteFile(nowFile+".tmp", b, 0644) == nil {
			os.Rename(nowFile+".tmp", nowFile)
		}
	}
}

// logins applies the server and login to the player's Subsonic client.
// The one read at startup arrives late (opening secrets.enc is slow on
// purpose), and must not replace a newer one that a /reload applied in the
// meantime.
type logins struct {
	mu       sync.Mutex
	reloaded bool
}

// initial runs apply, which sets the login read at startup, unless a
// reload has already set a newer one.
func (l *logins) initial(apply func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.reloaded {
		apply()
	}
}

// reload runs apply, which reads the settings again and sets them. Reloads
// run one at a time, so the settings read last are the ones that stay.
func (l *logins) reload(apply func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reloaded = true
	apply()
}

// ---- the screen's side: a client for the daemon ----

type remote struct {
	c     *http.Client
	addr  string       // the player's address
	start func() error // starts the player if it has stopped
	// The screen reads the player's state from here, kept fresh by a
	// background poll: drawing never waits on the player (which runs at a
	// lower priority, and over HTTP).
	mu      sync.Mutex
	last    NowPlaying
	misses  int // polls in a row that got no complete answer
	polling sync.Once
}

// staleAfter is how many failed polls in a row (at 250 ms each) it takes to
// treat the player as gone. A busy player that misses a poll or two keeps
// showing its last state rather than flickering to "nothing playing".
const staleAfter = 8

func newRemote(dir string) *remote {
	return &remote{c: &http.Client{Timeout: 3 * time.Second}, addr: daemonAddr,
		start: func() error { return ensureDaemon(dir) }}
}

func (r *remote) post(path string, body any) {
	send := func() error {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		resp, err := r.c.Post("http://"+r.addr+path, "application/json", rd)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	// Only a refused connection proves the player didn't get the request:
	// it had stopped (it does after a long idle), so start it and send
	// again. After a timeout the player may have acted on it already, and a
	// second /next would skip two tracks.
	if err := send(); dialFailed(err) && r.start() == nil {
		send()
	}
	r.fetch() // so the screen shows the result of the button at once
}

// dialFailed reports whether err is a failed connection attempt, which
// sent nothing.
func dialFailed(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

func (r *remote) Play(q []Song, i int) {
	r.post("/play", struct {
		Queue []Song
		Index int
	}{q, i})
}
func (r *remote) Toggle()    { r.post("/toggle", nil) }
func (r *remote) Next()      { r.post("/next", nil) }
func (r *remote) Prev()      { r.post("/prev", nil) }
func (r *remote) Stop()      { r.post("/stop", nil) }
func (r *remote) Seek(d int) { r.post("/seek?d="+strconv.Itoa(d), nil) }

// Sleep sets the sleep timer: minutes, -1 for the end of the song, 0 off.
func (r *remote) Sleep(min int) { r.post("/sleep?min="+strconv.Itoa(min), nil) }

// Quit stops the music and the background player. A player that isn't
// running is left that way.
func (r *remote) Quit() {
	if resp, err := r.c.Post("http://"+r.addr+"/quit", "application/json", nil); err == nil {
		resp.Body.Close()
	}
}
func (r *remote) Reload() { r.post("/reload", nil) }

// FlushScrobbles asks the player to send the listens waiting in the queue.
func (r *remote) FlushScrobbles() { r.post("/scrobble-flush", nil) }

// State returns the player's state as last polled. The first call asks the
// player directly and starts the poll.
func (r *remote) State() NowPlaying {
	r.polling.Do(func() {
		r.fetch()
		go func() {
			for {
				time.Sleep(250 * time.Millisecond)
				r.fetch()
			}
		}()
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// fetch polls the player once. Only a complete answer replaces the state:
// a reply cut short (the device under load) would otherwise give a song
// with its queue half filled in.
func (r *remote) fetch() {
	var st NowPlaying
	err := r.get(&st)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		r.last, r.misses = st, 0
		return
	}
	if r.misses++; r.misses >= staleAfter {
		r.last = NowPlaying{}
	}
}

func (r *remote) get(st *NowPlaying) error {
	resp, err := r.c.Get("http://" + r.addr + "/state")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("player said %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(st)
}

// daemonAlive reports whether the background player answers within wait.
func daemonAlive(wait time.Duration) bool {
	c := &http.Client{Timeout: wait}
	resp, err := c.Get("http://" + daemonAddr + "/state")
	if err == nil {
		resp.Body.Close()
	}
	return err == nil
}

// ensureDaemon starts the background player if it isn't running.
func ensureDaemon(dir string) error {
	alive := func() bool { return daemonAlive(500 * time.Millisecond) }
	if alive() {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// The daemon's log starts afresh past 64 KB (the last one kept as .old),
	// as launch.sh does for the screen's.
	logPath := filepath.Join(dir, "minitape-daemon.log")
	if fi, err := os.Stat(logPath); err == nil && fi.Size() > 64<<10 {
		os.Rename(logPath, logPath+".old")
	}
	cmd := exec.Command(exe, "daemon")
	cmd.Env = append(os.Environ(), "MINIDECK_APPDIR="+dir)
	cmd.Env = removeEnv(cmd.Env, "MINIDECK_UI_KEYS", "MINIDECK_UI_PNG", "MINIDECK_UI_NULL")
	if logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
		// (without a log, e.g. a read-only card, the output is dropped)
		cmd.Stdout, cmd.Stderr = logf, logf
		defer logf.Close() // the daemon has its own copy once started
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	for i := 0; i < 150; i++ { // up to 15 s on a slow start
		if alive() {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("the player didn't start (see minitape-daemon.log)")
}

func removeEnv(env []string, names ...string) []string {
	var out []string
	for _, e := range env {
		keep := true
		for _, n := range names {
			if strings.HasPrefix(e, n+"=") {
				keep = false
			}
		}
		if keep {
			out = append(out, e)
		}
	}
	return out
}
