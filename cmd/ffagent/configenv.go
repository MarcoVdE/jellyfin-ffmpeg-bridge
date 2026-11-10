package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"strings"

	"jellyfin-ffmpeg-bridge/internal/translate"
)

const isWindows = runtime.GOOS == "windows"

// defaultGPU: Windows hosts are usually the Intel QSV case this project
// started with; elsewhere the self-test is off until a GPU is chosen.
func defaultGPU() string {
	if isWindows {
		return GPUIntel
	}
	return GPUNone
}

// TranslateProfile resolves translate = auto.
//
// Jellyfin always runs on Linux (in its container) and builds Linux commands:
//   - agent on Linux: the commands fit as they are;
//   - agent on Windows with NVIDIA: NVENC commands are OS independent;
//   - agent on Windows with Intel: QSV must be translated.
func (c *Config) TranslateProfile() string {
	if c.Translate != "" && c.Translate != "auto" {
		return c.Translate
	}
	if isWindows && c.GPU == GPUIntel {
		return translate.ProfileQSVWindows
	}
	return translate.ProfileNone
}

// envOverrides lets a config be given entirely through the environment, which
// is how the Linux agent container is configured.
var envOverrides = []struct {
	name string
	set  func(*Config, string)
}{
	{"FFAGENT_TOKEN", func(c *Config, v string) { c.Token = v }},
	{"FFAGENT_TOKEN_FILE", func(c *Config, v string) {
		if b, err := os.ReadFile(v); err == nil {
			c.Token = strings.TrimSpace(string(b))
		}
	}},
	{"FFAGENT_LISTEN", func(c *Config, v string) { c.Listen = v }},
	{"FFAGENT_HEALTH_LISTEN", func(c *Config, v string) { c.HealthListen = v }},
	{"FFAGENT_GPU", func(c *Config, v string) { c.GPU = v }},
	{"FFAGENT_TRANSLATE", func(c *Config, v string) { c.Translate = v }},
	{"FFAGENT_FFMPEG", func(c *Config, v string) { c.FFmpeg = v }},
	{"FFAGENT_DEVICE", func(c *Config, v string) { c.Device = v }},
	{"FFAGENT_DRIVE", func(c *Config, v string) { c.Drive = v }},
}

func applyEnv(c *Config, getenv func(string) string) bool {
	any := false
	for _, e := range envOverrides {
		if v := strings.TrimSpace(getenv(e.name)); v != "" {
			e.set(c, v)
			any = true
		}
	}
	return any
}

// loadEffectiveConfig reads ffagent.conf if it exists and applies FFAGENT_*
// environment variables on top. Without a file, the environment alone must
// provide at least FFAGENT_TOKEN.
func loadEffectiveConfig(file string, getenv func(string) string) (*Config, error) {
	c, err := readConfigFile(file)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		c = &Config{}
		if !applyEnv(c, getenv) {
			return nil, fmt.Errorf("no config: %s does not exist and FFAGENT_TOKEN is not set (run `ffagent install` on Windows)", file)
		}
	default:
		return nil, err
	}
	applyEnv(c, getenv)
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func effectiveConfig() (*Config, error) { return loadEffectiveConfig(configPath(), os.Getenv) }
