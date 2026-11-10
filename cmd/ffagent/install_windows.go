//go:build windows

package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"

	"jellyfin-ffmpeg-bridge/internal/compose"
)

const (
	taskName     = "ffagent"
	firewallRule = "ffagent (Jellyfin ffmpeg agent)"
)

func installedExe() string { return filepath.Join(dataDir(), "ffagent.exe") }

func installCmd(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	composeFile := fs.String("compose", "", "docker-compose.yml of the Jellyfin container")
	service := fs.String("service", "jellyfin", "name of the Jellyfin service in the compose file")
	gpu := fs.String("gpu", "", "GPU on this machine: intel or nvidia (default: keep current, or intel)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q (paths with spaces need quotes)", fs.Arg(0))
	}

	if !isAdmin() {
		return fmt.Errorf("run this from an Administrator terminal (right-click Terminal > Run as administrator)")
	}
	if err := os.MkdirAll(dataDir(), 0o755); err != nil {
		return err
	}

	// 1. Config: keep an existing token, (re)read paths from the compose file.
	cfg, err := LoadConfig(configPath())
	isNew := false
	if err != nil {
		if !os.IsNotExist(err) && fileExists(configPath()) {
			return fmt.Errorf("existing config is invalid, fix or delete it first: %w", err)
		}
		cfg = &Config{Token: newToken()}
		cfg.applyDefaults()
		isNew = true
	}
	if *gpu != "" {
		cfg.GPU = strings.ToLower(*gpu)
	}
	if *composeFile != "" {
		mounts, warnings, err := readCompose(*composeFile, *service)
		if err != nil {
			return err
		}
		cfg.Paths = cfg.Paths[:0]
		for _, m := range mounts {
			cfg.Paths = append(cfg.Paths, PathMap{Container: m.Container, Host: m.Host})
		}
		for _, w := range warnings {
			fmt.Println("  warning:", w)
		}
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := cfg.Save(configPath()); err != nil {
		return err
	}

	// 1b. Compose side: token into .env, shim into docker-compose.override.yml.
	var composeNotes []string
	if *composeFile != "" {
		notes, err := writeComposeFiles(*composeFile, *service, cfg.Token)
		if err != nil {
			return err
		}
		composeNotes = notes
	}

	// 2. Binary: stop a running agent and copy this exe into the data folder.
	_ = run("schtasks", "/End", "/TN", taskName)
	if err := copySelf(installedExe()); err != nil {
		return err
	}

	// 3. Start at boot as SYSTEM, restart on failure, normal priority.
	if err := createTask(installedExe()); err != nil {
		return err
	}

	// 4. Firewall: allow the Jellyfin container and Home Assistant on the local network.
	_ = run("netsh", "advfirewall", "firewall", "delete", "rule", "name="+firewallRule)
	if err := run("netsh", "advfirewall", "firewall", "add", "rule", "name="+firewallRule,
		"dir=in", "action=allow", "program="+installedExe(), "enable=yes",
		"profile=any", "remoteip=localsubnet"); err != nil {
		fmt.Println("  warning: could not add firewall rule:", err)
	}

	// 5. Start and wait for it to come up.
	if err := run("schtasks", "/Run", "/TN", taskName); err != nil {
		return err
	}
	fmt.Println()
	fmt.Println("ffagent", agentVersion, "installed in", dataDir())
	fmt.Println()
	fmt.Println("Mirrored container paths:")
	if len(cfg.Paths) == 0 {
		fmt.Println("  (none yet: run install again with --compose, or edit", configPath()+")")
	}
	for _, p := range cfg.Paths {
		fmt.Printf("  %-12s -> %s\n", p.Container, p.Host)
	}

	h := waitForReady(cfg)
	fmt.Println()
	if h == nil {
		fmt.Println("The agent did not respond. Check", logPath())
	} else {
		printHealth(h)
	}

	fmt.Println()
	if *composeFile != "" {
		fmt.Println("Docker Compose:")
		for _, n := range composeNotes {
			fmt.Println("  " + n)
		}
		up := "docker compose up -d"
		if exe, err := os.Executable(); err == nil && repoBuildContext(filepath.Dir(exe), filepath.Dir(mustAbs(*composeFile))) != "" {
			up += " --build" // the override builds the shim image from this clone
		}
		fmt.Println()
		fmt.Println("Next, in", filepath.Dir(mustAbs(*composeFile))+":")
		fmt.Println()
		fmt.Println("  " + up)
	} else {
		if isNew {
			fmt.Println("Next: add this line to the .env file next to your docker-compose.yml:")
		} else {
			fmt.Println("Token (unchanged), for the .env file next to your docker-compose.yml:")
		}
		fmt.Println()
		fmt.Println("  FFSHIM_TOKEN=" + cfg.Token)
	}
	fmt.Println()
	fmt.Println("GPU:", cfg.GPU, "  translation:", cfg.TranslateProfile())
	switch cfg.GPU {
	case GPUNvidia:
		fmt.Println("Then set Jellyfin > Playback > Transcoding > Hardware acceleration to NVIDIA NVENC (see README).")
	default:
		fmt.Println("Then set the Jellyfin transcoding options listed in the README (Intel QuickSync).")
	}
	return nil
}

// writeComposeFiles puts the token into .env and adds the shim to the compose
// project via an override file. It never overwrites an override file.
func writeComposeFiles(composeFile, service, token string) ([]string, error) {
	abs := mustAbs(composeFile)
	dir := filepath.Dir(abs)
	var notes []string

	envFile := filepath.Join(dir, ".env")
	if err := setEnvVar(envFile, "FFSHIM_TOKEN", token); err != nil {
		return nil, fmt.Errorf("update %s: %w", envFile, err)
	}
	notes = append(notes, "set FFSHIM_TOKEN in "+envFile)

	exe, _ := os.Executable()
	opts := overrideOpts{Service: service, Build: repoBuildContext(filepath.Dir(exe), dir), Image: shimImage, Generated: true}
	if opts.Build == "" && opts.Image == "" {
		return append(notes,
			"could not find this repository next to ffagent.exe, so no override file was written.",
			"Run install with the ffagent.exe built inside the cloned repository, or copy",
			"examples/docker-compose.override.yml by hand."), nil
	}
	content := renderOverride(opts)

	target := overridePath(abs)
	if target != "" && fileExists(target) {
		b, _ := os.ReadFile(target)
		switch {
		case strings.Contains(string(b), overrideMarker):
			// Ours: rewrite below (picks up a new image tag or service name).
		case strings.Contains(string(b), "ffshim"):
			return append(notes, target+" already contains the shim (edited by hand), left unchanged"), nil
		default:
			target = ""
		}
	}
	if target == "" {
		alt := filepath.Join(dir, "ffshim.override.yml")
		if err := os.WriteFile(alt, []byte(content), 0o644); err != nil {
			return nil, err
		}
		return append(notes,
			"wrote "+alt,
			"you already use your own override file (or a custom compose file name), so merge",
			"ffshim.override.yml into it, or start with:",
			"  docker compose -f "+filepath.Base(abs)+" -f ffshim.override.yml up -d --build"), nil
	}
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		return nil, err
	}
	return append(notes, "wrote "+target), nil
}

func mustAbs(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

func readCompose(file, service string) ([]compose.Mount, []string, error) {
	abs, err := filepath.Abs(file)
	if err != nil {
		return nil, nil, err
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return nil, nil, err
	}
	return compose.Volumes(string(b), service, filepath.Dir(abs))
}

func uninstallCmd() error {
	if !isAdmin() {
		return fmt.Errorf("run this from an Administrator terminal")
	}
	_ = run("schtasks", "/End", "/TN", taskName)
	_ = run("schtasks", "/Delete", "/TN", taskName, "/F")
	_ = run("netsh", "advfirewall", "firewall", "delete", "rule", "name="+firewallRule)
	if cfg, err := LoadConfig(configPath()); err == nil {
		for _, p := range cfg.Paths {
			link := linkPath(cfg.Drive, p.Container)
			if fi, err := os.Lstat(link); err == nil && fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
				_ = os.Remove(link)
			}
		}
	}
	fmt.Println("ffagent removed. Config, logs and downloaded ffmpeg remain in", dataDir())
	return nil
}

func restartCmd() error {
	if !isAdmin() {
		return fmt.Errorf("run this from an Administrator terminal")
	}
	_ = run("schtasks", "/End", "/TN", taskName)
	time.Sleep(time.Second)
	return run("schtasks", "/Run", "/TN", taskName)
}

func isAdmin() bool {
	return exec.Command("net", "session").Run() == nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(out.String()))
	}
	return nil
}

func copySelf(dest string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if samePath(self, dest) {
		return nil
	}
	var lastErr error
	for i := 0; i < 10; i++ { // the old exe may take a moment to exit
		if lastErr = copyFile(self, dest); lastErr == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("copy to %s: %w", dest, lastErr)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".new"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// createTask registers a boot-time task running as SYSTEM. A scheduled task
// is used instead of a Windows service so no service wrapper is needed.
// Priority 4 = normal; the default (7) would make ffmpeg run below normal.
func createTask(exe string) error {
	xml := `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>Runs Jellyfin's ffmpeg jobs on this machine (jellyfin-ffmpeg-bridge).</Description>
  </RegistrationInfo>
  <Triggers>
    <BootTrigger><Enabled>true</Enabled></BootTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>S-1-5-18</UserId>
      <RunLevel>HighestAvailable</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>4</Priority>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>999</Count>
    </RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>` + xmlEscape(exe) + `</Command>
      <Arguments>run</Arguments>
      <WorkingDirectory>` + xmlEscape(filepath.Dir(exe)) + `</WorkingDirectory>
    </Exec>
  </Actions>
</Task>
`
	file := filepath.Join(dataDir(), "ffagent-task.xml")
	if err := os.WriteFile(file, utf16LE(xml), 0o644); err != nil {
		return err
	}
	defer os.Remove(file)
	return run("schtasks", "/Create", "/TN", taskName, "/XML", file, "/F")
}

func utf16LE(s string) []byte {
	u := utf16.Encode([]rune(strings.ReplaceAll(s, "\n", "\r\n")))
	b := make([]byte, 2+2*len(u))
	b[0], b[1] = 0xFF, 0xFE // BOM
	for i, c := range u {
		b[2+2*i] = byte(c)
		b[3+2*i] = byte(c >> 8)
	}
	return b
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}
