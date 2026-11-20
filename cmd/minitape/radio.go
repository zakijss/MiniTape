package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/zakijss/minideck/cache"
	"github.com/zakijss/minideck/tnet"
)

// Internet radio. The stations come from radio-browser.info, a free
// directory with a public JSON API, and from the music server's own list
// (Subsonic getInternetRadioStations). A station plays as a Song with its
// stream address in Radio, through the relay like the server's songs (see
// radiostream.go), so HTTPS streams, background playback and the pause for
// games work the same way.

const (
	// radioPrefix starts the song ids of radio-browser stations (then the
	// station's uuid), serverRadioPrefix those of the server's stations.
	radioPrefix       = "radio:"
	serverRadioPrefix = "radio-server:"
	// radioFavouritesFile keeps the starred stations, in the app's folder.
	radioFavouritesFile = "radio-favourites.json"
	// radioLookup lists the directory's servers; radioHosts are tried when
	// it can't be read.
	radioLookup = "https://all.api.radio-browser.info/json/servers"
	// stationsTTL and groupsTTL are how long lists of stations and lists of
	// genres, countries and languages are kept before they are asked for
	// again. An older copy is still shown when the directory can't be
	// reached.
	stationsTTL = 6 * time.Hour
	groupsTTL   = 7 * 24 * time.Hour
	// stationsPage is how many stations a list asks for.
	stationsPage = 100
)

var radioHosts = []string{"https://de1.api.radio-browser.info", "https://de2.api.radio-browser.info", "https://fi1.api.radio-browser.info"}

// Station is a station as radio-browser.info lists it (only the fields
// MiniTape uses), or one of the server's (Server set).
type Station struct {
	UUID     string `json:"stationuuid,omitempty"`
	Server   string `json:"server,omitempty"` // the server's id for it
	Name     string `json:"name"`
	URL      string `json:"url_resolved"`
	Country  string `json:"country,omitempty"`
	Language string `json:"language,omitempty"` // "german,english"
	Tags     string `json:"tags,omitempty"`     // "jazz,smooth jazz"
	Codec    string `json:"codec,omitempty"`    // "MP3", "AAC+"
	Bitrate  int    `json:"bitrate,omitempty"`  // kbit/s, 0 if not known
	Votes    int    `json:"votes,omitempty"`
	Trend    int    `json:"clicktrend,omitempty"`
	OK       int    `json:"lastcheckok"` // 1 if it played at the directory's last check
}

// id is the song id of the station.
func (s Station) id() string {
	if s.Server != "" {
		return serverRadioPrefix + s.Server
	}
	return radioPrefix + s.UUID
}

// song is the station as the player plays it: the name as the title, where
// it is from as the artist, its first tags as the genre.
func (s Station) song() Song {
	var tags []string
	for t := range strings.SplitSeq(s.Tags, ",") {
		if t = strings.TrimSpace(t); t != "" && len(tags) < 3 {
			tags = append(tags, t)
		}
	}
	return Song{ID: s.id(), Title: strings.TrimSpace(s.Name), Artist: s.place(), Genre: strings.Join(tags, ", "),
		Suffix: strings.ToLower(s.Codec), BitRate: s.Bitrate, Radio: s.URL}
}

// place is where a station is from and its language: "Germany · German".
func (s Station) place() string {
	lang, _, _ := strings.Cut(s.Language, ",")
	return strings.Join(slices.DeleteFunc([]string{strings.TrimSpace(s.Country), capital(strings.TrimSpace(lang))},
		func(p string) bool { return p == "" }), " · ")
}

// stationOf is the station a song came from, as far as the song tells: for
// one starred from Now Playing, which has only the song.
func stationOf(s Song) Station {
	st := Station{Name: s.Title, URL: s.Radio, Country: s.Artist, Tags: s.Genre, Codec: strings.ToUpper(s.Suffix), Bitrate: s.BitRate, OK: 1}
	if id, ok := strings.CutPrefix(s.ID, serverRadioPrefix); ok {
		st.Server = id
	} else {
		st.UUID = strings.TrimPrefix(s.ID, radioPrefix)
	}
	return st
}

// capital is s with its first letter in upper case: the directory lists
// tags and languages in lower case.
func capital(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}

// stationSub is a station row's second line: "Germany · German · MP3 128 kbps".
func stationSub(s Song) string {
	return strings.Join(slices.DeleteFunc([]string{s.Artist, codecText(s)}, func(p string) bool { return p == "" }), " · ")
}

// codecText is a station's format: "MP3 128 kbps", "AAC+".
func codecText(s Song) string {
	t := strings.ToUpper(s.Suffix)
	if s.BitRate > 0 {
		t = strings.TrimSpace(fmt.Sprintf("%s %d kbps", t, s.BitRate))
	}
	return t
}

// ---- options (Radio › Options, kept in settings.json) ----

// radioSorts are the orders of Radio › Options › Sort by.
var radioSorts = []struct{ key, label string }{
	{"votes", "Popularity"}, {"trend", "Trending"}, {"name", "Name"}, {"bitrate", "Bit rate"},
}

// radioMinRates are the choices of the lowest bit rate, in kbit/s (0: any).
var radioMinRates = []int{0, 64, 128}

// radioCodecs are the formats the device's ffplay plays; mp3AAC the ones
// Radio › Options › MP3 and AAC only keeps.
var (
	radioCodecs = map[string]bool{"MP3": true, "AAC": true, "AAC+": true, "OGG": true, "OPUS": true, "FLAC": true}
	mp3AAC      = map[string]bool{"MP3": true, "AAC": true, "AAC+": true}
)

// radioSort is the order station lists are in.
func (s Settings) radioSort() string {
	for _, o := range radioSorts {
		if o.key == s.RadioSort {
			return o.key
		}
	}
	return "votes"
}

// filterStations keeps the stations the options let through, in place:
// ones with an http or https stream, in a format ffplay plays (MP3 and AAC
// only, if set), at or above the lowest bit rate, and that played at the
// last check unless broken ones are shown. The server's stations say
// nothing of their format or checks, so only the address counts for them.
func filterStations(st []Station, cfg Settings) []Station {
	return slices.DeleteFunc(st, func(s Station) bool {
		if u, err := url.Parse(s.URL); err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
			return true
		}
		if s.Server != "" {
			return false
		}
		codec := strings.ToUpper(strings.TrimSpace(s.Codec))
		return !radioCodecs[codec] || cfg.RadioMP3AAC && !mp3AAC[codec] ||
			s.Bitrate < cfg.RadioMinKbps || !cfg.RadioBroken && s.OK == 0
	})
}

// sortStations puts stations in the order by (see radioSorts). Ties keep
// the directory's order.
func sortStations(st []Station, by string) {
	slices.SortStableFunc(st, func(a, b Station) int {
		switch by {
		case "trend":
			return cmp.Compare(b.Trend, a.Trend)
		case "name":
			return cmp.Compare(strings.ToLower(strings.TrimSpace(a.Name)), strings.ToLower(strings.TrimSpace(b.Name)))
		case "bitrate":
			return cmp.Compare(b.Bitrate, a.Bitrate)
		}
		return cmp.Compare(b.Votes, a.Votes)
	})
}

// stepRadio changes the Radio options row named label by n steps, as
// stepSetting does for Settings.
func stepRadio(cfg *Settings, label string, n int) bool {
	switch label {
	case "Sort by":
		i := slices.IndexFunc(radioSorts, func(o struct{ key, label string }) bool { return o.key == cfg.radioSort() })
		k := len(radioSorts)
		cfg.RadioSort = radioSorts[((i+n)%k+k)%k].key
	case "Broken stations":
		cfg.RadioBroken = !cfg.RadioBroken
	case "MP3 and AAC only":
		cfg.RadioMP3AAC = !cfg.RadioMP3AAC
	case "Lowest bit rate":
		cfg.RadioMinKbps = step(radioMinRates, cfg.RadioMinKbps, n)
	default:
		return false
	}
	return true
}

// sortLabel is the order station lists are in, as Options shows it.
func sortLabel(cfg Settings) string {
	for _, o := range radioSorts {
		if o.key == cfg.radioSort() {
			return o.label
		}
	}
	return ""
}

// ---- the directory ----

// radioDir is a client for radio-browser.info. Its servers come from
// radioLookup; when one doesn't answer, the next is asked, and the one that
// answered is asked first next time.
type radioDir struct {
	http   *http.Client
	cache  *cache.Disk // nil: nothing kept
	lookup string      // radioLookup ("" too); tests point it elsewhere

	mu    sync.Mutex
	hosts []string // base URLs, the last one that answered first
}

// radioGroup is a genre (tag), country or language, with its count of
// stations.
type radioGroup struct {
	Name  string `json:"name"`
	Code  string `json:"iso_3166_1,omitempty"` // countries
	Count int    `json:"stationcount"`
}

// servers are the directory's servers to ask, in order.
func (d *radioDir) servers() []string {
	d.mu.Lock()
	if len(d.hosts) > 0 {
		defer d.mu.Unlock()
		return slices.Clone(d.hosts)
	}
	d.mu.Unlock()
	var list []struct {
		Name string `json:"name"`
	}
	var hosts []string
	if b, err := d.read(cmp.Or(d.lookup, radioLookup)); err != nil {
		log.Printf("radio: listing the directory's servers: %v", err)
	} else if err := json.Unmarshal(b, &list); err != nil {
		log.Printf("radio: listing the directory's servers: %v", err)
	}
	for _, s := range list {
		h := s.Name
		if !strings.Contains(h, "://") {
			h = "https://" + h
		}
		if s.Name != "" && !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		return slices.Clone(radioHosts) // asked again next time
	}
	// spread the load, as the directory asks
	rand.Shuffle(len(hosts), func(i, j int) { hosts[i], hosts[j] = hosts[j], hosts[i] })
	d.mu.Lock()
	d.hosts = hosts
	d.mu.Unlock()
	return slices.Clone(hosts)
}

// answered puts host first, for the next request.
func (d *radioDir) answered(host string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if i := slices.Index(d.hosts, host); i > 0 {
		d.hosts = append([]string{host}, slices.Delete(d.hosts, i, i+1)...)
	}
}

// read GETs u, at most 4 MB of it.
func (d *radioDir) read(u string) ([]byte, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent())
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, tnet.Explain(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, tnet.Explain(err)
	}
	return b, nil
}

// fetch asks the directory's servers for path, one after another, until
// one answers.
func (d *radioDir) fetch(path string, q url.Values) ([]byte, error) {
	err := errors.New("no server")
	for _, h := range d.servers() {
		u := h + path
		if len(q) > 0 {
			u += "?" + q.Encode()
		}
		var b []byte
		if b, err = d.read(u); err == nil {
			d.answered(h)
			return b, nil
		}
		log.Printf("radio: %s: %v", h, err)
	}
	return nil, err
}

// get fills v from path with q. A copy younger than ttl is used as it is;
// fresh (Y) asks the directory anyway. Without an answer, an older copy is
// used. ttl 0 keeps nothing (a search). What is kept is v encoded again,
// only the fields MiniTape reads, not the directory's whole reply.
func (d *radioDir) get(path string, q url.Values, ttl time.Duration, fresh bool, v any) error {
	key := "radio|" + path + "?" + q.Encode()
	var old []byte
	if d.cache != nil && ttl > 0 {
		if b, ok := d.cache.Get(key); ok {
			stamp, body, _ := bytes.Cut(b, []byte("\n"))
			at, _ := strconv.ParseInt(string(stamp), 10, 64)
			if !fresh && time.Since(time.Unix(at, 0)) < ttl && json.Unmarshal(body, v) == nil {
				return nil
			}
			old = body
		}
	}
	b, err := d.fetch(path, q)
	if err == nil {
		if err = json.Unmarshal(b, v); err != nil {
			err = fmt.Errorf("bad reply from the radio directory: %v", err)
		}
	}
	if err != nil {
		if old != nil && json.Unmarshal(old, v) == nil {
			log.Printf("radio: %s: %v; showing the copy kept", path, err)
			return nil
		}
		return err
	}
	if d.cache != nil && ttl > 0 {
		if kept, err := json.Marshal(v); err == nil {
			d.cache.Put(key, append(fmt.Appendf(nil, "%d\n", time.Now().Unix()), kept...))
		}
	}
	return nil
}

// stations lists up to stationsPage stations matching q (radio-browser's
// station search: tag, countrycode, language, name), the most voted first
// unless q sets the order. hideBroken leaves out the stations that failed
// the directory's last check.
func (d *radioDir) stations(q url.Values, hideBroken, fresh bool) ([]Station, error) {
	q = maps.Clone(q) // the caller's stays as it was
	q.Set("hidebroken", strconv.FormatBool(hideBroken))
	q.Set("limit", strconv.Itoa(stationsPage))
	q.Set("reverse", "true")
	if !q.Has("order") {
		q.Set("order", "votes")
	}
	ttl := stationsTTL
	if q.Has("name") { // a search
		ttl = 0
	}
	var st []Station
	err := d.get("/json/stations/search", q, ttl, fresh, &st)
	return st, err
}

// groups lists the genres ("tags"), "countries" or "languages" that have
// stations: countries A to Z, the others the most stations first.
func (d *radioDir) groups(kind string, fresh bool) ([]radioGroup, error) {
	q := url.Values{"hidebroken": {"true"}, "order": {"stationcount"}, "reverse": {"true"}, "limit": {"100"}}
	if kind == "countries" {
		q = url.Values{"hidebroken": {"true"}, "order": {"name"}}
	}
	var gs []radioGroup
	err := d.get("/json/"+kind, q, groupsTTL, fresh, &gs)
	gs = slices.DeleteFunc(gs, func(g radioGroup) bool {
		return strings.TrimSpace(g.Name) == "" || g.Count <= 0 || kind == "countries" && g.Code == ""
	})
	return gs, err
}

// groupQuery is the station search for a group of kind (see groups).
func groupQuery(kind string, g radioGroup) url.Values {
	switch kind {
	case "countries":
		return url.Values{"countrycode": {g.Code}}
	case "languages":
		return url.Values{"language": {g.Name}, "languageExact": {"true"}}
	}
	return url.Values{"tag": {g.Name}, "tagExact": {"true"}}
}

// click counts a listen of the station with uuid, as the directory asks
// players to when a station starts: its trending list comes from these.
func (d *radioDir) click(uuid string) {
	if _, err := d.fetch("/json/url/"+url.PathEscape(uuid), nil); err != nil {
		log.Printf("radio: counting a listen of %s: %v", uuid, err)
	}
}

// ---- favourites ----

// favourites are the starred stations, kept in radioFavouritesFile in the
// order they were starred.
type favourites struct {
	mu   sync.Mutex
	path string
	list []Station
}

// loadFavourites reads the starred stations; a missing or unreadable file
// gives none.
func loadFavourites(path string) *favourites {
	f := &favourites{path: path}
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("favourites: %v", err)
		}
		return f
	}
	if err := json.Unmarshal(b, &f.list); err != nil {
		log.Printf("%s: %v", radioFavouritesFile, err)
	}
	return f
}

// has reports whether the station with song id is starred.
func (f *favourites) has(id string) bool {
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.ContainsFunc(f.list, func(s Station) bool { return s.id() == id })
}

// all is a copy of the starred stations.
func (f *favourites) all() []Station {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.list)
}

// toggle stars s, or unstars it if it is starred, and saves the list. On
// an error the list stays as it was.
func (f *favourites) toggle(s Station) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := s.id()
	list := slices.DeleteFunc(slices.Clone(f.list), func(x Station) bool { return x.id() == id })
	if len(list) == len(f.list) {
		list = append(list, s)
	}
	if list == nil {
		list = []Station{}
	}
	if err := writeJSON(f.path, list); err != nil {
		return err
	}
	f.list = list
	return nil
}
