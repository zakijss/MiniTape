package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zakijss/minideck/qr"
	"github.com/zakijss/minideck/secrets"
	"github.com/zakijss/minideck/tnet"
	"github.com/zakijss/minideck/ui"
)

// The scrobbling logins in secrets.enc. scrobble.txt uses the same names.
const (
	secLBToken    = "listenbrainz_token"
	secLFMKey     = "lastfm_api_key"
	secLFMSecret  = "lastfm_secret"
	secLFMSession = "lastfm_session"
)

// importFile is the plain-text file a user copies into App/MiniTape to set
// up scrobbling without typing long tokens on the d-pad. It is sealed into
// secrets.enc and deleted.
const importFile = "scrobble.txt"

// scrobbleServices builds the services that have a complete login.
func scrobbleServices(sec map[string]string, c *http.Client) []*service {
	var out []*service
	if t := sec[secLBToken]; t != "" {
		out = append(out, &service{name: svcListenBrainz, api: newListenBrainz(t, c), batch: listenBrainzBatch})
	}
	if k, s, sk := sec[secLFMKey], sec[secLFMSecret], sec[secLFMSession]; k != "" && s != "" && sk != "" {
		out = append(out, &service{name: svcLastFM, api: newLastFM(k, s, sk, c), batch: lastfmBatch})
	}
	return out
}

// ---- scrobble.txt ----

// readImportFile reads scrobble.txt: lines of name=value, with blank lines
// and # comments allowed. Errors name the line or setting, never a value.
func readImportFile(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")) // Notepad's byte-order mark
	names := map[string]string{
		secLBToken: secLBToken, secLFMKey: secLFMKey, secLFMSecret: secLFMSecret,
		"lastfm_shared_secret": secLFMSecret, // what Last.fm's page calls it
	}
	vals := map[string]string{}
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, screenError(fmt.Sprintf("Line %d of %s isn't name=value.", i+1, importFile))
		}
		k = strings.ToLower(strings.TrimSpace(k))
		name, known := names[k]
		if !known {
			return nil, screenError(fmt.Sprintf("%s has a setting MiniTape doesn't know: %q.", importFile, k))
		}
		vals[name] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	if len(vals) == 0 {
		return nil, screenError(fmt.Sprintf("%s has none of %s, %s or %s.", importFile, secLBToken, secLFMKey, secLFMSecret))
	}
	if err := checkLBToken(vals, true); err != nil {
		return nil, err
	}
	_, hasKey := vals[secLFMKey]
	_, hasSecret := vals[secLFMSecret]
	switch {
	case hasKey && !hasSecret:
		return nil, screenError(fmt.Sprintf("%s has the Last.fm API key but not the shared secret (%s).", importFile, secLFMSecret))
	case hasSecret && !hasKey:
		return nil, screenError(fmt.Sprintf("%s has the Last.fm shared secret but not the API key (%s).", importFile, secLFMKey))
	case hasKey:
		if err := checkHex32(vals[secLFMKey], "The Last.fm API key"); err != nil {
			return nil, err
		}
		if err := checkHex32(vals[secLFMSecret], "The Last.fm shared secret"); err != nil {
			return nil, err
		}
	}
	return vals, nil
}

// checkLBToken checks the shape of a ListenBrainz token, if vals has one
// (or must). The real check is asking ListenBrainz; this catches a value
// cut short or pasted with the wrong thing.
func checkLBToken(vals map[string]string, optional bool) error {
	t, ok := vals[secLBToken]
	if !ok && optional {
		return nil
	}
	if len(t) < 20 || strings.ContainsAny(t, " \t") {
		return screenError("That doesn't look like a ListenBrainz user token: copy it from listenbrainz.org/settings.")
	}
	return nil
}

// checkHex32 checks a Last.fm API key or secret: 32 hex digits.
func checkHex32(v, what string) error {
	ok := len(v) == 32
	for _, c := range v {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			ok = false
		}
	}
	if !ok {
		return errors.New(what + " should be 32 letters (a to f) and digits: copy it again from last.fm/api/accounts.")
	}
	return nil
}

// sealImport adds vals to the secrets, saves them sealed with the device
// key, and only then deletes the plain-text file. New Last.fm keys end the
// old session: it belongs to the old key. err means nothing was saved (sec
// is unchanged); notDeleted means the secrets were saved but the file is
// still on the card.
func sealImport(path, secPath string, sec map[string]string, vals map[string]string) (notDeleted, err error) {
	next := map[string]string{}
	for k, v := range sec {
		next[k] = v
	}
	for k, v := range vals {
		next[k] = v
	}
	if next[secLFMKey] != sec[secLFMKey] || next[secLFMSecret] != sec[secLFMSecret] {
		delete(next, secLFMSession)
	}
	if err := secrets.Save(secPath, next); err != nil {
		return nil, err
	}
	for k := range sec {
		delete(sec, k)
	}
	for k, v := range next {
		sec[k] = v
	}
	return shred(path), nil
}

// shred overwrites a file with zeros before removing it. On an SD card this
// is best effort (the controller may keep old blocks), but it stops the
// plain text from being read back through the file system.
func shred(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err == nil {
		f.Write(make([]byte, fi.Size()))
		f.Sync()
		f.Close()
	}
	return os.Remove(path)
}

// ---- Settings › Scrobbling ----

// scrobbleStatus is what the Scrobbling screen shows.
type scrobbleStatus struct {
	Server         bool
	LB, LBBroken   bool
	LBUser         string
	LFMKeys        bool // API key and secret saved
	LFM, LFMBroken bool // logged in; the session was refused
	LFMUser        string
	Waiting        int
}

func readScrobbleStatus(dir string, cfg *Settings, sec map[string]string) scrobbleStatus {
	q := readQueueFile(filepath.Join(dir, scrobbleQueueFile))
	st := scrobbleStatus{Server: cfg.serverScrobble(), LB: sec[secLBToken] != "", LBUser: cfg.ListenBrainzUser,
		LFMKeys: sec[secLFMKey] != "" && sec[secLFMSecret] != "", LFMUser: cfg.LastFMUser, Waiting: len(q.Listens)}
	st.LFM = st.LFMKeys && sec[secLFMSession] != ""
	for _, n := range q.NeedsLogin {
		switch n {
		case svcListenBrainz:
			st.LBBroken = st.LB
		case svcLastFM:
			st.LFMBroken = st.LFM
		}
	}
	return st
}

// summary is the Scrobbling row's line in Settings.
func (st scrobbleStatus) summary() string {
	var on []string
	if st.LFM {
		on = append(on, "Last.fm")
	}
	if st.LB {
		on = append(on, "ListenBrainz")
	}
	switch {
	case st.LFMBroken:
		return "Last.fm: log in again"
	case st.LBBroken:
		return "ListenBrainz needs a new token"
	case len(on) == 0 && st.Server:
		return "Music server only"
	case len(on) == 0:
		return "Off"
	}
	s := strings.Join(on, " · ")
	if st.Waiting > 0 {
		s += fmt.Sprintf(" · %d waiting", st.Waiting)
	}
	return s
}

// The rows of the Scrobbling screen, by Item.Value.
const (
	rowServer = "server"
	rowLB     = "listenbrainz"
	rowLFM    = "lastfm"
	rowQueue  = "queue"
	rowImport = "import"
)

func onOff(on bool) string {
	if on {
		return "On"
	}
	return "Off"
}

func (st scrobbleStatus) items() []ui.Item {
	server := ui.Item{Label: "Music server", Value: rowServer, Icon: "server", Detail: onOff(st.Server),
		Sub: "Play counts on your server", Tint: ui.Good}
	if !st.Server {
		server.Sub, server.Tint = "Not sent to your server", ui.Faint
	}
	lb := ui.Item{Label: "ListenBrainz", Value: rowLB, Icon: "chart", Chevron: true, Sub: "Off", Tint: ui.Faint}
	switch {
	case st.LBBroken:
		lb.Sub, lb.Tint = "Needs a new token", ui.Warn
	case st.LB && st.LBUser != "":
		lb.Sub, lb.Tint = "On as "+st.LBUser, ui.Good
	case st.LB:
		lb.Sub, lb.Tint = "On", ui.Good
	}
	lf := ui.Item{Label: "Last.fm", Value: rowLFM, Icon: "chart", Chevron: true, Sub: "Off", Tint: ui.Faint}
	switch {
	case st.LFMBroken:
		lf.Sub, lf.Tint = "Log in again", ui.Warn
	case st.LFM && st.LFMUser != "":
		lf.Sub, lf.Tint = "On as "+st.LFMUser, ui.Good
	case st.LFM:
		lf.Sub, lf.Tint = "On", ui.Good
	case st.LFMKeys:
		lf.Sub, lf.Tint = "Keys saved: log in to start", ui.Warn
	}
	queue := ui.Item{Label: "Nothing waiting to be sent", Value: rowQueue, Icon: "clock", Tint: ui.Muted,
		Sub: "Listens that can't be sent wait here"}
	if st.Waiting > 0 {
		queue.Label = fmt.Sprintf("%d waiting to be sent", st.Waiting)
		if st.Waiting == 1 {
			queue.Label = "1 waiting to be sent"
		}
		queue.Sub, queue.Tint = "A sends them now", ui.Accent
	}
	imp := ui.Item{Label: "Set up from " + importFile, Value: rowImport, Icon: "plus", Tint: ui.Accent,
		Sub: "Tokens and keys in App/MiniTape"}
	return []ui.Item{server, lf, lb, queue, imp}
}

// menu shows a list until B. act runs for A with n 0; on left and right it
// runs with n -1 and 1, and a row with a value steps through it and reports
// true (the others report false, and left and right page the list). items
// is called again after each action, so the rows show what it changed.
func menu(scr *ui.Screen, input <-chan ui.Event, title string, items func() []ui.Item, act func(it ui.Item, n int) bool) {
	l := ui.List{Items: items()}
	for {
		drawMenu(scr, title, &l)
		ev, ok, frame := scr.Next(input)
		if !ok {
			return
		}
		if frame || !ev.Pressed {
			continue
		}
		n := 0
		switch ev.Button {
		case ui.A:
		case ui.Left:
			n = -1
		case ui.Right:
			n = 1
		case ui.B, ui.Menu:
			return
		default:
			l.Handle(ev)
			continue
		}
		it := l.Selected()
		if it == nil {
			continue
		}
		if !act(*it, n) && n != 0 {
			l.Handle(ev)
			continue
		}
		l.Items = items()
		l.Sel = min(l.Sel, len(l.Items)-1)
	}
}

func drawMenu(scr *ui.Screen, title string, l *ui.List) {
	top := scr.StaticFrame(title, "", ui.Hint{Button: "A", Label: "Select"}, ui.Hint{Button: "B", Label: "Back"})
	l.Draw(scr, image.Rect(20, top, ui.W-26, ui.H-58))
	scr.Present()
}

// scrobbleUI is Settings › Scrobbling.
type scrobbleUI struct {
	scr    *ui.Screen
	input  <-chan ui.Event
	dir    string
	cfg    *Settings
	sec    map[string]string
	http   *http.Client
	reload func() // tells the player the settings changed
	flush  func() // asks the player to send the waiting listens now
}

func (u *scrobbleUI) status() scrobbleStatus { return readScrobbleStatus(u.dir, u.cfg, u.sec) }

func (u *scrobbleUI) run() {
	menu(u.scr, u.input, "Scrobbling", func() []ui.Item { return u.status().items() }, func(it ui.Item, n int) bool {
		if it.Value == rowServer {
			on := !u.cfg.serverScrobble()
			u.cfg.ServerScrobble = &on
			u.save(false)
			return true
		}
		if n != 0 {
			return false
		}
		switch it.Value {
		case rowLB:
			u.listenBrainz()
		case rowLFM:
			u.lastFM()
		case rowQueue:
			u.sendNow()
		case rowImport:
			u.importFile()
		}
		return false
	})
}

// save writes settings.json (and secrets.enc, with withSecrets) and tells
// the player.
func (u *scrobbleUI) save(withSecrets bool) bool {
	if withSecrets {
		if err := secrets.Save(filepath.Join(u.dir, "secrets.enc"), u.sec); err != nil {
			u.scr.Notice(u.input, "Couldn't save", err.Error())
			return false
		}
	}
	if err := saveSettings(u.dir, u.cfg); err != nil {
		u.scr.Notice(u.input, "Couldn't save", err.Error())
		return false
	}
	u.reload()
	return true
}

func (u *scrobbleUI) listenBrainz() {
	st := u.status()
	typeLabel := "Type the token"
	if st.LBBroken {
		typeLabel = "Type a new token"
	}
	items := []ui.Item{
		{Label: "Set up from " + importFile, Sub: secLBToken + "=… in App/MiniTape", Icon: "plus", Tint: ui.Accent},
		{Label: typeLabel, Sub: "From listenbrainz.org/settings", Icon: "list", Tint: ui.Muted},
	}
	if st.LB {
		items = append(items, ui.Item{Label: "Turn off", Sub: "Forget the token on this device", Icon: "cross", Tint: ui.Bad})
	}
	i, ok := u.scr.Choose(u.input, "ListenBrainz", items)
	if !ok {
		return
	}
	switch i {
	case 0:
		u.importFile()
	case 1:
		tok, ok := u.scr.Prompt(u.input, "ListenBrainz token", "", "From listenbrainz.org/settings · SELECT shows it", true)
		tok = strings.TrimSpace(tok)
		if !ok || tok == "" {
			return
		}
		if err := checkLBToken(map[string]string{secLBToken: tok}, false); err != nil {
			u.scr.Notice(u.input, "Not a token", err.Error())
			return
		}
		user, ok := u.checkLB(tok)
		if !ok {
			return
		}
		u.sec[secLBToken], u.cfg.ListenBrainzUser = tok, user
		if u.save(true) {
			u.scr.Notice(u.input, "ListenBrainz is on", "Your listens will be sent to ListenBrainz"+as(user)+".")
		}
	case 2:
		if u.scr.Confirm(u.input, "Turn off ListenBrainz?", "MiniTape forgets the token, and listens waiting for ListenBrainz are dropped.", "Turn off") {
			delete(u.sec, secLBToken)
			u.cfg.ListenBrainzUser = ""
			u.save(true)
		}
	}
}

func as(user string) string {
	if user == "" {
		return ""
	}
	return " as " + user
}

// checkLB asks ListenBrainz about a token. A token it refuses is an error;
// if ListenBrainz can't be reached, the user may keep the token anyway.
func (u *scrobbleUI) checkLB(tok string) (user string, ok bool) {
	u.scr.Alert("Checking…", "Asking ListenBrainz about the token.")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	user, err := newListenBrainz(tok, u.http).Validate(ctx)
	switch {
	case err == nil:
		return user, true
	case kindOf(err) == errLogin:
		u.scr.Notice(u.input, "ListenBrainz said no", "It doesn't know that token. Copy your user token from listenbrainz.org/settings again.")
		return "", false
	default:
		return "", u.scr.Confirm(u.input, "Couldn't check the token", err.Error()+"\nKeep the token anyway?", "Keep it")
	}
}

func (u *scrobbleUI) lastFM() {
	st := u.status()
	var items []ui.Item
	var acts []func()
	add := func(it ui.Item, f func()) { items, acts = append(items, it), append(acts, f) }
	if st.LFMKeys {
		label := "Log in"
		if st.LFM {
			label = "Log in again"
		}
		add(ui.Item{Label: label, Sub: "Scan a code and approve on your phone", Icon: "person", Tint: ui.Accent}, u.lastFMLogin)
	}
	add(ui.Item{Label: "Set up from " + importFile, Sub: "Your API key and shared secret, in App/MiniTape", Icon: "plus", Tint: ui.Accent}, u.importFile)
	add(ui.Item{Label: "Type the API key and secret", Sub: "From last.fm/api/account/create", Icon: "list", Tint: ui.Muted}, u.typeLastFM)
	if st.LFMKeys || st.LFM {
		add(ui.Item{Label: "Turn off", Sub: "Log out and forget the keys on this device", Icon: "cross", Tint: ui.Bad}, func() {
			if u.scr.Confirm(u.input, "Turn off Last.fm?", "MiniTape forgets the keys and the login, and scrobbles waiting for Last.fm are dropped.", "Turn off") {
				delete(u.sec, secLFMKey)
				delete(u.sec, secLFMSecret)
				delete(u.sec, secLFMSession)
				u.cfg.LastFMUser = ""
				u.save(true)
			}
		})
	}
	if i, ok := u.scr.Choose(u.input, "Last.fm", items); ok {
		acts[i]()
	}
}

func (u *scrobbleUI) typeLastFM() {
	key, ok := u.scr.Prompt(u.input, "Last.fm API key", "", "32 letters and digits from last.fm/api/accounts", true)
	key = strings.TrimSpace(key)
	if !ok || key == "" {
		return
	}
	if err := checkHex32(key, "The API key"); err != nil {
		u.scr.Notice(u.input, "Not an API key", err.Error())
		return
	}
	secret, ok := u.scr.Prompt(u.input, "Last.fm shared secret", "", "The shared secret, under the API key", true)
	secret = strings.TrimSpace(secret)
	if !ok || secret == "" {
		return
	}
	if err := checkHex32(secret, "The shared secret"); err != nil {
		u.scr.Notice(u.input, "Not a shared secret", err.Error())
		return
	}
	if key != u.sec[secLFMKey] || secret != u.sec[secLFMSecret] {
		delete(u.sec, secLFMSession) // the session belongs to the old key
		u.cfg.LastFMUser = ""
	}
	u.sec[secLFMKey], u.sec[secLFMSecret] = key, secret
	if u.save(true) && u.sec[secLFMSession] == "" &&
		u.scr.Confirm(u.input, "Keys saved", "Now log in to Last.fm: scan a code with your phone and approve MiniTape.", "Log in") {
		u.lastFMLogin()
	}
}

// lastFMLogin runs the login and saves the session.
func (u *scrobbleUI) lastFMLogin() {
	lf := newLastFM(u.sec[secLFMKey], u.sec[secLFMSecret], "", u.http)
	name, sk, ok := lastfmLogin(u.scr, u.input, lf)
	if !ok {
		return
	}
	u.sec[secLFMSession], u.cfg.LastFMUser = sk, name
	if u.save(true) {
		u.scr.Notice(u.input, "Logged in", "Your plays will be scrobbled to Last.fm"+as(name)+".")
	}
}

// importFile sets up from scrobble.txt: a ListenBrainz token is checked
// first, so a wrong one leaves the file on the card to be fixed.
func (u *scrobbleUI) importFile() {
	path := filepath.Join(u.dir, importFile)
	vals, err := readImportFile(path)
	if errors.Is(err, os.ErrNotExist) {
		u.scr.Notice(u.input, "No "+importFile, "Copy a text file named "+importFile+" into App/MiniTape with lines like "+
			secLBToken+"=…, "+secLFMKey+"=… and "+secLFMSecret+"=…, then choose this again.")
		return
	}
	if err != nil {
		u.scr.Notice(u.input, "Can't use "+importFile, err.Error())
		return
	}
	var done []string
	user := u.cfg.ListenBrainzUser
	if tok, ok := vals[secLBToken]; ok {
		if user, ok = u.checkLB(tok); !ok {
			return
		}
		done = append(done, "the ListenBrainz token")
	}
	if _, ok := vals[secLFMKey]; ok {
		done = append(done, "the Last.fm API key and secret")
	}
	notDeleted, err := sealImport(path, filepath.Join(u.dir, "secrets.enc"), u.sec, vals)
	if err != nil {
		u.scr.Notice(u.input, "Couldn't save", err.Error())
		return
	}
	if _, ok := vals[secLBToken]; ok {
		u.cfg.ListenBrainzUser = user
	}
	if u.sec[secLFMSession] == "" {
		u.cfg.LastFMUser = ""
	}
	if !u.save(false) {
		return
	}
	body := "Saved " + strings.Join(done, " and ") + ", sealed with this device's key."
	if notDeleted == nil {
		body += " Deleted " + importFile + "."
	} else {
		body += " Couldn't delete " + importFile + ": remove it from the card yourself."
	}
	if u.sec[secLFMKey] != "" && u.sec[secLFMSession] == "" {
		if u.scr.Confirm(u.input, "Saved", body+" Log in to Last.fm now?", "Log in") {
			u.lastFMLogin()
		}
		return
	}
	u.scr.Notice(u.input, "Saved", body)
}

// sendNow asks the player to send the waiting listens and waits a few
// seconds to show the result.
func (u *scrobbleUI) sendNow() {
	before := u.status().Waiting
	if before == 0 {
		return
	}
	u.scr.Alert("Sending…", fmt.Sprintf("Sending %d waiting listens.", before))
	u.flush()
	for range 20 {
		time.Sleep(250 * time.Millisecond)
		if u.status().Waiting < before {
			break
		}
	}
}

// ---- the Last.fm login ----

// lastfmLogin shows a QR code of Last.fm's approval page and waits (up to
// 5 minutes) for the user to approve MiniTape on their phone. It returns
// the user's name and the session key.
func lastfmLogin(scr *ui.Screen, input <-chan ui.Event, lf *LastFM) (name, sk string, ok bool) {
	scr.Alert("Last.fm", "Asking Last.fm for a login link…")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	token, err := lf.Token(ctx)
	cancel()
	if err != nil {
		msg := err.Error()
		if kindOf(err) == errLogin {
			msg = "Last.fm didn't accept the API key and shared secret. Check them at last.fm/api/accounts."
		}
		scr.Notice(input, "Couldn't start the login", msg)
		return "", "", false
	}
	link := lastfmAuthURL(lf.Key, token)
	code, _ := qr.Encode(link) // nil (link only) if it were ever too long

	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	type result struct {
		name, key string
		err       error
	}
	res := make(chan result, 1)
	go func() {
		n, k, err := lf.pollSession(ctx, token, 3*time.Second)
		res <- result{n, k, err}
	}()
	tick := time.NewTicker(time.Second) // the waiting dots
	defer tick.Stop()
	for {
		drawLastFMLogin(scr, link, code, time.Now())
		select {
		case ev, open := <-input:
			if !open {
				return "", "", false
			}
			if ev.Pressed && (ev.Button == ui.B || ev.Button == ui.Menu) {
				return "", "", false
			}
		case r := <-res:
			switch {
			case r.err == nil:
				return r.name, r.key, true
			case errors.Is(r.err, context.DeadlineExceeded):
				scr.Notice(input, "The code expired", "Choose Log in again for a new one.")
			default:
				scr.Notice(input, "Couldn't log in", r.err.Error())
			}
			return "", "", false
		case <-tick.C:
		}
	}
}

// drawLastFMLogin draws the QR code on white with a four-module quiet
// zone, in whole pixels per module (as MiniScale's login does). It is
// kept to 250 px, 5 px a module, so the long link beside it breaks into
// whole lines.
func drawLastFMLogin(s *ui.Screen, link string, code *qr.QR, now time.Time) {
	top := s.StaticFrame("Log in to Last.fm", "Approve on your phone", ui.Hint{Button: "B", Label: "Cancel"})
	area := image.Rect(20, top, ui.W-20, ui.H-62)
	tx := area.Min.X + 8
	qy := area.Min.Y + 8
	if code != nil {
		n := code.Size + 8
		px := min(area.Dy(), 250) / n
		side := n * px
		qx := area.Min.X + 8
		qy = area.Min.Y + (area.Dy()-side)/2
		s.Fill(image.Rect(qx-4, qy-4, qx+side+4, qy+side+4), ui.Ink)
		s.Fill(image.Rect(qx, qy, qx+side, qy+side), ui.Text)
		for y := range code.Size {
			for x := range code.Size {
				if code.Modules[y][x] {
					mx, my := qx+(x+4)*px, qy+(y+4)*px
					s.Fill(image.Rect(mx, my, mx+px, my+px), ui.Ink)
				}
			}
		}
		tx = qx + side + 22
	}
	tw := area.Max.X - tx
	y := qy + 4
	for _, l := range ui.Wrap(ui.Body, "Scan the code with your phone and choose Yes, allow access.", tw) {
		s.Text(tx, y, l, ui.Body, ui.Text)
		y += ui.LineHeight(ui.Body)
	}
	y += 10
	for _, l := range ui.Wrap(ui.Small, "Or open this link:", tw) {
		s.Text(tx, y, l, ui.Small, ui.Muted)
		y += ui.LineHeight(ui.Small)
	}
	for _, l := range wrapLink(link, tw) {
		s.Text(tx, y, l, ui.Small, ui.Accent)
		y += ui.LineHeight(ui.Small)
	}
	y += 12
	dots := strings.Repeat(".", int(now.Unix()%3)+1)
	s.Text(tx, y, "Waiting for you"+dots, ui.Small, ui.Muted)
	s.Present()
}

// wrapLink breaks a URL into lines that fit w: a new line after the ?
// and before each &, so each parameter starts a line, and within a
// parameter wherever the width runs out.
func wrapLink(link string, w int) []string {
	link = strings.TrimPrefix(link, "https://")
	var parts []string
	if i := strings.IndexByte(link, '?'); i >= 0 {
		parts = append(parts, link[:i+1])
		link = link[i+1:]
	}
	for i, p := range strings.Split(link, "&") {
		if i > 0 {
			p = "&" + p
		}
		parts = append(parts, p)
	}
	var out []string
	for _, p := range parts {
		for p != "" {
			n := len(p)
			for n > 1 && ui.Measure(ui.Small, p[:n]) > w {
				n--
			}
			out = append(out, p[:n])
			p = p[n:]
		}
	}
	return out
}

// scrobbleClient is the HTTP client for Last.fm and ListenBrainz: public
// hosts, reached directly (tnet only proxies tailnet names), with Go's
// copy of the CA roots, which tnet brings in.
func scrobbleClient() *http.Client { return tnet.Client(15 * time.Second) }
