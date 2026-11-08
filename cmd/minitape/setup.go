package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/zakijss/minideck/atomicfile"
	"github.com/zakijss/minideck/secrets"
	"github.com/zakijss/minideck/tnet"
	"github.com/zakijss/minideck/ui"
)

// setup shows the server form. first is the first-run version.
func setup(scr *ui.Screen, input <-chan ui.Event, dir string, cfg *Settings, sec map[string]string, first bool) bool {
	server := &ui.Field{Label: "Server", Value: cfg.URL, Placeholder: "http://homeserver:4533",
		Hint: "Your music server's address, e.g. homeserver:4533"}
	server.Pick = func() (string, bool) { return scr.PickHost(input, tnet.HostChoices, "4533") }
	user := &ui.Field{Label: "Username", Value: cfg.User, Placeholder: "your login on the server"}
	pass := &ui.Field{Label: "Password", Secret: true, Hint: "Stored encrypted on this device"}
	if sec[cfg.Secret] != "" {
		pass.Placeholder = "(saved)"
	}
	intro := ""
	title := "Music server"
	if first {
		title = "Set up MiniTape"
		intro = "Connect to your music server: Navidrome, Subsonic, Airsonic, Gonic or any other Subsonic-compatible one."
	}
	password := func() string {
		if pass.Value != "" {
			return pass.Value
		}
		return sec[cfg.Secret]
	}
	f := &ui.Form{Title: title, Intro: intro, Fields: []*ui.Field{server, user, pass}}
	f.Check = func() error {
		if password() == "" {
			return screenError("Enter the password.")
		}
		c := &Subsonic{Base: tnet.NormalizeURL(server.Value, "4533"), User: user.Value, Password: password(), HTTP: tnet.Client(12 * time.Second)}
		return c.Ping()
	}
	f.Commit = func() error {
		cfg.URL, cfg.User = tnet.NormalizeURL(server.Value, "4533"), strings.TrimSpace(user.Value)
		if cfg.Secret == "" {
			cfg.Secret = "navidrome"
		}
		if pass.Value != "" {
			sec[cfg.Secret] = pass.Value
			if err := secrets.Save(filepath.Join(dir, "secrets.enc"), sec); err != nil {
				return err
			}
		}
		return saveSettings(dir, cfg)
	}
	return f.Run(scr, input)
}

// saveSettings writes settings.json.
func saveSettings(dir string, cfg *Settings) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(dir, "settings.json"), append(b, '\n'), 0644)
}
