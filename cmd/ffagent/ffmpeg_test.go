package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGitHub serves a jellyfin-ffmpeg-like release API with real zip files.
type fakeGitHub struct {
	srv       *httptest.Server
	downloads atomic.Int32
}

func zipWithFFmpeg(t *testing.T, nested bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	prefix := ""
	if nested {
		prefix = "jellyfin-ffmpeg/"
	}
	for _, name := range []string{prefix + "ffmpeg.exe", prefix + "ffprobe.exe"} {
		w, _ := zw.Create(name)
		w.Write([]byte("MZ fake " + name))
	}
	zw.Close()
	return buf.Bytes()
}

func newFakeGitHub(t *testing.T, versions ...string) *fakeGitHub {
	g := &fakeGitHub{}
	mux := http.NewServeMux()
	var releases []ghRelease
	for i, v := range versions {
		name := "jellyfin-ffmpeg_" + v + "_portable_" + windowsArch() + "-clang-gpl.zip"
		releases = append(releases, ghRelease{
			TagName: "v" + v,
			Assets: []ghAsset{
				{Name: "jellyfin-ffmpeg_" + v + "_portable_winarm64-clang-gpl.zip", URL: "/nope"},
				{Name: name, URL: "/dl/" + name},
			},
		})
		data := zipWithFFmpeg(t, i%2 == 1)
		mux.HandleFunc("/dl/"+name, func(w http.ResponseWriter, _ *http.Request) {
			g.downloads.Add(1)
			w.Write(data)
		})
	}
	mux.HandleFunc("/releases", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(releases)
	})
	mux.HandleFunc("/releases/tags/{tag}", func(w http.ResponseWriter, r *http.Request) {
		for _, rel := range releases {
			if rel.TagName == r.PathValue("tag") {
				json.NewEncoder(w).Encode(rel)
				return
			}
		}
		http.NotFound(w, r)
	})
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	// Asset URLs are relative in the fixtures; make them absolute.
	for i := range releases {
		for j := range releases[i].Assets {
			releases[i].Assets[j].URL = g.srv.URL + releases[i].Assets[j].URL
		}
	}
	return g
}

func newTestFFmpeg(t *testing.T, g *fakeGitHub, dir string) *FFmpeg {
	f := NewFFmpeg(dir, "", GPUIntel, defaultDevice, log.New(io.Discard, "", 0))
	f.api = g.srv.URL
	f.selftest = func(context.Context, string) error { return nil }
	return f
}

func TestSyncInstallsExpectedVersion(t *testing.T) {
	g := newFakeGitHub(t, "8.1.3-2", "8.1.3-1", "8.1.2-5")
	dir := t.TempDir()
	f := newTestFFmpeg(t, g, dir)

	f.SetExpected("8.1.3-1-bookworm")
	exe, ver, err := f.Ready(context.Background())
	if err != nil || ver != "8.1.3-1" || !strings.HasSuffix(exe, filepath.Join("8.1.3-1", "ffmpeg.exe")) {
		t.Fatalf("got %q %q %v", exe, ver, err)
	}
	if _, err := os.Stat(exe); err != nil {
		t.Fatal(err)
	}
	if st := f.Status(); !st.InSync || st.Syncing {
		t.Fatalf("status %+v", st)
	}

	// Survives a restart via current.txt, no new download.
	f2 := newTestFFmpeg(t, g, dir)
	if e, v := f2.Exe(); v != "8.1.3-1" || e != exe {
		t.Fatalf("after restart: %q %q", e, v)
	}
	if f2.StartSync() != nil {
		t.Fatal("no sync expected when already current")
	}

	// Jellyfin image updated: switches version.
	f2.SetExpected("8.1.3-2")
	if _, v, err := f2.Ready(context.Background()); err != nil || v != "8.1.3-2" {
		t.Fatalf("upgrade: %q %v", v, err)
	}
	if g.downloads.Load() != 2 {
		t.Fatalf("downloads %d", g.downloads.Load())
	}
}

func TestSyncLatestWhenNothingKnown(t *testing.T) {
	g := newFakeGitHub(t, "8.1.3-2", "8.1.3-1")
	f := newTestFFmpeg(t, g, t.TempDir())
	if _, v, err := f.Ready(context.Background()); err != nil || v != "8.1.3-2" {
		t.Fatalf("got %q %v", v, err)
	}
}

func TestSyncFailureKeepsWorkingVersion(t *testing.T) {
	g := newFakeGitHub(t, "8.1.3-2", "8.1.3-1")
	f := newTestFFmpeg(t, g, t.TempDir())
	f.SetExpected("8.1.3-1")
	if _, _, err := f.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}

	f.selftest = func(context.Context, string) error { return errors.New("MFX session failed") }
	f.SetExpected("8.1.3-2")
	_, v, err := f.Ready(context.Background())
	if err != nil || v != "8.1.3-1" {
		t.Fatalf("should keep 8.1.3-1, got %q %v", v, err)
	}
	st := f.Status()
	if st.InSync || st.LastSyncError == "" || !strings.Contains(st.LastSyncError, "self-test") {
		t.Fatalf("status %+v", st)
	}
	// Backoff: no immediate retry for the same version.
	if f.StartSync() != nil {
		t.Fatal("expected retry backoff")
	}
	f.failedAt = time.Now().Add(-2 * syncRetryBackoff)
	ch := f.StartSync()
	if ch == nil {
		t.Fatal("expected retry after backoff")
	}
	<-ch // let the retry finish before the temp dir is removed
}

func TestSyncUnknownVersionWithoutFFmpeg(t *testing.T) {
	g := newFakeGitHub(t, "8.1.3-2")
	f := newTestFFmpeg(t, g, t.TempDir())
	f.SetExpected("9.0.0-1")
	if _, _, err := f.Ready(context.Background()); err == nil || !strings.Contains(err.Error(), "no ffmpeg installed") {
		t.Fatalf("got %v", err)
	}
	if st := f.Status(); st.Path != "" || st.LastSyncError == "" {
		t.Fatalf("status %+v", st)
	}
}

func TestFixedFFmpegDisablesSync(t *testing.T) {
	f := NewFFmpeg(t.TempDir(), `C:\tools\ffmpeg.exe`, GPUIntel, defaultDevice, log.New(io.Discard, "", 0))
	f.SetExpected("8.1.3-2")
	if f.StartSync() != nil {
		t.Fatal("fixed ffmpeg must not sync")
	}
	if exe, _, err := f.Ready(context.Background()); err != nil || exe != `C:\tools\ffmpeg.exe` {
		t.Fatalf("got %q %v", exe, err)
	}
}

func TestUnzipRejectsTraversal(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("../evil.exe")
	w.Write([]byte("x"))
	zw.Close()
	dir := t.TempDir()
	archive := filepath.Join(dir, "a.zip")
	os.WriteFile(archive, buf.Bytes(), 0o644)
	if _, err := unzip(archive, filepath.Join(dir, "out")); err == nil {
		t.Fatal("path traversal accepted")
	}
}

// TestLiveGitHubRelease checks the real jellyfin-ffmpeg release layout.
// Run with: FFAGENT_LIVE=1 go test -run Live ./cmd/ffagent
func TestLiveGitHubRelease(t *testing.T) {
	if os.Getenv("FFAGENT_LIVE") == "" {
		t.Skip("set FFAGENT_LIVE=1 to query api.github.com")
	}
	f := NewFFmpeg(t.TempDir(), "", GPUIntel, defaultDevice, log.New(io.Discard, "", 0))
	ver, asset, err := f.findRelease(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("latest: %s -> %s", ver, asset.Name)
	exact, asset2, err := f.findRelease(context.Background(), ver)
	if err != nil || exact != ver || asset2.Name != asset.Name {
		t.Fatalf("exact lookup: %q %q %v", exact, asset2.Name, err)
	}
	base := strings.SplitN(ver, "-", 2)[0]
	if v, _, err := f.findRelease(context.Background(), base); err != nil || !strings.HasPrefix(v, base+"-") {
		t.Fatalf("prefix lookup %q: %q %v", base, v, err)
	}
}

func TestInstallReportsProgress(t *testing.T) {
	g := newFakeGitHub(t, "8.1.3-2")
	f := newTestFFmpeg(t, g, t.TempDir())
	var phases []string
	f.selftest = func(context.Context, string) error {
		phases = append(phases, f.Status().Download.Phase)
		return nil
	}
	if _, _, err := f.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(phases) != 1 || phases[0] != "testing" {
		t.Fatalf("phase during self-test: %v", phases)
	}
	if f.Status().Download != nil {
		t.Fatal("progress must be cleared after the install")
	}
}

func TestDescribeDownload(t *testing.T) {
	got := describeDownload(&DownloadStatus{Version: "8.1.3-2", Phase: "downloading", Bytes: 45 << 20, Total: 90 << 20})
	if got != "downloading jellyfin-ffmpeg 8.1.3-2: 45 / 90 MB (50%)" {
		t.Fatalf("got %q", got)
	}
}
