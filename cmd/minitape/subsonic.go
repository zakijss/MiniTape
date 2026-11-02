package main

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/zakijss/minideck/cache"
	"github.com/zakijss/minideck/tnet"
	"github.com/zakijss/minideck/ui"
	_ "golang.org/x/image/webp" // some libraries have WebP covers
)

// Subsonic is a client for the Subsonic API (Navidrome, Subsonic,
// Airsonic, Gonic, LMS, Ampache...). It logs in with a salted token, and
// falls back to the older hex-encoded password for servers that don't
// accept tokens (error 41, e.g. Airsonic with LDAP accounts).
type Subsonic struct {
	Base, User, Password string
	HTTP                 *http.Client
	legacy               bool
	// Cache, if set, keeps lists, album pages, lyrics and covers on the
	// card: a cached reply is returned at once and refreshed in the
	// background for next time.
	Cache      *cache.Disk
	refreshMu  sync.Mutex
	refreshing map[string]bool
	// loginMu guards Base, User, Password and legacy once requests run in
	// the background: the player daemon swaps the login on /reload while
	// streams and scrobbles are in flight.
	loginMu sync.RWMutex
}

// SetLogin changes the server and login (after Settings).
func (c *Subsonic) SetLogin(base, user, password string) {
	c.loginMu.Lock()
	c.Base, c.User, c.Password, c.legacy = base, user, password, false
	c.loginMu.Unlock()
}

// cacheable endpoints: things that rarely change. Random lists, searches
// and scrobbles always go to the server.
var cacheable = map[string]bool{
	"getAlbumList2": true, "getAlbum": true, "getArtists": true, "getArtist": true,
	"getPlaylists": true, "getPlaylist": true, "getStarred2": true,
	"getLyricsBySongId": true, "getLyrics": true,
}

// cacheKey names a request without its per-request salt and token.
func (c *Subsonic) cacheKey(endpoint string, extra url.Values) string {
	c.loginMu.RLock()
	defer c.loginMu.RUnlock()
	return c.Base + "|" + c.User + "|" + endpoint + "|" + extra.Encode()
}

// Song is a track as the server lists it.
type Song struct {
	ID       string `json:"id"`
	AlbumID  string `json:"albumId"`
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	Album    string `json:"album"`
	CoverArt string `json:"coverArt"`
	Duration int    `json:"duration"`
	Size     int64  `json:"size,omitempty"` // bytes, of the file on the server
	Track    int    `json:"track"`
	// what the file on the server is (not what's streamed)
	Suffix       string `json:"suffix,omitempty"`
	BitRate      int    `json:"bitRate,omitempty"`      // kbit/s
	SamplingRate int    `json:"samplingRate,omitempty"` // Hz (OpenSubsonic)
	Year         int    `json:"year,omitempty"`
	Genre        string `json:"genre,omitempty"`
}

// Album is an album; Song is filled in only by Subsonic.Album.
type Album struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Artist    string `json:"artist"`
	Year      int    `json:"year"`
	SongCount int    `json:"songCount"`
	CoverArt  string `json:"coverArt"`
	Song      []Song `json:"song"`
}

// Artist is an artist; Album is filled in only by Subsonic.Artist.
type Artist struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	AlbumCount int     `json:"albumCount"`
	Album      []Album `json:"album"`
}

// Playlist is a playlist; Entry is filled in only by Subsonic.Playlist.
type Playlist struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	SongCount int    `json:"songCount"`
	Entry     []Song `json:"entry"`
}

type response struct {
	R struct {
		Status string `json:"status"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		AlbumList2 *struct {
			Album []Album `json:"album"`
		} `json:"albumList2"`
		Album   *Album  `json:"album"`
		Artist  *Artist `json:"artist"`
		Artists *struct {
			Index []struct {
				Artist []Artist `json:"artist"`
			} `json:"index"`
		} `json:"artists"`
		Playlists *struct {
			Playlist []Playlist `json:"playlist"`
		} `json:"playlists"`
		Playlist    *Playlist `json:"playlist"`
		RandomSongs *struct {
			Song []Song `json:"song"`
		} `json:"randomSongs"`
		SearchResult3 *struct {
			Song []Song `json:"song"`
		} `json:"searchResult3"`
		Lyrics *struct {
			Value string `json:"value"`
		} `json:"lyrics"`
		LyricsList *struct {
			StructuredLyrics []struct {
				Synced bool `json:"synced"`
				Offset int  `json:"offset"` // ms
				Line   []struct {
					Start int    `json:"start"` // ms
					Value string `json:"value"`
				} `json:"line"`
			} `json:"structuredLyrics"`
		} `json:"lyricsList"`
		Starred2 *struct {
			Album []Album `json:"album"`
			Song  []Song  `json:"song"`
		} `json:"starred2"`
	} `json:"subsonic-response"`
}

// params adds the auth parameters: token = md5(password + salt), or the
// legacy "enc:" password for servers without token support.
// Called with loginMu held for reading.
func (c *Subsonic) params(extra url.Values) url.Values {
	v := url.Values{"u": {c.User}, "v": {"1.16.1"}, "c": {"minitape"}, "f": {"json"}}
	if c.legacy {
		v.Set("p", "enc:"+hex.EncodeToString([]byte(c.Password)))
	} else {
		salt := make([]byte, 6)
		rand.Read(salt)
		s := hex.EncodeToString(salt)
		sum := md5.Sum([]byte(c.Password + s))
		v.Set("t", hex.EncodeToString(sum[:]))
		v.Set("s", s)
	}
	for k, vals := range extra {
		v[k] = vals
	}
	return v
}

// URL is the address of an API call, with the login in its parameters.
func (c *Subsonic) URL(endpoint string, extra url.Values) string {
	c.loginMu.RLock()
	defer c.loginMu.RUnlock()
	return strings.TrimRight(c.Base, "/") + "/rest/" + endpoint + "?" + c.params(extra).Encode()
}

func (c *Subsonic) call(endpoint string, extra url.Values) (*response, error) {
	useCache := c.Cache != nil && cacheable[endpoint] && !(endpoint == "getAlbumList2" && extra.Get("type") == "random")
	if useCache {
		key := c.cacheKey(endpoint, extra)
		if b, ok := c.Cache.Get(key); ok {
			var r response
			if json.Unmarshal(b, &r) == nil && r.R.Status == "ok" {
				go c.refresh(endpoint, extra, key)
				return &r, nil
			}
		}
	}
	r, raw, err := c.fetch(endpoint, extra)
	if err == nil && useCache {
		c.Cache.Put(c.cacheKey(endpoint, extra), raw)
	}
	return r, err
}

// refresh fetches a cached reply again in the background (once at a time
// per request), so the next visit shows the server's current list.
func (c *Subsonic) refresh(endpoint string, extra url.Values, key string) {
	c.refreshMu.Lock()
	if c.refreshing == nil {
		c.refreshing = map[string]bool{}
	}
	if c.refreshing[key] {
		c.refreshMu.Unlock()
		return
	}
	c.refreshing[key] = true
	c.refreshMu.Unlock()
	if _, raw, err := c.fetch(endpoint, extra); err == nil {
		c.Cache.Put(key, raw)
	}
	c.refreshMu.Lock()
	delete(c.refreshing, key)
	c.refreshMu.Unlock()
}

// fetch asks the server; it returns the reply and its raw JSON.
func (c *Subsonic) fetch(endpoint string, extra url.Values) (*response, []byte, error) {
	resp, err := c.HTTP.Get(c.URL(endpoint, extra))
	if err != nil {
		return nil, nil, cleanErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, nil, cleanErr(err)
	}
	var r response
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, nil, fmt.Errorf("bad reply: %v", err)
	}
	if r.R.Status != "ok" {
		c.loginMu.Lock()
		retry := r.R.Error != nil && r.R.Error.Code == 41 && !c.legacy
		if retry {
			c.legacy = true // the server doesn't take tokens: retry with the password
		}
		c.loginMu.Unlock()
		if retry {
			return c.fetch(endpoint, extra)
		}
		if r.R.Error != nil {
			if r.R.Error.Code == 40 {
				return nil, nil, screenError("Wrong username or password.")
			}
			return nil, nil, errors.New(r.R.Error.Message)
		}
		return nil, nil, errors.New("server said " + r.R.Status)
	}
	return &r, raw, nil
}

func cleanErr(err error) error { return tnet.Explain(err) }

// screenError is an error written as a sentence for the screen, where it
// is shown as it is.
type screenError string

func (e screenError) Error() string { return string(e) }

// Ping checks the server address and the login.
func (c *Subsonic) Ping() error {
	_, err := c.call("ping", nil)
	return err
}

// AlbumList returns up to size albums of a list type: newest, random,
// frequent, recent or alphabeticalByName.
func (c *Subsonic) AlbumList(typ string, size int) ([]Album, error) {
	r, err := c.call("getAlbumList2", url.Values{"type": {typ}, "size": {fmt.Sprint(size)}})
	if err != nil || r.R.AlbumList2 == nil {
		return nil, err
	}
	return r.R.AlbumList2.Album, nil
}

// Album returns an album with its songs.
func (c *Subsonic) Album(id string) (*Album, error) {
	r, err := c.call("getAlbum", url.Values{"id": {id}})
	if err != nil {
		return nil, err
	}
	if r.R.Album == nil {
		return nil, errors.New("album not found")
	}
	return r.R.Album, nil
}

// Artists returns every artist, A to Z.
func (c *Subsonic) Artists() ([]Artist, error) {
	r, err := c.call("getArtists", nil)
	if err != nil || r.R.Artists == nil {
		return nil, err
	}
	var out []Artist
	for _, ix := range r.R.Artists.Index {
		out = append(out, ix.Artist...)
	}
	return out, nil
}

// Artist returns an artist with their albums.
func (c *Subsonic) Artist(id string) (*Artist, error) {
	r, err := c.call("getArtist", url.Values{"id": {id}})
	if err != nil {
		return nil, err
	}
	if r.R.Artist == nil {
		return nil, errors.New("artist not found")
	}
	return r.R.Artist, nil
}

// Playlists returns the user's playlists, without their songs.
func (c *Subsonic) Playlists() ([]Playlist, error) {
	r, err := c.call("getPlaylists", nil)
	if err != nil || r.R.Playlists == nil {
		return nil, err
	}
	return r.R.Playlists.Playlist, nil
}

// Playlist returns a playlist with its songs.
func (c *Subsonic) Playlist(id string) (*Playlist, error) {
	r, err := c.call("getPlaylist", url.Values{"id": {id}})
	if err != nil {
		return nil, err
	}
	if r.R.Playlist == nil {
		return nil, errors.New("playlist not found")
	}
	return r.R.Playlist, nil
}

// RandomSongs returns up to n songs picked at random by the server.
func (c *Subsonic) RandomSongs(n int) ([]Song, error) {
	r, err := c.call("getRandomSongs", url.Values{"size": {fmt.Sprint(n)}})
	if err != nil || r.R.RandomSongs == nil {
		return nil, err
	}
	return r.R.RandomSongs.Song, nil
}

// Search finds songs matching q (Subsonic search3).
func (c *Subsonic) Search(q string, n int) ([]Song, error) {
	r, err := c.call("search3", url.Values{"query": {q}, "songCount": {fmt.Sprint(n)}, "albumCount": {"0"}, "artistCount": {"0"}})
	if err != nil || r.R.SearchResult3 == nil {
		return nil, err
	}
	return r.R.SearchResult3.Song, nil
}

// Starred returns the starred albums and songs.
func (c *Subsonic) Starred() ([]Album, []Song, error) {
	r, err := c.call("getStarred2", nil)
	if err != nil || r.R.Starred2 == nil {
		return nil, nil, err
	}
	return r.R.Starred2.Album, r.R.Starred2.Song, nil
}

// StreamURL is the address of a song's stream, transcoded by the server to
// format at up to maxBitRate kbit/s, so the device's ffplay only ever sees
// MP3. at starts the stream that many seconds in (Subsonic's timeOffset,
// which Navidrome honours when transcoding), to resume after a game.
func (c *Subsonic) StreamURL(id, format string, maxBitRate, at int) string {
	v := url.Values{"id": {id}, "format": {format}, "maxBitRate": {fmt.Sprint(maxBitRate)}}
	if at > 0 {
		v.Set("timeOffset", fmt.Sprint(at))
	}
	return c.URL("stream", v)
}

// maxCoverPixels caps the size of a cover the server sends before it is
// decoded. Servers that ignore getCoverArt's size send the original, and a
// 3000x3000 scan decodes to 13-36 MB, more than the Miyoo may have free.
const maxCoverPixels = 2500 * 2500

// decodeMu lets one cover decode at a time, so three downloads finishing
// together don't hold three full-size originals in memory at once.
var decodeMu sync.Mutex

// CoverArt returns the cover id as a size x size image, centre-cropped and
// scaled with a good filter. It asks the server for that size and keeps
// the scaled result on the card (as PNG, lossless), one entry per size, so
// a thumbnail is never shown where the large cover belongs.
func (c *Subsonic) CoverArt(id string, size int) (image.Image, error) {
	if id == "" {
		return nil, errors.New("no cover")
	}
	extra := url.Values{"id": {id}, "size": {fmt.Sprint(size)}}
	key := c.cacheKey("getCoverArt", extra) + "|scaled"
	if c.Cache != nil {
		if b, ok := c.Cache.Get(key); ok {
			if img, err := png.Decode(bytes.NewReader(b)); err == nil && img.Bounds().Dx() == size {
				return img, nil
			}
		}
	}
	resp, err := c.HTTP.Get(c.URL("getCoverArt", extra))
	if err != nil {
		return nil, cleanErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, cleanErr(err)
	}
	img, err := decodeCover(b, size)
	if err == nil && c.Cache != nil {
		var buf bytes.Buffer
		if png.Encode(&buf, img) == nil {
			c.Cache.Put(key, buf.Bytes())
		}
	}
	return img, err
}

// decodeCover decodes an image the server sent, refusing very large ones,
// and scales it to size x size at once so the original isn't kept.
func decodeCover(b []byte, size int) (*image.RGBA, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > maxCoverPixels {
		return nil, fmt.Errorf("cover too large (%dx%d)", cfg.Width, cfg.Height)
	}
	decodeMu.Lock()
	defer decodeMu.Unlock()
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	return ui.ScaleCrop(img, size, size), nil
}

// Scrobble tells the server what's playing: submission false marks it as
// playing now (Navidrome shows it to other clients), true records a play
// (play counts, "Most played", Last.fm if the server forwards it).
func (c *Subsonic) Scrobble(id string, submission bool) error {
	_, err := c.call("scrobble", url.Values{"id": {id}, "submission": {fmt.Sprint(submission)}})
	return err
}

// LyricLine is one line of lyrics; At is when it starts (synced lyrics).
type LyricLine struct {
	At   time.Duration
	Text string
}

// Lyrics gets a song's lyrics: timed, line by line, from servers with the
// OpenSubsonic lyrics extension (Navidrome reads them from the file's tags
// or a .lrc file next to it), otherwise the plain text from getLyrics.
// synced reports whether the lines have times. No lyrics is not an error.
func (c *Subsonic) Lyrics(s Song) (lines []LyricLine, synced bool, err error) {
	r, err := c.call("getLyricsBySongId", url.Values{"id": {s.ID}})
	if err == nil && r.R.LyricsList != nil {
		best := -1
		for i, l := range r.R.LyricsList.StructuredLyrics {
			if len(l.Line) > 0 && (best < 0 || l.Synced && !r.R.LyricsList.StructuredLyrics[best].Synced) {
				best = i
			}
		}
		if best >= 0 {
			l := r.R.LyricsList.StructuredLyrics[best]
			for _, x := range l.Line {
				lines = append(lines, LyricLine{At: time.Duration(x.Start-l.Offset) * time.Millisecond, Text: strings.TrimSpace(x.Value)})
			}
			return lines, l.Synced, nil
		}
	}
	r, err = c.call("getLyrics", url.Values{"artist": {s.Artist}, "title": {s.Title}})
	if err != nil {
		return nil, false, err
	}
	if r.R.Lyrics != nil {
		text := strings.ReplaceAll(r.R.Lyrics.Value, "\r\n", "\n")
		for _, t := range strings.Split(text, "\n") {
			lines = append(lines, LyricLine{Text: strings.TrimSpace(t)})
		}
		for len(lines) > 0 && lines[len(lines)-1].Text == "" {
			lines = lines[:len(lines)-1]
		}
	}
	return lines, false, nil
}
