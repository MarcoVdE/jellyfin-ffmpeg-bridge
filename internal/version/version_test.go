package version

import (
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"8.1.3-2-bookworm": "8.1.3-2",
		"v8.1.3-2":         "8.1.3-2",
		"1:7.1.1-7-noble":  "7.1.1-7",
		"8.1.3":            "8.1.3",
		" 8.1.3-2\n":       "8.1.3-2",
		"garbage":          "",
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFromDpkgStatus(t *testing.T) {
	status := `Package: curl
Status: install ok installed
Version: 7.88.1-10

Package: jellyfin-ffmpeg7
Status: deinstall ok config-files
Version: 7.1.1-7-bookworm

Package: jellyfin-ffmpeg8
Status: install ok installed
Priority: optional
Version: 8.1.3-2-bookworm
Description: Tools for transcoding

Package: zlib1g
Status: install ok installed
Version: 1:1.2.13
`
	if got := FromDpkgStatus(strings.NewReader(status)); got != "8.1.3-2" {
		t.Fatalf("got %q", got)
	}
	if got := FromDpkgStatus(strings.NewReader("Package: curl\nStatus: install ok installed\nVersion: 1\n")); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
	// Last stanza without trailing blank line.
	if got := FromDpkgStatus(strings.NewReader("Package: jellyfin-ffmpeg8\nStatus: install ok installed\nVersion: 8.0.1-1-noble")); got != "8.0.1-1" {
		t.Fatalf("got %q", got)
	}
}

func TestMatches(t *testing.T) {
	cases := []struct {
		installed, wanted string
		want              bool
	}{
		{"8.1.3-2", "8.1.3-2", true},
		{"8.1.3-2", "8.1.3-1", false},
		{"8.1.3-2", "8.1.3", true},
		{"8.1.30-1", "8.1.3", false},
		{"", "8.1.3-2", false},
		{"8.1.3-2", "", false},
	}
	for _, c := range cases {
		if got := Matches(c.installed, c.wanted); got != c.want {
			t.Errorf("Matches(%q, %q) = %v", c.installed, c.wanted, got)
		}
	}
}
