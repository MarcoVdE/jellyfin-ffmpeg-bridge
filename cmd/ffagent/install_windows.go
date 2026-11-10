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
	"syscall"
	"time"
	"unicode/utf16"

	"jellyfin-ffmpeg-bridge/internal/compose"
)

const (
	taskName     = "ffagent"
	firewallRule = "ffagent (Jellyfin ffmpeg agent)"
)

// ownedFiles are the only files ffagent ever creates in its install folder.
// Uninstall and moves remove exactly these (plus the ffmpeg folder), never
// anything else that might have been put there.
var ownedFiles = []string{"ffagent.exe", "ffagent.exe.new", "ffagent.conf", "ffagent.conf.tmp",
	"ffagent.log", "ffagent.log.1", "ffagent-task.xml", "uninstall.cmd", tokenFileName}

// tokenFileName holds the token for the Jellyfin container (a compose secret).
const tokenFileName = "ffshim-token"

const uninstallScript = "@echo off\r\n" +
	"rem Removes ffagent and everything it added. Jellyfin's own files are not touched.\r\n" +
	"\"%~dp0ffagent.exe\" uninstall\r\n"

// installedDir returns the folder of the current install, or "".
func installedDir() string {
	if d := dataDir(); fileExistsAt(filepath.Join(d, "ffagent.conf")) {
		return d
	}
	return ""
}

func installCmd(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	composeFile := fs.String("compose", "", "docker-compose.yml of the Jellyfin container")
	service := fs.String("service", "", "name of the Jellyfin service (default: jellyfin, or the service running a Jellyfin image)")
	gpu := fs.String("gpu", "", "GPU on this machine: intel or nvidia (default: keep current, or intel)")
	dirFlag := fs.String("dir", "", `install folder (default: <compose folder>\ffagent, or the current install)`)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q (paths with spaces need quotes)", fs.Arg(0))
	}
	if !isAdmin() {
		return relaunchElevated(append([]string{"install"}, args...))
	}

	// Where things are now and where they go.
	oldDir := installedDir()
	composeAbs, svc := "", ""
	if *composeFile != "" {
		composeAbs = mustAbs(*composeFile)
		b, err := os.ReadFile(composeAbs)
		if err != nil {
			return err
		}
		preferred := *service
		if preferred == "" {
			preferred = "jellyfin"
		}
		if svc, err = compose.FindJellyfinService(string(b), preferred); err != nil {
			return fmt.Errorf("%s: %w", composeAbs, err)
		}
	}
	dir := *dirFlag
	switch {
	case dir != "":
		dir = mustAbs(dir)
	case composeAbs != "":
		dir = filepath.Join(filepath.Dir(composeAbs), "ffagent")
	case oldDir != "":
		dir = oldDir
	default:
		dir = programDataDir()
	}
	conf := filepath.Join(dir, "ffagent.conf")
	exe := filepath.Join(dir, "ffagent.exe")

	// 1. Config: keep the existing token and settings (also when moving).
	cfg, isNew := (*Config)(nil), false
	for _, from := range []string{conf, filepath.Join(oldDir, "ffagent.conf")} {
		if c, err := LoadConfig(from); err == nil {
			cfg = c
			break
		} else if fileExistsAt(from) {
			return fmt.Errorf("existing config is invalid, fix or delete it first: %w", err)
		}
	}
	if cfg == nil {
		cfg = &Config{Token: newToken()}
		cfg.applyDefaults()
		isNew = true
	}
	if *gpu != "" {
		cfg.GPU = strings.ToLower(*gpu)
	}
	if composeAbs != "" {
		mounts, warnings, err := compose.Volumes(mustRead(composeAbs), svc, filepath.Dir(composeAbs))
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
		cfg.Compose = composeAbs
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	// 2. Stop a running agent, prepare the (locked-down) install folder.
	_ = run("schtasks", "/End", "/TN", taskName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := lockDown(dir); err != nil {
		fmt.Println("  warning: could not restrict permissions on", dir+":", err)
	}
	if err := cfg.Save(conf); err != nil {
		return err
	}
	if err := copySelf(exe); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "uninstall.cmd"), []byte(uninstallScript), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, tokenFileName), []byte(cfg.Token), 0o644); err != nil {
		return err
	}

	// 3. Moving from an older location: take the downloaded ffmpeg along,
	// remove what ffagent left there, and remember the new location.
	if oldDir != "" && !samePath(oldDir, dir) {
		if !fileExistsAt(filepath.Join(dir, "ffmpeg")) {
			_ = os.Rename(filepath.Join(oldDir, "ffmpeg"), filepath.Join(dir, "ffmpeg"))
		}
		removeOwned(oldDir)
		fmt.Println("Moved the install from", oldDir)
	}
	if samePath(dir, programDataDir()) {
		_ = os.Remove(locationFile())
	} else {
		_ = os.MkdirAll(programDataDir(), 0o755)
		if err := os.WriteFile(locationFile(), []byte(dir), 0o644); err != nil {
			return err
		}
	}

	// 4. Compose side: token into .env, shim into the override file.
	var composeNotes []string
	if composeAbs != "" {
		notes, err := writeComposeFiles(composeAbs, svc, filepath.Join(dir, tokenFileName))
		if err != nil {
			return err
		}
		composeNotes = notes
	}

	// 5. Start at boot as SYSTEM, firewall for the local network, start now.
	if err := createTask(exe); err != nil {
		return err
	}
	_ = run("netsh", "advfirewall", "firewall", "delete", "rule", "name="+firewallRule)
	if err := run("netsh", "advfirewall", "firewall", "add", "rule", "name="+firewallRule,
		"dir=in", "action=allow", "program="+exe, "enable=yes",
		"profile=any", "remoteip=localsubnet"); err != nil {
		fmt.Println("  warning: could not add firewall rule:", err)
	}
	if err := run("schtasks", "/Run", "/TN", taskName); err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("ffagent", agentVersion, "installed in", dir)
	fmt.Println("To remove it again, run uninstall.cmd in that folder.")
	fmt.Println()
	fmt.Println("Mirrored container paths:")
	if len(cfg.Paths) == 0 {
		fmt.Println("  (none yet: run install again with --compose, or edit", conf+")")
	}
	for _, p := range cfg.Paths {
		fmt.Printf("  %-12s -> %s\n", p.Container, p.Host)
	}

	h := waitForReady(cfg)
	fmt.Println()
	if h == nil {
		fmt.Println("The agent did not respond. Check", filepath.Join(dir, "ffagent.log"))
	} else {
		printHealth(h)
	}

	fmt.Println()
	if composeAbs != "" {
		fmt.Println("Docker Compose:")
		for _, n := range composeNotes {
			fmt.Println("  " + n)
		}
		up := "docker compose up -d"
		if self, err := os.Executable(); err == nil && repoBuildContext(filepath.Dir(self), filepath.Dir(composeAbs)) != "" {
			up += " --build" // the override builds the shim image from this clone
		}
		fmt.Println()
		fmt.Println("Next, in", filepath.Dir(composeAbs)+":")
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

	// The exe used for installing is an identical copy of the installed one:
	// remove it once this window closes, so only ffagent\ffagent.exe remains.
	if self, err := os.Executable(); err == nil && !samePath(self, exe) {
		exitHooks = append(exitHooks, func() { deleteAfterExit(self, "") })
		fmt.Println()
		fmt.Println("Removing the installer", self, "when this window closes")
		fmt.Println("(an identical copy is installed; from now on use", exe+")")
	}
	return nil
}

// lockDown lets only Administrators and SYSTEM change dir (users may read and
// run), because the agent runs as SYSTEM: nobody else may swap its files.
func lockDown(dir string) error {
	return run("icacls", dir, "/inheritance:r",
		"/grant:r", "*S-1-5-32-544:(OI)(CI)F", // Administrators
		"/grant:r", "*S-1-5-18:(OI)(CI)F", // SYSTEM
		"/grant:r", "*S-1-5-32-545:(OI)(CI)RX") // Users
}

// removeOwned deletes ffagent's own files from dir, then dir if it is empty.
func removeOwned(dir string) {
	for _, f := range ownedFiles {
		_ = os.Remove(filepath.Join(dir, f))
	}
	_ = os.RemoveAll(filepath.Join(dir, "ffmpeg"))
	_ = os.Remove(dir) // only succeeds when nothing else is in it
}

// writeComposeFiles adds the shim to the compose project via an override
// file that passes the token as a compose secret (tokenFile). Only override
// files written by ffagent are ever replaced; .env is no longer used.
func writeComposeFiles(composeAbs, service, tokenFile string) ([]string, error) {
	dir := filepath.Dir(composeAbs)
	var notes []string

	self, _ := os.Executable()
	opts := overrideOpts{Service: service, Build: repoBuildContext(filepath.Dir(self), dir), Image: shimImage,
		Generated: true, TokenFile: composeRelPath(dir, tokenFile)}
	content := renderOverride(opts)

	target := overridePath(composeAbs)
	if target != "" && fileExists(target) {
		b, _ := os.ReadFile(target)
		switch {
		case isGeneratedOverride(target):
			// Ours (also from older versions): rewrite below.
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
			"  docker compose -f "+filepath.Base(composeAbs)+" -f ffshim.override.yml up -d"), nil
	}
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		return nil, err
	}
	notes = append(notes, "wrote "+target+" (token passed as a compose secret)")
	// Older versions put the token in .env for this file; that line was ours.
	if ok, _ := removeEnvVar(filepath.Join(dir, ".env"), "FFSHIM_TOKEN"); ok {
		notes = append(notes, "removed the old FFSHIM_TOKEN line from .env")
	}
	return notes, nil
}

// uninstallCmd removes everything ffagent added, and nothing else: the boot
// task, the firewall rule, the C:\ links, its install folder, and on the
// compose side FFSHIM_TOKEN in .env and the override file it generated.
func uninstallCmd() error {
	if !isAdmin() {
		return relaunchElevated([]string{"uninstall"})
	}
	dir := installedDir()
	if dir == "" {
		dir = dataDir()
	}
	cfg, _ := readConfigFile(filepath.Join(dir, "ffagent.conf"))

	_ = run("schtasks", "/End", "/TN", taskName)
	if run("schtasks", "/Delete", "/TN", taskName, "/F") == nil {
		fmt.Println("removed the boot task")
	}
	if run("netsh", "advfirewall", "firewall", "delete", "rule", "name="+firewallRule) == nil {
		fmt.Println("removed the firewall rule")
	}

	var manual []string
	if cfg != nil {
		cfg.applyDefaults()
		for _, p := range cfg.Paths {
			link := linkPath(cfg.Drive, p.Container)
			fi, err := os.Lstat(link)
			if err != nil || fi.Mode()&(os.ModeSymlink|os.ModeIrregular) == 0 {
				continue // missing, or a real folder: not ours
			}
			if cur, err := os.Readlink(link); err == nil && !samePath(cur, p.Host) {
				continue // points somewhere else now: not ours
			}
			if os.Remove(link) == nil {
				fmt.Println("removed link", link)
			}
		}
		if cfg.Compose != "" {
			cdir := filepath.Dir(cfg.Compose)
			if ok, err := removeEnvVar(filepath.Join(cdir, ".env"), "FFSHIM_TOKEN"); ok && err == nil {
				fmt.Println("removed FFSHIM_TOKEN from", filepath.Join(cdir, ".env"))
			}
			for _, f := range []string{overridePath(cfg.Compose), filepath.Join(cdir, "ffshim.override.yml")} {
				if f == "" || !fileExists(f) {
					continue
				}
				if isGeneratedOverride(f) {
					if os.Remove(f) == nil {
						fmt.Println("removed", f)
					}
				} else if b, _ := os.ReadFile(f); strings.Contains(string(b), "ffshim") {
					manual = append(manual, "remove the ffshim parts from "+f+" (edited by hand, left alone)")
				}
			}
			manual = append(manual, "run  docker compose up -d --remove-orphans  in "+cdir+" (Jellyfin goes back to its own ffmpeg)")
		}
	}
	manual = append(manual, "switch Jellyfin > Playback > Transcoding back to what you used before")

	// Everything in the install folder except the running exe, which is
	// removed right after this process exits.
	self, _ := os.Executable()
	for _, f := range ownedFiles {
		if f != "ffagent.exe" || !samePath(filepath.Join(dir, f), self) {
			_ = os.Remove(filepath.Join(dir, f))
		}
	}
	_ = os.RemoveAll(filepath.Join(dir, "ffmpeg"))
	_ = os.Remove(locationFile())
	_ = os.Remove(programDataDir()) // only if empty
	if samePath(filepath.Dir(self), dir) {
		exitHooks = append(exitHooks, func() { deleteAfterExit(self, dir) })
	} else {
		_ = os.Remove(filepath.Join(dir, "ffagent.exe"))
		_ = os.Remove(dir)
	}
	fmt.Println("removed", dir)

	fmt.Println()
	fmt.Println("ffagent is uninstalled. Left for you:")
	for _, m := range manual {
		fmt.Println("  - " + m)
	}
	return nil
}

// deleteAfterExit removes the running exe, and dir when given and then empty,
// a few seconds after this process has exited (a running exe is locked).
func deleteAfterExit(exe, dir string) {
	cmd := exec.Command("cmd")
	// The command line must reach cmd.exe verbatim: Go would escape the inner
	// quotes as \" (which cmd.exe doesn't understand) if passed as arguments.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
		CmdLine:       deleteLaterCmdLine(exe, dir),
	}
	_ = cmd.Start()
}

func restartCmd() error {
	if !isAdmin() {
		return relaunchElevated([]string{"restart"})
	}
	_ = run("schtasks", "/End", "/TN", taskName)
	time.Sleep(time.Second)
	return run("schtasks", "/Run", "/TN", taskName)
}

func mustAbs(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

func mustRead(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
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
	file := filepath.Join(filepath.Dir(exe), "ffagent-task.xml")
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
