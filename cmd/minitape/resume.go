package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/zakijss/minideck/atomicfile"
)

// Resume on launch: the player keeps the queue in queue.json, written when
// it changes, and the place in it in place.json, a few bytes written every
// 10 s while music plays and when it stops. At start it sets them up
// again, paused: A plays on.

const (
	savedQueueFile = "queue.json"
	savedPlaceFile = "place.json"
)

type savedQueue struct {
	// Server is whose songs these are (see serverOf): the server's songs
	// are dropped when another server, or none, is set up now.
	Server string    `json:"server,omitempty"`
	Songs  []ownSong `json:"songs"`
}

type savedPlace struct {
	Pos    int    `json:"pos"`
	ID     string `json:"id"` // the song at Pos, in case the queue file is older
	Sec    int    `json:"sec"`
	Repeat int    `json:"repeat,omitempty"`
}

// serverOf names the server and user set up, "" for none.
func serverOf(cfg Settings) string {
	if cfg.URL == "" || cfg.LocalOnly {
		return ""
	}
	return cfg.URL + " " + cfg.User
}

// resumer saves the queue and the place when they change.
type resumer struct {
	dir   string
	qver  int // the queue version last saved
	place savedPlace
}

// save writes what changed since the last call. playing is whether the
// music is playing: while it does, the time is saved every 10 s.
func (r *resumer) save(pl *Player, st NowPlaying, playing bool, server string) {
	if st.Queue != r.qver {
		q, _ := pl.Queue()
		if err := writeJSON(filepath.Join(r.dir, savedQueueFile), savedQueue{server, ownSongs(q)}); err != nil {
			log.Printf("saving the queue: %v", err)
			return
		}
		r.qver = st.Queue
	}
	p := savedPlace{st.Pos, st.Song.ID, int(st.Elapsed.Seconds()), st.Repeat}
	moved := abs(p.Sec - r.place.Sec)
	if p.Pos == r.place.Pos && p.ID == r.place.ID && p.Repeat == r.place.Repeat && (moved == 0 || playing && moved < 10) {
		return
	}
	if err := writeJSON(filepath.Join(r.dir, savedPlaceFile), p); err != nil {
		log.Printf("saving the place: %v", err)
		return
	}
	r.place = p
}

func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, b, 0o644)
}

// loadResume reads the saved queue and place, keeping the songs that can
// still play: the card's whose files are there in the music folder root,
// and the server's if it is still the one set up (server). The place moves
// to the next song kept when its own is gone.
func loadResume(dir, server, root string) (q []Song, pos int, at time.Duration, repeat int) {
	var sq savedQueue
	var sp savedPlace
	b, err := os.ReadFile(filepath.Join(dir, savedQueueFile))
	if err != nil || json.Unmarshal(b, &sq) != nil || len(sq.Songs) == 0 {
		return nil, 0, 0, 0
	}
	songs := fromOwn(sq.Songs)
	if b, err := os.ReadFile(filepath.Join(dir, savedPlaceFile)); err == nil {
		json.Unmarshal(b, &sp) // without it: from the start
	}
	pos, at = sp.Pos, time.Duration(max(0, sp.Sec))*time.Second
	if pos < 0 || pos >= len(songs) || songs[pos].ID != sp.ID {
		if pos = slices.IndexFunc(songs, func(s Song) bool { return s.ID == sp.ID }); pos < 0 {
			pos, at = 0, 0
		}
	}
	newPos := -1
	for i, s := range songs {
		if s.Path != "" {
			p, ok := inside(s.Path, root)
			if !ok {
				continue
			}
			if _, err := os.Stat(p); err != nil {
				continue
			}
		} else if s.Radio == "" && (server == "" || sq.Server != server) { // stations play from anywhere
			continue
		}
		if i >= pos && newPos < 0 {
			newPos = len(q)
			if i != pos {
				at = 0
			}
		}
		q = append(q, s)
	}
	if newPos < 0 {
		newPos, at = 0, 0
	}
	return q, newPos, at, sp.Repeat
}
