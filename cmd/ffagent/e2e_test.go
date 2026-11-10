//go:build !windows

package main

// End-to-end tests: the real ffshim binary talks to a real Agent, which runs
// a fake ffmpeg shell script. Runs on Linux/macOS (the script needs /bin/sh).

import (
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const fakeFFmpeg = `#!/bin/sh
case "$1" in
  -hwaccels) printf 'Hardware acceleration methods:\nqsv\nd3d11va\nopencl\n\n'; exit 0 ;;
  -version)  echo run >> "$0.count"; echo "ffmpeg version 8.1.3-Jellyfin"; exit 0 ;;
  -hwfail)   echo "[h264_qsv @ 0x1] Error creating a MFX session: -9." >&2; exit 1 ;;
  -readq)    echo started >&2; read line; echo "stdin:$line" >&2; exit 0 ;;
  -sleep)    echo started >&2; exec sleep 30 ;;
  -pwd)      pwd; exit 0 ;;
esac
echo "args:$*" >&2
exit 3
`

type harness struct {
	t     *testing.T
	agent *Agent
	shim  string
	addr  string
	token string
}

func newHarness(t *testing.T) *harness { return newHarnessWith(t, "qsv-windows") }

func newHarnessWith(t *testing.T, translate string) *harness {
	t.Helper()
	dir := t.TempDir()

	shim := filepath.Join(dir, "ffmpeg")
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", shim, "../ffshim")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build shim: %v\n%s", err, out)
	}

	ff := filepath.Join(dir, "fake-ffmpeg.sh")
	if err := os.WriteFile(ff, []byte(fakeFFmpeg), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{Token: newToken(), FFmpeg: ff, Translate: translate}
	cfg.applyDefaults()
	a := NewAgent(cfg, dir, log.New(io.Discard, "", 0))
	a.ff.selftest = func(context.Context, string) error { return nil }

	jobLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	healthLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = a.Serve(ctx, jobLn, healthLn) }()

	return &harness{t: t, agent: a, shim: shim, addr: jobLn.Addr().String(), token: cfg.Token}
}

func (h *harness) cmd(token string, args ...string) *exec.Cmd {
	c := exec.Command(h.shim, args...)
	c.Env = append(os.Environ(),
		"FFSHIM_AGENT="+h.addr,
		"FFSHIM_TOKEN="+token,
		"FFSHIM_FFMPEG_VERSION=none",
	)
	return c
}

func (h *harness) run(stdin string, args ...string) (stdout, stderr string, code int) {
	h.t.Helper()
	c := h.cmd(h.token, args...)
	c.Stdin = strings.NewReader(stdin)
	var o, e bytes.Buffer
	c.Stdout, c.Stderr = &o, &e
	err := c.Run()
	code = 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		h.t.Fatal(err)
	}
	return o.String(), e.String(), code
}

func TestE2EVersionAndArgs(t *testing.T) {
	h := newHarness(t)
	out, _, code := h.run("", "-version")
	if code != 0 || !strings.Contains(out, "8.1.3-Jellyfin") {
		t.Fatalf("version probe: code %d out %q", code, out)
	}
	// Device args are translated before ffmpeg sees them; exit code relayed.
	_, errOut, code := h.run("", "-init_hw_device", "vaapi=va:,vendor_id=0x8086,driver=iHD", "-i", "/media6/a b.mkv")
	if code != 3 || !strings.Contains(errOut, "args:-init_hw_device d3d11va=va:,vendor=0x8086 -i /media6/a b.mkv") {
		t.Fatalf("code %d stderr %q", code, errOut)
	}
}

func TestE2EHwaccelsAdvertisesVaapi(t *testing.T) {
	h := newHarness(t)
	out, _, code := h.run("", "-hwaccels")
	if code != 0 || !strings.Contains(out, "\nvaapi\n") || !strings.Contains(out, "qsv") {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestE2EStdinIsForwarded(t *testing.T) {
	h := newHarness(t)
	_, errOut, code := h.run("q\n", "-readq")
	if code != 0 || !strings.Contains(errOut, "stdin:q") {
		t.Fatalf("code %d stderr %q", code, errOut)
	}
}

func TestE2ERejectsUntranslatable(t *testing.T) {
	h := newHarness(t)
	_, errOut, code := h.run("", "-hwaccel", "vaapi", "-i", "x")
	if code != 1 || !strings.Contains(errOut, "Prefer OS native DXVA or VA-API hardware decoders") {
		t.Fatalf("code %d stderr %q", code, errOut)
	}
	if hl := h.agent.health(); hl.Status != "degraded" || hl.Problems[0] != ProblemTranslationRejected {
		t.Fatalf("health %v", hl.Problems)
	}
}

func TestE2EHwFailureIsReported(t *testing.T) {
	h := newHarness(t)
	_, errOut, code := h.run("", "-hwfail")
	if code != 1 || !strings.Contains(errOut, "MFX session") {
		t.Fatalf("code %d stderr %q", code, errOut)
	}
	hl := h.agent.health()
	if hl.Jobs.HwErrors15m != 1 || hl.LastError == nil || hl.LastError.Kind != "hw_error" {
		t.Fatalf("health %+v", hl.Jobs)
	}
}

func TestE2EWrongToken(t *testing.T) {
	h := newHarness(t)
	c := h.cmd("wrong-token-value", "-version")
	var e bytes.Buffer
	c.Stderr = &e
	if err := c.Run(); err == nil || !strings.Contains(e.String(), "authentication failed") {
		t.Fatalf("err %v stderr %q", err, e.String())
	}
	if hl := h.agent.health(); hl.Problems[0] != ProblemAuthFailed {
		t.Fatalf("problems %v", hl.Problems)
	}
}

func TestE2EPing(t *testing.T) {
	h := newHarness(t)
	out, _, code := h.run("", "-ffshim-ping")
	if code != 0 || !strings.HasPrefix(out, "ok: ffagent") {
		t.Fatalf("code %d out %q", code, out)
	}
	if hl := h.agent.health(); hl.Shim.LastPing == nil {
		t.Fatal("ping not recorded")
	}
}

// When Jellyfin kills the shim (seek, stop), ffmpeg on the host must die too.
func TestE2EKillingShimKillsFFmpeg(t *testing.T) {
	h := newHarness(t)
	c := h.cmd(h.token, "-sleep")
	stderr, _ := c.StderrPipe()
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, _ := stderr.Read(buf) // wait for "started"
	if !strings.Contains(string(buf[:n]), "started") {
		t.Fatalf("unexpected %q", buf[:n])
	}
	if hl := h.agent.health(); hl.Jobs.Active != 1 {
		t.Fatalf("active %d", hl.Jobs.Active)
	}
	_ = c.Process.Kill()
	_ = c.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		hl := h.agent.health()
		if hl.Jobs.Active == 0 && hl.Jobs.Cancelled == 1 {
			if hl.Status != "ok" {
				t.Fatalf("a cancelled job must not degrade status: %v", hl.Problems)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("ffmpeg was not killed: %+v", h.agent.health().Jobs)
}

func TestE2EConcurrentJobs(t *testing.T) {
	h := newHarness(t)
	done := make(chan string, 8)
	for i := 0; i < 8; i++ {
		go func() {
			out, _, _ := h.run("", "-version")
			done <- out
		}()
	}
	for i := 0; i < 8; i++ {
		if out := <-done; !strings.Contains(out, "8.1.3-Jellyfin") {
			t.Fatalf("got %q", out)
		}
	}
}

// With translate = none (Linux agent, NVIDIA on Windows) everything passes
// through untouched, including VA-API arguments, and -hwaccels is not altered.
func TestE2EPassThroughProfile(t *testing.T) {
	h := newHarnessWith(t, "none")
	_, errOut, code := h.run("", "-init_hw_device", "vaapi=va:/dev/dri/renderD128", "-hwaccel", "vaapi", "-i", "/media/x.mkv")
	if code != 3 || !strings.Contains(errOut, "args:-init_hw_device vaapi=va:/dev/dri/renderD128 -hwaccel vaapi -i /media/x.mkv") {
		t.Fatalf("code %d stderr %q", code, errOut)
	}
	out, _, _ := h.run("", "-hwaccels")
	if strings.Contains(out, "vaapi") {
		t.Fatalf("none profile must not inject vaapi: %q", out)
	}
}

// The installer's wait loop follows a real agent through its first start:
// release lookup, download, unpack, GPU test, ready.
func TestWaitForReadyFollowsFirstStart(t *testing.T) {
	g := newFakeGitHub(t, "8.1.3-2")
	dir := t.TempDir()
	cfg := &Config{Token: newToken(), GPU: GPUIntel}
	cfg.applyDefaults()
	a := NewAgent(cfg, dir, log.New(io.Discard, "", 0))
	a.ff.api = g.srv.URL
	a.ff.selftest = func(context.Context, string) error { time.Sleep(300 * time.Millisecond); return nil }

	jobLn, _ := net.Listen("tcp", "127.0.0.1:0")
	healthLn, _ := net.Listen("tcp", "127.0.0.1:0")
	cfg.HealthListen = healthLn.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = a.Serve(ctx, jobLn, healthLn) }()

	h := waitForReadyWithin(cfg, 10*time.Second, 50*time.Millisecond)
	if h == nil || h.FFmpeg.Version != "8.1.3-2" || h.Selftest.LastRun == nil || !h.Selftest.OK || h.Status != "ok" {
		t.Fatalf("not ready: %+v", h)
	}
}

// The real binary, started the way a double-click starts it (no arguments)
// and with a few other argument shapes, must never crash.
func TestBinaryArgumentShapes(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "ffagent")
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cases := []struct {
		args []string
		code int
	}{
		{nil, 0},
		{[]string{"help"}, 0},
		{[]string{"version"}, 0},
		{[]string{"--compose", "x.yml"}, 2},
		{[]string{"bogus"}, 2},
		{[]string{"status", "--bogus"}, 1},
	}
	for _, c := range cases {
		cmd := exec.Command(bin, c.args...)
		cmd.Env = append(os.Environ(), "FFAGENT_HOME="+t.TempDir())
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), "panic:") || code != c.code {
			t.Errorf("args %q: exit %d (want %d)\n%s", c.args, code, c.code, out)
		}
	}
}

// Info commands run ffmpeg once per executable; repeats come from memory, so
// tight timeouts such as IntroSkipper's 2 s for -version are no problem.
func TestE2EInfoCommandsAreCached(t *testing.T) {
	h := newHarness(t)
	countFile := h.agent.cfg.FFmpeg + ".count"
	for i := 0; i < 5; i++ {
		out, _, code := h.run("", "-version")
		if code != 0 || !strings.Contains(out, "8.1.3-Jellyfin") {
			t.Fatalf("run %d: code %d out %q", i, code, out)
		}
	}
	b, _ := os.ReadFile(countFile)
	if n := strings.Count(string(b), "run"); n != 1 {
		t.Fatalf("ffmpeg -version ran %d times, want 1", n)
	}
	// A real job is never cached.
	if _, _, code := h.run("", "-i", "/media/a.mkv"); code != 3 {
		t.Fatalf("real job: exit %d", code)
	}
}
