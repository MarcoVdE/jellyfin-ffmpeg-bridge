package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	"jellyfin-ffmpeg-bridge/internal/translate"
)

// Config is ffagent.conf. It is a small INI file so Windows paths need no
// escaping:
//
//	token = 3f9c...
//
//	[paths]
//	/media6 = D:\Videos\Movies
//
//	[credentials]
//	\\nas\media = user:password
type Config struct {
	Token        string
	Listen       string // job port for the shim
	HealthListen string // read-only status for Home Assistant
	Drive        string // drive whose root mirrors the container's "/"
	FFmpeg       string // fixed ffmpeg; empty = managed + version sync (Windows)
	GPU          string // intel | nvidia | vaapi | none: selects the self-test
	Translate    string // auto | none | qsv-windows
	Device       string // render node for intel/vaapi on Linux
	Paths        []PathMap
	Credentials  []Credential
}

// PathMap mirrors one container path on Windows.
type PathMap struct {
	Container string // "/media6"
	Host      string // `D:\Videos\Movies` or `\\nas\movies`
}

// Credential is used to log on to an SMB share before linking to it.
type Credential struct {
	Share    string // `\\server\share`
	User     string
	Password string
}

const (
	defaultListen       = "0.0.0.0:7878"
	defaultHealthListen = "0.0.0.0:7879"
	defaultDrive        = "C:"
	defaultDevice       = "/dev/dri/renderD128"
)

// GPU kinds.
const (
	GPUIntel  = "intel"
	GPUNvidia = "nvidia"
	GPUVaapi  = "vaapi"
	GPUNone   = "none"
)

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = defaultListen
	}
	if c.HealthListen == "" {
		c.HealthListen = defaultHealthListen
	}
	if c.Drive == "" {
		c.Drive = defaultDrive
	}
	c.Drive = strings.ToUpper(strings.TrimRight(c.Drive, `\/`))
	if c.GPU == "" {
		c.GPU = defaultGPU()
	}
	c.GPU = strings.ToLower(c.GPU)
	if c.Translate == "" {
		c.Translate = "auto"
	}
	c.Translate = strings.ToLower(c.Translate)
	if c.Device == "" {
		c.Device = defaultDevice
	}
}

// Validate checks the config for mistakes a user is likely to make.
func (c *Config) Validate() error {
	if len(c.Token) < 16 {
		return fmt.Errorf("token must be at least 16 characters")
	}
	if len(c.Drive) != 2 || c.Drive[1] != ':' {
		return fmt.Errorf("drive must look like C:")
	}
	switch c.GPU {
	case GPUIntel, GPUNvidia, GPUNone:
	case GPUVaapi:
		if isWindows {
			return fmt.Errorf("gpu = vaapi only works on Linux; use intel, nvidia or none")
		}
	default:
		return fmt.Errorf("gpu must be intel, nvidia, vaapi or none, not %q", c.GPU)
	}
	if c.Translate != "auto" && !translate.ValidProfile(c.Translate) {
		return fmt.Errorf("translate must be auto, none or qsv-windows, not %q", c.Translate)
	}
	seen := map[string]bool{}
	for _, p := range c.Paths {
		if !strings.HasPrefix(p.Container, "/") || p.Container == "/" {
			return fmt.Errorf("[paths] %q: container path must start with / and not be /", p.Container)
		}
		if path.Clean(p.Container) != p.Container {
			return fmt.Errorf("[paths] %q: use the clean form %q", p.Container, path.Clean(p.Container))
		}
		key := strings.ToLower(p.Container)
		if seen[key] {
			return fmt.Errorf("[paths] %q listed twice", p.Container)
		}
		seen[key] = true
		if p.Host == "" {
			return fmt.Errorf("[paths] %q: host folder is empty", p.Container)
		}
	}
	return nil
}

// LoadConfig reads and validates the config file (no environment overrides).
func LoadConfig(file string) (*Config, error) {
	c, err := readConfigFile(file)
	if err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return c, nil
}

func readConfigFile(file string) (*Config, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	c, err := parseConfig(bufio.NewScanner(f))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return c, nil
}

func parseConfig(sc *bufio.Scanner) (*Config, error) {
	c := &Config{}
	section := ""
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if n == 1 {
			line = strings.TrimPrefix(line, "\ufeff") // Notepad BOM
		}
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' && line[len(line)-1] == ']' {
			section = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			if section != "paths" && section != "credentials" {
				return nil, fmt.Errorf("line %d: unknown section [%s]", n, section)
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected key = value", n)
		}
		k, v = strings.TrimSpace(k), unquoteValue(strings.TrimSpace(v))
		switch section {
		case "":
			switch strings.ToLower(k) {
			case "token":
				c.Token = v
			case "listen":
				c.Listen = v
			case "health_listen":
				c.HealthListen = v
			case "drive":
				c.Drive = v
			case "ffmpeg":
				c.FFmpeg = v
			case "gpu":
				c.GPU = v
			case "translate":
				c.Translate = v
			case "device":
				c.Device = v
			default:
				return nil, fmt.Errorf("line %d: unknown setting %q", n, k)
			}
		case "paths":
			c.Paths = append(c.Paths, PathMap{Container: k, Host: strings.TrimRight(v, `\`)})
		case "credentials":
			user, pass, ok := strings.Cut(v, ":")
			if !ok {
				return nil, fmt.Errorf("line %d: credentials must be user:password", n)
			}
			c.Credentials = append(c.Credentials, Credential{Share: strings.TrimRight(k, `\`), User: user, Password: pass})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	c.applyDefaults()
	return c, nil
}

func unquoteValue(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

// Render writes the config back with explanatory comments.
func (c *Config) Render() string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\r\n", a...) }
	w("# ffagent configuration. Restart after editing:  ffagent restart")
	w("")
	w("# Shared secret. Put the same value in FFSHIM_TOKEN next to your docker-compose.yml.")
	w("token = %s", c.Token)
	w("")
	w("# Where the Jellyfin container connects (job port) and where Home Assistant")
	w("# reads /health. Defaults: %s and %s", defaultListen, defaultHealthListen)
	if c.Listen != defaultListen {
		w("listen = %s", c.Listen)
	} else {
		w("# listen = %s", defaultListen)
	}
	if c.HealthListen != defaultHealthListen {
		w("health_listen = %s", c.HealthListen)
	} else {
		w("# health_listen = %s", defaultHealthListen)
	}
	w("")
	w("# Container paths are mirrored at the root of this drive, e.g. /media6 -> %s\\media6", c.Drive)
	if c.Drive != defaultDrive {
		w("drive = %s", c.Drive)
	} else {
		w("# drive = %s", defaultDrive)
	}
	w("")
	w("# GPU on this machine: intel (Quick Sync), nvidia (NVENC), or none.")
	w("# Selects the GPU self-test and, with translate = auto, the command translation.")
	w("gpu = %s", c.GPU)
	w("")
	w("# How Jellyfin's (Linux) ffmpeg commands are adapted: auto, none or qsv-windows.")
	w("# auto = qsv-windows for gpu = intel on Windows, otherwise none.")
	if c.Translate != "" && c.Translate != "auto" {
		w("translate = %s", c.Translate)
	} else {
		w("# translate = auto")
	}
	w("")
	w("# Use a fixed ffmpeg.exe instead of the automatically synced jellyfin-ffmpeg.")
	if c.FFmpeg != "" {
		w("ffmpeg = %s", c.FFmpeg)
	} else {
		w("# ffmpeg = C:\\path\\to\\ffmpeg.exe")
	}
	w("")
	w("# Every volume of the Jellyfin container: container path = Windows folder.")
	w("# Must match docker-compose.yml exactly. UNC paths (\\\\server\\share) work too.")
	w("[paths]")
	paths := append([]PathMap(nil), c.Paths...)
	sort.SliceStable(paths, func(i, j int) bool { return paths[i].Container < paths[j].Container })
	for _, p := range paths {
		w("%s = %s", p.Container, p.Host)
	}
	w("")
	w("# Only needed when [paths] point at network shares that require a login:")
	w("# \\\\server\\share = username:password")
	w("[credentials]")
	for _, cr := range c.Credentials {
		w("%s = %s:%s", cr.Share, cr.User, cr.Password)
	}
	return b.String()
}

// Save writes the config atomically. The file holds secrets, so it lives in
// the agent's data folder, which only administrators can write.
func (c *Config) Save(file string) error {
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, []byte(c.Render()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

func newToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
