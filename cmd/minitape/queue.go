package main

import "github.com/zakijss/minideck/ui"

// queueView lists the queue (Down on Now Playing), the current song
// marked. A plays a song, X takes one out (not the current one), L2 and R2
// move it up and down; B goes back to Now Playing. The list is read from
// the player again whenever the queue changes.
func (a *app) queueView() *view {
	v := &view{title: "Queue", loading: true,
		hints: []ui.Hint{{Button: "A", Label: "Play"}, {Button: "X", Label: "Remove"}, {Button: "L2/R2", Label: "Move"}}}
	var songs []Song
	qver, first := -1, true
	// seq numbers the reads of the queue: an older one that answers late
	// is dropped
	seq := 0
	// moved is the song L2 or R2 moved and the place it went to: the
	// selection goes there once the queue read shows it (not when the
	// player refused the move)
	var moved struct {
		i  int
		id string
	}
	v.update = func(st NowPlaying) {
		if st.Queue != qver {
			qver = st.Queue
			seq++
			mine := seq
			go func() {
				q, pos, err := a.player.Queue()
				a.done <- func() {
					if mine != seq {
						return
					}
					v.loading = false
					if err != nil {
						v.err = err.Error()
						return
					}
					songs, v.err = q, ""
					v.list.Items = a.songItems(q)
					v.sub = songCount(len(q))
					if first { // start on the current song
						v.list.Sel, first = pos, false
					}
					if moved.id != "" && moved.i < len(q) && q[moved.i].ID == moved.id {
						v.list.Sel = moved.i
					}
					moved.id = ""
					if len(q) == 0 {
						v.err = "Nothing in the queue."
					}
				}
			}()
		}
		for i := range v.list.Items {
			it := &v.list.Items[i]
			switch {
			case i >= len(songs):
			case i != st.Pos:
				it.Detail = durText(songs[i])
			case st.Playing && !st.Paused:
				it.Detail = "playing"
			default:
				it.Detail = "paused"
			}
		}
	}
	// sel is the selected song's place and id.
	sel := func() (int, string, bool) {
		i := v.list.Sel
		if v.loading || i < 0 || i >= len(songs) || i >= len(v.list.Items) {
			return 0, "", false
		}
		return i, songs[i].ID, true
	}
	v.open = func(it *ui.Item) {
		if i, id, ok := sel(); ok {
			a.player.Jump(i, id)
			a.stack = a.stack[:len(a.stack)-1]
			a.np = true
		}
	}
	v.keys = func(b ui.Button) bool {
		switch b {
		case ui.B:
			a.stack = a.stack[:len(a.stack)-1]
			a.np = true
		case ui.X:
			if i, id, ok := sel(); ok {
				a.player.Remove(i, id)
			}
		case ui.L2, ui.R2:
			d := 1
			if b == ui.L2 {
				d = -1
			}
			if i, id, ok := sel(); ok && i+d >= 0 && i+d < len(songs) {
				moved.i, moved.id = i+d, id
				a.player.Move(i, d, id)
			}
		default:
			return false
		}
		return true
	}
	return v
}
