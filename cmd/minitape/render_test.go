package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zakijss/minideck/qr"
	"github.com/zakijss/minideck/ui"
)

// stillPlayer is a playerCtl that reports a fixed state.
type stillPlayer struct{ st NowPlaying }

func (p *stillPlayer) Play(q []Song, i int) {}
func (p *stillPlayer) Toggle()              {}
func (p *stillPlayer) Next()                {}
func (p *stillPlayer) Prev()                {}
func (p *stillPlayer) Stop()                {}
func (p *stillPlayer) Seek(d int)           {}
func (p *stillPlayer) State() NowPlaying    { return p.st }

// testCover is a 1000x1000 JPEG with fine detail (thin rings, stripes and
// a gradient), so blurred or blocky scaling shows in the screenshots.
func testCover(t *testing.T) []byte {
	const n = 1000
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			dx, dy := float64(x-n/2), float64(y-n/2)
			d := math.Hypot(dx, dy)
			r := uint8(40 + 180*x/n)
			g := uint8(30 + 120*y/n)
			b := uint8(160 - 100*x/n)
			if int(d)%24 < 3 { // thin rings
				r, g, b = 250, 240, 220
			}
			if (x+y)%60 < 4 && d > 300 { // stripes near the edge
				r, g, b = 20, 20, 30
			}
			img.Set(x, y, color.RGBA{r, g, b, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 92}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestScreens draws Now Playing, an album page, an album list with the
// mini player, the lyrics and a failed lyrics fetch to PNG files in DIR
// for review. It runs only with MINIDECK_GALLERY=DIR set.
func TestScreens(t *testing.T) {
	dir := os.Getenv("MINIDECK_GALLERY")
	if dir == "" {
		t.Skip("set MINIDECK_GALLERY=DIR to draw the screens")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := testCover(t)
	a := &app{covers: map[coverKey]image.Image{}, pending: map[coverKey]bool{}, failed: map[coverKey]time.Time{},
		backdrops: map[string]*image.RGBA{}, fetchSem: make(chan struct{}, 3), cfg: &Settings{},
		lyr: map[string]*songLyrics{}, poke: func() {}}
	for _, px := range []int{thumbPx, largePx} {
		img, err := decodeCover(raw, px)
		if err != nil {
			t.Fatal(err)
		}
		a.covers[coverKey{"al1", px}] = img
	}
	songs := []Song{
		{ID: "s1", Title: "Signal Fires", Artist: "The Static Gardens", Album: "Night Signals", CoverArt: "al1", Duration: 214, Track: 1},
		{ID: "s2", Title: "Low Tide Radio", Artist: "The Static Gardens", Album: "Night Signals", CoverArt: "al1", Duration: 187, Track: 2},
		{ID: "s3", Title: "Antenna", Artist: "The Static Gardens", Album: "Night Signals", CoverArt: "al1", Duration: 251, Track: 3},
	}
	a.player = &stillPlayer{NowPlaying{Song: songs[0], Has: true, Playing: true, Elapsed: 71 * time.Second,
		Pos: 0, Len: 3, Next: songs[1:]}}

	open := func(name string) {
		t.Setenv("MINIDECK_UI_PNG", filepath.Join(dir, name+".png"))
		s, err := ui.Open()
		if err != nil {
			t.Fatal(err)
		}
		a.scr = s
	}

	// The cover screens in both styles: pixel (the default) and smooth.
	for _, style := range []string{"", "smooth"} {
		a.cfg.CoverStyle = style
		suffix := ""
		if style != "" {
			suffix = "-" + style
		}
		open("minitape-now-playing" + suffix)
		a.stack = []*view{{title: "MiniTape"}}
		a.np = true
		a.draw()

		open("minitape-album" + suffix)
		a.np = false
		items := make([]ui.Item, len(songs))
		for i, x := range songs {
			items[i] = ui.Item{Label: trackLabel(x.Track, x.Title), Detail: fmtDur(x.Duration), Value: i}
		}
		a.stack = []*view{{title: "Night Signals", kind: "Album", art: "al1", info: []string{"The Static Gardens"},
			year: 2007, songs: songs, list: ui.List{Items: items}, refresh: func() {}, shuffle: func() {}}}
		a.draw()

		open("minitape-albums" + suffix)
		al := []Album{{ID: "al1", Name: "Night Signals", Artist: "The Static Gardens", Year: 2007, CoverArt: "al1"},
			{ID: "al2", Name: "Low Orbit", Artist: "Paper Satellites", Year: 2001, CoverArt: "al1"}}
		a.stack = []*view{{title: "Recently added", list: ui.List{Items: a.albumItems(al)}, play: func() {}, refresh: func() {}}}
		a.draw()
	}
	a.cfg.CoverStyle = ""

	// An album with Arabic titles (one with Latin and digits in it), as
	// the album page and the album list draw them.
	arSongs := []Song{
		{ID: "a1", Title: "جمال المحيم", Artist: "فيروز", Album: "كيفك إنت", CoverArt: "al1", Duration: 252, Track: 1},
		{ID: "a2", Title: "كيفك إنت", Artist: "فيروز", Album: "كيفك إنت", CoverArt: "al1", Duration: 310, Track: 2},
		{ID: "a3", Title: "سلام لبيروت (Live) 1994", Artist: "فيروز", Album: "كيفك إنت", CoverArt: "al1", Duration: 198, Track: 3},
		{ID: "a4", Title: "مُحَمَّد وَالقَمَر وأغنية طويلة جداً لا تتسع في السطر", Artist: "فيروز", Album: "كيفك إنت", CoverArt: "al1", Duration: 421, Track: 4},
	}
	open("minitape-album-arabic")
	items := make([]ui.Item, len(arSongs))
	for i, x := range arSongs {
		items[i] = ui.Item{Label: trackLabel(x.Track, x.Title), Detail: fmtDur(x.Duration), Value: i}
	}
	a.stack = []*view{{title: "كيفك إنت", kind: "Album", art: "al1", info: []string{"فيروز"},
		year: 1991, songs: arSongs, list: ui.List{Items: items, Sel: 2}}}
	a.draw()

	open("minitape-albums-arabic")
	arAl := []Album{{ID: "al1", Name: "كيفك إنت", Artist: "فيروز", Year: 1991, CoverArt: "al1"},
		{ID: "al2", Name: "שירים", Artist: "עידן רייכל", Year: 2006, CoverArt: "al1"},
		{ID: "al3", Name: "夜に駆ける", Artist: "YOASOBI", Year: 2020, CoverArt: "al1"}}
	a.stack = []*view{{title: "Recently added", list: ui.List{Items: a.albumItems(arAl)}}}
	a.draw()

	open("minitape-lyrics")
	a.np, a.lyricsOn = true, true
	a.lyr["s1"] = &songLyrics{synced: true, lines: []LyricLine{
		{At: 50 * time.Second, Text: "We lit the hills to call you home"},
		{At: 62 * time.Second, Text: "and every fire answered back"},
		{At: 70 * time.Second, Text: "a line of light along the coast, as far as anyone could see"},
		{At: 80 * time.Second, Text: "so far from the harbour"},
		{At: 90 * time.Second, Text: ""},
		{At: 95 * time.Second, Text: "Signal fires, signal fires"},
	}}
	a.draw()

	open("minitape-lyrics-error")
	a.lyr["s1"] = &songLyrics{err: "The server didn't answer in time. Check the address and port.", failed: time.Now()}
	a.draw()

	st := scrobbleStatus{Server: true, LFMKeys: true, LFM: true, LFMUser: "alex", LB: true, LBBroken: true, Waiting: 3}
	open("minitape-settings")
	drawMenu(a.scr, "Settings", &ui.List{Items: settingsItems(Settings{URL: "http://homeserver:4533", User: "alex"},
		scrobbleStatus{Server: true, LFM: true, LB: true, Waiting: 3}.summary(), 312<<20)})

	open("minitape-settings-more")
	off := false
	drawMenu(a.scr, "Settings", &ui.List{Items: settingsItems(Settings{URL: "http://homeserver:4533", User: "alex",
		MaxBitRate: 96, SongCacheMB: 1024, Background: &off}, "Off", 0), Sel: 6})

	open("minitape-scrobbling")
	drawMenu(a.scr, "Scrobbling", &ui.List{Items: st.items()})

	open("minitape-scrobbling-off")
	drawMenu(a.scr, "Scrobbling", &ui.List{Items: scrobbleStatus{LFMKeys: true}.items(), Sel: 1})

	open("minitape-lastfm-login")
	link := lastfmAuthURL(testKey, testToken)
	code, err := qr.Encode(link)
	if err != nil {
		t.Fatal(err)
	}
	drawLastFMLogin(a.scr, link, code, time.Unix(1, 0))
}
