package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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
	// endOfSong, set by the sleep timer, stops at the end of this track
	// instead of going on to the next.
	endOfSong bool
	repeat    int // repeatOff, repeatAll or repeatOne
	// plays counts the tracks started from the beginning: a song played
	// again is a new play to scrobble, though its place and id are the
	// same.
	plays int
	// qver counts the changes to the queue (a new one, a song removed or
	// moved), so the screen and the saved queue know to read it again.
	qver int

	// url gives the stream URL for a song, starting at a whole second.
	url     func(s Song, at int) string
	ffplay  string
	changed func()
	// prefetch, if set, is told the song after the one starting, so its
	// stream can be fetched ahead.
	prefetch func(s Song)
	// root, if set, gives the music folder: songs on the card play only
	// from inside it.
	root func() string
	// minPlayed is the least a song started from its beginning must play
	// to count as played rather than failed.
	minPlayed time.Duration
}

// The repeat modes: off, the queue (it starts over after its last song)
// and one (the song plays again).
const (
	repeatOff = iota
	repeatAll
	repeatOne
)

// NewPlayer makes a player that streams each song from urlFor(song,
// seconds in). changed, if set, is called when a track ends by itself.
func NewPlayer(urlFor func(Song, int) string, changed func()) *Player {
	ff := os.Getenv("MINIDECK_FFPLAY")
	if ff == "" {
		ff = "/mnt/SDCARD/.tmp_update/bin/ffplay"
	}
	return &Player{url: urlFor, ffplay: ff, changed: changed, idleSince: time.Now(), minPlayed: time.Second}
}

// inside cleans path and reports whether it is an absolute path in the
// folder root.
func inside(path, root string) (string, bool) {
	path, root = filepath.Clean(path), filepath.Clean(root)
	return path, filepath.IsAbs(path) && filepath.IsAbs(root) && strings.HasPrefix(path, strings.TrimSuffix(root, "/")+"/")
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
	p.qver++
	p.startLocked(0)
	p.mu.Unlock()
}

// Restore sets the queue saved at the last exit, paused at the place it
// was in song pos: Toggle (A) plays on from there.
func (p *Player) Restore(q []Song, pos int, at time.Duration, repeat int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(q) == 0 || pos < 0 || pos >= len(q) {
		return
	}
	p.stopLocked()
	p.queue, p.pos, p.offset, p.suspended = q, pos, max(0, at), false
	p.repeat = min(max(repeat, repeatOff), repeatOne)
	p.qver++
}

// Queue returns a copy of the queue and the place in it.
func (p *Player) Queue() ([]Song, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Song(nil), p.queue...), p.pos
}

// at reports whether song i of the queue is the one with id: the screen
// names a song by both, so a queue changed meanwhile isn't acted on.
func (p *Player) at(i int, id string) bool {
	return i >= 0 && i < len(p.queue) && p.queue[i].ID == id
}

// Jump plays song i of the queue from its start.
func (p *Player) Jump(i int, id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.at(i, id) {
		p.pos, p.suspended = i, false
		p.startLocked(0)
	}
}

// Remove takes song i out of the queue; not the one playing.
func (p *Player) Remove(i int, id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.at(i, id) || i == p.pos {
		return
	}
	p.queue = slices.Delete(p.queue, i, i+1)
	if i < p.pos {
		p.pos--
	}
	p.qver++
}

// Move swaps song i with the one d (-1 or 1) places away; the playing song
// keeps playing, at its new place.
func (p *Player) Move(i, d int, id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	j := i + d
	if !p.at(i, id) || j < 0 || j >= len(p.queue) || abs(d) != 1 {
		return
	}
	p.queue[i], p.queue[j] = p.queue[j], p.queue[i]
	switch p.pos {
	case i:
		p.pos = j
	case j:
		p.pos = i
	}
	p.qver++
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
	song := p.queue[p.pos]
	args := []string{"-nodisp", "-autoexit", "-loglevel", "quiet"}
	if song.Path != "" { // a file on the card: ffplay seeks in it
		root := defaultMusicDir
		if p.root != nil {
			root = p.root()
		}
		path, ok := inside(song.Path, root)
		if !ok {
			p.err = "Not a song in the music folder."
			return
		}
		if sec > 0 {
			args = append(args, "-ss", strconv.Itoa(sec))
		}
		// file: so a name can't be read as an option or another protocol
		args = append(args, "file:"+path)
	} else {
		args = append(args, p.url(song, sec))
	}
	cmd := ffplayCommand(p.ffplay, args...)
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
	if at == 0 {
		p.plays++
	}
	p.cmd, p.started, p.pausedD, p.offset = cmd, time.Now(), 0, time.Duration(sec)*time.Second
	if p.prefetch != nil && p.pos+1 < len(p.queue) && p.queue[p.pos+1].Path == "" {
		p.prefetch(p.queue[p.pos+1])
	}
	gen := p.gen
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		if gen == p.gen { // the track ended by itself
			played := time.Since(p.started) - p.pausedD
			// a song that ends at once from its start (empty, or a format
			// ffplay can't read) fails too: with repeat it would start
			// again and again, a play counted each time
			if err != nil && played < 3*time.Second || p.offset == 0 && played < p.minPlayed {
				p.err = "Playback failed (server, network or format)."
				p.cmd = nil
				p.idleSince = time.Now()
			} else {
				p.cmd = nil
				p.songEndedLocked()
			}
		}
		p.mu.Unlock()
		if p.changed != nil {
			p.changed()
		}
	}()
}

// songEndedLocked goes on after a track has finished (ffplay has
// exited): to the next one, the same one again (repeat one), the first
// (repeat all, at the end), or nowhere at the end of the queue or when the
// sleep timer stops after this song, whatever the repeat mode (A later
// plays the next one).
func (p *Player) songEndedLocked() {
	switch {
	case p.endOfSong:
		p.endOfSong = false
		if p.pos+1 < len(p.queue) {
			p.pos++
		}
	case p.repeat == repeatOne:
		p.startLocked(0)
		return
	case p.pos+1 < len(p.queue):
		p.pos++
		p.startLocked(0)
		return
	case p.repeat == repeatAll && len(p.queue) > 0:
		p.pos = 0
		p.startLocked(0)
		return
	}
	p.offset = 0
	p.idleSince = time.Now()
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

// Next skips to the next track, if there is one; with repeat all, from
// the last to the first.
func (p *Player) Next() {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.pos+1 < len(p.queue):
		p.pos++
		p.startLocked(0)
	case p.repeat == repeatAll && len(p.queue) > 0:
		p.pos = 0
		p.startLocked(0)
	}
}

// SetRepeat sets the repeat mode (repeatOff, repeatAll or repeatOne).
func (p *Player) SetRepeat(mode int) {
	p.mu.Lock()
	p.repeat = min(max(mode, repeatOff), repeatOne)
	p.mu.Unlock()
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
		p.stopLocked()
		p.songEndedLocked()
		return
	}
	p.restartLocked(at)
}

// restartLocked starts the track again at at, as the same play: back to
// 0:00 by seeking, or after a game, isn't a new one to scrobble.
func (p *Player) restartLocked(at time.Duration) {
	plays := p.plays
	p.startLocked(at)
	p.plays = plays
}

// StopAfterSong sets (or clears) stopping at the end of the current track.
func (p *Player) StopAfterSong(on bool) {
	p.mu.Lock()
	p.endOfSong = on
	p.mu.Unlock()
}

// Pause pauses, if playing.
func (p *Player) Pause() {
	p.mu.Lock()
	playing := p.cmd != nil && !p.paused && !p.suspended
	p.mu.Unlock()
	if playing {
		p.Toggle()
	}
}

// Stop ends playback and empties the queue.
func (p *Player) Stop() {
	p.mu.Lock()
	p.stopLocked()
	p.queue, p.pos, p.offset, p.suspended, p.endOfSong = nil, 0, 0, false, false
	p.qver++
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
	p.restartLocked(p.offset)
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
	// the sleep timer: seconds until the music pauses, or SleepEnd to stop
	// at the end of this song
	SleepLeft int  `json:"sleepLeft,omitempty"`
	SleepEnd  bool `json:"sleepEnd,omitempty"`
	Repeat    int  `json:"repeat,omitempty"`
	// Plays counts the tracks started from the beginning (see
	// Player.plays).
	Plays int `json:"plays"`
	// Queue changes when the queue does (see Player.qver).
	Queue int `json:"queue"`
}

// State returns the player's state, with up to three songs coming next.
func (p *Player) State() NowPlaying {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := NowPlaying{Pos: p.pos, Len: len(p.queue), Err: p.err, Paused: p.paused,
		Playing: p.cmd != nil, Suspended: p.suspended, SleepEnd: p.endOfSong, Repeat: p.repeat, Plays: p.plays, Queue: p.qver}
	if p.pos >= 0 && p.pos < len(p.queue) {
		n.Song, n.Has = p.queue[p.pos], true
		if p.pos+1 < len(p.queue) {
			n.Next = append([]Song(nil), p.queue[p.pos+1:min(len(p.queue), p.pos+4)]...)
		}
	}
	n.Elapsed = p.elapsedLocked()
	return n
}
