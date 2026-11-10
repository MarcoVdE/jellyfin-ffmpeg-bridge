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

func main() {
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "run":
		err = runCmd()
	case "install":
		err = installCmd(os.Args[2:])
	case "uninstall":
		err = uninstallCmd()
	case "restart":
		err = restartCmd()
	case "status":
		err = statusCmd(os.Args[2:])
	case "token":
		err = tokenCmd()
	case "selftest":
		err = selftestCmd()
	case "version", "-version", "--version":
		fmt.Println("ffagent", agentVersion)
	default:
		fmt.Printf(usageText, agentVersion, dataDir())
		if cmd != "" && cmd != "help" && cmd != "-h" && cmd != "--help" {
			os.Exit(2)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ffagent:", err)
		os.Exit(1)
	}
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
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print raw JSON")
	_ = fs.Parse(args)

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
	fmt.Printf("ffmpeg:      %s (Jellyfin image ships %s, in sync: %v)\n", orNone(ff.Version), orNone(ff.Expected), ff.InSync)
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
