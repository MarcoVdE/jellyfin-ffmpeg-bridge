//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// On Linux the agent normally runs in a container started from the
// jellyfin/jellyfin image, with the same volume paths as Jellyfin itself.
// There are no links to create: the paths in ffmpeg's arguments must simply
// exist here, which ensureLinks verifies.

var errWindowsOnly = errors.New("this command is only available on Windows; on Linux run `ffagent run` (see docs/linux-agent.md)")

type jobGroup interface{ Add(*os.Process) error }

// No job object needed: ffmpeg gets SIGKILL when the agent exits (see
// setProcAttr), and in a container everything stops with the agent anyway.
func newJobGroup() (jobGroup, error) { return nil, nil }

func dataDir() string {
	if d := os.Getenv("FFAGENT_HOME"); d != "" {
		return d
	}
	if os.Geteuid() == 0 {
		return "/var/lib/ffagent"
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ffagent")
}

// ensureLinks checks that every [paths] entry exists at its container path.
func ensureLinks(cfg *Config) []PathStatus {
	out := make([]PathStatus, 0, len(cfg.Paths))
	for _, p := range cfg.Paths {
		st := PathStatus{Container: p.Container, Link: p.Container, Target: p.Host}
		if fi, err := os.Stat(p.Container); err != nil || !fi.IsDir() {
			st.Error = "not found on this machine: mount " + p.Host + " at " + p.Container
		} else {
			st.OK = true
		}
		out = append(out, st)
	}
	return out
}

// mirrorRoot is where container paths resolve; outside Windows that is "/".
func mirrorRoot(*Config) string { return "/" }

func installCmd([]string) error { return errWindowsOnly }
func uninstallCmd() error       { return errWindowsOnly }
func restartCmd() error         { return errWindowsOnly }

func pauseIfOwnConsole() {}

func noArgs() int {
	fmt.Printf(usageText, agentVersion, dataDir())
	return 0
}
