// MiniTape plays music from any Subsonic-compatible server (Navidrome,
// Subsonic, Airsonic, Gonic, ...) over your tailnet or LAN. Playback goes
// through the device's ffplay, in a background service.
package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"image"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zakijss/minideck/appdir"
	"github.com/zakijss/minideck/cache"
	"github.com/zakijss/minideck/secrets"
	"github.com/zakijss/minideck/tnet"
	"github.com/zakijss/minideck/ui"
	"github.com/zakijss/minideck/version"
)

// Settings is settings.json: the server, the login and the stream format.
type Settings struct {
	URL    string `json:"url"`
	User   string `json:"user"`
	Secret string `json:"secret"` // key in secrets.enc holding the password
	Format string `json:"format"`
	// MaxBitRate is the bit rate songs are asked for (kbit/s; 0 means
	// 192). The relay steps down from it when the link can't keep up.
	MaxBitRate int `json:"max_bitrate"`
	// SongCacheMB is how much of the card downloaded songs may use, in
	// MB (0 means 500).
	SongCacheMB int `json:"song_cache_mb,omitempty"`
	// MenuTicker shows what's playing across the top of the Onion menu
	// (default on).
	MenuTicker *bool `json:"menu_ticker,omitempty"`
	// Background keeps the music playing after MiniTape is closed, in the
	// Onion menu and other apps (default on). Off, closing MiniTape stops
	// it.
	Background *bool `json:"background_playback,omitempty"`
	// CoverStyle is "pixel" (the default: covers redrawn in the
	// interface's pixel style) or "smooth" (as the server sends them).
	CoverStyle string `json:"cover_style,omitempty"`
	// ServerScrobble sends now playing and plays to the music server
	// (default on). Last.fm and ListenBrainz are on when their logins are
	// in secrets.enc; the names they belong to are kept here for Settings.
	ServerScrobble   *bool  `json:"scrobble_server,omitempty"`
	ListenBrainzUser string `json:"listenbrainz_user,omitempty"`
	LastFMUser       string `json:"lastfm_user,omitempty"`
}

// pixelCovers reports whether covers are drawn in the pixel style.
func (s Settings) pixelCovers() bool { return s.CoverStyle != "smooth" }

// coverLevels is how many levels each colour channel of a pixel-style
// cover keeps.
const coverLevels = 6

// background reports whether the music keeps playing after MiniTape is
// closed.
func (s Settings) background() bool { return s.Background == nil || *s.Background }

// serverScrobble reports whether plays go to the music server.
func (s Settings) serverScrobble() bool { return s.ServerScrobble == nil || *s.ServerScrobble }

// menuTicker reports whether the song shows across the top of the Onion
// menu.
func (s Settings) menuTicker() bool { return s.MenuTicker == nil || *s.MenuTicker }

// bitRates are the choices in Settings › Streaming quality, in kbit/s.
var bitRates = []int{96, 128, 192, 256, 320}

// bitRate is the bit rate songs are asked for, before any step down.
func (s Settings) bitRate() int {
	if s.MaxBitRate <= 0 {
		return 192
	}
	return s.MaxBitRate
}

// songCaches are the choices in Settings › Downloaded songs, in MB.
var songCaches = []int{100, 250, 500, 1024}

// songCache is how many bytes of the card downloaded songs may use.
func (s Settings) songCache() int64 {
	mb := s.SongCacheMB
	if mb <= 0 {
		mb = 500
	}
	return int64(mb) << 20
}

// step returns the choice step places from cur in choices, wrapping
// around. A value that isn't one of the choices (set by hand in
// settings.json) steps from the nearest one.
func step(choices []int, cur, n int) int {
	i := 0
	for j, c := range choices {
		if abs(c-cur) < abs(choices[i]-cur) {
			i = j
		}
	}
	if choices[i] != cur && n != 0 {
		// cur lies between choices: the first step lands on a neighbour
		// in the direction asked, not past it.
		if (n > 0) == (choices[i] > cur) {
			n -= sign(n)
		}
	}
	k := len(choices)
	return choices[((i+n)%k+k)%k]
}

func abs(n int) int { return max(n, -n) }

func sign(n int) int {
	if n < 0 {
		return -1
	}
	return 1
}

// loadSettings reads settings.json. A missing or unreadable file gives the
// defaults, and the setup screen then asks for the server.
func loadSettings(dir string) Settings {
	defaults := Settings{Format: "mp3", MaxBitRate: 192, Secret: "navidrome"}
	cfg := defaults
	b, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("settings: %v", err)
		}
		return cfg
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		log.Printf("settings.json: %v", err)
		return defaults
	}
	return cfg
}

// playerCtl is the background player, as the screen sees it.
type playerCtl interface {
	Play(q []Song, i int)
	Toggle()
	Next()
	Prev()
	Stop()
	State() NowPlaying
}

// view is one screen on the navigation stack.
type view struct {
	title   string
	sub     string
	list    ui.List
	loading bool
	err     string
	open    func(it *ui.Item) // A on an item
	play    func()            // START: play everything shown
	toNP    bool              // set by a loader: replace this view with Now Playing

	// Album and playlist pages show the cover and details on the left, and
	// a short kind ("Album") in the header instead of repeating the name.
	kind  string
	art   string
	info  []string
	year  int
	songs []Song
}

type app struct {
	scr    *ui.Screen
	sub    *Subsonic
	player playerCtl
	stack  []*view
	np     bool // showing Now Playing
	poke   func()
	// done carries work finished in the background back to the event loop,
	// which applies it: views are changed only there, where draw reads
	// them.
	done chan func()

	coverMu   sync.Mutex
	covers    map[coverKey]image.Image
	failed    map[coverKey]time.Time
	pending   map[coverKey]bool
	backdrops map[string]*image.RGBA
	// covers redrawn in the pixel style, at the size drawn (see shown)
	pixelMu    sync.Mutex
	pixels     map[pixelKey]image.Image
	pixelBytes int
	listThumb  int // the thumbnail size of the list being drawn
	fetchSem   chan struct{}
	coverOrder []coverKey // covers in memory, oldest first

	settings func()
	cfg      *Settings // for the stream format shown on Now Playing

	// lyrics, on Now Playing (Y)
	lyricsOn  bool
	lyrMu     sync.Mutex
	lyr       map[string]*songLyrics
	lyrScroll int // first line shown, for lyrics without times
	lyrShown  int // the line highlighted in the last draw
	lyrTick   int
}

type songLyrics struct {
	loading bool
	lines   []LyricLine
	synced  bool
	err     string
	failed  time.Time // when err was set
}

// lyricsRetry is how long a failed lyrics fetch is shown before it is
// tried again. Turning the lyrics off and on again (Y twice) retries at
// once.
const lyricsRetry = time.Minute

func main() {
	version.Flag("minitape")
	if len(os.Args) > 1 && os.Args[1] == "daemon" {
		version.Note("minitape daemon")
		runDaemon(appdir.Dir())
		return
	}
	version.Note("minitape")
	// The screen draws at a lower priority than the music.
	syscall.Setpriority(syscall.PRIO_PROCESS, 0, 5)
	scr, err := ui.Open()
	if err != nil {
		log.Fatal(err)
	}
	defer scr.Close()
	input := ui.Input()
	exit := ui.Hint{Button: "B", Label: "Exit"}

	dir := appdir.Dir()
	cfg := loadSettings(dir)
	ui.TraceMark("screen open")
	// Unreadable (new device?): ask again, keeping the old file aside.
	sec, err := secrets.LoadOrSetAside(filepath.Join(dir, "secrets.enc"))
	if err != nil {
		log.Printf("secrets: %v", err)
	}
	ui.TraceMark("secrets read")
	if cfg.URL == "" || cfg.User == "" || sec[cfg.Secret] == "" {
		if !setup(scr, input, dir, &cfg, sec, true) {
			return
		}
	}

	redraw := make(chan struct{}, 1)
	a := &app{scr: scr, covers: map[coverKey]image.Image{}, pending: map[coverKey]bool{}, failed: map[coverKey]time.Time{},
		backdrops: map[string]*image.RGBA{}, fetchSem: make(chan struct{}, 3),
		cfg: &cfg, lyr: map[string]*songLyrics{}, done: make(chan func(), 8)}
	a.poke = func() {
		select {
		case redraw <- struct{}{}:
		default:
		}
	}
	a.sub = &Subsonic{Base: cfg.URL, User: cfg.User, Password: sec[cfg.Secret], HTTP: tnet.Client(20 * time.Second),
		Cache: cache.New(filepath.Join(dir, "cache"))}
	go a.sub.Cache.Prune(150 << 20) // covers and lists, least recently used first
	// The music itself plays in a background service, so it keeps going
	// after this screen closes.
	if !daemonAlive(300 * time.Millisecond) {
		scr.Alert("MiniTape", "Starting the player…")
	}
	err = ensureDaemon(dir)
	ui.TraceMark("player ready")
	if err != nil {
		scr.Message("MiniTape", "Can't start the player: "+err.Error(), exit)
		waitExit(input)
		return
	}
	rp := newRemote(dir)
	a.player = rp
	// With background playback off, the music stops as MiniTape closes.
	defer func() {
		if !cfg.background() {
			rp.Quit()
		}
	}()
	a.settings = func() {
		scrobbling := &scrobbleUI{scr: scr, input: input, dir: dir, cfg: &cfg, sec: sec, http: scrobbleClient(),
			reload: rp.Reload, flush: rp.FlushScrobbles}
		songs := filepath.Join(dir, "streams")
		items := func() []ui.Item { return settingsItems(cfg, scrobbling.status().summary(), dirSize(songs)) }
		// save writes settings.json; on an error the change is undone.
		save := func(old Settings) {
			if err := saveSettings(dir, &cfg); err != nil {
				cfg = old
				scr.Notice(input, "Couldn't save", err.Error())
			}
		}
		before := cfg
		// n is 0 for A, -1 and 1 for left and right.
		menu(scr, input, "Settings", items, func(it ui.Item, n int) bool {
			if old := cfg; stepSetting(&cfg, it.Label, cmp.Or(n, 1)) {
				save(old)
				return true
			}
			if n != 0 {
				return false
			}
			switch it.Label {
			case "Clear downloaded songs":
				if dirSize(songs) > 0 && scr.Confirm(input, "Clear downloaded songs?", "They download again the next time they play.", "Clear") {
					cache.New(songs).Prune(0)
				}
			case "Appearance":
				scr.Appearance(input)
			case "Music server":
				if setup(scr, input, dir, &cfg, sec, false) {
					a.player.Stop()
					rp.Reload()
					a.sub.SetLogin(cfg.URL, cfg.User, sec[cfg.Secret])
					a.stack = a.stack[:1]
				}
			case "Scrobbling":
				scrobbling.run()
			}
			return false
		})
		// The player reads these; one reload when Settings closes, not one
		// per press (it opens the password again).
		if cfg.bitRate() != before.bitRate() || cfg.songCache() != before.songCache() || cfg.menuTicker() != before.menuTicker() {
			rp.Reload()
		}
	}
	// Opening MiniTape while music plays goes straight to it.
	if st := a.player.State(); st.Has && st.Playing {
		a.np = true
	}
	a.push(a.home())

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	marquee := ui.TickerClock() // the now-playing strip's frames
	defer marquee.Stop()
	skipDraw := false
	for {
		if !skipDraw {
			a.draw()
		}
		skipDraw = false
		var frame <-chan time.Time
		if ui.NeedsFrame() {
			frame = time.After(ui.FrameDelay)
		}
		select {
		case <-frame:
		case ev, ok := <-input:
			if !ok {
				return
			}
			if !ev.Pressed {
				continue
			}
			if ev.Button == ui.Menu || (ev.Button == ui.B && !a.np && len(a.stack) == 1) {
				return
			}
			a.handle(ev)
		case <-redraw:
		case apply := <-a.done:
			apply()
		case <-tick.C:
		case <-marquee.C:
			skipDraw = true
			if a.np && a.lyricsOn {
				// timed lyrics: redraw when the line changes (checked ~3x/s)
				if a.lyrTick++; a.lyrTick%6 == 0 {
					st := a.player.State()
					if l := a.lyricsFor(st.Song, false); l != nil && l.synced && currentLine(l.lines, st.Elapsed) != a.lyrShown {
						skipDraw = false
					}
				}
				break
			}
			// Animate only the now-playing strip; no full redraw.
			if !a.np && a.player.State().Has {
				scr.DrawTicker()
				scr.Present()
			}
		}
	}
}

// stepSetting changes the Settings row named label by n steps (1 for A
// and right, -1 for left). It reports whether the row is one with a value;
// the others open a screen or ask first.
func stepSetting(cfg *Settings, label string, n int) bool {
	switch label {
	case "Streaming quality":
		cfg.MaxBitRate = step(bitRates, cfg.bitRate(), n)
	case "Downloaded songs":
		cfg.SongCacheMB = step(songCaches, int(cfg.songCache()>>20), n)
	case "Background playback":
		on := !cfg.background()
		cfg.Background = &on
	case "Menu ticker":
		on := !cfg.menuTicker()
		cfg.MenuTicker = &on
	case "Cover art":
		if cfg.pixelCovers() {
			cfg.CoverStyle = "smooth"
		} else {
			cfg.CoverStyle = "pixel"
		}
	default:
		return false
	}
	return true
}

// settingsItems is the Settings menu; scrobble sums up the scrobbling
// setup and songs is how many bytes of downloaded songs are on the card.
// It is built again after every change, so the rows stay current.
func settingsItems(cfg Settings, scrobble string, songs int64) []ui.Item {
	return []ui.Item{
		{Label: "Music server", Sub: serverLine(cfg), Icon: "server", Chevron: true},
		{Label: "Scrobbling", Sub: scrobble, Icon: "chart", Chevron: true},
		qualityItem(cfg),
		songCacheItem(cfg),
		clearSongsItem(songs),
		backgroundItem(cfg),
		menuTickerItem(cfg),
		coverStyleItem(cfg),
		ui.AppearanceItem(),
	}
}

// qualityItem is the Streaming quality row in Settings.
func qualityItem(cfg Settings) ui.Item {
	it := ui.Item{Label: "Streaming quality", Icon: "wifi", Detail: fmt.Sprintf("%d kbit/s", cfg.bitRate()),
		Sub: "Lower when Wi-Fi is slow"}
	if cfg.bitRate() <= bitRates[0] {
		it.Sub = "The least data"
	}
	return it
}

// songCacheItem is the Downloaded songs row in Settings: how much of the
// card songs kept for offline play may use.
func songCacheItem(cfg Settings) ui.Item {
	return ui.Item{Label: "Downloaded songs", Icon: "note", Detail: sizeText(cfg.songCache()),
		Sub: "Kept to play offline"}
}

// clearSongsItem is the row that deletes the downloaded songs.
func clearSongsItem(songs int64) ui.Item {
	it := ui.Item{Label: "Clear downloaded songs", Icon: "cross", Sub: sizeText(songs) + " on the card now"}
	if songs == 0 {
		it.Sub, it.Tint = "None on the card", ui.Faint
	}
	return it
}

// menuTickerItem is the Menu ticker row in Settings.
func menuTickerItem(cfg Settings) ui.Item {
	it := ui.Item{Label: "Menu ticker", Icon: "eye", Detail: "On", Sub: "The song shows over the Onion menu"}
	if !cfg.menuTicker() {
		it.Detail, it.Sub = "Off", "Nothing shows over the Onion menu"
	}
	return it
}

// sizeText writes a size on the card: "320 MB", "1 GB", "1.4 GB".
func sizeText(n int64) string {
	switch {
	case n >= 1<<30:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/(1<<30)), ".0") + " GB"
	case n >= 1<<20:
		return fmt.Sprintf("%d MB", n>>20)
	case n > 0:
		return "Under 1 MB"
	}
	return "0 MB"
}

// dirSize is how many bytes the files in dir take.
func dirSize(dir string) int64 {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var n int64
	for _, e := range ents {
		if fi, err := e.Info(); err == nil && !e.IsDir() {
			n += fi.Size()
		}
	}
	return n
}

// backgroundItem is the Background playback row in Settings: A switches
// it.
func backgroundItem(cfg Settings) ui.Item {
	it := ui.Item{Label: "Background playback", Icon: "play", Detail: "On", Sub: "Plays on after you leave MiniTape"}
	if !cfg.background() {
		it.Detail, it.Sub = "Off", "Stops when you leave MiniTape"
	}
	return it
}

// coverStyleItem is the Cover art row in Settings: A switches the style.
func coverStyleItem(cfg Settings) ui.Item {
	it := ui.Item{Label: "Cover art", Icon: "disc", Detail: "Pixel", Sub: "Drawn in pixels, like the menus"}
	if !cfg.pixelCovers() {
		it.Detail, it.Sub = "Smooth", "As your server sends them"
	}
	return it
}

// serverLine describes the login for Settings: "alex on homeserver:4533".
func serverLine(cfg Settings) string {
	host := strings.TrimPrefix(strings.TrimPrefix(cfg.URL, "http://"), "https://")
	if cfg.User == "" || host == "" {
		return "Address and login"
	}
	return cfg.User + " on " + strings.TrimSuffix(host, "/")
}

func waitExit(input <-chan ui.Event) {
	for ev := range input {
		if ev.Pressed && (ev.Button == ui.B || ev.Button == ui.Menu) {
			return
		}
	}
}

func (a *app) top() *view { return a.stack[len(a.stack)-1] }

func (a *app) push(v *view) { a.stack = append(a.stack, v) }

// load runs fn in the background with v showing a loading state. fn
// returns the list's items and, if set, fill: what else of v the result
// changes. Both are applied to v by the event loop (see app.done).
func (a *app) load(v *view, fn func() (items []ui.Item, fill func(), err error)) *view {
	v.loading = true
	go func() {
		items, fill, err := fn()
		a.done <- func() {
			if fill != nil {
				fill()
			}
			v.list.Items, v.loading = items, false
			if err != nil {
				v.err = err.Error()
			} else if len(items) == 0 {
				v.err = "Nothing here yet."
			}
		}
	}()
	return v
}

func (a *app) home() *view {
	albums := func(title, typ string) func() {
		return func() { a.push(a.albumList(title, typ)) }
	}
	items := []ui.Item{
		{Label: "Shuffle everything", Icon: "shuffle", Sub: "Start playing right away", Value: func() { a.shuffleAll() }},
		{Label: "Recently added", Sub: "The newest albums", Icon: "disc", Chevron: true, Value: albums("Recently added", "newest")},
		{Label: "Random albums", Sub: "Something different", Icon: "dice", Chevron: true, Value: albums("Random albums", "random")},
		{Label: "Most played", Sub: "Your favourites", Icon: "chart", Chevron: true, Value: albums("Most played", "frequent")},
		{Label: "Recently played", Sub: "Pick up where you left off", Icon: "clock", Chevron: true, Value: albums("Recently played", "recent")},
		{Label: "Starred", Sub: "Albums and songs you starred", Icon: "star", Chevron: true, Value: func() { a.push(a.starred()) }},
		{Label: "Artists", Icon: "person", Chevron: true, Sub: "Everyone, A to Z", Value: func() { a.push(a.artists()) }},
		{Label: "Playlists", Icon: "list", Chevron: true, Sub: "Your playlists", Value: func() { a.push(a.playlists()) }},
		{Label: "Settings", Icon: "gear", Sub: "Server, scrobbling and appearance", Chevron: true, Value: func() { a.settings() }},
	}
	return &view{title: "MiniTape", list: ui.List{Items: items},
		open: func(it *ui.Item) { it.Value.(func())() }}
}

func (a *app) albumItems(al []Album) []ui.Item {
	items := make([]ui.Item, len(al))
	for i, x := range al {
		sub := x.Artist
		if x.Year > 0 {
			sub = fmt.Sprintf("%s · %d", x.Artist, x.Year)
		}
		id := x.CoverArt
		if id == "" {
			id = x.ID
		}
		items[i] = ui.Item{Label: x.Name, Sub: sub, Value: x, Image: func() image.Image { return a.shown(a.thumb(id), a.listThumb) }}
	}
	return items
}

func (a *app) openAlbum(it *ui.Item) {
	al := it.Value.(Album)
	v := a.songList(al.Name, true, func() ([]Song, error) {
		full, err := a.sub.Album(al.ID)
		if err != nil {
			return nil, err
		}
		return full.Song, nil
	})
	v.art = al.CoverArt
	if v.art == "" {
		v.art = al.ID
	}
	v.kind = "Album"
	v.info = []string{al.Artist}
	v.year = al.Year
	a.push(v)
}

// playAlbum starts an album straight from a list (START), no extra screen.
func (a *app) playAlbum(al Album) {
	go func() {
		full, err := a.sub.Album(al.ID)
		if err != nil || len(full.Song) == 0 {
			return
		}
		a.player.Play(full.Song, 0)
		a.poke()
	}()
	a.np = true
}

func (a *app) albumList(title, typ string) *view {
	v := &view{title: title, open: a.openAlbum}
	v.play = func() {
		if it := v.list.Selected(); it != nil {
			if al, ok := it.Value.(Album); ok {
				a.playAlbum(al)
			}
		}
	}
	return a.load(v, func() ([]ui.Item, func(), error) {
		al, err := a.sub.AlbumList(typ, 100)
		sub := fmt.Sprintf("%d albums", len(al))
		return a.albumItems(al), func() { v.sub = sub }, err
	})
}

func (a *app) starred() *view {
	v := &view{title: "Starred"}
	v.open = func(it *ui.Item) {
		switch x := it.Value.(type) {
		case Album:
			a.openAlbum(it)
		case int:
			a.player.Play(v.songs, x)
			a.np = true
		}
	}
	v.play = func() {
		it := v.list.Selected()
		if it == nil {
			return
		}
		if al, ok := it.Value.(Album); ok {
			a.playAlbum(al)
		} else {
			v.open(it)
		}
	}
	return a.load(v, func() ([]ui.Item, func(), error) {
		al, s, err := a.sub.Starred()
		items := a.albumItems(al)
		for i, x := range s {
			id := x.CoverArt
			items = append(items, ui.Item{Label: x.Title, Sub: x.Artist, Value: i, Image: func() image.Image { return a.shown(a.thumb(id), a.listThumb) }})
		}
		return items, func() { v.songs = s }, err
	})
}

func (a *app) artists() *view {
	v := &view{title: "Artists"}
	v.open = func(it *ui.Item) {
		ar := it.Value.(Artist)
		av := &view{title: ar.Name, open: a.openAlbum}
		a.push(a.load(av, func() ([]ui.Item, func(), error) {
			full, err := a.sub.Artist(ar.ID)
			if err != nil {
				return nil, nil, err
			}
			sub := fmt.Sprintf("%d albums", len(full.Album))
			return a.albumItems(full.Album), func() { av.sub = sub }, nil
		}))
	}
	return a.load(v, func() ([]ui.Item, func(), error) {
		ars, err := a.sub.Artists()
		sub := fmt.Sprintf("%d artists", len(ars))
		items := make([]ui.Item, len(ars))
		for i, x := range ars {
			n := "1 album"
			if x.AlbumCount != 1 {
				n = fmt.Sprintf("%d albums", x.AlbumCount)
			}
			items[i] = ui.Item{Label: x.Name, Sub: n, Icon: "person", Tint: ui.Muted, Value: x}
		}
		return items, func() { v.sub = sub }, err
	})
}

func (a *app) playlists() *view {
	v := &view{title: "Playlists"}
	v.open = func(it *ui.Item) {
		pl := it.Value.(Playlist)
		pv := a.songList(pl.Name, false, func() ([]Song, error) {
			full, err := a.sub.Playlist(pl.ID)
			if err != nil {
				return nil, err
			}
			return full.Entry, nil
		})
		pv.art = "pl-" + pl.ID
		pv.kind = "Playlist"
		pv.info = []string{"Playlist"}
		a.push(pv)
	}
	return a.load(v, func() ([]ui.Item, func(), error) {
		pls, err := a.sub.Playlists()
		items := make([]ui.Item, len(pls))
		for i, x := range pls {
			items[i] = ui.Item{Label: x.Name, Sub: fmt.Sprintf("%d songs", x.SongCount), Icon: "list", Value: x}
		}
		return items, nil, err
	})
}

func fmtDur(sec int) string { return fmt.Sprintf("%d:%02d", sec/60, sec%60) }

func totalDur(s []Song) string {
	t := 0
	for _, x := range s {
		t += x.Duration
	}
	if t >= 3600 {
		return fmt.Sprintf("%d h %d min", t/3600, t%3600/60)
	}
	return fmt.Sprintf("%d min", (t+30)/60)
}

// songList shows tracks; A plays from the selected one, START plays all.
func (a *app) songList(title string, numbered bool, fetch func() ([]Song, error)) *view {
	v := &view{title: title}
	v.open = func(it *ui.Item) {
		a.player.Play(v.songs, it.Value.(int))
		a.np = true
	}
	// START (play) does the same as A: play from the highlighted song.
	v.play = func() {
		if it := v.list.Selected(); it != nil {
			v.open(it)
		}
	}
	return a.load(v, func() ([]ui.Item, func(), error) {
		s, err := fetch()
		items := make([]ui.Item, len(s))
		for i, x := range s {
			label := x.Title
			if numbered && x.Track > 0 {
				label = trackLabel(x.Track, x.Title)
			}
			items[i] = ui.Item{Label: label, Detail: fmtDur(x.Duration), Value: i}
		}
		// The caller sets the year and info after songList returns: read
		// them when the result is applied, not here.
		fill := func() {
			v.songs = s
			if len(s) > 0 {
				line := fmt.Sprintf("%d songs · %s", len(s), totalDur(s))
				if v.year > 0 {
					line = fmt.Sprintf("%d · %s", v.year, line)
				}
				v.info = append(v.info, line)
			}
		}
		return items, fill, err
	})
}

func (a *app) shuffleAll() {
	v := &view{title: "Shuffle everything", loading: true}
	a.push(v)
	go func() {
		s, err := a.sub.RandomSongs(200)
		if err == nil && len(s) > 0 {
			rand.Shuffle(len(s), func(i, j int) { s[i], s[j] = s[j], s[i] })
			a.player.Play(s, 0)
		}
		a.done <- func() {
			v.loading = false
			switch {
			case err != nil:
				v.err = err.Error()
			case len(s) == 0:
				v.err = "No songs."
			default:
				v.toNP = true // draw pops this view
			}
		}
	}()
}

func (a *app) handle(ev ui.Event) {
	if a.np {
		switch ev.Button {
		case ui.Y:
			a.lyricsOn = !a.lyricsOn
			a.lyrScroll = 0
			if a.lyricsOn {
				a.retryLyrics(a.player.State().Song)
			}
		case ui.Up:
			a.lyrScroll = max(0, a.lyrScroll-3)
		case ui.Down:
			a.lyrScroll += 3
		case ui.A, ui.Start:
			a.player.Toggle()
		case ui.L1, ui.Left:
			a.player.Prev()
		case ui.R1, ui.Right:
			a.player.Next()
		case ui.B, ui.Select:
			a.np = false
		case ui.X:
			a.player.Stop()
			a.np = false
		}
		return
	}
	v := a.top()
	switch ev.Button {
	case ui.A:
		if it := v.list.Selected(); it != nil && v.open != nil && !v.loading {
			v.open(it)
		}
	case ui.Start:
		if v.play != nil && !v.loading {
			v.play()
		} else if a.player.State().Has {
			a.np = true
		}
	case ui.B:
		if len(a.stack) > 1 {
			a.stack = a.stack[:len(a.stack)-1]
		}
	case ui.Select:
		if a.player.State().Has {
			a.np = true
		}
	default:
		v.list.Handle(ev)
	}
}

// Covers come in two sizes, each fetched from the server at that size:
// thumbnails for list rows (44-54 px) and the mini player, and the large
// cover for Now Playing (232 px), the album page (up to 188 px, scaled down
// from it) and the backdrop.
const (
	thumbPx = 64
	largePx = 232
)

// In memory at most: 48 thumbnails (16 KB each) and 4 large covers
// (210 KB each). The rest are on the card, quick to read again.
var coverKeep = map[int]int{thumbPx: 48, largePx: 4}

type coverKey struct {
	id string
	px int
}

// cover is the large cover for id; thumb the small one.
func (a *app) cover(id string) image.Image { return a.coverAt(id, largePx) }
func (a *app) thumb(id string) image.Image { return a.coverAt(id, thumbPx) }

// coverAt returns the cached cover for id at px, fetching it in the
// background (at most three at a time).
func (a *app) coverAt(id string, px int) image.Image {
	if id == "" {
		return nil
	}
	k := coverKey{id, px}
	a.coverMu.Lock()
	defer a.coverMu.Unlock()
	if img, ok := a.covers[k]; ok {
		return img
	}
	if t, ok := a.failed[k]; ok && time.Since(t) < 20*time.Second {
		return nil
	}
	if !a.pending[k] {
		a.pending[k] = true
		go func() {
			a.fetchSem <- struct{}{}
			img, err := a.sub.CoverArt(id, px)
			<-a.fetchSem
			a.coverMu.Lock()
			delete(a.pending, k)
			if err != nil || img == nil {
				a.failed[k] = time.Now() // try again later
			} else {
				a.covers[k] = img
				delete(a.failed, k)
				a.coverOrder = append(a.coverOrder, k)
				a.evictCovers(px)
			}
			a.coverMu.Unlock()
			a.poke()
		}()
	}
	return nil
}

// evictCovers drops the oldest covers of size px beyond coverKeep[px].
// Called with coverMu held.
func (a *app) evictCovers(px int) {
	n := 0
	for _, k := range a.coverOrder {
		if k.px == px {
			n++
		}
	}
	keep := a.coverOrder[:0]
	for _, k := range a.coverOrder {
		if k.px == px && n > coverKeep[px] {
			delete(a.covers, k)
			n--
			continue
		}
		keep = append(keep, k)
	}
	a.coverOrder = keep
}

// shown returns a cover as it is drawn at size x size: in the pixel style
// (Settings › Cover art), redrawn with ui.PixelArt at exactly that size.
// The dither is made at the size drawn because scaling it afterwards
// (232 px to the album page's 188, 64 px to a 46 px thumbnail) smears its
// 2x2 cells into uneven blocks. The smooth cover stays in memory and on
// the card, so switching the style takes effect at once with nothing to
// fetch again.
func (a *app) shown(img image.Image, size int) image.Image {
	if img == nil || size <= 0 || !a.cfg.pixelCovers() {
		return img
	}
	k := pixelKey{img, size}
	a.pixelMu.Lock()
	defer a.pixelMu.Unlock()
	if p, ok := a.pixels[k]; ok {
		return p
	}
	// Few are on screen at once: start over past about 1.5 MB (seven
	// large covers' worth).
	if a.pixelBytes > 1536<<10 {
		a.pixels, a.pixelBytes = map[pixelKey]image.Image{}, 0
	}
	if a.pixels == nil {
		a.pixels = map[pixelKey]image.Image{}
	}
	p := ui.PixelArt(ui.ScaleCrop(img, size, size), coverLevels)
	a.pixels[k] = p
	a.pixelBytes += size * size * 4
	return p
}

// pixelKey is a cover redrawn in the pixel style at one size.
type pixelKey struct {
	img  image.Image
	size int
}

// listThumbSize is the side of the thumbnails ui.List draws in a list of n
// rows with pictures, h pixels high: its row height (66, stretched by up
// to 10 to fill h) less 22. It follows List.layout; were the two ever to
// differ, a thumbnail is scaled to fit as before, only softer.
func listThumbSize(h, n int) int {
	const base = 66
	rows := max(1, (h+base/3)/base)
	rh := h / rows
	if rh > base+10 || n < rows {
		rh = base
	}
	return rh - 22
}

// backdrop is the blurred cover behind Now Playing. It is made from the
// smooth cover in both styles: after the blur they look the same.
func (a *app) backdrop(id string) *image.RGBA {
	img := a.cover(id)
	if img == nil {
		return nil
	}
	a.coverMu.Lock()
	defer a.coverMu.Unlock()
	if b, ok := a.backdrops[id]; ok {
		return b
	}
	b := ui.Backdrop(img)
	// Backdrops are full-screen (1.2 MB each): keep only the current one.
	for k := range a.backdrops {
		delete(a.backdrops, k)
	}
	a.backdrops[id] = b
	return b
}

// ---- drawing ----

const footerTop = ui.H - 50

func (a *app) draw() {
	s := a.scr
	if v := a.top(); v.toNP && len(a.stack) > 1 {
		a.stack = a.stack[:len(a.stack)-1]
		a.np = true
	}
	if a.np {
		a.drawNowPlaying()
		s.Present()
		return
	}
	v := a.top()
	st := a.player.State()
	if st.Has && st.Playing {
		s.SetTicker(st.Song.Title + "   ·   " + st.Song.Artist + "   ·   " + st.Song.Album)
	} else {
		s.SetTicker("")
	}
	head := v.title
	if v.kind != "" {
		head = v.kind
	}
	hints := []ui.Hint{{Button: "A", Label: "Open"}}
	if v.songs != nil && v.kind != "" { // an album or playlist: A plays
		hints[0].Label = "Play"
	} else if v.play != nil {
		hints = append(hints, ui.Hint{Button: "START", Label: "Play"})
	}
	if st.Has {
		hints = append(hints, ui.Hint{Button: "SELECT", Label: "Now playing"})
	}
	if len(a.stack) > 1 {
		hints = append(hints, ui.Hint{Button: "B", Label: "Back"})
	} else {
		hints = append(hints, ui.Hint{Button: "B", Label: "Exit"})
	}
	// background, title and buttons are drawn once and reused (StaticFrame)
	top := s.StaticFrame(head, v.sub, hints...)

	bottom := footerTop - 6
	if st.Has {
		a.drawMini(st, image.Rect(20, footerTop-72, ui.W-20, footerTop-10))
		bottom = footerTop - 80
	}
	area := image.Rect(20, top, ui.W-26, bottom)
	if v.art != "" {
		a.drawArtPanel(v, top, bottom)
		area.Min.X = 238
	}
	if v.songs != nil {
		// mark the playing track
		for i := range v.list.Items {
			v.list.Items[i].Icon = ""
			if st.Has && st.Playing && i < len(v.songs) && v.songs[i].ID == st.Song.ID {
				v.list.Items[i].Detail = "playing"
			} else if i < len(v.songs) {
				v.list.Items[i].Detail = fmtDur(v.songs[i].Duration)
			}
		}
	}
	switch {
	case v.loading:
		s.Loading((area.Min.Y+area.Max.Y)/2-12, "Loading")
	case v.err != "":
		y := (area.Min.Y+area.Max.Y)/2 - 20
		for i, line := range ui.Wrap(ui.Body, v.err, area.Dx()-40) {
			s.TextCenter((area.Min.X+area.Max.X)/2, y+i*ui.LineHeight(ui.Body), line, ui.Body, ui.Muted)
		}
	default:
		a.listThumb = listThumbSize(area.Dy(), len(v.list.Items))
		v.list.Draw(s, area)
	}

	s.Present()
}

// drawArtPanel draws an album or playlist's cover and details on the left,
// sized to fit between the header (top) and bottom.
func (a *app) drawArtPanel(v *view, top, bottom int) {
	s := a.scr
	size := min(188, bottom-top-74)
	r := image.Rect(24, top+4, 24+size, top+4+size)
	s.Shadow(r, 14)
	if img := a.shown(a.cover(v.art), size); img != nil {
		s.RoundPicture(img, r, 14)
	} else {
		s.Panel(r, 14)
		icon := "disc"
		if len(v.art) > 3 && v.art[:3] == "pl-" {
			icon = "list"
		}
		s.Icon(icon, r.Min.X+size/4, r.Min.Y+size/4, size/2, ui.Muted)
	}
	y := r.Max.Y + 12
	lines := ui.Wrap(ui.Heading, v.title, 188)
	if bottom-y < 70 && len(lines) > 1 {
		lines = []string{ui.Fit(ui.Heading, v.title, 188)}
	}
	for i, line := range lines {
		if i == 2 || y+ui.LineHeight(ui.Heading) > bottom {
			break
		}
		s.Text(24, y, line, ui.Heading, ui.Text)
		y += ui.LineHeight(ui.Heading)
	}
	y += 2
	for i, info := range v.info {
		c := ui.Muted
		if i == 0 {
			c = ui.Text
		}
		for _, line := range packParts(info, 188) {
			if y+ui.LineHeight(ui.Small) > bottom {
				return
			}
			s.Text(24, y, ui.Fit(ui.Small, line, 188), ui.Small, c)
			y += ui.LineHeight(ui.Small) + 2
		}
	}
}

// packParts breaks "a · b · c" into lines no wider than w, at the dots, so
// "2018 · 12 songs · 58 min" becomes two whole lines instead of one cut short.
func packParts(text string, w int) []string {
	if ui.Measure(ui.Small, text) <= w {
		return []string{text}
	}
	var lines []string
	cur := ""
	for _, p := range strings.Split(text, " · ") {
		if cur != "" && ui.Measure(ui.Small, cur+" · "+p) <= w {
			cur += " · " + p
			continue
		}
		if cur != "" {
			lines = append(lines, cur)
		}
		cur = p
	}
	return append(lines, cur)
}

// drawMini draws the mini player above the footer.
func (a *app) drawMini(st NowPlaying, r image.Rectangle) {
	s := a.scr
	s.PanelTint(r, 16, 0.14)
	th := r.Dy() - 16
	art := image.Rect(r.Min.X+8, r.Min.Y+8, r.Min.X+8+th, r.Min.Y+8+th)
	if img := a.shown(a.thumb(st.Song.CoverArt), th); img != nil {
		s.RoundPicture(img, art, 8)
	} else {
		s.RoundRectAlpha(art, 8, ui.Text, 0.12)
		s.Icon("note", art.Min.X+th/4, art.Min.Y+th/4, th/2, ui.Faint)
	}
	x := art.Max.X + 12
	room := r.Max.X - 70 - x
	s.Text(x, r.Min.Y+6, ui.Fit(ui.Heading, st.Song.Title, room), ui.Heading, ui.Text)
	s.Text(x, r.Min.Y+29, ui.Fit(ui.Small, st.Song.Artist, room), ui.Small, ui.Muted)
	icon := "pause"
	if st.Paused || !st.Playing {
		icon = "play"
	}
	s.Circle(float64(r.Max.X-34), float64(r.Min.Y+r.Dy()/2), 20, ui.Accent)
	s.Icon(icon, r.Max.X-46, r.Min.Y+r.Dy()/2-12, 24, ui.BG)
	if st.Song.Duration > 0 {
		frac := min(1, st.Elapsed.Seconds()/float64(st.Song.Duration))
		bar := image.Rect(x, r.Max.Y-12, r.Max.X-70, r.Max.Y-8)
		s.Bar(bar, frac, ui.Text)
	}
}

func (a *app) drawNowPlaying() {
	s := a.scr
	st := a.player.State()
	s.SetTicker("") // the song is on screen already
	if bd := a.backdrop(st.Song.CoverArt); st.Has && bd != nil {
		s.Blit(bd)
	} else {
		s.Background()
	}
	s.StatusBar()
	if a.lyricsOn && st.Has {
		a.drawLyrics(st)
		return
	}
	s.Text(24, 18, "NOW PLAYING", ui.Caps, ui.Muted)
	if st.Len > 0 {
		s.Text(24+ui.Measure(ui.Caps, "NOW PLAYING")+12, 18, fmt.Sprintf("%d OF %d", st.Pos+1, st.Len), ui.Caps, ui.Faint)
	}
	if !st.Has {
		s.Message("Nothing playing", "Pick an album or a playlist.", ui.Hint{Button: "B", Label: "Back"})
		return
	}

	art := image.Rect(28, 62, 28+232, 62+232)
	s.Shadow(art, 16)
	if img := a.shown(a.cover(st.Song.CoverArt), art.Dx()); img != nil {
		s.RoundPicture(img, art, 16)
	} else {
		s.Panel(art, 16)
		s.Icon("note", art.Min.X+66, art.Min.Y+66, 100, ui.Muted)
	}

	x, w := 286, ui.W-286-28
	y := 70
	lines := ui.Wrap(ui.Display, st.Song.Title, w)
	f := ui.Display
	if len(lines) > 2 {
		f = ui.Title
		lines = ui.Wrap(f, st.Song.Title, w)
	}
	for i, line := range lines {
		if i == 2 {
			break
		}
		if i == 1 && len(lines) > 2 {
			line = ui.Fit(f, line+"…", w)
		}
		s.Text(x, y, line, f, ui.Text)
		y += ui.LineHeight(f)
	}
	y += 6
	s.Text(x, y, ui.Fit(ui.Heading, st.Song.Artist, w), ui.Heading, ui.Text)
	y += ui.LineHeight(ui.Heading) + 2
	s.Text(x, y, ui.Fit(ui.Small, st.Song.Album, w), ui.Small, ui.Muted)
	y += ui.LineHeight(ui.Small) + 6
	rate := st.Kbps // what the player streams at: lower on a slow link
	if rate == 0 {
		rate = a.cfg.bitRate()
	}
	if info := streamInfo(st.Song, a.cfg.Format, rate); info != "" && y+ui.LineHeight(ui.Caps) < 240 {
		s.Text(x, y, ui.Fit(ui.Caps, info, w), ui.Caps, ui.Faint)
	}

	// progress
	el := int(st.Elapsed.Seconds())
	dur := st.Song.Duration
	if dur > 0 && el > dur {
		el = dur
	}
	frac := 0.0
	if dur > 0 {
		frac = float64(el) / float64(dur)
	}
	py := 246
	s.Bar(image.Rect(x, py, ui.W-28, py+6), frac, ui.Text)
	kx := x + int(float64(ui.W-28-x)*frac)
	s.Circle(float64(kx), float64(py+3), 7, ui.Text)
	s.Text(x, py+14, fmtDur(el), ui.Small, ui.Muted)
	s.TextRight(ui.W-28, py+14, fmtDur(dur), ui.Small, ui.Muted)

	// transport
	cx := x + w/2
	cy := 334
	s.Icon("prev", cx-104, cy-18, 36, ui.Text)
	s.Circle(float64(cx), float64(cy), 34, ui.Accent)
	icon := "pause"
	if st.Paused || !st.Playing {
		icon = "play"
	}
	s.Icon(icon, cx-20, cy-20, 40, ui.BG)
	s.Icon("next", cx+68, cy-18, 36, ui.Text)

	// status or up next
	y = 392
	switch {
	case st.Err != "":
		s.Text(28, y, ui.Fit(ui.Small, st.Err, ui.W-56), ui.Small, ui.Bad)
	case len(st.Next) > 0:
		lbl := "UP NEXT"
		s.Text(28, y+2, lbl, ui.Caps, ui.Muted)
		s.Text(28+ui.Measure(ui.Caps, lbl)+12, y-1,
			ui.Fit(ui.Small, st.Next[0].Title+"  ·  "+st.Next[0].Artist, ui.W-56-ui.Measure(ui.Caps, lbl)-12), ui.Small, ui.Text)
	}
	play := "Pause"
	if st.Paused || !st.Playing {
		play = "Play"
	}
	s.Footer(ui.Hint{Button: "A", Label: play}, ui.Hint{Button: "L1/R1", Label: "Skip"}, ui.Hint{Button: "Y", Label: "Lyrics"}, ui.Hint{Button: "X", Label: "Stop"}, ui.Hint{Button: "B", Label: "Browse"})
}

// streamInfo describes the file and what's streamed, e.g.
// "FLAC 44.1 kHz → MP3 192 kbps · 2009 · Indie Pop".
func streamInfo(s Song, format string, maxBR int) string {
	var parts []string
	if s.Suffix != "" {
		src := strings.ToUpper(s.Suffix)
		lossless := map[string]bool{"flac": true, "alac": true, "wav": true, "aiff": true, "ape": true, "wv": true}[strings.ToLower(s.Suffix)]
		switch {
		case lossless && s.SamplingRate > 0:
			src += " " + strings.TrimSuffix(fmt.Sprintf("%.1f", float64(s.SamplingRate)/1000), ".0") + " kHz"
		case s.BitRate > 0:
			src += fmt.Sprintf(" %d kbps", s.BitRate)
		}
		if format != "" && format != "raw" && (!strings.EqualFold(s.Suffix, format) || maxBR > 0 && s.BitRate > maxBR) {
			to := strings.ToUpper(format)
			if maxBR > 0 {
				to += fmt.Sprintf(" %d kbps", maxBR)
			}
			src += "  →  " + to
		}
		parts = append(parts, src)
	}
	if s.Year > 0 {
		parts = append(parts, fmt.Sprint(s.Year))
	}
	if s.Genre != "" {
		parts = append(parts, s.Genre)
	}
	return strings.Join(parts, "  ·  ")
}

// lyricsFor returns the song's lyrics, fetching them in the background the
// first time (when fetch is set). nil means not asked for yet.
//
// A failed fetch stays, error and all, for lyricsRetry: every redraw asks
// again, and retrying at once would ask the server in a tight loop and
// never leave the error on screen.
func (a *app) lyricsFor(s Song, fetch bool) *songLyrics {
	if s.ID == "" {
		return nil
	}
	a.lyrMu.Lock()
	defer a.lyrMu.Unlock()
	l, ok := a.lyr[s.ID]
	if !fetch || ok && (l.err == "" || time.Since(l.failed) < lyricsRetry) {
		return l
	}
	if !ok {
		if len(a.lyr) >= 32 { // one entry per song looked at: start over now and then
			for k, v := range a.lyr {
				if !v.loading {
					delete(a.lyr, k)
				}
			}
		}
		l = &songLyrics{}
		a.lyr[s.ID] = l
	}
	l.loading, l.err = true, ""
	go func() {
		lines, synced, err := a.sub.Lyrics(s)
		a.lyrMu.Lock()
		l.loading, l.lines, l.synced = false, lines, synced
		if err != nil {
			l.err, l.failed = err.Error(), time.Now()
		}
		a.lyrMu.Unlock()
		a.poke()
	}()
	return l
}

// retryLyrics forgets a failed fetch of s's lyrics, so the next draw asks
// the server again.
func (a *app) retryLyrics(s Song) {
	a.lyrMu.Lock()
	defer a.lyrMu.Unlock()
	if l, ok := a.lyr[s.ID]; ok && !l.loading && l.err != "" {
		delete(a.lyr, s.ID)
	}
}

// currentLine is the index of the line being sung at t, or -1 before the
// first.
func currentLine(lines []LyricLine, t time.Duration) int {
	cur := -1
	for i, l := range lines {
		if l.At > t {
			break
		}
		cur = i
	}
	return cur
}

// drawLyrics shows the lyrics over the cover's backdrop: timed lyrics follow
// the song with the current line in white; others scroll with Up/Down.
func (a *app) drawLyrics(st NowPlaying) {
	s := a.scr
	s.Text(24, 18, "LYRICS", ui.Caps, ui.Muted)
	lx := 24 + ui.Measure(ui.Caps, "LYRICS") + 12
	s.Text(lx, 16, ui.Fit(ui.Small, st.Song.Title+"  ·  "+st.Song.Artist, 400-lx), ui.Small, ui.Text)

	top, bottom := 60, footerTop-22
	// a thin progress line above the footer
	if d := st.Song.Duration; d > 0 {
		frac := min(1, st.Elapsed.Seconds()/float64(d))
		s.Bar(image.Rect(28, bottom+8, ui.W-28, bottom+11), frac, ui.Text)
	}
	play := "Pause"
	if st.Paused || !st.Playing {
		play = "Play"
	}
	hints := []ui.Hint{{Button: "A", Label: play}, {Button: "L1/R1", Label: "Skip"}}

	l := a.lyricsFor(st.Song, true)
	a.lyrMu.Lock()
	loading, lines, synced, errText := l.loading, l.lines, l.synced, l.err
	a.lyrMu.Unlock()
	mid := (top + bottom) / 2
	switch {
	case loading:
		s.Loading(mid-12, "Loading lyrics")
	case errText != "":
		lines := ui.Wrap(ui.Body, "Couldn't get the lyrics: "+errText, ui.W-80)
		for i, line := range lines {
			s.TextCenter(ui.W/2, mid-20+i*ui.LineHeight(ui.Body), line, ui.Body, ui.Muted)
		}
		y := mid - 20 + len(lines)*ui.LineHeight(ui.Body) + 8
		s.TextCenter(ui.W/2, y, "Press Y twice to try again.", ui.Small, ui.Faint)
	case len(lines) == 0:
		s.TextCenter(ui.W/2, mid-40, "No lyrics for this song", ui.Heading, ui.Text)
		for i, line := range ui.Wrap(ui.Small, "Your server reads them from the song's tags or from a .lrc file next to it.", 440) {
			s.TextCenter(ui.W/2, mid-4+i*ui.LineHeight(ui.Small), line, ui.Small, ui.Muted)
		}
	default:
		const gap = 10
		f := ui.Heading // a heading, not a screen title: more words fit a line
		lh := ui.LineHeight(f)
		type block struct {
			rows []string
			y    int
		}
		blocks := make([]block, len(lines))
		y := 0
		for i, ln := range lines {
			rows := ui.Wrap(f, ln.Text, ui.W-80)
			if ln.Text == "" {
				rows = nil
			}
			blocks[i] = block{rows, y}
			if len(rows) == 0 {
				y += lh / 2 // a verse break
			} else {
				y += len(rows)*lh + gap
			}
		}
		cur := -1
		var off int
		if synced {
			cur = currentLine(lines, st.Elapsed)
			a.lyrShown = cur
			// keep the current line a third of the way down
			if cur >= 0 {
				off = blocks[cur].y - (bottom-top)/3
			}
		} else {
			first := min(a.lyrScroll, len(blocks)-1)
			a.lyrScroll = first
			off = blocks[first].y
			hints = append(hints, ui.Hint{Button: "UP/DOWN", Label: "Scroll"})
		}
		for i, b := range blocks {
			for r, row := range b.rows {
				yy := top + b.y - off + r*lh
				if yy < top || yy+lh > bottom {
					continue
				}
				c := ui.Text
				if synced && i != cur {
					c = ui.Faint
				}
				s.Text(40, yy, row, f, c)
			}
		}
	}
	hints = append(hints, ui.Hint{Button: "Y", Label: "Song"}, ui.Hint{Button: "B", Label: "Browse"})
	s.Footer(hints...)
}

// trackLabel is "3  Title". The title is a directional isolate (FSI..PDI),
// so a right-to-left title doesn't carry the number to its right: track
// numbers stay in one column on the left.
func trackLabel(n int, title string) string {
	return fmt.Sprintf("%d  \u2068%s\u2069", n, title)
}
