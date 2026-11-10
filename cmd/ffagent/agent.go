package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"jellyfin-ffmpeg-bridge/internal/proto"
	"jellyfin-ffmpeg-bridge/internal/translate"
)

const (
	selftestInterval = 15 * time.Minute
	linkRetry        = time.Minute
)

// Agent runs ffmpeg jobs for ffshim and serves /health.
type Agent struct {
	cfg    *Config
	log    *log.Logger
	state  *State
	ff     *FFmpeg
	jobs   jobGroup
	now    func() time.Time
	linker func(*Config) []PathStatus
	info   *infoCache
}

func NewAgent(cfg *Config, dataDir string, logger *log.Logger) *Agent {
	a := &Agent{
		cfg:    cfg,
		log:    logger,
		state:  NewState(time.Now()),
		ff:     NewFFmpeg(dataDir, cfg.FFmpeg, cfg.GPU, cfg.Device, logger),
		now:    time.Now,
		linker: ensureLinks,
		info:   newInfoCache(),
	}
	a.ff.onTested = func(err error) { a.state.SetSelftest(a.now(), err) }
	return a
}

// Run blocks until ctx is cancelled or a listener fails.
func (a *Agent) Run(ctx context.Context) error {
	jobLn, err := net.Listen("tcp", a.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", a.cfg.Listen, err)
	}
	healthLn, err := net.Listen("tcp", a.cfg.HealthListen)
	if err != nil {
		jobLn.Close()
		return fmt.Errorf("listen %s: %w", a.cfg.HealthListen, err)
	}
	a.log.Printf("ffagent %s: jobs on %s, health on http://%s/health", agentVersion, a.cfg.Listen, a.cfg.HealthListen)
	return a.Serve(ctx, jobLn, healthLn)
}

// Serve runs the agent on existing listeners.
func (a *Agent) Serve(ctx context.Context, jobLn, healthLn net.Listener) error {
	jg, err := newJobGroup()
	if err != nil {
		a.log.Printf("warning: cannot create job object (ffmpeg may outlive the agent): %v", err)
	}
	a.jobs = jg

	srv := &http.Server{Handler: a.healthHandler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(healthLn) }()
	go a.background(ctx)

	go func() {
		<-ctx.Done()
		jobLn.Close()
		srv.Close()
	}()

	for {
		c, err := jobLn.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		go a.handle(c)
	}
}

// background keeps path links healthy, installs ffmpeg and runs self-tests.
func (a *Agent) background(ctx context.Context) {
	a.refreshLinks()
	if ch := a.ff.StartSync(); ch != nil {
		select {
		case <-ch:
		case <-ctx.Done():
			return
		}
	}
	a.runSelftest(ctx)
	a.warmInfo()

	links := time.NewTicker(linkRetry)
	tests := time.NewTicker(selftestInterval)
	defer links.Stop()
	defer tests.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-links.C:
			a.refreshLinks()
		case <-tests.C:
			a.ff.StartSync()
			a.runSelftest(ctx)
		}
	}
}

func (a *Agent) refreshLinks() {
	a.state.SetPaths(a.linker(a.cfg))
}

func (a *Agent) runSelftest(ctx context.Context) {
	exe, _ := a.ff.Exe()
	if exe == "" || a.cfg.GPU == GPUNone {
		return
	}
	tctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	err := a.ff.selftest(tctx, exe)
	if err != nil {
		a.log.Printf("GPU self-test failed: %v", err)
	}
	a.state.SetSelftest(a.now(), err)
}

func (a *Agent) handle(c net.Conn) {
	defer c.Close()
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(15 * time.Second)
	}
	w := proto.NewWriter(c)
	r := bufio.NewReaderSize(c, 64*1024)

	nonce := make([]byte, proto.NonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return
	}
	if err := w.Send(proto.Challenge, nonce); err != nil {
		return
	}
	_ = c.SetReadDeadline(time.Now().Add(15 * time.Second))
	t, p, err := proto.ReadFrame(r)
	if err != nil || t != proto.Hello || len(p) > 1<<20 {
		return
	}
	_ = c.SetReadDeadline(time.Time{})

	var h proto.HelloMsg
	if err := json.Unmarshal(p, &h); err != nil {
		return
	}
	if !proto.Verify(a.cfg.Token, nonce, h.MAC) {
		a.log.Printf("rejected connection from %s: wrong token", c.RemoteAddr())
		a.state.AuthFailed(a.now(), c.RemoteAddr().String())
		_ = w.Send(proto.Error, []byte("authentication failed: FFSHIM_TOKEN does not match the token in ffagent.conf"))
		return
	}
	if h.Proto != proto.Version {
		_ = w.Send(proto.Error, []byte(fmt.Sprintf("protocol mismatch: shim speaks %d, agent speaks %d; update both", h.Proto, proto.Version)))
		return
	}

	a.state.SawShim(a.now(), h.Kind, h.ShimVersion)
	a.ff.SetExpected(h.FFmpegVersion)

	switch h.Kind {
	case proto.KindPing:
		a.ff.StartSync()
		st := a.ff.Status()
		msg := fmt.Sprintf("ok: ffagent %s, jellyfin-ffmpeg %s\n", agentVersion, orNone(st.Version))
		_ = w.Send(proto.Stdout, []byte(msg))
		_ = w.Send(proto.Exit, proto.ExitPayload(0))
	case proto.KindRun:
		a.runJob(r, w, h)
	default:
		_ = w.Send(proto.Error, []byte("unknown request kind "+h.Kind))
	}
}

func (a *Agent) runJob(r io.Reader, w *proto.Writer, h proto.HelloMsg) {
	res, err := translate.For(a.cfg.TranslateProfile(), h.Args)
	if err != nil {
		a.log.Printf("rejected job: %v", err)
		a.state.Rejected(a.now(), err.Error())
		_ = w.Send(proto.Error, []byte(err.Error()))
		return
	}

	exe, ver, err := a.ff.Ready(context.Background())
	if err != nil {
		a.state.JobError(a.now(), err.Error())
		_ = w.Send(proto.Error, []byte("ffagent: "+err.Error()))
		return
	}
	if translate.IsVersionProbe(h.Args) {
		a.state.Probed(ver)
	}

	if isInfoCommand(res.Args) {
		ir, ran, took := a.info.get(exe, res.Args)
		if ran {
			a.log.Printf("info %s: ran ffmpeg in %s, answered from memory from now on", summarizeArgs(res.Args), took.Round(time.Millisecond))
		}
		if ir.err != nil {
			a.state.JobError(a.now(), "start ffmpeg: "+ir.err.Error())
			_ = w.Send(proto.Error, []byte("ffagent: start ffmpeg: "+ir.err.Error()))
			return
		}
		out := ir.stdout
		if len(res.ExtraHwaccels) > 0 {
			out = translate.AppendHwaccels(out, res.ExtraHwaccels)
		}
		if len(ir.stderr) > 0 {
			_ = w.Send(proto.Stderr, ir.stderr)
		}
		if len(out) > 0 {
			_ = w.Send(proto.Stdout, out)
		}
		_ = w.Send(proto.Exit, proto.ExitPayload(ir.code))
		return
	}

	started := time.Now()

	cmd := exec.Command(exe, res.Args...)
	cmd.Dir = a.workDir(h.Cwd)
	setProcAttr(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = w.Send(proto.Error, []byte(err.Error()))
		return
	}
	var stdoutBuf *bytes.Buffer
	if len(res.ExtraHwaccels) > 0 {
		stdoutBuf = &bytes.Buffer{}
		cmd.Stdout = stdoutBuf
	} else {
		cmd.Stdout = proto.StreamWriter{W: w, T: proto.Stdout}
	}
	tail := newTail(8192)
	cmd.Stderr = io.MultiWriter(proto.StreamWriter{W: w, T: proto.Stderr}, tail)

	if err := cmd.Start(); err != nil {
		a.state.JobError(a.now(), "start ffmpeg: "+err.Error())
		_ = w.Send(proto.Error, []byte("ffagent: start ffmpeg: "+err.Error()))
		return
	}
	if a.jobs != nil {
		if err := a.jobs.Add(cmd.Process); err != nil {
			a.log.Printf("warning: job object: %v", err)
		}
	}
	a.state.JobStarted()

	// Forward stdin; a dropped connection means Jellyfin killed the shim.
	var cancelled atomic.Bool
	go func() {
		for {
			t, p, err := proto.ReadFrame(r)
			if err != nil {
				cancelled.Store(true)
				_ = cmd.Process.Kill()
				return
			}
			switch t {
			case proto.Stdin:
				_, _ = stdin.Write(p)
			case proto.StdinEOF:
				_ = stdin.Close()
			}
		}
	}()

	_ = cmd.Wait()
	code := cmd.ProcessState.ExitCode()
	wasCancelled := cancelled.Load()
	a.state.JobFinished(a.now(), code, wasCancelled, tail.String())
	outcome := fmt.Sprintf("exit %d", code)
	if wasCancelled {
		outcome = "stopped by Jellyfin"
	}
	a.log.Printf("job %s: %s after %s", summarizeArgs(res.Args), outcome, time.Since(started).Round(time.Millisecond))
	if wasCancelled {
		return
	}
	if stdoutBuf != nil {
		_ = w.Send(proto.Stdout, translate.AppendHwaccels(stdoutBuf.Bytes(), res.ExtraHwaccels))
	}
	_ = w.Send(proto.Exit, proto.ExitPayload(code))
}

// workDir maps the shim's working directory onto the mirror drive, so that
// "/cache/transcodes" becomes C:\cache\transcodes. Paths that start with "/"
// in ffmpeg arguments resolve against the root of this directory's drive.
func (a *Agent) workDir(cwd string) string {
	root := mirrorRoot(a.cfg)
	if cwd == "" || cwd == "/" || !strings.HasPrefix(cwd, "/") {
		return root
	}
	d := filepath.Join(root, filepath.FromSlash(cwd))
	if fi, err := os.Stat(d); err == nil && fi.IsDir() {
		return d
	}
	return root
}

func (a *Agent) health() Health {
	return a.state.Snapshot(a.now(), a.ff.Status(), agentVersion)
}

func (a *Agent) healthHandler() http.Handler {
	mux := http.NewServeMux()
	serve := func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		rw.Header().Set("Cache-Control", "no-store")
		enc := json.NewEncoder(rw)
		enc.SetIndent("", "  ")
		_ = enc.Encode(a.health())
	}
	mux.HandleFunc("GET /health", serve)
	mux.HandleFunc("GET /{$}", serve)
	return mux
}

// warmInfo pre-runs the info commands Jellyfin and plugins ask first, so even
// the first request after a (re)start is answered instantly.
func (a *Agent) warmInfo() {
	exe, _ := a.ff.Exe()
	if exe == "" {
		return
	}
	for _, args := range warmInfoCommands {
		if _, ran, took := a.info.get(exe, args); ran {
			a.log.Printf("info %s: pre-ran ffmpeg in %s", summarizeArgs(args), took.Round(time.Millisecond))
		}
	}
}

// summarizeArgs shortens a command line for the log.
func summarizeArgs(args []string) string {
	s := strings.Join(args, " ")
	if len(s) > 160 {
		s = s[:157] + "..."
	}
	return s
}
