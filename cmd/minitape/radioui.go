package main

import (
	"cmp"
	"fmt"
	"image/color"
	"log"
	"net/url"
	"strings"

	"github.com/zakijss/minideck/ui"
)

// ---- the radio screens ----

// radioHome is Radio on the first screen: the favourites, the server's
// stations (when it has some), the directory's lists, search and options.
func (a *app) radioHome() *view {
	stations := func(title string, q url.Values) func() {
		return func() { a.push(a.stationsBy(title, q)) }
	}
	groups := func(title, kind, icon string) func() {
		return func() { a.push(a.radioGroups(title, kind, icon)) }
	}
	items := []ui.Item{
		{Label: "Favourites", Icon: "star", Chevron: true, Value: func() {
			a.push(a.stationList("Favourites", "No favourites yet. X on a station stars it.",
				func() ([]Station, error) { return a.favs.all(), nil }))
		}},
		{Label: "Popular", Icon: "chart", Chevron: true, Sub: "The most votes", Value: stations("Popular", url.Values{"order": {"votes"}})},
		{Label: "Trending", Icon: "play", Chevron: true, Sub: "The most listened to lately", Value: stations("Trending", url.Values{"order": {"clicktrend"}})},
		{Label: "Genres", Icon: "note", Chevron: true, Sub: "Jazz, news, rock and more", Value: groups("Genres", "tags", "note")},
		{Label: "Countries", Icon: "sun", Chevron: true, Sub: "A to Z", Value: groups("Countries", "countries", "sun")},
		{Label: "Languages", Icon: "person", Chevron: true, Sub: "The most stations first", Value: groups("Languages", "languages", "person")},
		{Label: "Search", Icon: "eye", Chevron: true, Sub: "Stations by name", Value: func() { a.radioSearch() }},
		{Label: "Options", Icon: "gear", Chevron: true, Value: func() { a.radioOptions() }},
	}
	v := &view{title: "Radio", sub: "radio-browser.info", list: ui.List{Items: items},
		open: func(it *ui.Item) { it.Value.(func())() }}
	// the counts and the options can change in the screens it opens
	v.update = func(NowPlaying) {
		its := v.list.Items
		its[0].Sub = plural(len(a.favs.all()), "station")
		its[len(its)-1].Sub = "Sorted by " + strings.ToLower(sortLabel(*a.cfg))
	}
	if a.server {
		// The server's own stations, under Favourites, once it says it has
		// some (from the copy on the card at once, after the first time).
		go func() {
			st, err := a.sub.InternetRadioStations()
			if err != nil {
				log.Printf("radio: the server's stations: %v", err)
			}
			if len(st) == 0 {
				return
			}
			row := ui.Item{Label: "Your server", Icon: "server", Chevron: true, Sub: plural(len(st), "station"), Value: func() {
				a.push(a.stationList("Your server", "No stations on your server.", a.sub.InternetRadioStations))
			}}
			a.done <- func() {
				v.list.Items = append(v.list.Items[:1], append([]ui.Item{row}, v.list.Items[1:]...)...)
				if v.list.Sel >= 1 {
					v.list.Sel++
				}
			}
		}()
	}
	return v
}

// stationsBy lists the directory's stations matching q (see
// radioDir.stations).
func (a *app) stationsBy(title string, q url.Values) *view {
	hide := !a.cfg.RadioBroken
	return a.stationList(title, "No stations here.", func() ([]Station, error) {
		return a.radio.stations(q, hide, a.sub.fresh.Load() > 0)
	})
}

// stationList lists stations, filtered and sorted by Radio › Options. A
// plays a station, with the list as the queue, so L1 and R1 on Now Playing
// go to the stations next to it; X stars or unstars one. empty is shown for
// a list with no stations.
func (a *app) stationList(title, empty string, fetch func() ([]Station, error)) *view {
	opts := *a.cfg
	v := &view{title: title, hints: []ui.Hint{{Button: "A", Label: "Play"}, {Button: "X", Label: "Star"}}}
	var stations []Station // as listed
	v.open = func(it *ui.Item) {
		a.player.Play(v.songs, it.Value.(int))
		a.np = true
	}
	v.play = func() {
		if it := v.list.Selected(); it != nil {
			v.open(it)
		}
	}
	v.keys = func(b ui.Button) bool {
		if b != ui.X || v.loading {
			return false
		}
		if i := v.list.Sel; i >= 0 && i < len(stations) {
			a.toggleFavourite(stations[i])
		}
		return true
	}
	// a star on the favourites, starred here or on Now Playing
	v.update = func(NowPlaying) {
		for i := range v.list.Items {
			it := &v.list.Items[i]
			if k, ok := it.Value.(int); ok && k < len(v.songs) {
				it.Icon, it.Tint = "wifi", ui.Muted
				if a.favs.has(v.songs[k].ID) {
					it.Icon, it.Tint = "star", color.RGBA{} // its own yellow
				}
			}
		}
	}
	return a.load(v, func() ([]ui.Item, func(), error) {
		st, err := fetch()
		found := len(st)
		st = filterStations(st, opts)
		sortStations(st, opts.radioSort())
		songs := make([]Song, len(st))
		items := make([]ui.Item, len(st))
		for i, s := range st {
			songs[i] = s.song()
			items[i] = ui.Item{Label: songs[i].Title, Sub: stationSub(songs[i]), Icon: "wifi", Tint: ui.Muted, Value: i}
		}
		switch {
		case err != nil:
		case found > 0 && len(st) == 0:
			err = screenError("None of these stations match Radio › Options.")
		case len(st) == 0:
			err = screenError(empty)
		}
		sub := plural(len(st), "station")
		return items, func() { stations, v.songs, v.sub = st, songs, sub }, err
	})
}

// radioGroups lists the genres, countries or languages (kind, see
// radioDir.groups); A lists a group's stations.
func (a *app) radioGroups(title, kind, icon string) *view {
	v := &view{title: title}
	if kind == "countries" {
		v.letter = labelLetter
	}
	v.open = func(it *ui.Item) {
		a.push(a.stationsBy(it.Label, groupQuery(kind, it.Value.(radioGroup))))
	}
	return a.load(v, func() ([]ui.Item, func(), error) {
		gs, err := a.radio.groups(kind, a.sub.fresh.Load() > 0)
		items := make([]ui.Item, len(gs))
		for i, g := range gs {
			items[i] = ui.Item{Label: capital(strings.TrimSpace(g.Name)), Sub: plural(g.Count, "station"), Icon: icon, Tint: ui.Muted, Chevron: true, Value: g}
		}
		return items, nil, err
	})
}

// radioSearch asks for a station's name with the keyboard and lists the
// stations that match.
func (a *app) radioSearch() {
	q, ok := a.scr.Prompt(a.input, "Search stations", "", "A station's name", false)
	if q = strings.TrimSpace(q); !ok || q == "" {
		return
	}
	v := a.stationsBy("Search", url.Values{"name": {q}})
	v.sub = q
	a.push(v)
}

// radioOptions is Radio › Options: how lists of stations are sorted and
// which they leave out. Lists opened after a change follow it.
func (a *app) radioOptions() {
	menu(a.scr, a.input, "Radio options", func() []ui.Item { return radioOptionItems(*a.cfg) }, func(it ui.Item, n int) bool {
		old := *a.cfg
		if !stepRadio(a.cfg, it.Label, cmp.Or(n, 1)) {
			return false
		}
		if err := saveSettings(a.dir, a.cfg); err != nil {
			*a.cfg = old
			a.scr.Notice(a.input, "Couldn't save", err.Error())
		}
		return true
	})
}

// radioOptionItems are the rows of Radio › Options.
func radioOptionItems(cfg Settings) []ui.Item {
	broken := ui.Item{Label: "Broken stations", Icon: "cross", Detail: "Hidden", Sub: "Ones that failed their last check"}
	if cfg.RadioBroken {
		broken.Detail = "Shown"
	}
	formats := ui.Item{Label: "MP3 and AAC only", Icon: "note", Detail: "Off", Sub: "Also Ogg, Opus and FLAC streams"}
	if cfg.RadioMP3AAC {
		formats.Detail, formats.Sub = "On", "Leaves out Ogg, Opus and FLAC"
	}
	rate := ui.Item{Label: "Lowest bit rate", Icon: "wifi", Detail: "Any", Sub: "Leaves out stations below it"}
	if cfg.RadioMinKbps > 0 {
		rate.Detail = fmt.Sprintf("%d kbit/s", cfg.RadioMinKbps)
	}
	return []ui.Item{
		{Label: "Sort by", Icon: "chart", Detail: sortLabel(cfg), Sub: "Every list of stations"},
		broken, formats, rate,
	}
}

// toggleFavourite stars or unstars a station.
func (a *app) toggleFavourite(s Station) {
	if err := a.favs.toggle(s); err != nil {
		a.scr.Notice(a.input, "Couldn't save the favourites", err.Error())
	}
}
