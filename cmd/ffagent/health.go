package main

import (
	"strings"
	"sync"
	"time"

	"jellyfin-ffmpeg-bridge/internal/version"
)

// Problem codes reported in /health. See docs/home-assistant.md.
const (
	ProblemNoFFmpeg            = "no_ffmpeg"
	ProblemSelftestFailed      = "selftest_failed"
	ProblemTranslationRejected = "translation_rejected"
	ProblemHwErrors            = "hw_errors"
	ProblemAuthFailed          = "auth_failed"
	ProblemShimNotSeen         = "shim_not_seen"
	ProblemVersionMismatch     = "version_mismatch"
	ProblemVersionSyncFailed   = "version_sync_failed"
	ProblemRestartJellyfin     = "restart_jellyfin"
	ProblemPathLinks           = "path_links"
)

const (
	problemWindow   = 15 * time.Minute // how long a rejected/failed job keeps status degraded
	shimSilentAfter = 15 * time.Minute // heartbeat runs every 5 minutes
)

// hwErrorMarkers identify ffmpeg failures caused by the GPU/driver or by an
// impossible filter graph, as opposed to e.g. a corrupt input file.
var hwErrorMarkers = []string{
	"failed to initialise",
	"failed to initialize",
	"device creation failed",
	"error creating a mfx session",
	"mfx_err",
	"error initializing output stream",
	"error while opening encoder",
	"no device available",
	"failed to create",
	"hwaccel initialisation returned error",
	"impossible to convert between the formats",
	"error reinitializing filters",
	"failed to inject frame into filter network",
	// NVIDIA
	"openencodesessionex failed",
	"no capable devices found",
	"cannot load libcuda",
	"cuda_error",
	"cuinit",
	"no nvenc capable devices",
}

// ErrorInfo is the most recent error, shown in Home Assistant notifications.
type ErrorInfo struct {
	Time    time.Time `json:"time"`
	Kind    string    `json:"kind"`
	Message string    `json:"message"`
}

// PathStatus describes one mirrored container path.
type PathStatus struct {
	Container string `json:"container"`
	Link      string `json:"link"`
	Target    string `json:"target"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
}

// FFmpegStatus is filled in by the ffmpeg manager.
type FFmpegStatus struct {
	Version       string     `json:"version"`
	Expected      string     `json:"expected"`
	InSync        bool       `json:"in_sync"`
	Managed       bool       `json:"managed"`
	Syncing       bool       `json:"syncing"`
	Path          string     `json:"path"`
	LastSync      *time.Time `json:"last_sync"`
	LastSyncError string     `json:"last_sync_error"`
}

// SelftestStatus is the result of the periodic GPU self-test.
type SelftestStatus struct {
	OK      bool       `json:"ok"`
	LastRun *time.Time `json:"last_run"`
	Error   string     `json:"error"`
}

// Health is the JSON document served on /health.
type Health struct {
	Status       string         `json:"status"`
	Problems     []string       `json:"problems"`
	AgentVersion string         `json:"agent_version"`
	UptimeS      int64          `json:"uptime_s"`
	FFmpeg       FFmpegStatus   `json:"ffmpeg"`
	Selftest     SelftestStatus `json:"selftest"`
	Shim         ShimStatus     `json:"shim"`
	Jobs         JobStats       `json:"jobs"`
	Paths        []PathStatus   `json:"paths"`
	LastError    *ErrorInfo     `json:"last_error"`
}

// ShimStatus tells when the Jellyfin container last talked to the agent.
type ShimStatus struct {
	LastSeen     *time.Time `json:"last_seen"`
	LastPing     *time.Time `json:"last_ping"`
	Version      string     `json:"version"`
	ProbeVersion string     `json:"jellyfin_probed_ffmpeg"`
}

// JobStats are ffmpeg job counters.
type JobStats struct {
	Active      int `json:"active"`
	Total       int `json:"total"`
	Cancelled   int `json:"cancelled"`
	Failed15m   int `json:"failed_15m"`
	Rejected15m int `json:"rejected_15m"`
	HwErrors15m int `json:"hw_errors_15m"`
}

// State collects everything /health reports. Safe for concurrent use.
type State struct {
	mu        sync.Mutex
	started   time.Time
	active    int
	total     int
	cancelled int
	failed    []time.Time
	rejected  []time.Time
	hwErrors  []time.Time
	authFails []time.Time
	lastErr   *ErrorInfo
	shimSeen  time.Time
	shimPing  time.Time
	shimVer   string
	probeVer  string
	selftest  SelftestStatus
	paths     []PathStatus
}

func NewState(now time.Time) *State { return &State{started: now} }

func (s *State) setError(now time.Time, kind, msg string) {
	s.lastErr = &ErrorInfo{Time: now, Kind: kind, Message: msg}
}

func (s *State) SawShim(now time.Time, kind, shimVersion string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shimSeen = now
	if kind == "ping" {
		s.shimPing = now
	}
	s.shimVer = shimVersion
}

func (s *State) AuthFailed(now time.Time, remote string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.authFails = appendRecent(s.authFails, now)
	s.setError(now, "auth", "connection from "+remote+" used the wrong FFSHIM_TOKEN")
}

func (s *State) Rejected(now time.Time, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejected = appendRecent(s.rejected, now)
	s.setError(now, "rejected", msg)
}

func (s *State) JobError(now time.Time, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed = appendRecent(s.failed, now)
	s.setError(now, "failed", msg)
}

func (s *State) Probed(ffmpegVersion string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.probeVer = ffmpegVersion
}

func (s *State) JobStarted() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active++
	s.total++
}

// JobFinished classifies a finished job. stderrTail is the end of ffmpeg's
// stderr, used to tell GPU problems from bad input files.
func (s *State) JobFinished(now time.Time, exitCode int, cancelled bool, stderrTail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active--
	switch {
	case cancelled:
		s.cancelled++
	case exitCode != 0:
		s.failed = appendRecent(s.failed, now)
		msg := lastLines(stderrTail, 3)
		if isHwError(stderrTail) {
			s.hwErrors = appendRecent(s.hwErrors, now)
			s.setError(now, "hw_error", msg)
		} else {
			s.setError(now, "failed", msg)
		}
	}
}

func (s *State) SetSelftest(now time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := now
	s.selftest = SelftestStatus{OK: err == nil, LastRun: &t}
	if err != nil {
		s.selftest.Error = err.Error()
		s.setError(now, "selftest", err.Error())
	}
}

func (s *State) SetPaths(p []PathStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paths = p
}

// Snapshot builds the /health document.
func (s *State) Snapshot(now time.Time, ff FFmpegStatus, agentVersion string) Health {
	s.mu.Lock()
	defer s.mu.Unlock()

	h := Health{
		AgentVersion: agentVersion,
		UptimeS:      int64(now.Sub(s.started).Seconds()),
		FFmpeg:       ff,
		Selftest:     s.selftest,
		Shim: ShimStatus{
			LastSeen:     timePtr(s.shimSeen),
			LastPing:     timePtr(s.shimPing),
			Version:      s.shimVer,
			ProbeVersion: s.probeVer,
		},
		Jobs: JobStats{
			Active:      s.active,
			Total:       s.total,
			Cancelled:   s.cancelled,
			Failed15m:   countSince(s.failed, now.Add(-problemWindow)),
			Rejected15m: countSince(s.rejected, now.Add(-problemWindow)),
			HwErrors15m: countSince(s.hwErrors, now.Add(-problemWindow)),
		},
		Paths:     s.paths,
		LastError: s.lastErr,
		Problems:  []string{},
	}

	add := func(p string) { h.Problems = append(h.Problems, p) }
	if ff.Path == "" {
		add(ProblemNoFFmpeg)
	}
	if s.selftest.LastRun != nil && !s.selftest.OK {
		add(ProblemSelftestFailed)
	}
	if h.Jobs.Rejected15m > 0 {
		add(ProblemTranslationRejected)
	}
	if h.Jobs.HwErrors15m > 0 {
		add(ProblemHwErrors)
	}
	if countSince(s.authFails, now.Add(-problemWindow)) > 0 {
		add(ProblemAuthFailed)
	}
	if !s.shimPing.IsZero() && now.Sub(s.shimPing) > shimSilentAfter {
		add(ProblemShimNotSeen)
	}
	// A fixed ffmpeg of unknown version ("custom") cannot be compared.
	if ff.Expected != "" && ff.Version != "" && ff.Version != "custom" && !ff.InSync {
		if ff.Managed && ff.LastSyncError != "" && !ff.Syncing {
			add(ProblemVersionSyncFailed)
		} else {
			add(ProblemVersionMismatch)
		}
	}
	if s.probeVer != "" && ff.Version != "" && s.probeVer != ff.Version {
		add(ProblemRestartJellyfin)
	}
	for _, p := range s.paths {
		if !p.OK {
			add(ProblemPathLinks)
			break
		}
	}

	h.Status = "ok"
	if len(h.Problems) > 0 {
		h.Status = "degraded"
	}
	return h
}

func (f FFmpegStatus) withSync() FFmpegStatus {
	f.InSync = f.Expected == "" || version.Matches(f.Version, f.Expected)
	return f
}

func isHwError(stderr string) bool {
	l := strings.ToLower(stderr)
	for _, m := range hwErrorMarkers {
		if strings.Contains(l, m) {
			return true
		}
	}
	return false
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(s, "\r", "")), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

func appendRecent(ts []time.Time, now time.Time) []time.Time {
	ts = append(ts, now)
	if len(ts) > 200 {
		ts = ts[len(ts)-200:]
	}
	return ts
}

func countSince(ts []time.Time, since time.Time) int {
	n := 0
	for _, t := range ts {
		if t.After(since) {
			n++
		}
	}
	return n
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func newTail(max int) *tailBuffer { return &tailBuffer{max: max} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
