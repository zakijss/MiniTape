package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zakijss/minideck/atomicfile"
	"github.com/zakijss/minideck/cache"
	"github.com/zakijss/minideck/ui"
)

// Music on the SD card. The songs under the music folder (Onion's
// Media/Music, or music_dir in settings.json) are read once, tags and
// all, into an index kept in the app's folder, so the lists open at once
// after that. Y reads the folder again, opening only new or changed files.
// The songs play through the same player as the server's, from the file.

const (
	defaultMusicDir = "/mnt/SDCARD/Media/Music"
	localIndexFile  = "local-music.json"
	// localPrefix starts the ids of albums and artists made from the
	// card's tags; filePrefix the ids of covers that are files on the
	// card, embedPrefix of covers inside a song's file.
	localPrefix = "local:"
	filePrefix  = "file:"
	embedPrefix = "embed:"
	// maxLocalQueue caps a queue of songs from the card (Shuffle this
	// card, a folder), as the server's Shuffle everything asks for 200.
	maxLocalQueue = 500
)

// audioExts are the files that count as songs: what the device's ffplay
// plays.
var audioExts = map[string]bool{".mp3": true, ".flac": true, ".ogg": true, ".oga": true,
	".opus": true, ".m4a": true, ".m4b": true, ".aac": true, ".wav": true}

// coverNames are the pictures taken as a folder's cover, best first.
var coverNames = []string{"cover", "folder", "front", "albumart"}

// localSong is a song in the index, with what tells a changed file.
type localSong struct {
	Song
	AlbumArtist string `json:"albumArtist,omitempty"`
	Disc        int    `json:"disc,omitempty"`
	Mod         int64  `json:"mod"`
}

type localIndex struct {
	Root  string      `json:"root"`
	Songs []localSong `json:"songs"`
}

// localLib is the index of the songs on the card.
type localLib struct {
	file string // where the index is kept

	mu     sync.Mutex
	root   string
	songs  []localSong
	loaded bool // the index file has been read
	stale  bool // read the folder again on the next use
}

// all returns the songs under root: from the index, or by reading the
// folder when there is no index for root yet or after refresh. progress,
// if set, is told now and then how many songs have been read.
func (l *localLib) all(root string, progress func(n int)) ([]localSong, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.loaded {
		l.loaded = true
		var ix localIndex
		if b, err := os.ReadFile(l.file); err == nil {
			if err := json.Unmarshal(b, &ix); err != nil {
				log.Printf("%s: %v", l.file, err)
			}
			l.root, l.songs = ix.Root, ix.Songs
			for i := range l.songs { // a song on the card is named by its path
				l.songs[i].Path = l.songs[i].ID
			}
		}
	}
	if l.root == root && l.songs != nil && !l.stale {
		return l.songs, nil
	}
	old := map[string]localSong{}
	if l.root == root {
		for _, s := range l.songs {
			old[s.Path] = s
		}
	}
	songs, err := scanMusic(root, old, progress)
	if err != nil {
		if os.IsNotExist(err) {
			err = screenError("No music folder on the card. Copy your music to " + root + ".")
		}
		return nil, err
	}
	l.root, l.songs, l.stale = root, songs, false
	b, err := json.Marshal(localIndex{root, songs})
	if err == nil {
		err = atomicfile.Write(l.file, b, 0644)
	}
	if err != nil {
		log.Printf("saving the music index: %v", err)
	}
	return songs, nil
}

// refresh makes the next use read the folder again.
func (l *localLib) refresh() {
	l.mu.Lock()
	l.stale = true
	l.mu.Unlock()
}

// scanMusic reads the songs under root, folder by folder in name order.
// A song in old with the same size and time is taken from there instead of
// opening the file again.
func scanMusic(root string, old map[string]localSong, progress func(n int)) ([]localSong, error) {
	songs := []localSong{}
	var walk func(dir string) error
	walk = func(dir string) error {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		cover, rank := "", len(coverNames)
		for _, e := range ents {
			name := strings.ToLower(e.Name())
			ext := filepath.Ext(name)
			if e.IsDir() || ext != ".jpg" && ext != ".jpeg" && ext != ".png" {
				continue
			}
			if i := slices.Index(coverNames, strings.TrimSuffix(name, ext)); i >= 0 && i < rank {
				cover, rank = filePrefix+filepath.Join(dir, e.Name()), i
			}
		}
		for _, e := range ents {
			name := e.Name()
			if strings.HasPrefix(name, ".") {
				continue // hidden, and macOS's ._ files
			}
			p := filepath.Join(dir, name)
			if e.IsDir() {
				if err := walk(p); err != nil {
					log.Printf("music: %v", err)
				}
				continue
			}
			if !audioExts[strings.ToLower(filepath.Ext(name))] {
				continue
			}
			fi, err := e.Info()
			if err != nil {
				continue
			}
			s, ok := old[p]
			if !ok || s.Size != fi.Size() || s.Mod != fi.ModTime().Unix() {
				s = readSong(p, fi.Size(), fi.ModTime())
			}
			// without a picture in the folder, the one in the file, if any
			// (read when it is shown)
			s.CoverArt = cmp.Or(cover, embedPrefix+p)
			songs = append(songs, s)
			if progress != nil && len(songs)%100 == 0 {
				progress(len(songs))
			}
		}
		return nil
	}
	return songs, walk(filepath.Clean(root))
}

// readSong reads a song's tags. Without a title the file's name stands in.
func readSong(path string, size int64, mod time.Time) localSong {
	t := readTags(path)
	ext := filepath.Ext(path)
	title := cmp.Or(t.Title, strings.TrimSuffix(filepath.Base(path), ext))
	return localSong{Song: Song{ID: path, Path: path, Title: title, Artist: t.Artist, Album: t.Album,
		Duration: t.Duration, Size: size, Track: t.Track, Suffix: strings.ToLower(strings.TrimPrefix(ext, ".")),
		BitRate: t.BitRate, SamplingRate: t.SampleRate, Year: t.Year, Genre: t.Genre},
		AlbumArtist: t.AlbumArtist, Disc: t.Disc, Mod: mod.Unix()}
}

// byTrack orders songs by disc and track number, then by file.
func byTrack(a, b localSong) int {
	return cmp.Or(cmp.Compare(a.Disc, b.Disc), cmp.Compare(a.Track, b.Track), strings.Compare(a.Path, b.Path))
}

func songsOf(ls []localSong) []Song {
	s := make([]Song, len(ls))
	for i, x := range ls {
		s[i] = x.Song
	}
	return s
}

// folder is a folder with songs in it, somewhere below.
type folder struct {
	name, path string
	songs      int
}

// folderContents sorts out the songs below dir: the ones in it (by track
// number when every one has one, else by file name) come first in songs,
// then the ones in its folders. direct is how many are in dir itself.
func folderContents(all []localSong, dir string) (songs []Song, direct int, dirs []folder) {
	prefix := filepath.Clean(dir) + "/"
	var here []localSong
	var below []Song
	idx := map[string]int{}
	numbered := true
	for _, s := range all {
		rel, ok := strings.CutPrefix(s.Path, prefix)
		if !ok {
			continue
		}
		name, _, deeper := strings.Cut(rel, "/")
		if !deeper {
			here = append(here, s)
			numbered = numbered && s.Track > 0
			continue
		}
		if i, ok := idx[name]; ok {
			dirs[i].songs++
		} else {
			idx[name] = len(dirs)
			dirs = append(dirs, folder{name, prefix + name, 1})
		}
		below = append(below, s.Song)
	}
	if numbered {
		slices.SortStableFunc(here, byTrack)
	}
	slices.SortFunc(dirs, func(a, b folder) int { return strings.Compare(strings.ToLower(a.name), strings.ToLower(b.name)) })
	return append(songsOf(here), below...), len(here), dirs
}

// albumArtistOf is who an album is filed under: its album artist, or the
// song's artist.
func albumArtistOf(s localSong) string { return cmp.Or(s.AlbumArtist, s.Artist) }

func localAlbumID(s localSong) string { return localPrefix + albumArtistOf(s) + "\x00" + s.Album }

// localAlbums are the albums the songs' tags name, A to Z. Songs without
// an album tag are only in Folders.
func localAlbums(all []localSong) []Album {
	idx := map[string]int{}
	var al []Album
	for _, s := range all {
		if s.Album == "" {
			continue
		}
		id := localAlbumID(s)
		i, ok := idx[id]
		if !ok {
			i = len(al)
			idx[id] = i
			al = append(al, Album{ID: id, Name: s.Album, Artist: cmp.Or(albumArtistOf(s), "Unknown artist")})
		}
		x := &al[i]
		x.SongCount++
		x.Year = max(x.Year, s.Year)
		x.CoverArt = cmp.Or(x.CoverArt, s.CoverArt)
	}
	slices.SortFunc(al, func(a, b Album) int {
		return cmp.Or(strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), strings.Compare(a.Artist, b.Artist))
	})
	return al
}

// localAlbumSongs are the songs of the album id, in track order.
func localAlbumSongs(all []localSong, id string) []Song {
	var ls []localSong
	for _, s := range all {
		if s.Album != "" && localAlbumID(s) == id {
			ls = append(ls, s)
		}
	}
	slices.SortStableFunc(ls, byTrack)
	return songsOf(ls)
}

// localArtists are the artists the albums are filed under, A to Z.
func localArtists(all []localSong) []Artist {
	idx := map[string]int{}
	var ars []Artist
	for _, al := range localAlbums(all) {
		i, ok := idx[al.Artist]
		if !ok {
			i = len(ars)
			idx[al.Artist] = i
			ars = append(ars, Artist{ID: localPrefix + al.Artist, Name: al.Artist})
		}
		ars[i].AlbumCount++
		ars[i].Album = append(ars[i].Album, al)
	}
	slices.SortFunc(ars, func(a, b Artist) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	return ars
}

// ---- the screens ----

// localSongs are the songs on the card, read in the background.
func (a *app) localSongs() ([]localSong, error) { return a.lib.all(a.cfg.musicDir(), nil) }

// loadLocal fills v from the songs on the card, as load does from the
// server; Y reads the folder again.
func (a *app) loadLocal(v *view, fn func(all []localSong) ([]ui.Item, func(), error)) *view {
	root := a.cfg.musicDir()
	a.load(v, func() ([]ui.Item, func(), error) {
		all, err := a.lib.all(root, func(n int) {
			select { // skipped while the screen is busy: it's only a count
			case a.done <- func() { v.sub = fmt.Sprintf("%d songs read", n) }:
			default:
			}
		})
		if err != nil {
			return nil, nil, err
		}
		return fn(all)
	})
	reload := v.refresh
	v.refresh = func() {
		a.lib.refresh()
		reload()
	}
	return v
}

// localItems are the first screen's rows for the music on the card.
func (a *app) localItems() []ui.Item {
	root := a.cfg.musicDir()
	return []ui.Item{
		{Label: "Shuffle this card", Icon: "shuffle", Sub: "Every song on the card", Value: func() {
			a.shuffleFrom("Shuffle this card", func() ([]Song, error) {
				all, err := a.localSongs()
				return songsOf(all), err
			}, "No songs in "+root+" yet.")
		}},
		{Label: "Folders", Icon: "folder", Chevron: true, Sub: strings.TrimPrefix(root, "/mnt/SDCARD/"),
			Value: func() { a.push(a.localFolder(root)) }},
		{Label: "Artists", Icon: "person", Chevron: true, Sub: "From the songs' tags", Value: func() { a.push(a.localArtists()) }},
		{Label: "Albums", Icon: "disc", Chevron: true, Sub: "From the songs' tags", Value: func() { a.push(a.localAlbums()) }},
	}
}

// localHome is "On this card", on the first screen when a server is set
// up too.
func (a *app) localHome() *view {
	return &view{title: "On this card", list: ui.List{Items: a.localItems()},
		open: func(it *ui.Item) { it.Value.(func())() }}
}

// localFolder lists a folder on the card: its folders with songs, then
// its songs. A plays from a song; START plays the folder or song selected;
// X shuffles everything below this folder.
func (a *app) localFolder(dir string) *view {
	title := filepath.Base(dir)
	if dir == a.cfg.musicDir() {
		title = "Folders"
	}
	v := &view{title: title}
	// the folders are A to Z; the songs after them are one group
	v.letter = func(it *ui.Item) string {
		if _, ok := it.Value.(string); ok {
			return labelLetter(it)
		}
		return ""
	}
	v.open = func(it *ui.Item) {
		switch x := it.Value.(type) {
		case string:
			a.push(a.localFolder(x))
		case int:
			a.player.Play(window(v.songs, x))
			a.np = true
		}
	}
	v.play = func() {
		it := v.list.Selected()
		if it == nil {
			return
		}
		d, ok := it.Value.(string)
		if !ok {
			v.open(it)
			return
		}
		go func() {
			all, err := a.localSongs()
			if songs, _, _ := folderContents(all, d); err == nil && len(songs) > 0 {
				a.player.Play(window(songs, 0))
				a.poke()
			}
		}()
		a.np = true
	}
	v.shuffle = func() { a.playShuffled(v.songs) }
	return a.loadLocal(v, func(all []localSong) ([]ui.Item, func(), error) {
		songs, direct, dirs := folderContents(all, dir)
		var items []ui.Item
		for _, d := range dirs {
			items = append(items, ui.Item{Label: d.name, Sub: songCount(d.songs), Icon: "folder", Tint: ui.Muted, Chevron: true, Value: d.path})
		}
		for i, s := range songs[:direct] {
			items = append(items, ui.Item{Label: s.Title, Detail: fmtDur(s.Duration), Value: i})
		}
		sub := songCount(len(songs))
		return items, func() { v.songs, v.sub = songs, sub }, nil
	})
}

// localArtists lists the artists on the card; A shows their albums.
func (a *app) localArtists() *view {
	v := &view{title: "Artists", letter: labelLetter}
	v.open = func(it *ui.Item) { a.openArtist(it.Value.(Artist)) }
	return a.loadLocal(v, func(all []localSong) ([]ui.Item, func(), error) {
		ars := localArtists(all)
		items := make([]ui.Item, len(ars))
		for i, x := range ars {
			items[i] = ui.Item{Label: x.Name, Sub: plural(x.AlbumCount, "album"), Icon: "person", Tint: ui.Muted, Value: x}
		}
		sub := plural(len(ars), "artist")
		return items, func() { v.sub = sub }, nil
	})
}

// localAlbums lists the albums on the card, A to Z.
func (a *app) localAlbums() *view {
	v := &view{title: "Albums", open: a.openAlbum, letter: labelLetter}
	v.play = func() {
		if it := v.list.Selected(); it != nil {
			a.playAlbum(it.Value.(Album))
		}
	}
	return a.loadLocal(v, func(all []localSong) ([]ui.Item, func(), error) {
		al := localAlbums(all)
		sub := plural(len(al), "album")
		return a.albumItems(al), func() { v.sub = sub }, nil
	})
}

// albumSongs are an album's songs, from the card or the server.
func (a *app) albumSongs(al Album) ([]Song, error) {
	if strings.HasPrefix(al.ID, localPrefix) {
		all, err := a.localSongs()
		return localAlbumSongs(all, al.ID), err
	}
	full, err := a.sub.Album(al.ID)
	if err != nil {
		return nil, err
	}
	return full.Song, nil
}

// artistAlbums are an artist's albums, from the card or the server.
func (a *app) artistAlbums(ar Artist) ([]Album, error) {
	if strings.HasPrefix(ar.ID, localPrefix) {
		all, err := a.localSongs()
		for _, x := range localArtists(all) {
			if x.ID == ar.ID {
				return x.Album, err
			}
		}
		return nil, err
	}
	full, err := a.sub.Artist(ar.ID)
	if err != nil {
		return nil, err
	}
	return full.Album, nil
}

// window is at most maxLocalQueue songs of q from around song i (a few
// before it, the rest after), and i's place in them: a folder can hold
// thousands below it.
func window(q []Song, i int) ([]Song, int) {
	start := max(0, min(i-maxLocalQueue/10, len(q)-maxLocalQueue))
	return q[start:min(len(q), start+maxLocalQueue)], i - start
}

// playShuffled plays songs in a random order.
func (a *app) playShuffled(songs []Song) {
	if len(songs) == 0 {
		return
	}
	q := append([]Song(nil), songs...)
	rand.Shuffle(len(q), func(i, j int) { q[i], q[j] = q[j], q[i] })
	a.player.Play(q[:min(len(q), maxLocalQueue)], 0)
	a.np = true
}

// coverImage is the cover id at px: a picture on the card (in the music
// folder root), or the server's.
func (a *app) coverImage(id, root string, px int) (image.Image, error) {
	switch {
	case strings.HasPrefix(id, filePrefix), strings.HasPrefix(id, embedPrefix):
		return localCover(a.sub.Cache, id, root, px)
	case strings.HasPrefix(id, localPrefix):
		return nil, errors.New("no cover")
	}
	return a.sub.CoverArt(id, px)
}

// localCover reads the cover id on the card, a picture file (filePrefix)
// or the one in a song's tags (embedPrefix), scaled to px, keeping the
// scaled one in c. Only files in the music folder root are read: a cover
// id can come from the server.
func localCover(c *cache.Disk, id, root string, px int) (image.Image, error) {
	kind, path, _ := strings.Cut(id, ":")
	path, fi, err := cardFile(path, root)
	if err != nil {
		return nil, err
	}
	key := id + "|" + strconv.FormatInt(fi.Size(), 10) + "|" + strconv.FormatInt(fi.ModTime().Unix(), 10) + "|" + strconv.Itoa(px)
	return cachedCover(c, key, px, func() ([]byte, error) {
		if kind+":" == embedPrefix {
			if b := readPicture(path); b != nil {
				return b, nil
			}
			return nil, errors.New("no cover")
		}
		b, err := readCardFile(path, root, maxPicture)
		if err == nil && len(b) > maxPicture {
			err = errors.New("cover too large")
		}
		return b, err
	})
}

// cardFile cleans path and checks it is a regular file in the music folder
// root: not a file elsewhere, a device or a FIFO, which a hostile id could
// name to read any file or hang the reader.
func cardFile(path, root string) (string, os.FileInfo, error) {
	path, ok := inside(path, root)
	if !ok {
		return "", nil, errors.New("not in the music folder")
	}
	fi, err := os.Lstat(path)
	if err == nil && !fi.Mode().IsRegular() {
		err = errors.New("not a file")
	}
	return path, fi, err
}

// readCardFile reads the file at path, checked by cardFile, up to max+1
// bytes: more than max means it is too big.
func readCardFile(path, root string, max int64) ([]byte, error) {
	path, _, err := cardFile(path, root)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, max+1))
}

// songLyrics are a song's lyrics, from the card (in the music folder root)
// or the server.
func (a *app) songLyrics(s Song, root string) ([]LyricLine, bool, error) {
	if s.Path != "" {
		lines, synced := localLyrics(s.Path, root)
		return lines, synced, nil
	}
	return a.sub.Lyrics(s)
}

// localLyrics reads a song's lyrics from a .lrc file next to it, or from
// its tags.
func localLyrics(path, root string) ([]LyricLine, bool) {
	if b, err := readCardFile(strings.TrimSuffix(path, filepath.Ext(path))+".lrc", root, maxFrame); err == nil {
		return parseLRC(string(b))
	}
	if _, _, err := cardFile(path, root); err != nil {
		return nil, false
	}
	return parseLRC(readTags(path).Lyrics)
}

// parseLRC reads lyrics: timed, with [mm:ss.xx] before each line (LRC), or
// plain text. Lines of timed lyrics without a time, and LRC's [ar:...]
// header lines, are left out.
func parseLRC(text string) (lines []LyricLine, synced bool) {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	var plain []LyricLine
	for _, raw := range strings.Split(text, "\n") {
		s := strings.TrimSpace(raw)
		var at []time.Duration
		tagged := false
		for strings.HasPrefix(s, "[") {
			end := strings.IndexByte(s, ']')
			if end < 0 {
				break
			}
			if d, ok := lrcTime(s[1:end]); ok {
				at = append(at, d)
			} else {
				tagged = true
			}
			s = strings.TrimSpace(s[end+1:])
		}
		for _, d := range at {
			lines = append(lines, LyricLine{At: d, Text: s})
		}
		if len(at) == 0 && !tagged {
			plain = append(plain, LyricLine{Text: s})
		}
	}
	if len(lines) > 0 {
		slices.SortStableFunc(lines, func(a, b LyricLine) int { return cmp.Compare(a.At, b.At) })
		return lines, true
	}
	for len(plain) > 0 && plain[len(plain)-1].Text == "" {
		plain = plain[:len(plain)-1]
	}
	for len(plain) > 0 && plain[0].Text == "" {
		plain = plain[1:]
	}
	return plain, false
}

// lrcTime reads an LRC time, "01:23.45" or "01:23:45".
func lrcTime(s string) (time.Duration, bool) {
	m, rest, ok := strings.Cut(s, ":")
	if !ok {
		return 0, false
	}
	mins, err := strconv.Atoi(m)
	if err != nil || mins < 0 {
		return 0, false
	}
	rest = strings.Replace(rest, ":", ".", 1)
	sec, err := strconv.ParseFloat(rest, 64)
	if err != nil || sec < 0 || sec >= 60 {
		return 0, false
	}
	return time.Duration(mins)*time.Minute + time.Duration(sec*float64(time.Second)), true
}
