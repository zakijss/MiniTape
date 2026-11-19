package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"
)

// tags is what MiniTape shows of a music file on the card: its tags
// (ID3, Vorbis comments, MP4) and its length. readTags reads only the
// headers, a few KB of most files, and skips embedded pictures.
type tags struct {
	Title, Artist, Album, AlbumArtist, Genre, Lyrics string
	Track, Disc, Year                                int
	Duration                                         int // seconds
	BitRate                                          int // kbit/s
	SampleRate                                       int // Hz

	// pic asks for the embedded cover, which readPicture returns: the
	// front cover, or else the first picture.
	pic      bool
	picture  []byte
	picFront bool
	boxes    int // MP4 boxes read, in all levels
}

const (
	// maxFrame caps a tag field read into memory; bigger ones are pictures.
	maxFrame = 1 << 20
	// maxPicture caps an embedded cover read into memory.
	maxPicture = 8 << 20
	// maxSecs is the longest length believed: a broken or hostile file
	// can claim anything.
	maxSecs = 100 * 3600
	// maxChunks caps the MP4 boxes or WAV chunks walked in a file: a
	// crafted one can hold millions of empty ones.
	maxChunks = 4096
)

// secs is a length in seconds, 0 when it can't be right (and before it
// is cut to a 32-bit int on the device).
func secs(n int64) int {
	if n < 0 || n > maxSecs {
		return 0
	}
	return int(n)
}

// readTags reads the tags of the file at path. A file it can't read, or a
// format it doesn't know, gives empty tags.
func readTags(path string) tags {
	var t tags
	f, err := os.Open(path)
	if err != nil {
		return t
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return t
	}
	size := fi.Size()
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp3":
		t.mp3(f, size)
	case ".flac":
		t.flac(f, size)
	case ".ogg", ".oga", ".opus":
		t.ogg(f, size)
	case ".m4a", ".m4b", ".mp4":
		t.atoms(f, 0, size, false, 0)
		if t.Duration > 0 {
			t.BitRate = int(size * 8 / int64(t.Duration) / 1000)
		}
	case ".wav":
		t.wav(f, size)
	default:
		t.id3(f)
	}
	return t
}

// readPicture returns the cover embedded in the music file at path (ID3
// APIC, FLAC or Vorbis PICTURE, MP4 covr), or nil. Only the tags are read.
func readPicture(path string) []byte {
	t := tags{pic: true}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	switch size := fi.Size(); strings.ToLower(filepath.Ext(path)) {
	case ".flac":
		t.flac(f, size)
	case ".ogg", ".oga", ".opus":
		t.ogg(f, size)
	case ".m4a", ".m4b", ".mp4":
		t.atoms(f, 0, size, false, 0)
	case ".wav":
	default:
		t.id3(f)
	}
	return t.picture
}

// setPicture keeps an embedded picture of type typ (3 is the front cover).
func (t *tags) setPicture(typ int64, b []byte) {
	if len(b) > 0 && (t.picture == nil || typ == 3 && !t.picFront) {
		t.picture, t.picFront = b, typ == 3
	}
}

// flacPicture reads a FLAC PICTURE block (also base64 in Vorbis comments).
func (t *tags) flacPicture(b []byte) {
	off := int64(4) // the picture type
	for range 2 {   // the MIME type, then the description
		if int64(len(b)) < off+4 {
			return
		}
		off += 4 + be32(b[off:off+4])
	}
	off += 16 // width, height, depth and colours
	if int64(len(b)) < off+4 {
		return
	}
	n := be32(b[off : off+4])
	if off += 4; n > int64(len(b))-off {
		return
	}
	t.setPicture(be32(b[:4]), b[off:off+n])
}

// id3Picture reads an APIC (or v2.2 PIC) frame: the encoding, the MIME
// type (PIC: a 3-letter format), the picture type, a description, then
// the picture.
func (t *tags) id3Picture(id string, b []byte) {
	if len(b) < 1 {
		return
	}
	enc, b := b[0], b[1:]
	if id == "PIC" {
		b = b[min(3, len(b)):]
	} else {
		_, b = id3Split(0, b)
	}
	if len(b) < 1 {
		return
	}
	_, data := id3Split(enc, b[1:])
	t.setPicture(int64(b[0]), data)
}

// tagKeys maps the tag names of each format to the fields they fill.
var tagKeys = map[string]string{
	// Vorbis comments (upper case)
	"TITLE": "title", "ARTIST": "artist", "ALBUM": "album", "ALBUMARTIST": "albumartist",
	"ALBUM ARTIST": "albumartist", "GENRE": "genre", "DATE": "year", "YEAR": "year",
	"TRACKNUMBER": "track", "DISCNUMBER": "disc", "LYRICS": "lyrics", "UNSYNCEDLYRICS": "lyrics",
	// ID3v2.3 and 2.4
	"TIT2": "title", "TPE1": "artist", "TALB": "album", "TPE2": "albumartist", "TCON": "genre",
	"TYER": "year", "TDRC": "year", "TRCK": "track", "TPOS": "disc", "USLT": "lyrics",
	// ID3v2.2
	"TT2": "title", "TP1": "artist", "TAL": "album", "TP2": "albumartist", "TCO": "genre",
	"TYE": "year", "TRK": "track", "TPA": "disc", "ULT": "lyrics",
	// MP4 (iTunes)
	"\xa9nam": "title", "\xa9ART": "artist", "\xa9alb": "album", "aART": "albumartist",
	"\xa9gen": "genre", "\xa9day": "year", "\xa9lyr": "lyrics",
}

// set fills the field key names with v, unless an earlier tag already
// did.
func (t *tags) set(key, v string) {
	v = strings.TrimSpace(strings.Trim(v, "\x00"))
	if v == "" {
		return
	}
	str := func(p *string) {
		if *p == "" {
			*p = v
		}
	}
	num := func(p *int) {
		if *p == 0 {
			*p = leadingInt(v)
		}
	}
	switch tagKeys[key] {
	case "title":
		str(&t.Title)
	case "artist":
		str(&t.Artist)
	case "album":
		str(&t.Album)
	case "albumartist":
		str(&t.AlbumArtist)
	case "genre":
		str(&t.Genre)
		t.Genre = id3Genre(t.Genre)
	case "lyrics":
		str(&t.Lyrics)
	case "year":
		num(&t.Year)
	case "track":
		num(&t.Track)
	case "disc":
		num(&t.Disc)
	}
}

// leadingInt is the number v starts with: 3 for "3/12", 2009 for
// "2009-05-01".
func leadingInt(v string) int {
	i := 0
	for i < len(v) && v[i] >= '0' && v[i] <= '9' {
		i++
	}
	if i > 9 { // nothing real, and it wouldn't fit
		return 0
	}
	n, _ := strconv.Atoi(v[:i])
	return n
}

// id3Genre drops ID3's numbered genre references: "(17)Rock" is "Rock",
// and "(17)" or "17" alone is nothing.
func id3Genre(g string) string {
	if strings.HasPrefix(g, "(") {
		if i := strings.IndexByte(g, ')'); i > 0 {
			g = g[i+1:]
		}
	}
	if leadingInt(g) > 0 && strconv.Itoa(leadingInt(g)) == g {
		return ""
	}
	return g
}

func readAt(r io.ReaderAt, off int64, n int) []byte {
	b := make([]byte, n)
	got, _ := r.ReadAt(b, off)
	return b[:got]
}

func be32(b []byte) int64 { return int64(binary.BigEndian.Uint32(b)) }

func syncsafe(b []byte) int64 {
	return int64(b[0]&0x7f)<<21 | int64(b[1]&0x7f)<<14 | int64(b[2]&0x7f)<<7 | int64(b[3]&0x7f)
}

// id3 reads an ID3v2 tag at the start of r, and returns its length (0
// when there is none).
func (t *tags) id3(r io.ReaderAt) int64 {
	h := readAt(r, 0, 10)
	if len(h) < 10 || string(h[:3]) != "ID3" {
		return 0
	}
	ver, flags := h[3], h[5]
	end := 10 + syncsafe(h[6:10])
	length := end
	if flags&0x10 != 0 { // a footer
		length += 10
	}
	pos := int64(10)
	if flags&0x40 != 0 && ver >= 3 { // an extended header
		ext := readAt(r, pos, 4)
		if len(ext) < 4 {
			return length
		}
		if ver == 4 {
			pos += syncsafe(ext)
		} else {
			pos += 4 + be32(ext)
		}
	}
	idLen, hdrLen := 4, int64(10)
	if ver == 2 {
		idLen, hdrLen = 3, 6
	}
	// (frames, blocks and boxes are counted too: a broken file of
	// tiny ones would take long to walk)
	for frames := 0; pos+hdrLen <= end && frames < 4096; frames++ {
		fh := readAt(r, pos, int(hdrLen))
		if len(fh) < int(hdrLen) || fh[0] == 0 { // padding
			break
		}
		id := string(fh[:idLen])
		var n int64
		var fflags byte
		switch ver {
		case 2:
			n = int64(fh[3])<<16 | int64(fh[4])<<8 | int64(fh[5])
		case 3:
			n, fflags = be32(fh[4:8]), fh[9]&0xc0 // compressed or encrypted
		default:
			n, fflags = syncsafe(fh[4:8]), fh[9]
		}
		pos += hdrLen
		if n <= 0 || pos+n > end {
			break
		}
		isPic := id == "APIC" || id == "PIC"
		want := tagKeys[id] != "" && n <= maxFrame || t.pic && isPic && n <= maxPicture
		if want && fflags&0xcc == 0 {
			body := readAt(r, pos, int(n))
			if ver == 4 && fflags&0x01 != 0 && len(body) >= 4 { // a data length first
				body = body[4:]
			}
			if ver == 4 && fflags&0x02 != 0 || ver < 4 && flags&0x80 != 0 { // unsynchronised
				body = bytes.ReplaceAll(body, []byte{0xff, 0}, []byte{0xff})
			}
			if isPic {
				t.id3Picture(id, body)
			} else {
				t.set(id, id3Text(id, body))
			}
		}
		pos += n
	}
	return length
}

// id3Text is the text of an ID3 text frame (the first value) or lyrics
// frame.
func id3Text(id string, b []byte) string {
	if len(b) < 1 {
		return ""
	}
	enc, b := b[0], b[1:]
	if id == "USLT" || id == "ULT" {
		if len(b) < 3 {
			return ""
		}
		_, b = id3Split(enc, b[3:]) // the language, then a description
	}
	s, _ := id3Split(enc, b)
	return id3Decode(enc, s)
}

// id3Split cuts b at the first string terminator of encoding enc.
func id3Split(enc byte, b []byte) (first, rest []byte) {
	if enc == 1 || enc == 2 { // UTF-16: two zero bytes, on a character boundary
		for i := 0; i+1 < len(b); i += 2 {
			if b[i] == 0 && b[i+1] == 0 {
				return b[:i], b[i+2:]
			}
		}
		return b, nil
	}
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return b[:i], b[i+1:]
	}
	return b, nil
}

// id3Decode turns ID3 text in encoding enc into UTF-8.
func id3Decode(enc byte, b []byte) string {
	switch enc {
	case 0: // ISO-8859-1
		r := make([]rune, len(b))
		for i, c := range b {
			r[i] = rune(c)
		}
		return string(r)
	case 1, 2: // UTF-16 with a byte order mark, or big-endian
		order := binary.ByteOrder(binary.BigEndian)
		if len(b) >= 2 && b[0] == 0xff && b[1] == 0xfe {
			order, b = binary.LittleEndian, b[2:]
		} else if len(b) >= 2 && b[0] == 0xfe && b[1] == 0xff {
			b = b[2:]
		}
		u := make([]uint16, len(b)/2)
		for i := range u {
			u[i] = order.Uint16(b[2*i:])
		}
		return string(utf16.Decode(u))
	}
	return string(b)
}

// MP3 bit rates by index, in kbit/s: MPEG-1 and MPEG-2/2.5 Layer III.
var (
	mp3Rates1 = [15]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}
	mp3Rates2 = [15]int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}
)

// mp3 reads the ID3 tags and the length: from the Xing or VBRI header of
// a variable bit rate file, else from the size and the bit rate.
func (t *tags) mp3(r io.ReaderAt, size int64) {
	start := t.id3(r)
	if t.Title == "" {
		t.id3v1(r, size)
	}
	buf := readAt(r, start, 8<<10)
	for i := 0; i+4 <= len(buf); i++ {
		h := buf[i:]
		if h[0] != 0xff || h[1]&0xe0 != 0xe0 {
			continue
		}
		ver, layer := h[1]>>3&3, h[1]>>1&3 // ver 3: MPEG-1; 2: MPEG-2; 0: MPEG-2.5. layer 1: III
		bri, sri := h[2]>>4, h[2]>>2&3
		if ver == 1 || layer != 1 || bri == 0 || bri == 15 || sri == 3 {
			continue
		}
		mpeg1 := ver == 3
		rate := [3]int{44100, 48000, 32000}[sri]
		kbps, spf, side := mp3Rates1[bri], 1152, 32
		if !mpeg1 {
			rate /= 2
			if ver == 0 {
				rate /= 2
			}
			kbps, spf, side = mp3Rates2[bri], 576, 17
		}
		if h[3]>>6 == 3 { // mono
			side = 9
			if mpeg1 {
				side = 17
			}
		}
		t.SampleRate = rate
		frames := int64(0)
		if x := h[min(len(h), 4+side):]; len(x) >= 12 && (string(x[:4]) == "Xing" || string(x[:4]) == "Info") {
			if be32(x[4:8])&1 != 0 {
				frames = be32(x[8:12])
			}
		} else if x := h[min(len(h), 36):]; len(x) >= 18 && string(x[:4]) == "VBRI" {
			frames = be32(x[14:18])
		}
		audio := size - start - int64(i)
		if string(readAt(r, size-128, 3)) == "TAG" {
			audio -= 128
		}
		if frames > 0 {
			t.Duration = secs(frames * int64(spf) / int64(rate))
			if t.Duration > 0 {
				kbps = int(audio * 8 / int64(t.Duration) / 1000)
			}
		} else {
			t.Duration = secs(audio * 8 / int64(kbps*1000))
		}
		t.BitRate = kbps
		return
	}
}

// id3v1 reads the old 128-byte tag at the end of an MP3.
func (t *tags) id3v1(r io.ReaderAt, size int64) {
	b := readAt(r, size-128, 128)
	if len(b) < 128 || string(b[:3]) != "TAG" {
		return
	}
	field := func(b []byte) string {
		b, _ = id3Split(0, b)
		return id3Decode(0, b)
	}
	t.set("TIT2", field(b[3:33]))
	t.set("TPE1", field(b[33:63]))
	t.set("TALB", field(b[63:93]))
	t.set("TYER", field(b[93:97]))
	if b[125] == 0 && b[126] != 0 { // ID3v1.1: a track number
		t.set("TRCK", strconv.Itoa(int(b[126])))
	}
}

// flac reads the stream info (the length) and the Vorbis comments.
func (t *tags) flac(r io.ReaderAt, size int64) {
	pos := t.id3(r) // a few taggers put ID3 in front
	if string(readAt(r, pos, 4)) != "fLaC" {
		return
	}
	pos += 4
	for blocks := 0; pos+4 <= size && blocks < 1024; blocks++ {
		h := readAt(r, pos, 4)
		if len(h) < 4 {
			return
		}
		typ, n := h[0]&0x7f, int64(h[1])<<16|int64(h[2])<<8|int64(h[3])
		pos += 4
		switch {
		case typ == 0 && n >= 18:
			b := readAt(r, pos, 18)
			if len(b) == 18 {
				rate := int(b[10])<<12 | int(b[11])<<4 | int(b[12])>>4
				total := int64(b[13]&0x0f)<<32 | be32(b[14:18])
				if rate > 0 {
					t.SampleRate, t.Duration = rate, secs(total/int64(rate))
				}
			}
		case typ == 4 && n <= maxFrame:
			t.vorbis(readAt(r, pos, int(n)))
		case typ == 6 && t.pic && n <= maxPicture:
			t.flacPicture(readAt(r, pos, int(n)))
		}
		pos += n
		if h[0]&0x80 != 0 { // the last block
			break
		}
	}
	if t.Duration > 0 {
		t.BitRate = int(size * 8 / int64(t.Duration) / 1000)
	}
}

// vorbis reads a Vorbis comment block (FLAC, Ogg Vorbis, Opus). A block
// cut short gives the comments before the cut.
func (t *tags) vorbis(b []byte) {
	next := func() ([]byte, bool) {
		if len(b) < 4 {
			return nil, false
		}
		n := int64(binary.LittleEndian.Uint32(b))
		b = b[4:]
		if n > int64(len(b)) {
			s := b
			b = nil
			return s, false
		}
		s := b[:n]
		b = b[n:]
		return s, true
	}
	if _, ok := next(); !ok || len(b) < 4 { // the vendor
		return
	}
	count := binary.LittleEndian.Uint32(b)
	b = b[4:]
	for range min(count, 10000) {
		c, ok := next()
		if k, v, found := strings.Cut(string(c), "="); found {
			if k = strings.ToUpper(k); k == "METADATA_BLOCK_PICTURE" && t.pic {
				if p, err := base64.StdEncoding.DecodeString(v); err == nil {
					t.flacPicture(p)
				}
			}
			t.set(k, v)
		}
		if !ok {
			return
		}
	}
}

// ogg reads an Ogg Vorbis or Opus file: the first two packets (the
// stream's details and the comments), and the length from the last page.
func (t *tags) ogg(r io.ReaderAt, size int64) {
	var pkts [][]byte
	var cur []byte
	pos := int64(0)
	for len(pkts) < 2 && pos < min(size, maxFrame) {
		h := readAt(r, pos, 27)
		if len(h) < 27 || string(h[:4]) != "OggS" {
			break
		}
		segs := readAt(r, pos+27, int(h[26]))
		total := 0
		for _, s := range segs {
			total += int(s)
		}
		data := readAt(r, pos+27+int64(len(segs)), total)
		off := 0
		for _, s := range segs {
			end := min(off+int(s), len(data))
			cur = append(cur, data[off:end]...)
			off = end
			if s < 255 {
				pkts, cur = append(pkts, cur), nil
				if len(pkts) == 2 {
					break
				}
			}
		}
		pos += 27 + int64(len(segs)) + int64(total)
	}
	if len(pkts) == 1 && cur != nil { // comments too big to read whole
		pkts = append(pkts, cur)
	}
	rate, skip := int64(0), int64(0)
	if len(pkts) > 0 {
		p := pkts[0]
		switch {
		case len(p) >= 16 && string(p[:7]) == "\x01vorbis":
			rate = int64(binary.LittleEndian.Uint32(p[12:16]))
			t.SampleRate = int(rate)
		case len(p) >= 12 && string(p[:8]) == "OpusHead":
			rate, skip = 48000, int64(binary.LittleEndian.Uint16(p[10:12]))
		}
	}
	if len(pkts) > 1 {
		if p := pkts[1]; bytes.HasPrefix(p, []byte("\x03vorbis")) {
			t.vorbis(p[7:])
		} else if bytes.HasPrefix(p, []byte("OpusTags")) {
			t.vorbis(p[8:])
		}
	}
	if rate <= 0 {
		return
	}
	tailAt := max(0, size-64<<10)
	tail := readAt(r, tailAt, int(size-tailAt))
	for i := bytes.LastIndex(tail, []byte("OggS")); i >= 0; i = bytes.LastIndex(tail[:i], []byte("OggS")) {
		if i+14 > len(tail) {
			continue
		}
		if g := int64(binary.LittleEndian.Uint64(tail[i+6 : i+14])); g > skip {
			if t.Duration = secs((g - skip) / rate); t.Duration > 0 {
				t.BitRate = int(size * 8 / int64(t.Duration) / 1000)
			}
			return
		}
	}
}

// atoms reads the MP4 boxes between start and end: the length from mvhd,
// the tags from the items in ilst.
func (t *tags) atoms(r io.ReaderAt, start, end int64, inList bool, depth int) {
	for pos := start; pos+8 <= end && depth < 6 && t.boxes < maxChunks; t.boxes++ {
		h := readAt(r, pos, 16)
		if len(h) < 8 {
			return
		}
		n, typ, hdr := be32(h[:4]), string(h[4:8]), int64(8)
		switch {
		case n == 1 && len(h) == 16:
			n, hdr = int64(binary.BigEndian.Uint64(h[8:16])), 16
		case n == 0:
			n = end - pos
		}
		if n < hdr || n > end-pos { // (n > end-pos: pos+n could overflow)
			return
		}
		body, bodyEnd := pos+hdr, pos+n
		switch {
		case typ == "moov" || typ == "udta":
			t.atoms(r, body, bodyEnd, false, depth+1)
		case typ == "meta":
			// a full box (version and flags first), except in QuickTime files
			if string(readAt(r, body+4, 4)) != "hdlr" {
				body += 4
			}
			t.atoms(r, body, bodyEnd, false, depth+1)
		case typ == "ilst":
			t.atoms(r, body, bodyEnd, true, depth+1)
		case typ == "mvhd":
			b := readAt(r, body, 32)
			if len(b) >= 20 && b[0] == 0 && be32(b[12:16]) > 0 {
				t.Duration = secs(be32(b[16:20]) / be32(b[12:16]))
			} else if len(b) == 32 && b[0] == 1 && be32(b[20:24]) > 0 {
				t.Duration = secs(int64(binary.BigEndian.Uint64(b[24:32])) / be32(b[20:24]))
			}
		case inList && (typ == "covr" && t.pic && n-hdr <= maxPicture ||
			n-hdr <= maxFrame && (typ == "trkn" || typ == "disk" || tagKeys[typ] != "")):
			// the value is in a "data" box: type, locale, then the value
			b := readAt(r, body, int(n-hdr))
			if len(b) < 16 || string(b[4:8]) != "data" || be32(b[:4]) < 16 {
				break
			}
			v := b[16:min(int64(len(b)), be32(b[:4]))]
			switch {
			case typ == "covr":
				t.setPicture(3, v)
			case typ == "trkn" && len(v) >= 4:
				t.Track = int(binary.BigEndian.Uint16(v[2:4]))
			case typ == "disk" && len(v) >= 4:
				t.Disc = int(binary.BigEndian.Uint16(v[2:4]))
			case typ != "trkn" && typ != "disk" && typ != "covr":
				t.set(typ, string(v))
			}
		}
		pos = bodyEnd
	}
}

// wav reads the length of a WAV file from its format and data chunks.
func (t *tags) wav(r io.ReaderAt, size int64) {
	h := readAt(r, 0, 12)
	if len(h) < 12 || string(h[:4]) != "RIFF" || string(h[8:12]) != "WAVE" {
		return
	}
	byteRate := int64(0)
	for pos, chunks := int64(12), 0; pos+8 <= size && chunks < maxChunks; chunks++ {
		c := readAt(r, pos, 8)
		if len(c) < 8 {
			return
		}
		n := int64(binary.LittleEndian.Uint32(c[4:8]))
		switch string(c[:4]) {
		case "fmt ":
			if b := readAt(r, pos+8, 16); len(b) == 16 {
				t.SampleRate = int(binary.LittleEndian.Uint32(b[4:8]))
				byteRate = int64(binary.LittleEndian.Uint32(b[8:12]))
			}
		case "data":
			if byteRate > 0 {
				t.Duration = secs(min(n, size-pos-8) / byteRate)
				t.BitRate = int(byteRate * 8 / 1000)
			}
			return
		}
		pos += 8 + n + n&1
	}
}
