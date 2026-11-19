package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zakijss/minideck/ui"
)

// id3Frame is an ID3v2.3 text frame in ISO-8859-1.
func id3Frame(id, text string) []byte {
	var b bytes.Buffer
	b.WriteString(id)
	binary.Write(&b, binary.BigEndian, uint32(len(text)+1))
	b.Write([]byte{0, 0, 0})
	b.WriteString(text)
	return b.Bytes()
}

// testMP3 is an ID3v2.3 tag, then an MPEG-1 Layer III frame (128 kbit/s,
// 44.1 kHz, stereo) with a Xing header counting frames.
func testMP3(frames uint32) []byte {
	body := bytes.Join([][]byte{id3Frame("TIT2", "Signal Fires"), id3Frame("TPE1", "The Static Gardens"),
		id3Frame("TALB", "Night Signals"), id3Frame("TRCK", "3/12"), id3Frame("TYER", "2007"),
		id3Frame("TCON", "(17)Rock")}, nil)
	body = append(body, make([]byte, 64)...) // padding
	n := len(body)
	tag := []byte{'I', 'D', '3', 3, 0, 0, byte(n >> 21 & 0x7f), byte(n >> 14 & 0x7f), byte(n >> 7 & 0x7f), byte(n & 0x7f)}
	frame := make([]byte, 417)
	copy(frame, []byte{0xff, 0xfb, 0x90, 0x00})
	x := frame[4+32:]
	copy(x, "Xing")
	binary.BigEndian.PutUint32(x[4:], 1)
	binary.BigEndian.PutUint32(x[8:], frames)
	return append(append(tag, body...), frame...)
}

func vorbisBlock(comments ...string) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, uint32(4))
	b.WriteString("test")
	binary.Write(&b, binary.LittleEndian, uint32(len(comments)))
	for _, c := range comments {
		binary.Write(&b, binary.LittleEndian, uint32(len(c)))
		b.WriteString(c)
	}
	return b.Bytes()
}

func testFLAC() []byte {
	b := []byte("fLaC")
	info := make([]byte, 18)
	// 44100 Hz (20 bits), stereo, 16 bits, 44100*200 samples
	rate, total := uint64(44100), uint64(44100*200)
	v := rate<<44 | 1<<41 | 15<<36 | total
	binary.BigEndian.PutUint64(info[10:], v)
	b = append(b, 0, 0, 0, 18)
	b = append(b, info...)
	c := vorbisBlock("TITLE=Low Tide Radio", "ARTIST=A", "ALBUMARTIST=Various", "ALBUM=B", "TRACKNUMBER=2", "DISCNUMBER=1/2", "DATE=2009-05-01")
	b = append(b, 0x84, byte(len(c)>>16), byte(len(c)>>8), byte(len(c)))
	return append(b, c...)
}

// oggPage is one Ogg page holding packet p whole.
func oggPage(granule uint64, p []byte) []byte {
	h := make([]byte, 27)
	copy(h, "OggS")
	binary.LittleEndian.PutUint64(h[6:], granule)
	var segs []byte
	n := len(p)
	for n >= 255 {
		segs = append(segs, 255)
		n -= 255
	}
	segs = append(segs, byte(n))
	h[26] = byte(len(segs))
	return append(append(h, segs...), p...)
}

func testOpus() []byte {
	head := make([]byte, 19)
	copy(head, "OpusHead")
	binary.LittleEndian.PutUint16(head[10:], 312)
	b := oggPage(0, head)
	b = append(b, oggPage(0, append([]byte("OpusTags"), vorbisBlock("title=Antenna", "artist=C")...))...)
	return append(b, oggPage(48000*95+312, make([]byte, 300))...)
}

func box(typ string, body ...[]byte) []byte {
	b := bytes.Join(body, nil)
	h := make([]byte, 8)
	binary.BigEndian.PutUint32(h, uint32(8+len(b)))
	copy(h[4:], typ)
	return append(h, b...)
}

func testM4A() []byte {
	mvhd := make([]byte, 100)
	binary.BigEndian.PutUint32(mvhd[12:], 1000)
	binary.BigEndian.PutUint32(mvhd[16:], 181000)
	data := func(typ uint32, v []byte) []byte {
		h := make([]byte, 8)
		binary.BigEndian.PutUint32(h, typ)
		return box("data", h, v)
	}
	ilst := box("ilst", box("\xa9nam", data(1, []byte("Tape Hiss"))), box("\xa9ART", data(1, []byte("D"))),
		box("trkn", data(0, []byte{0, 0, 0, 7, 0, 9, 0, 0})))
	meta := box("meta", []byte{0, 0, 0, 0}, box("hdlr", make([]byte, 25)), ilst)
	return append(box("ftyp", []byte("M4A ")), box("moov", box("mvhd", mvhd), box("udta", meta))...)
}

func testWAV(secs int) []byte {
	b := []byte("RIFF\x00\x00\x00\x00WAVE")
	f := make([]byte, 16)
	binary.LittleEndian.PutUint16(f[2:], 2)
	binary.LittleEndian.PutUint32(f[4:], 8000)
	binary.LittleEndian.PutUint32(f[8:], 32000)
	b = append(b, "fmt \x10\x00\x00\x00"...)
	b = append(b, f...)
	d := make([]byte, 8)
	copy(d, "data")
	binary.LittleEndian.PutUint32(d[4:], uint32(32000*secs))
	return append(append(b, d...), make([]byte, 32000*secs)...)
}

func TestReadTags(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// 1000 frames of 1152 samples at 44.1 kHz: 26 s
	if got := readTags(write("a.mp3", testMP3(1000))); got.Title != "Signal Fires" || got.Artist != "The Static Gardens" ||
		got.Album != "Night Signals" || got.Track != 3 || got.Year != 2007 || got.Genre != "Rock" || got.Duration != 26 || got.SampleRate != 44100 {
		t.Errorf("mp3: %+v", got)
	}
	if got := readTags(write("b.flac", testFLAC())); got.Title != "Low Tide Radio" || got.AlbumArtist != "Various" ||
		got.Track != 2 || got.Disc != 1 || got.Year != 2009 || got.Duration != 200 || got.SampleRate != 44100 {
		t.Errorf("flac: %+v", got)
	}
	if got := readTags(write("c.opus", testOpus())); got.Title != "Antenna" || got.Artist != "C" || got.Duration != 95 {
		t.Errorf("opus: %+v", got)
	}
	if got := readTags(write("d.m4a", testM4A())); got.Title != "Tape Hiss" || got.Artist != "D" || got.Track != 7 || got.Duration != 181 {
		t.Errorf("m4a: %+v", got)
	}
	if got := readTags(write("e.wav", testWAV(3))); got.Duration != 3 || got.SampleRate != 8000 {
		t.Errorf("wav: %+v", got)
	}
	// broken or empty files give empty tags, not a panic
	for _, name := range []string{"f.mp3", "f.flac", "f.ogg", "f.m4a", "f.wav"} {
		readTags(write(name, []byte("ID3\x04\x00\x00\x7f\x7f\x7f\x7f fLaC OggS")))
		readTags(write("0"+name, nil))
	}
}

func TestID3Text(t *testing.T) {
	utf16le := []byte{1, 0xff, 0xfe, 'H', 0, 0xe9, 0, 0, 0, 'x', 0}
	if got := id3Text("TIT2", utf16le); got != "Hé" {
		t.Errorf("UTF-16: %q", got)
	}
	if got := id3Text("TIT2", []byte{0, 'C', 'a', 'f', 0xe9}); got != "Café" {
		t.Errorf("Latin-1: %q", got)
	}
	if got := id3Text("USLT", []byte("\x03engdesc\x00Line one\nLine two")); got != "Line one\nLine two" {
		t.Errorf("lyrics: %q", got)
	}
}

func TestParseLRC(t *testing.T) {
	lines, synced := parseLRC("[ar:Someone]\r\n[00:12.50]Second\n[00:01.00][00:20.00]First and third\n\nno time\n")
	if !synced || len(lines) != 3 || lines[0].At != time.Second || lines[0].Text != "First and third" ||
		lines[1].At != 12500*time.Millisecond || lines[2].At != 20*time.Second {
		t.Fatalf("timed: %v %+v", synced, lines)
	}
	lines, synced = parseLRC("\nOne\n\nTwo\n\n")
	if synced || len(lines) != 3 || lines[0].Text != "One" || lines[2].Text != "Two" {
		t.Fatalf("plain: %v %+v", synced, lines)
	}
	if lines, _ := parseLRC(""); len(lines) != 0 {
		t.Fatalf("empty: %+v", lines)
	}
}

// The folder is read once into the index; a second read opens only the
// files that changed, and a new cover is picked up.
func TestScanMusic(t *testing.T) {
	root := t.TempDir()
	al := filepath.Join(root, "Gardens", "Night Signals")
	os.MkdirAll(al, 0o755)
	os.WriteFile(filepath.Join(al, "02 b.mp3"), testMP3(100), 0o644)
	os.WriteFile(filepath.Join(al, "01 untagged.wav"), testWAV(1), 0o644)
	os.WriteFile(filepath.Join(al, "notes.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(al, "._02 b.mp3"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, "loose.flac"), testFLAC(), 0o644)

	lib := &localLib{file: filepath.Join(t.TempDir(), localIndexFile)}
	all, err := lib.all(root, nil)
	if err != nil || len(all) != 3 {
		t.Fatalf("%d songs, %v", len(all), err)
	}
	if all[0].Title != "01 untagged" || all[0].Path != all[0].ID || all[0].Suffix != "wav" {
		t.Errorf("untagged: %+v", all[0])
	}
	// from the index file, without reading the folder
	again := &localLib{file: lib.file}
	os.WriteFile(filepath.Join(al, "Cover.JPG"), []byte("x"), 0o644)
	if all, _ := again.all(root, nil); len(all) != 3 || all[1].CoverArt != embedPrefix+all[1].Path {
		t.Fatalf("index: %+v", all)
	}
	again.refresh()
	all, _ = again.all(root, nil)
	if all[1].CoverArt != filePrefix+filepath.Join(al, "Cover.JPG") || all[1].Title != "Signal Fires" {
		t.Errorf("after refresh: %+v", all[1])
	}
	if _, err := (&localLib{file: lib.file}).all(filepath.Join(root, "missing"), nil); err == nil {
		t.Error("no error for a missing folder")
	}
}

func TestLocalLists(t *testing.T) {
	ls := func(path, album, artist, aa string, disc, track int) localSong {
		return localSong{Song: Song{ID: path, Path: path, Title: filepath.Base(path), Album: album, Artist: artist, Track: track}, AlbumArtist: aa, Disc: disc}
	}
	all := []localSong{
		ls("/m/x.mp3", "", "", "", 0, 0),
		ls("/m/b/2.mp3", "B", "P", "", 1, 2),
		ls("/m/b/1.mp3", "B", "P", "", 1, 1),
		ls("/m/b/cd2/1.mp3", "B", "P", "", 2, 1),
		ls("/m/a/1.mp3", "Mix", "Q", "Various", 0, 1),
		ls("/m/a/2.mp3", "Mix", "R", "Various", 0, 2),
	}
	songs, direct, dirs := folderContents(all, "/m/b")
	if direct != 2 || len(songs) != 3 || songs[0].Path != "/m/b/1.mp3" || len(dirs) != 1 || dirs[0].path != "/m/b/cd2" {
		t.Fatalf("folder: %d %+v %+v", direct, songs, dirs)
	}
	if _, direct, dirs := folderContents(all, "/m"); direct != 1 || len(dirs) != 2 || dirs[0].name != "a" || dirs[0].songs != 2 {
		t.Fatalf("root: %d %+v", direct, dirs)
	}
	al := localAlbums(all)
	if len(al) != 2 || al[0].Name != "B" || al[0].SongCount != 3 || al[1].Artist != "Various" {
		t.Fatalf("albums: %+v", al)
	}
	var paths []string
	for _, s := range localAlbumSongs(all, al[0].ID) {
		paths = append(paths, s.Path)
	}
	if strings.Join(paths, " ") != "/m/b/1.mp3 /m/b/2.mp3 /m/b/cd2/1.mp3" {
		t.Fatalf("album order: %v", paths)
	}
	if ars := localArtists(all); len(ars) != 2 || ars[0].Name != "P" || ars[1].Name != "Various" {
		t.Fatalf("artists: %+v", ars)
	}
}

// The playing marker goes on the song's own row, not on the row at the
// song's index: Search and Starred list albums above the songs.
func TestPlayingMarkerFollowsValue(t *testing.T) {
	a := testApp(t, "http://127.0.0.1:1")
	songs := []Song{{ID: "s1", Duration: 61}, {ID: "s2", Duration: 62}}
	a.player = &stillPlayer{NowPlaying{Song: songs[1], Has: true, Playing: true}}
	items := append([]ui.Item{{Label: "An album", Value: Album{ID: "al"}}}, a.songItems(songs)...)
	a.stack = []*view{{title: "Search", list: ui.List{Items: items}, songs: songs}}
	a.draw()
	got := []string{items[0].Detail, items[1].Detail, items[2].Detail}
	if strings.Join(got, ",") != ",1:01,playing" {
		t.Fatalf("details: %q", got)
	}
}

// A local song plays from its file, seeking with -ss, and isn't
// prefetched through the relay.
func TestPlayerPlaysFiles(t *testing.T) {
	dir := t.TempDir()
	ff := filepath.Join(dir, "ffplay")
	os.WriteFile(ff, []byte("#!/bin/sh\necho \"$@\" >> "+filepath.Join(dir, "args")+"\nexec sleep 30\n"), 0o755)
	t.Setenv("MINIDECK_FFPLAY", ff)
	old := chrtPath
	chrtPath = ""
	defer func() { chrtPath = old }()
	p := NewPlayer(func(s Song, at int) string { return "http://relay/" + s.ID }, nil)
	p.prefetch = func(s Song) { t.Errorf("prefetched %s", s.ID) }
	defer p.Stop()
	q := []Song{{ID: "/m/a.mp3", Path: "/m/a.mp3", Duration: 100}, {ID: "/m/b.mp3", Path: "/m/b.mp3"}}
	// waitArgs returns the arguments ffplay was started with, once it has
	// been started n times.
	waitArgs := func(n int) string {
		deadline := time.Now().Add(3 * time.Second)
		for {
			b, _ := os.ReadFile(filepath.Join(dir, "args"))
			if strings.Count(string(b), "\n") >= n {
				return string(b)
			}
			if time.Now().After(deadline) {
				t.Fatalf("ffplay not started %d times: %q", n, b)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	p.Play(q, 0)
	waitArgs(1)
	p.Seek(30)
	if got := waitArgs(2); !strings.HasSuffix(strings.Split(got, "\n")[0], "-loglevel quiet /m/a.mp3") || !strings.Contains(got, "-ss 30 /m/a.mp3\n") {
		t.Fatalf("ffplay args:\n%s", got)
	}
}

// Repeat one plays the song again, repeat all goes from the last song to
// the first; each counts as a new play.
func TestPlayerRepeat(t *testing.T) {
	ff := filepath.Join(t.TempDir(), "ffplay")
	os.WriteFile(ff, []byte("#!/bin/sh\nexec sleep 0.2\n"), 0o755)
	t.Setenv("MINIDECK_FFPLAY", ff)
	old := chrtPath
	chrtPath = ""
	defer func() { chrtPath = old }()
	p := NewPlayer(func(s Song, at int) string { return "x" }, nil)
	defer p.Stop()
	p.SetRepeat(repeatOne)
	p.Play([]Song{{ID: "a"}, {ID: "b"}}, 0)
	plays := p.State().Plays
	time.Sleep(700 * time.Millisecond)
	if st := p.State(); st.Song.ID != "a" || !st.Playing || st.Plays <= plays || st.Repeat != repeatOne {
		t.Fatalf("repeat one: %+v", st)
	}
	p.SetRepeat(repeatAll)
	p.Play([]Song{{ID: "a"}, {ID: "b"}}, 1)
	p.Next()
	if st := p.State(); st.Song.ID != "a" || !st.Playing {
		t.Fatalf("repeat all, next from the last: %+v", st)
	}
	p.SetRepeat(repeatOff)
	p.Play([]Song{{ID: "a"}}, 0)
	before := p.State().Plays
	p.Stop()
	p.Play([]Song{{ID: "a"}}, 0) // the same song again is a new play
	if p.State().Plays == before {
		t.Fatal("playing the song again isn't a new play")
	}
}

// testPNG is a small picture, as a cover in a file would be.
func testPNG(t *testing.T) []byte {
	var b bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	img.Set(1, 1, color.RGBA{200, 10, 10, 255})
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// flacPictureBlock is a FLAC PICTURE block's body.
func flacPictureBlock(typ uint32, pic []byte) []byte {
	var b bytes.Buffer
	for _, v := range []any{typ, uint32(9), "image/png", uint32(0), [16]byte{}, uint32(len(pic)), pic} {
		if s, ok := v.(string); ok {
			b.WriteString(s)
		} else {
			binary.Write(&b, binary.BigEndian, v)
		}
	}
	return b.Bytes()
}

// The cover in a file's tags: ID3 APIC (the front cover over the first),
// FLAC PICTURE, Vorbis METADATA_BLOCK_PICTURE and MP4 covr.
func TestReadPicture(t *testing.T) {
	dir := t.TempDir()
	pic := testPNG(t)
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, b, 0o644)
		return p
	}
	apic := func(typ byte, data []byte) []byte {
		body := append(append([]byte("\x00image/png\x00"), typ), "a cover\x00"...)
		body = append(body, data...)
		var b bytes.Buffer
		b.WriteString("APIC")
		binary.Write(&b, binary.BigEndian, uint32(len(body)))
		b.Write([]byte{0, 0})
		b.Write(body)
		return b.Bytes()
	}
	body := append(apic(4, []byte("back")), apic(3, pic)...)
	n := len(body)
	mp3 := append([]byte{'I', 'D', '3', 3, 0, 0, byte(n >> 21 & 0x7f), byte(n >> 14 & 0x7f), byte(n >> 7 & 0x7f), byte(n & 0x7f)}, body...)

	fl := testFLAC()
	fl[4+4+18] &^= 0x80 // the comments are no longer the last block
	pb := flacPictureBlock(3, pic)
	fl = append(fl, 0x86, byte(len(pb)>>16), byte(len(pb)>>8), byte(len(pb)))
	fl = append(fl, pb...)

	opus := append(oggPage(0, append([]byte("OpusHead"), make([]byte, 11)...)),
		oggPage(0, append([]byte("OpusTags"), vorbisBlock("title=x", "METADATA_BLOCK_PICTURE="+base64.StdEncoding.EncodeToString(pb))...))...)

	data := make([]byte, 8)
	binary.BigEndian.PutUint32(data, 14)
	m4a := box("moov", box("udta", box("meta", []byte{0, 0, 0, 0}, box("hdlr", make([]byte, 25)),
		box("ilst", box("covr", box("data", data, pic))))))

	for name, b := range map[string][]byte{"a.mp3": mp3, "b.flac": fl, "c.opus": opus, "d.m4a": m4a} {
		if got := readPicture(write(name, b)); !bytes.Equal(got, pic) {
			t.Errorf("%s: %d bytes, not the cover", name, len(got))
		}
	}
	if got := readPicture(write("e.flac", testFLAC())); got != nil {
		t.Errorf("a cover from a file without one: %d bytes", len(got))
	}
	// shown as a cover, scaled
	if img, err := localCover(nil, embedPrefix+filepath.Join(dir, "b.flac"), 16); err != nil || img.Bounds().Dx() != 16 {
		t.Fatalf("cover: %v", err)
	}
	if _, err := localCover(nil, embedPrefix+filepath.Join(dir, "e.flac"), 16); err == nil {
		t.Fatal("a cover from a file without one")
	}
}

// Broken files, cut short or with bytes changed, never panic, and never
// claim a length below zero or beyond belief.
func TestTagsOnBrokenFiles(t *testing.T) {
	dir := t.TempDir()
	rng := rand.New(rand.NewPCG(1, 2))
	files := map[string][]byte{".mp3": testMP3(1000), ".flac": testFLAC(), ".opus": testOpus(), ".m4a": testM4A(), ".wav": testWAV(1)}
	for ext, good := range files {
		for i := range 300 {
			b := append([]byte(nil), good[:rng.IntN(len(good)+1)]...)
			if i%2 == 0 {
				b = append([]byte(nil), good...)
			}
			for range 1 + rng.IntN(8) {
				if len(b) > 0 {
					b[rng.IntN(len(b))] = byte(rng.Uint32())
				}
			}
			p := filepath.Join(dir, "x"+ext)
			os.WriteFile(p, b, 0o644)
			got := readTags(p)
			readPicture(p)
			if got.Duration < 0 || got.Duration > maxSecs || got.BitRate < 0 {
				t.Fatalf("%s: %+v", ext, got)
			}
		}
	}
	// sizes in the headers far beyond the file: nothing read, nothing huge
	huge := append([]byte("fLaC"), 0x06, 0xff, 0xff, 0xff)
	os.WriteFile(filepath.Join(dir, "h.flac"), huge, 0o644)
	if readPicture(filepath.Join(dir, "h.flac")) != nil {
		t.Fatal("a picture from nothing")
	}
}
