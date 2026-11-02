package main

import "testing"

// Settings ends with Appearance.
func TestSettingsHaveAppearance(t *testing.T) {
	items := settingsItems(Settings{URL: "http://homeserver:4533", User: "alex"}, "Off", 0)
	if last := items[len(items)-1]; last.Label != "Appearance" || last.Sub == "" {
		t.Fatalf("Settings end with %+v", last)
	}
}

// Background playback is on by default and switches off.
func TestBackgroundPlaybackSetting(t *testing.T) {
	var cfg Settings
	if !cfg.background() || backgroundItem(cfg).Detail != "On" {
		t.Fatal("background playback isn't on by default")
	}
	off := false
	cfg.Background = &off
	if cfg.background() || backgroundItem(cfg).Detail != "Off" {
		t.Fatal("background playback didn't switch off")
	}
	found := false
	for _, it := range settingsItems(cfg, "Off", 0) {
		found = found || it.Label == "Background playback"
	}
	if !found {
		t.Error("Settings has no Background playback row")
	}
}
