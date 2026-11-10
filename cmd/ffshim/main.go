// Command ffshim stands in for ffmpeg inside the Jellyfin container. It sends
// the command line to ffagent on the GPU machine and relays stdin, stdout,
// stderr and the exit code, so Jellyfin cannot tell the difference.
//
// Usage inside the container (Jellyfin calls it as /opt/ffshim/ffmpeg):
//
//	ffmpeg <ffmpeg arguments>   run ffmpeg on the agent's machine
//	ffmpeg -ffshim-ping         check the connection (used by the healthcheck)
//	ffmpeg -ffshim-version      print the shim version
//
// Used as the entrypoint of the ffshim image:
//
//	ffshim install /out         copy the shim to /out/ffmpeg and link ffprobe
//
// Environment:
//
//	FFSHIM_TOKEN            shared secret printed by `ffagent install` (required)
//	FFSHIM_AGENT            host:port of ffagent (default host.docker.internal:7878)
//	FFSHIM_FFMPEG_VERSION   override the detected jellyfin-ffmpeg version ("none" disables sync)
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"jellyfin-ffmpeg-bridge/internal/proto"
	"jellyfin-ffmpeg-bridge/internal/version"
)

var shimVersion = "dev" // set with -ldflags "-X main.shimVersion=..."

const (
	defaultAgent   = "host.docker.internal:7878"
	localFFprobe   = "/usr/lib/jellyfin-ffmpeg/ffprobe"
	dpkgStatusPath = "/var/lib/dpkg/status"
)

func main() {
	if filepath.Base(os.Args[0]) == "ffshim" && len(os.Args) == 3 && os.Args[1] == "install" {
		if err := install(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, "ffshim install:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 {
		switch os.Args[1] {
		case "-ffshim-version":
			fmt.Println("ffshim", shimVersion)
			return
		case "-ffshim-ping":
			os.Exit(run(proto.KindPing, nil))
		}
	}
	os.Exit(run(proto.KindRun, os.Args[1:]))
}

func fail(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "ffshim: "+format+"\n", a...)
	return 1
}

func run(kind string, args []string) int {
	token := os.Getenv("FFSHIM_TOKEN")
	if token == "" {
		return fail("FFSHIM_TOKEN is not set (run `ffagent token` on the agent machine)")
	}
	addr := os.Getenv("FFSHIM_AGENT")
	if addr == "" {
		addr = defaultAgent
	}

	conn, err := dial(addr)
	if err != nil {
		return fail("cannot reach ffagent at %s: %v (is it running? see `ffagent status` on the agent machine)", addr, err)
	}
	defer conn.Close()

	r := bufio.NewReaderSize(conn, 64*1024)
	w := proto.NewWriter(conn)

	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	t, nonce, err := proto.ReadFrame(r)
	if err != nil || t != proto.Challenge || len(nonce) != proto.NonceSize {
		return fail("bad handshake from %s: %v", addr, err)
	}
	_ = conn.SetReadDeadline(time.Time{})

	cwd, _ := os.Getwd()
	hello, _ := json.Marshal(proto.HelloMsg{
		Proto:         proto.Version,
		Kind:          kind,
		Args:          args,
		Cwd:           cwd,
		MAC:           proto.Sign(token, nonce),
		FFmpegVersion: localFFmpegVersion(),
		ShimVersion:   shimVersion,
	})
	if err := w.Send(proto.Hello, hello); err != nil {
		return fail("send: %v", err)
	}

	if kind == proto.KindRun {
		go pumpStdin(w)
	}

	for {
		t, p, err := proto.ReadFrame(r)
		if err != nil {
			return fail("lost connection to ffagent at %s: %v", addr, err)
		}
		switch t {
		case proto.Stdout:
			_, _ = os.Stdout.Write(p)
		case proto.Stderr:
			_, _ = os.Stderr.Write(p)
		case proto.Error:
			return fail("%s", p)
		case proto.Exit:
			code, err := proto.ParseExit(p)
			if err != nil {
				return fail("%v", err)
			}
			return code
		}
	}
}

func dial(addr string) (net.Conn, error) {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Second)
		}
		var c net.Conn
		c, err = net.DialTimeout("tcp", addr, 5*time.Second)
		if err == nil {
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetNoDelay(true)
				_ = tc.SetKeepAlive(true)
				_ = tc.SetKeepAlivePeriod(15 * time.Second)
			}
			return c, nil
		}
	}
	return nil, err
}

// pumpStdin forwards stdin (Jellyfin writes "q" here to stop ffmpeg).
func pumpStdin(w *proto.Writer) {
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			if w.Send(proto.Stdin, buf[:n]) != nil {
				return
			}
		}
		if err != nil {
			_ = w.Send(proto.StdinEOF, nil)
			return
		}
	}
}

// localFFmpegVersion returns the jellyfin-ffmpeg version shipped in this
// Jellyfin image, so the agent can install the same version on Windows.
func localFFmpegVersion() string {
	if v := os.Getenv("FFSHIM_FFMPEG_VERSION"); v != "" {
		if v == "none" {
			return ""
		}
		return version.Normalize(v)
	}
	f, err := os.Open(dpkgStatusPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	return version.FromDpkgStatus(f)
}

// install copies this binary to dir/ffmpeg and points dir/ffprobe at the
// container's own ffprobe (probing needs no GPU, so it stays local). If the
// Linux agent binary sits next to the shim (as in the image), it is copied to
// dir/ffagent too, for running the agent on a Linux GPU machine.
func install(dir string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := copyExecutable(self, filepath.Join(dir, "ffmpeg")); err != nil {
		return err
	}

	link := filepath.Join(dir, "ffprobe")
	if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Symlink(localFFprobe, link); err != nil {
		return err
	}
	fmt.Printf("ffshim %s installed to %s (ffprobe -> %s)\n", shimVersion, dir, localFFprobe)

	agent := filepath.Join(filepath.Dir(self), "ffagent")
	if _, err := os.Stat(agent); err == nil {
		if err := copyExecutable(agent, filepath.Join(dir, "ffagent")); err != nil {
			return err
		}
		fmt.Printf("ffagent (Linux) installed to %s\n", dir)
	}
	return nil
}

// copyExecutable copies src to dst atomically with mode 0755.
func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp")
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
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
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
