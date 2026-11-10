package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"jellyfin-ffmpeg-bridge/internal/version"
)

const (
	githubAPI        = "https://api.github.com/repos/jellyfin/jellyfin-ffmpeg"
	syncWaitTimeout  = 3 * time.Minute // how long a job waits for a version switch
	syncRetryBackoff = time.Hour       // after a failed sync for the same version
	keepVersions     = 2               // current + one fallback
	ffmpegExeName    = "ffmpeg.exe"
)

// FFmpeg manages jellyfin-ffmpeg builds under <data>\ffmpeg\<version>\ and
// keeps them in sync with the version shipped in the Jellyfin image.
type FFmpeg struct {
	dir      string // <data>\ffmpeg
	fixed    string // config "ffmpeg = ..." disables management
	log      *log.Logger
	api      string
	client   *http.Client
	fixedVer string // version of the fixed ffmpeg, or "custom"
	selftest func(ctx context.Context, exe string) error
	onTested func(err error)

	mu          sync.Mutex
	current     string // version
	currentExe  string
	expected    string
	syncing     chan struct{}
	lastSync    time.Time
	lastSyncErr error
	failedFor   string
	failedAt    time.Time
}

// NewFFmpeg creates the manager. gpu and device select the self-test.
func NewFFmpeg(dataDir, fixed, gpu, device string, logger *log.Logger) *FFmpeg {
	f := &FFmpeg{
		dir:      filepath.Join(dataDir, "ffmpeg"),
		fixed:    fixed,
		fixedVer: detectFixedVersion(fixed),
		log:      logger,
		api:      githubAPI,
		client:   &http.Client{Timeout: 10 * time.Minute},
		selftest: func(ctx context.Context, exe string) error { return runSelftest(ctx, exe, gpu, device) },
	}
	f.loadCurrent()
	return f
}

func (f *FFmpeg) currentFile() string { return filepath.Join(f.dir, "current.txt") }

func (f *FFmpeg) loadCurrent() {
	if f.fixed != "" {
		return
	}
	b, err := os.ReadFile(f.currentFile())
	if err != nil {
		return
	}
	v := strings.TrimSpace(string(b))
	exe := filepath.Join(f.dir, v, ffmpegExeName)
	if _, err := os.Stat(exe); err == nil {
		f.current, f.currentExe = v, exe
	}
}

// Exe returns the ffmpeg executable to use right now ("" if none yet).
func (f *FFmpeg) Exe() (exe, ver string) {
	if f.fixed != "" {
		return f.fixed, f.fixedVer
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.currentExe, f.current
}

// SetExpected records the version the Jellyfin image ships.
func (f *FFmpeg) SetExpected(v string) {
	v = version.Normalize(v)
	if v == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if v != f.expected {
		here := f.current
		if f.fixed != "" {
			here = f.fixedVer
		}
		f.log.Printf("jellyfin image ships jellyfin-ffmpeg %s (here: %s)", v, orNone(here))
		f.expected = v
	}
}

// Ready returns a usable ffmpeg, waiting up to syncWaitTimeout for a
// pending version switch so Jellyfin probes the right version.
func (f *FFmpeg) Ready(ctx context.Context) (exe, ver string, err error) {
	if f.fixed != "" {
		return f.fixed, f.fixedVer, nil
	}
	if ch := f.StartSync(); ch != nil {
		ctx, cancel := context.WithTimeout(ctx, syncWaitTimeout)
		defer cancel()
		select {
		case <-ch:
		case <-ctx.Done():
			f.log.Printf("ffmpeg sync still running after %s, using installed version", syncWaitTimeout)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.currentExe == "" {
		msg := "no ffmpeg installed yet"
		if f.lastSyncErr != nil {
			msg += ": " + f.lastSyncErr.Error()
		}
		return "", "", errors.New(msg)
	}
	return f.currentExe, f.current, nil
}

// StartSync starts a background sync if one is needed and returns a channel
// closed when it finishes, or nil if nothing needs doing.
func (f *FFmpeg) StartSync() <-chan struct{} {
	if f.fixed != "" {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.syncing != nil {
		return f.syncing
	}
	target := f.expected
	needed := f.currentExe == "" || (target != "" && !version.Matches(f.current, target))
	if !needed {
		return nil
	}
	if target == f.failedFor && time.Since(f.failedAt) < syncRetryBackoff && f.currentExe != "" {
		return nil
	}
	ch := make(chan struct{})
	f.syncing = ch
	go f.sync(target, ch)
	return ch
}

func (f *FFmpeg) sync(target string, done chan struct{}) {
	f.log.Printf("installing jellyfin-ffmpeg %s", orLatest(target))
	ver, exe, err := f.install(context.Background(), target)
	if err == nil {
		// Before signalling done, so nobody waiting on the sync (or a test's
		// temp dir cleanup) races with the removal of old versions.
		f.cleanup(ver)
	}

	f.mu.Lock()
	if err != nil {
		f.lastSyncErr = err
		f.failedFor, f.failedAt = target, time.Now()
		f.log.Printf("ffmpeg sync failed: %v", err)
	} else {
		f.current, f.currentExe = ver, exe
		f.lastSync, f.lastSyncErr, f.failedFor = time.Now(), nil, ""
		_ = os.WriteFile(f.currentFile(), []byte(ver), 0o644)
		f.log.Printf("now using jellyfin-ffmpeg %s", ver)
	}
	f.syncing = nil
	f.mu.Unlock()
	close(done)
}

type ghRelease struct {
	TagName    string    `json:"tag_name"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	Assets     []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

func windowsArch() string {
	if runtime.GOARCH == "arm64" {
		return "winarm64"
	}
	return "win64"
}

// findRelease picks the release for target ("8.1.3-2", "8.1.3" or "" for latest).
func (f *FFmpeg) findRelease(ctx context.Context, target string) (string, ghAsset, error) {
	var releases []ghRelease
	if strings.Contains(target, "-") {
		var r ghRelease
		if err := f.getJSON(ctx, f.api+"/releases/tags/v"+target, &r); err != nil {
			return "", ghAsset{}, err
		}
		releases = []ghRelease{r}
	} else if err := f.getJSON(ctx, f.api+"/releases?per_page=50", &releases); err != nil {
		return "", ghAsset{}, err
	}
	for _, r := range releases {
		if r.Draft || r.Prerelease {
			continue
		}
		v := version.Normalize(r.TagName)
		if target != "" && !version.Matches(v, target) {
			continue
		}
		if a, ok := pickAsset(r.Assets); ok {
			return v, a, nil
		}
	}
	return "", ghAsset{}, fmt.Errorf("no jellyfin-ffmpeg release with a portable %s build for %s", windowsArch(), orLatest(target))
}

func pickAsset(assets []ghAsset) (ghAsset, bool) {
	marker := "_portable_" + windowsArch() + "-"
	var best ghAsset
	for _, a := range assets {
		if !strings.Contains(a.Name, marker) || !strings.HasSuffix(a.Name, ".zip") {
			continue
		}
		if best.Name == "" || strings.Contains(a.Name, "-gpl") {
			best = a
		}
	}
	return best, best.Name != ""
}

func (f *FFmpeg) getJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ffagent")
	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// install downloads, unpacks and self-tests a release; returns version and exe.
func (f *FFmpeg) install(ctx context.Context, target string) (string, string, error) {
	ver, asset, err := f.findRelease(ctx, target)
	if err != nil {
		return "", "", err
	}
	dest := filepath.Join(f.dir, ver)
	exe := filepath.Join(dest, ffmpegExeName)
	if _, err := os.Stat(exe); err != nil {
		if err := os.MkdirAll(f.dir, 0o755); err != nil {
			return "", "", err
		}
		zipPath := filepath.Join(f.dir, asset.Name+".download")
		defer os.Remove(zipPath)
		if err := f.download(ctx, asset.URL, zipPath); err != nil {
			return "", "", fmt.Errorf("download %s: %w", asset.Name, err)
		}
		partial := dest + ".partial"
		_ = os.RemoveAll(partial)
		found, err := unzip(zipPath, partial)
		if err != nil {
			_ = os.RemoveAll(partial)
			return "", "", fmt.Errorf("unpack %s: %w", asset.Name, err)
		}
		_ = os.RemoveAll(dest)
		if err := os.Rename(filepath.Dir(found), dest); err != nil {
			_ = os.RemoveAll(partial)
			return "", "", err
		}
		_ = os.RemoveAll(partial)
	}

	tctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	err = f.selftest(tctx, exe)
	if f.onTested != nil {
		f.onTested(err)
	}
	if err != nil {
		return "", "", fmt.Errorf("jellyfin-ffmpeg %s failed the GPU self-test, keeping the previous version: %w", ver, err)
	}
	return ver, exe, nil
}

func (f *FFmpeg) download(ctx context.Context, url, to string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "ffagent")
	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// unzip extracts archive into dir and returns the path of ffmpeg.exe.
func unzip(archive, dir string) (string, error) {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return "", err
	}
	defer zr.Close()
	found := ""
	root := filepath.Clean(dir) + string(os.PathSeparator)
	for _, zf := range zr.File {
		target := filepath.Join(dir, filepath.FromSlash(zf.Name))
		if !strings.HasPrefix(target, root) {
			return "", fmt.Errorf("unsafe path in archive: %s", zf.Name)
		}
		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		if err := extractFile(zf, target); err != nil {
			return "", err
		}
		if strings.EqualFold(filepath.Base(target), ffmpegExeName) {
			found = target
		}
	}
	if found == "" {
		return "", fmt.Errorf("%s not found in archive", ffmpegExeName)
	}
	return found, nil
}

func extractFile(zf *zip.File, target string) error {
	rc, err := zf.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// cleanup removes all but the newest keepVersions installs. Folders still in
// use by a running ffmpeg cannot be deleted on Windows; that is fine.
func (f *FFmpeg) cleanup(keep string) {
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		return
	}
	type inst struct {
		name string
		mod  time.Time
	}
	var all []inst
	for _, e := range entries {
		if !e.IsDir() || strings.HasSuffix(e.Name(), ".partial") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		all = append(all, inst{e.Name(), info.ModTime()})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].mod.After(all[j].mod) })
	kept := 0
	for _, in := range all {
		if in.name == keep || kept < keepVersions-1 {
			if in.name != keep {
				kept++
			}
			continue
		}
		if err := os.RemoveAll(filepath.Join(f.dir, in.name)); err == nil {
			f.log.Printf("removed old jellyfin-ffmpeg %s", in.name)
		}
	}
}

// Status reports the manager state for /health.
func (f *FFmpeg) Status() FFmpegStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fixed != "" {
		return FFmpegStatus{Version: f.fixedVer, Expected: f.expected, Managed: false, Path: f.fixed}.withSync()
	}
	s := FFmpegStatus{
		Version:  f.current,
		Expected: f.expected,
		Managed:  true,
		Syncing:  f.syncing != nil,
		Path:     f.currentExe,
		LastSync: timePtr(f.lastSync),
	}
	if f.lastSyncErr != nil {
		s.LastSyncError = f.lastSyncErr.Error()
	}
	return s.withSync()
}

// selftestArgs encodes a short 720p test clip on the GPU, using the device
// chain Jellyfin itself needs (for Intel including OpenCL, used for HDR tone
// mapping). It returns nil when there is nothing to test.
func selftestArgs(gpu, device string, windows bool) []string {
	var dev []string
	var vf, enc string
	switch {
	case gpu == GPUIntel && windows:
		dev = []string{
			"-init_hw_device", "d3d11va=dx11:,vendor=0x8086",
			"-init_hw_device", "qsv=qs@dx11",
			"-init_hw_device", "opencl=ocl@dx11",
			"-filter_hw_device", "qs",
		}
		vf, enc = "vpp_qsv=w=640:h=360", "h264_qsv"
	case gpu == GPUIntel:
		dev = []string{
			"-init_hw_device", "vaapi=va:" + device + ",driver=iHD",
			"-init_hw_device", "qsv=qs@va",
			"-init_hw_device", "opencl=ocl@va",
			"-filter_hw_device", "qs",
		}
		vf, enc = "vpp_qsv=w=640:h=360", "h264_qsv"
	case gpu == GPUVaapi:
		dev = []string{"-init_hw_device", "vaapi=va:" + device, "-filter_hw_device", "va"}
		vf, enc = "scale_vaapi=w=640:h=360", "h264_vaapi"
	case gpu == GPUNvidia:
		dev = []string{"-init_hw_device", "cuda=cu:0", "-filter_hw_device", "cu"}
		vf, enc = "scale_cuda=w=640:h=360", "h264_nvenc"
	default:
		return nil
	}
	args := append([]string{"-hide_banner", "-nostdin", "-v", "error"}, dev...)
	return append(args,
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=25:duration=1",
		"-vf", "format=nv12,hwupload=extra_hw_frames=16,"+vf,
		"-c:v", enc, "-f", "null", "-",
	)
}

func runSelftest(ctx context.Context, exe, gpu, device string) error {
	args := selftestArgs(gpu, device, isWindows)
	if args == nil {
		return nil
	}
	cmd := exec.CommandContext(ctx, exe, args...)
	setProcAttr(cmd)
	tail := newTail(4096)
	cmd.Stdout = io.Discard
	cmd.Stderr = tail
	if err := cmd.Run(); err != nil {
		msg := lastLines(tail.String(), 3)
		if msg == "" {
			msg = err.Error()
		}
		return errors.New(msg)
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func orLatest(s string) string {
	if s == "" {
		return "(latest)"
	}
	return s
}

// Linux: the jellyfin-ffmpeg that ships in the official Jellyfin image.
const jellyfinImageFFmpeg = "/usr/lib/jellyfin-ffmpeg/ffmpeg"

var dpkgStatusPath = "/var/lib/dpkg/status"

// detectFixedVersion returns the jellyfin-ffmpeg package version when the
// fixed ffmpeg is the one installed by the Debian package (as in the Jellyfin
// image), so version mismatches can be reported. Otherwise "custom".
func detectFixedVersion(exe string) string {
	if exe == "" {
		return ""
	}
	if strings.HasPrefix(filepath.ToSlash(exe), "/usr/lib/jellyfin-ffmpeg/") {
		if f, err := os.Open(dpkgStatusPath); err == nil {
			defer f.Close()
			if v := version.FromDpkgStatus(f); v != "" {
				return v
			}
		}
	}
	return "custom"
}

// defaultFFmpeg picks the ffmpeg when the config doesn't name one: managed
// downloads on Windows, the Jellyfin image's jellyfin-ffmpeg elsewhere.
func defaultFFmpeg() (string, error) {
	if isWindows {
		return "", nil
	}
	if _, err := os.Stat(jellyfinImageFFmpeg); err == nil {
		return jellyfinImageFFmpeg, nil
	}
	return "", fmt.Errorf("no ffmpeg configured and %s not found; set ffmpeg = /path/to/ffmpeg (or FFAGENT_FFMPEG), "+
		"or run the agent in the jellyfin/jellyfin image (see docs/linux-agent.md)", jellyfinImageFFmpeg)
}
