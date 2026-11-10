// Package compose extracts bind-mount volumes of one service from a
// docker-compose file, so the agent can mirror them on the Windows host.
//
// It is a deliberately small, indentation-based reader for the common
// compose layouts (short and long volume syntax), not a full YAML parser.
// Anything it cannot resolve is returned as a warning instead of guessed.
package compose

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Mount is one bind mount: a path inside the container and the Windows
// folder it comes from.
type Mount struct {
	Container string // e.g. "/media6"
	Host      string // e.g. `D:\Videos\Movies`
}

var keyLine = regexp.MustCompile(`^([A-Za-z0-9_.\-"']+):\s*(.*)$`)

type line struct {
	indent int
	text   string
	num    int
}

// Volumes returns the bind mounts of service. composeDir is the Windows
// folder that contains the compose file (used to resolve "./config").
func Volumes(content, service, composeDir string) ([]Mount, []string, error) {
	lines := splitLines(content)

	svcStart, svcIndent := -1, -1
	inServices, servicesIndent := false, -1
	for i, l := range lines {
		if !inServices {
			if l.text == "services:" {
				inServices, servicesIndent = true, l.indent
			}
			continue
		}
		if l.indent <= servicesIndent {
			inServices = false
			continue
		}
		if unquote(strings.TrimSuffix(l.text, ":")) == service && strings.HasSuffix(l.text, ":") {
			svcStart, svcIndent = i, l.indent
			break
		}
	}
	if svcStart < 0 {
		return nil, nil, fmt.Errorf("service %q not found under services:", service)
	}

	volStart, volIndent := -1, -1
	for i := svcStart + 1; i < len(lines) && lines[i].indent > svcIndent; i++ {
		if lines[i].text == "volumes:" {
			volStart, volIndent = i, lines[i].indent
			break
		}
	}
	if volStart < 0 {
		return nil, nil, fmt.Errorf("service %q has no volumes:", service)
	}

	var items [][]line
	for i := volStart + 1; i < len(lines) && lines[i].indent > volIndent; i++ {
		l := lines[i]
		if strings.HasPrefix(l.text, "- ") || l.text == "-" {
			items = append(items, []line{l})
		} else if len(items) > 0 {
			items[len(items)-1] = append(items[len(items)-1], l)
		}
	}

	var mounts []Mount
	var warnings []string
	for _, it := range items {
		first := strings.TrimSpace(strings.TrimPrefix(it[0].text, "-"))
		var src, dst string
		if m := keyLine.FindStringSubmatch(first); m != nil && !looksLikeShortVolume(first) {
			// Long syntax: "- type: bind" followed by source:/target: keys.
			kv := map[string]string{unquote(m[1]): unquote(m[2])}
			for _, l := range it[1:] {
				if m := keyLine.FindStringSubmatch(l.text); m != nil {
					kv[unquote(m[1])] = unquote(m[2])
				}
			}
			if t := kv["type"]; t != "" && t != "bind" {
				continue
			}
			src, dst = kv["source"], kv["target"]
		} else {
			var ok bool
			src, dst, ok = splitShort(unquote(first))
			if !ok {
				warnings = append(warnings, fmt.Sprintf("line %d: could not parse volume %q", it[0].num, first))
				continue
			}
		}
		if src == "" || dst == "" {
			warnings = append(warnings, fmt.Sprintf("line %d: incomplete volume entry", it[0].num))
			continue
		}
		host, err := resolveHost(src, composeDir)
		if err != nil {
			if err != errNamedVolume {
				warnings = append(warnings, fmt.Sprintf("line %d: %s: %v", it[0].num, src, err))
			}
			continue
		}
		mounts = append(mounts, Mount{Container: path.Clean(dst), Host: host})
	}
	return mounts, warnings, nil
}

func looksLikeShortVolume(s string) bool {
	s = unquote(s)
	return strings.Contains(s, ":/") && !strings.HasPrefix(s, "type:") &&
		!strings.HasPrefix(s, "source:") && !strings.HasPrefix(s, "target:")
}

// splitShort splits "SRC:DST[:MODE]" where SRC may carry a drive letter.
func splitShort(s string) (src, dst string, ok bool) {
	start := 0
	if len(s) >= 2 && s[1] == ':' && isLetter(s[0]) {
		start = 2 // skip "D:"
	}
	i := strings.Index(s[start:], ":/")
	if i < 0 {
		return "", "", false
	}
	i += start
	src = s[:i]
	rest := s[i+1:]
	if j := strings.IndexByte(rest, ':'); j >= 0 {
		rest = rest[:j]
	}
	return src, rest, src != "" && rest != ""
}

var errNamedVolume = fmt.Errorf("named volume")

func resolveHost(src, composeDir string) (string, error) {
	switch {
	case strings.Contains(src, "${") || strings.HasPrefix(src, "$"):
		return "", fmt.Errorf("uses a variable; add it to [paths] by hand")
	case len(src) >= 2 && src[1] == ':' && isLetter(src[0]):
		return toWindows(path.Clean(strings.ReplaceAll(src, `\`, "/"))), nil
	case strings.HasPrefix(src, `\\`) || strings.HasPrefix(src, "//"):
		return toWindows(src), nil
	case strings.HasPrefix(src, "./") || strings.HasPrefix(src, "../") || src == "." || src == "..":
		if composeDir == "" {
			return "", fmt.Errorf("relative path but compose folder unknown")
		}
		base := strings.ReplaceAll(composeDir, `\`, "/")
		return toWindows(path.Join(base, src)), nil
	case strings.HasPrefix(src, "/") || strings.HasPrefix(src, "~"):
		return "", fmt.Errorf("Linux/WSL path; add the matching Windows folder to [paths] by hand")
	case !strings.ContainsAny(src, `/\`):
		return "", errNamedVolume
	default:
		return "", fmt.Errorf("unsupported source path")
	}
}

func toWindows(p string) string {
	p = strings.ReplaceAll(p, "/", `\`)
	if len(p) == 2 && p[1] == ':' {
		p += `\`
	}
	return p
}

func isLetter(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

func splitLines(content string) []line {
	var out []line
	for n, raw := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		text := stripComment(raw)
		trimmed := strings.TrimLeft(text, " \t")
		trimmed = strings.TrimRight(trimmed, " \t")
		if trimmed == "" || trimmed == "---" {
			continue
		}
		out = append(out, line{indent: len(text) - len(strings.TrimLeft(text, " \t")), text: trimmed, num: n + 1})
	}
	return out
}

// stripComment removes "# ..." comments that are not inside quotes.
func stripComment(s string) string {
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0:
			if c == q {
				q = 0
			}
		case c == '"' || c == '\'':
			q = c
		case c == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t'):
			return s[:i]
		}
	}
	return s
}
