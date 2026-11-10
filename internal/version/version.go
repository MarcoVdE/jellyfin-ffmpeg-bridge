// Package version handles jellyfin-ffmpeg version strings.
//
// The Jellyfin image installs jellyfin-ffmpeg as a Debian package whose
// version looks like "8.1.3-2-bookworm". The matching GitHub release of
// jellyfin/jellyfin-ffmpeg is tagged "v8.1.3-2".
package version

import (
	"bufio"
	"io"
	"regexp"
	"strings"
)

var releaseVersion = regexp.MustCompile(`^\d+\.\d+(\.\d+)?(-\d+)?`)

// Normalize turns a package or tag version into "8.1.3-2" form.
// It returns "" if v does not look like a jellyfin-ffmpeg version.
func Normalize(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexByte(v, ':'); i >= 0 { // Debian epoch
		v = v[i+1:]
	}
	return releaseVersion.FindString(v)
}

// FromDpkgStatus finds the installed jellyfin-ffmpeg package in a dpkg status
// database (/var/lib/dpkg/status) and returns its normalized version.
func FromDpkgStatus(r io.Reader) string {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var pkg, ver string
	installed := false
	flush := func() string {
		if strings.HasPrefix(pkg, "jellyfin-ffmpeg") && installed {
			return Normalize(ver)
		}
		return ""
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if v := flush(); v != "" {
				return v
			}
			pkg, ver, installed = "", "", false
		case strings.HasPrefix(line, "Package: "):
			pkg = strings.TrimPrefix(line, "Package: ")
		case strings.HasPrefix(line, "Version: "):
			ver = strings.TrimPrefix(line, "Version: ")
		case strings.HasPrefix(line, "Status: "):
			installed = strings.HasSuffix(line, " installed")
		}
	}
	return flush()
}

// Matches reports whether an installed version satisfies a wanted version.
// A wanted version without a revision ("8.1.3") matches any revision.
func Matches(installed, wanted string) bool {
	installed, wanted = Normalize(installed), Normalize(wanted)
	if installed == "" || wanted == "" {
		return false
	}
	if installed == wanted {
		return true
	}
	return !strings.Contains(wanted, "-") && strings.HasPrefix(installed, wanted+"-")
}
