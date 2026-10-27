package app

import "testing"

func TestServarrQualityBase(t *testing.T) {
	cases := map[string]string{
		"480p":  "SD-480p",
		"720p":  "HD-720p",
		"1080p": "HD-1080p",
		"2160p": "UHD-2160p",
		"1080":  "HD-1080p",
	}
	for in, want := range cases {
		if got := servarrQualityBase(in); got != want {
			t.Errorf("%s: got %q, want %q", in, got, want)
		}
	}
}

func TestServarrLanguageNames(t *testing.T) {
	if got := servarrLanguageName("en"); got != "English" {
		t.Fatalf("en -> %q", got)
	}
	if got := servarrLanguageCode("Hungarian"); got != "hu" {
		t.Fatalf("Hungarian -> %q", got)
	}
	if got := servarrLanguageName("xx"); got != "" {
		t.Fatalf("unknown language -> %q", got)
	}
}

func TestProfileQualityFromName(t *testing.T) {
	cases := map[string]string{
		"HD-720p":             "720p",
		"HD-1080p · English":  "1080p",
		"SD-480p · Hungarian": "480p",
		"UHD-2160p · English": "2160p",
		"Ultra-HD":            "2160p",
	}
	for in, want := range cases {
		if got := profileQualityFromName(in, ""); got != want {
			t.Errorf("%s: got %q, want %q", in, got, want)
		}
	}
}

func TestBaseProfileName(t *testing.T) {
	profiles := []map[string]any{
		{"name": "SD"},
		{"name": "HD-720p"},
		{"name": "HD-1080p"},
		{"name": "Ultra-HD"},
	}
	cases := map[string]string{
		"480p":  "SD",
		"720p":  "HD-720p",
		"1080p": "HD-1080p",
		"2160p": "Ultra-HD",
	}
	for quality, want := range cases {
		if got := baseProfileName(profiles, quality); got != want {
			t.Errorf("%s: got %q, want %q", quality, got, want)
		}
	}
}
