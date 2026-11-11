package main

import (
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/zakijss/minideck/procs"
)

// Player plays a queue of songs through ffplay (no video), one process per
// track. Pause stops the process (SIGSTOP), resume continues it. Suspend
// (while a game runs) ends ffplay but remembers the position; Resume starts
// the track again from there.
type Player struct {
	mu        sync.Mutex
	queue     []Song
	pos       int
	cmd       *exec.Cmd
	paused    bool
	suspended bool
	offset    time.Duration // where in the track this ffplay started
	started   time.Time     // when this ffplay started
	pausedAt  time.Time
	pausedD   time.Duration // time spent paused since started
	gen       int           // bumps on every track change, to ignore stale exits
	err       string
	idleSince time.Time

	// url gives the stream URL for a song, starting at a whole second.
	url     func(s Song, at int) string
	ffplay  string
	changed func()
	// prefetch, if set, is told the song after the one starting, so its
	// stream can be fetched ahead.
	prefetch func(s Song)
}

// NewPlayer makes a player that streams each song from urlFor(song,
// seconds in). changed, if set, is called when a track ends by itself.
func NewPlayer(urlFor func(Song, int) string, changed func()) *Player {
	ff := os.Getenv("MINIDECK_FFPLAY")
	if ff == "" {
		ff = "/mnt/SDCARD/.tmp_update/bin/ffplay"
	}
	return &Player{url: urlFor, ffplay: ff, changed: changed, idleSince: time.Now()}
}

// ffplayEnv routes ffplay's audio through Onion's audioserver (the same
// shim the menu uses), so music can play while the menu or another app is
// open. Without the audioserver ffplay falls back to the device directly.
func ffplayEnv() []string {
	env := os.Environ()
	env = append(env, "LD_LIBRARY_PATH=/lib:/config/lib:/mnt/SDCARD/miyoo/lib:/mnt/SDCARD/.tmp_update/lib:/mnt/SDCARD/.tmp_update/lib/parasyte")
	if procs.Running("audioserver") {
		env = append(env, "LD_PRELOAD=/mnt/SDCARD/miyoo/lib/libpadsp.so")
	}
	return env
}

// Play replaces the queue and starts at index i.
func (p *Player) Play(q []Song, i int) {
	p.mu.Lock()
	p.queue = append([]Song(nil), q...)
	p.pos = i
	p.suspended = false
	p.startLocked(0)
	p.mu.Unlock()
}

func (p *Player) stopLocked() {
	p.gen++
	if p.cmd != nil && p.cmd.Process != nil {
		if p.paused {
			p.cmd.Process.Signal(syscall.SIGCONT)
		}
		p.cmd.Process.Kill()
	}
	p.cmd = nil
	p.paused = false
	p.idleSince = time.Now()
}

func (p *Player) elapsedLocked() time.Duration {
	if p.cmd == nil {
		return p.offset
	}
	e := p.offset + time.Since(p.started) - p.pausedD
	if p.paused {
		e -= time.Since(p.pausedAt)
	}
	return e
}

func (p *Player) startLocked(at time.Duration) {
	p.stopLocked()
	if p.pos < 0 || p.pos >= len(p.queue) {
		return
	}
	sec := int(at.Seconds())
	cmd := ffplayCommand(p.ffplay, "-nodisp", "-autoexit", "-loglevel", "quiet", p.url(p.queue[p.pos], sec))
	cmd.Env = ffplayEnv()
	if err := cmd.Start(); err != nil {
		p.err = "Can't start ffplay: " + err.Error()
		return
	}
	// ffplay runs with real-time scheduling (see realtime.go); without
	// chrt, a raised priority at least.
	if chrtPath == "" {
		syscall.Setpriority(syscall.PRIO_PROCESS, cmd.Process.Pid, -10)
	}
	p.err = ""
	p.cmd, p.started, p.pausedD, p.offset = cmd, time.Now(), 0, time.Duration(sec)*time.Second
	if p.prefetch != nil && p.pos+1 < len(p.queue) {
		p.prefetch(p.queue[p.pos+1])
	}
	gen := p.gen
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		if gen == p.gen { // the track ended by itself
			played := time.Since(p.started) - p.pausedD
			if err != nil && played < 3*time.Second {
				p.err = "Playback failed (server, network or format)."
				p.cmd = nil
				p.idleSince = time.Now()
			} else if p.pos+1 < len(p.queue) {
				p.pos++
				p.startLocked(0)
			} else {
				p.cmd = nil
				p.offset = 0
				p.idleSince = time.Now()
			}
		}
		p.mu.Unlock()
		if p.changed != nil {
			p.changed()
		}
	}()
}

// Toggle pauses or continues, or starts the current track again if it
// had stopped.
func (p *Player) Toggle() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.suspended {
		return
	}
	if p.cmd == nil || p.cmd.Process == nil {
		if len(p.queue) > 0 {
			p.startLocked(p.offset)
		}
		return
	}
	if p.paused {
		p.cmd.Process.Signal(syscall.SIGCONT)
		p.pausedD += time.Since(p.pausedAt)
		p.paused = false
	} else {
		p.cmd.Process.Signal(syscall.SIGSTOP)
		p.pausedAt = time.Now()
		p.paused = true
	}
}

// Next skips to the next track, if there is one.
func (p *Player) Next() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pos+1 < len(p.queue) {
		p.pos++
		p.startLocked(0)
	}
}

// Prev restarts the track, or goes to the previous one in the first 5 s.
func (p *Player) Prev() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.elapsedLocked() > 5*time.Second || p.pos == 0 {
		p.startLocked(0)
		return
	}
	p.pos--
	p.startLocked(0)
}

// Seek moves d seconds forward or back in the track, starting the stream
// again from there; past the end it goes to the next track. A paused track
// plays on from the new place.
func (p *Player) Seek(d int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.suspended || p.pos < 0 || p.pos >= len(p.queue) {
		return
	}
	at := max(0, p.elapsedLocked()+time.Duration(d)*time.Second)
	if dur := p.queue[p.pos].Duration; dur > 0 && at >= time.Duration(dur)*time.Second {
		if p.pos+1 < len(p.queue) {
			p.pos++
			p.startLocked(0)
		}
		return
	}
	p.startLocked(at)
}

// Stop ends playback and empties the queue.
func (p *Player) Stop() {
	p.mu.Lock()
	p.stopLocked()
	p.queue, p.pos, p.offset, p.suspended = nil, 0, 0, false
	p.mu.Unlock()
}

// Suspend ends playback for a game, keeping the place.
func (p *Player) Suspend() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.suspended || p.cmd == nil {
		return
	}
	wasPaused := p.paused
	at := p.elapsedLocked()
	p.stopLocked()
	p.offset = at
	p.paused = wasPaused
	p.suspended = true
}

// Resume continues after a game, where Suspend left off.
func (p *Player) Resume() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.suspended {
		return
	}
	p.suspended = false
	if p.paused { // it was paused before the game: stay paused
		p.paused = false
		return
	}
	p.startLocked(p.offset)
}

// IdleFor reports how long nothing has been playing (0 while playing).
func (p *Player) IdleFor() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd != nil || p.suspended {
		return 0
	}
	return time.Since(p.idleSince)
}

// NowPlaying is the player's state, as the daemon reports it to the screen.
type NowPlaying struct {
	Song      Song
	Has       bool
	Playing   bool
	Paused    bool
	Suspended bool
	Elapsed   time.Duration
	Pos       int
	Len       int
	Err       string
	Next      []Song
	// Kbps is the bit rate the song is streamed at (the setting, or lower
	// on a slow link), 0 when not known.
	Kbps int `json:"kbps,omitempty"`
}

// State returns the player's state, with up to three songs coming next.
func (p *Player) State() NowPlaying {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := NowPlaying{Pos: p.pos, Len: len(p.queue), Err: p.err, Paused: p.paused,
		Playing: p.cmd != nil, Suspended: p.suspended}
	if p.pos >= 0 && p.pos < len(p.queue) {
		n.Song, n.Has = p.queue[p.pos], true
		if p.pos+1 < len(p.queue) {
			n.Next = append([]Song(nil), p.queue[p.pos+1:min(len(p.queue), p.pos+4)]...)
		}
	}
	n.Elapsed = p.elapsedLocked()
	return n
}
