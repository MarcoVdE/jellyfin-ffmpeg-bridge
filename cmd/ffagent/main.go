// Command ffagent runs ffmpeg on behalf of the ffshim inside a Jellyfin
// container: on Windows (Intel QSV or NVIDIA) or on another Linux machine.
// It keeps ffmpeg in sync with the Jellyfin image and reports its health.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"
)

var agentVersion = "dev" // set with -ldflags "-X main.agentVersion=..."

const usageText = `ffagent %s - runs Jellyfin's ffmpeg jobs on this machine (jellyfin-ffmpeg-bridge)

Windows setup (in an Administrator terminal):
  ffagent install --compose C:\path\to\docker-compose.yml [--gpu intel|nvidia]

Linux: run "ffagent run" in the jellyfin/jellyfin image, configured with
FFAGENT_* environment variables (see docs/linux-agent.md).

Commands:
  install    (Windows) Install or update the agent and start it at boot.
               --compose FILE   read the Jellyfin volumes from this compose file
               --service NAME   compose service name (default: jellyfin)
               --gpu KIND       intel (default) or nvidia
  uninstall  (Windows) Stop and remove the agent (keeps %s)
  restart    (Windows) Restart the agent, e.g. after editing ffagent.conf
  status     Show the agent's health
  token      Print the token for FFSHIM_TOKEN
  selftest   Test hardware encoding with the configured GPU
  run        Run in the foreground (what the scheduled task / container runs)
  version    Print the version
`

var (
	// skipPause is set when another window takes over (after elevating).
	skipPause bool
	// exitHooks run right before the process exits (e.g. self-delete on uninstall).
	exitHooks []func()
)

func main() {
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	code := dispatch(cmd, os.Args[2:])
	// Started in a console window of its own (double-click, or Windows sudo
	// in "new window" mode)? Keep it open so the output can be read. Never
	// for "run": the boot task must exit (and be restarted) without waiting.
	if cmd != "run" && !skipPause {
		pauseIfOwnConsole()
	}
	for _, f := range exitHooks {
		f()
	}
	os.Exit(code)
}

func dispatch(cmd string, args []string) int {
	var err error
	switch cmd {
	case "run":
		err = runCmd()
	case "install":
		err = installCmd(args)
	case "uninstall":
		err = uninstallCmd()
	case "restart":
		err = restartCmd()
	case "status":
		err = statusCmd(args)
	case "token":
		err = tokenCmd()
	case "selftest":
		err = selftestCmd()
	case "version", "-version", "--version":
		fmt.Println("ffagent", agentVersion)
	case "":
		return noArgs()
	case "help", "-h", "--help", "/?":
		fmt.Printf(usageText, agentVersion, dataDir())
	default:
		fmt.Fprintf(os.Stderr, "ffagent: unknown command %q\n", cmd)
		if strings.HasPrefix(cmd, "-") {
			fmt.Fprintf(os.Stderr, "Did you mean:  ffagent install %s\n", strings.Join(os.Args[1:], " "))
		}
		fmt.Fprintln(os.Stderr, "Run `ffagent help` for all commands.")
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ffagent:", err)
		return 1
	}
	return 0
}

func configPath() string { return filepath.Join(dataDir(), "ffagent.conf") }
func logPath() string    { return filepath.Join(dataDir(), "ffagent.log") }

func runCmd() error {
	if err := os.MkdirAll(dataDir(), 0o755); err != nil {
		return err
	}
	lw := newRotatingFile(logPath(), 10<<20)
	logger := log.New(io.MultiWriter(os.Stdout, lw), "", log.LstdFlags)

	cfg, err := effectiveConfig()
	if err == nil && cfg.FFmpeg == "" {
		cfg.FFmpeg, err = defaultFFmpeg()
	}
	if err != nil {
		logger.Printf("cannot start: %v", err)
		return err
	}
	logger.Printf("gpu = %s, translate = %s", cfg.GPU, cfg.TranslateProfile())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	err = NewAgent(cfg, dataDir(), logger).Run(ctx)
	if err != nil {
		logger.Printf("stopped: %v", err)
	}
	return err
}

func tokenCmd() error {
	cfg, err := effectiveConfig()
	if err != nil {
		return err
	}
	fmt.Println(cfg.Token)
	fmt.Fprintf(os.Stderr, "\nAdd this line to the .env file next to your docker-compose.yml:\nFFSHIM_TOKEN=%s\n", cfg.Token)
	return nil
}

func selftestCmd() error {
	cfg, err := effectiveConfig()
	if err != nil {
		return err
	}
	if cfg.GPU == GPUNone {
		return fmt.Errorf("gpu = none: nothing to test (set gpu in ffagent.conf or FFAGENT_GPU)")
	}
	if cfg.FFmpeg == "" {
		if cfg.FFmpeg, err = defaultFFmpeg(); err != nil {
			return err
		}
	}
	ff := NewFFmpeg(dataDir(), cfg.FFmpeg, cfg.GPU, cfg.Device, log.New(io.Discard, "", 0))
	exe, ver := ff.Exe()
	if exe == "" {
		return fmt.Errorf("no ffmpeg installed yet; it is downloaded when the agent starts")
	}
	fmt.Printf("Testing %s (%s) with gpu = %s ...\n", exe, ver, cfg.GPU)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := runSelftest(ctx, exe, cfg.GPU, cfg.Device); err != nil {
		return fmt.Errorf("self-test FAILED: %v", err)
	}
	fmt.Println("OK: hardware encoding works (" + cfg.GPU + ").")
	return nil
}

func localHealthURL(cfg *Config) string {
	_, port, err := net.SplitHostPort(cfg.HealthListen)
	if err != nil {
		port = "7879"
	}
	return "http://127.0.0.1:" + port + "/health"
}

func fetchHealth(cfg *Config) (*Health, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(localHealthURL(cfg))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var h Health
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return nil, err
	}
	return &h, nil
}

func statusCmd(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print raw JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q (paths with spaces need quotes)", fs.Arg(0))
	}

	cfg, err := effectiveConfig()
	if err != nil {
		return err
	}
	h, err := fetchHealth(cfg)
	if err != nil {
		return fmt.Errorf("agent is not responding on %s: %v\nlog: %s", localHealthURL(cfg), err, logPath())
	}
	if *asJSON {
		b, _ := json.MarshalIndent(h, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	printHealth(h)
	return nil
}

func printHealth(h *Health) {
	fmt.Printf("Status:      %s\n", strings.ToUpper(h.Status))
	for _, p := range h.Problems {
		fmt.Printf("  problem:   %s\n", p)
	}
	fmt.Printf("Agent:       %s, up %s\n", h.AgentVersion, (time.Duration(h.UptimeS) * time.Second).String())
	ff := h.FFmpeg
	switch {
	case ff.Expected == "":
		fmt.Printf("ffmpeg:      %s (Jellyfin's version: not reported yet, Jellyfin hasn't connected)\n", orNone(ff.Version))
	case ff.InSync:
		fmt.Printf("ffmpeg:      %s (same as Jellyfin's image)\n", orNone(ff.Version))
	default:
		fmt.Printf("ffmpeg:      %s (Jellyfin's image has %s, NOT in sync)\n", orNone(ff.Version), ff.Expected)
	}
	if ff.Download != nil {
		fmt.Printf("  update:    %s\n", describeDownload(ff.Download))
	}
	if ff.LastSyncError != "" {
		fmt.Printf("  sync error: %s\n", ff.LastSyncError)
	}
	if h.Selftest.LastRun != nil {
		res := "OK"
		if !h.Selftest.OK {
			res = "FAILED: " + h.Selftest.Error
		}
		fmt.Printf("GPU test:    %s (%s)\n", res, h.Selftest.LastRun.Local().Format(time.DateTime))
	}
	if h.Shim.LastSeen != nil {
		fmt.Printf("Jellyfin:    last contact %s\n", h.Shim.LastSeen.Local().Format(time.DateTime))
	} else {
		fmt.Printf("Jellyfin:    no contact yet\n")
	}
	j := h.Jobs
	fmt.Printf("Jobs:        %d running, %d total, %d failed / %d rejected in last 15 min\n", j.Active, j.Total, j.Failed15m, j.Rejected15m)
	for _, p := range h.Paths {
		mark := "ok"
		if !p.OK {
			mark = "ERROR: " + p.Error
		}
		fmt.Printf("Path:        %-12s %s -> %s  %s\n", p.Container, p.Link, p.Target, mark)
	}
	if h.LastError != nil {
		fmt.Printf("Last error:  [%s] %s: %s\n", h.LastError.Time.Local().Format(time.DateTime), h.LastError.Kind, h.LastError.Message)
	}
}

// rotatingFile is a log file that keeps one previous generation.
type rotatingFile struct {
	path string
	max  int64
}

func newRotatingFile(path string, max int64) *rotatingFile { return &rotatingFile{path, max} }

func (r *rotatingFile) Write(p []byte) (int, error) {
	if fi, err := os.Stat(r.path); err == nil && fi.Size()+int64(len(p)) > r.max {
		_ = os.Remove(r.path + ".1")
		_ = os.Rename(r.path, r.path+".1")
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return len(p), nil // never fail the caller because of logging
	}
	defer f.Close()
	return f.Write(p)
}

// describeDownload formats install progress for status and install output.
func describeDownload(d *DownloadStatus) string {
	switch d.Phase {
	case "downloading":
		const mb = 1 << 20
		if d.Total > 0 {
			return fmt.Sprintf("downloading jellyfin-ffmpeg %s: %d / %d MB (%d%%)",
				d.Version, d.Bytes/mb, d.Total/mb, d.Bytes*100/d.Total)
		}
		return fmt.Sprintf("downloading jellyfin-ffmpeg %s: %d MB", d.Version, d.Bytes/mb)
	case "unpacking":
		return "unpacking jellyfin-ffmpeg " + d.Version + " ..."
	case "testing":
		return "testing the GPU with jellyfin-ffmpeg " + d.Version + " ..."
	}
	return d.Phase + " " + d.Version
}

// waitForReady follows the agent's first start: the jellyfin-ffmpeg download
// and the GPU self-test. It returns the last health report (nil if the agent
// never answered).
func waitForReady(cfg *Config) *Health { return waitForReadyWithin(cfg, 15*time.Minute, time.Second) }

func waitForReadyWithin(cfg *Config, limit, interval time.Duration) *Health {
	fmt.Println()
	fmt.Println("Getting ready (jellyfin-ffmpeg download and GPU test):")
	show := func(s string) { fmt.Printf("\r  %-72s", s) }
	deadline := time.Now().Add(limit)
	var h *Health
	for time.Now().Before(deadline) {
		cur, err := fetchHealth(cfg)
		if err == nil {
			h = cur
		}
		ff := FFmpegStatus{}
		if h != nil {
			ff = h.FFmpeg
		}
		switch {
		case err != nil:
			show("starting the agent ...")
		case ff.Download != nil:
			show(describeDownload(ff.Download))
		case ff.Path == "" && ff.LastSyncError != "" && !ff.Syncing:
			show("jellyfin-ffmpeg could not be installed (see below)")
			fmt.Println()
			return h
		case ff.Path == "":
			show("looking up the jellyfin-ffmpeg release ...")
		case cfg.GPU != GPUNone && h.Selftest.LastRun == nil:
			show("testing the GPU ...")
		default:
			show("ready")
			fmt.Println()
			return h
		}
		time.Sleep(interval)
	}
	fmt.Println()
	fmt.Printf("  still not ready after %s; follow along with:  ffagent status\n", limit)
	return h
}
