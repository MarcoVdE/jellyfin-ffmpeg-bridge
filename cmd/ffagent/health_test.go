package main

import (
	"errors"
	"slices"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

func okFF() FFmpegStatus {
	return FFmpegStatus{Version: "8.1.3-2", Expected: "8.1.3-2", Managed: true, Path: `C:\x\ffmpeg.exe`}.withSync()
}

func TestHealthyByDefault(t *testing.T) {
	s := NewState(t0)
	s.SetSelftest(t0, nil)
	h := s.Snapshot(t0.Add(time.Minute), okFF(), "1.0.0")
	if h.Status != "ok" || len(h.Problems) != 0 {
		t.Fatalf("expected ok, got %s %v", h.Status, h.Problems)
	}
	if h.UptimeS != 60 {
		t.Fatalf("uptime %d", h.UptimeS)
	}
}

func TestProblemsAndExpiry(t *testing.T) {
	s := NewState(t0)
	s.Rejected(t0, "cannot run on Windows")
	s.JobStarted()
	s.JobFinished(t0, 1, false, "line\n[h264_qsv @ 0x1] Error creating a MFX session: -9.\nConversion failed!")
	s.JobStarted()
	s.JobFinished(t0, 255, true, "") // cancelled by Jellyfin: not a failure
	s.JobStarted()
	s.JobFinished(t0, 1, false, "moov atom not found") // bad file: failed but not hw
	s.AuthFailed(t0, "10.0.0.5:1234")

	h := s.Snapshot(t0.Add(time.Minute), okFF(), "")
	want := []string{ProblemTranslationRejected, ProblemHwErrors, ProblemAuthFailed}
	if !slices.Equal(h.Problems, want) || h.Status != "degraded" {
		t.Fatalf("problems %v, want %v", h.Problems, want)
	}
	if h.Jobs.Failed15m != 2 || h.Jobs.HwErrors15m != 1 || h.Jobs.Cancelled != 1 || h.Jobs.Active != 0 || h.Jobs.Total != 3 {
		t.Fatalf("job stats %+v", h.Jobs)
	}
	if h.LastError == nil || h.LastError.Kind != "auth" {
		t.Fatalf("last error %+v", h.LastError)
	}

	later := s.Snapshot(t0.Add(problemWindow+time.Minute), okFF(), "")
	if later.Status != "ok" {
		t.Fatalf("problems should expire, got %v", later.Problems)
	}
}

func TestShimNotSeenOnlyAfterHeartbeatStarted(t *testing.T) {
	s := NewState(t0)
	s.SawShim(t0, "run", "1.0.0")
	if h := s.Snapshot(t0.Add(time.Hour), okFF(), ""); slices.Contains(h.Problems, ProblemShimNotSeen) {
		t.Fatal("no heartbeat configured: must not flag shim_not_seen")
	}
	s.SawShim(t0, "ping", "1.0.0")
	if h := s.Snapshot(t0.Add(10*time.Minute), okFF(), ""); slices.Contains(h.Problems, ProblemShimNotSeen) {
		t.Fatal("flagged too early")
	}
	if h := s.Snapshot(t0.Add(20*time.Minute), okFF(), ""); !slices.Contains(h.Problems, ProblemShimNotSeen) {
		t.Fatal("expected shim_not_seen")
	}
}

func TestVersionProblems(t *testing.T) {
	s := NewState(t0)
	ff := FFmpegStatus{Version: "8.1.3-1", Expected: "8.1.3-2", Managed: true, Path: "x", Syncing: true}.withSync()
	if h := s.Snapshot(t0, ff, ""); !slices.Equal(h.Problems, []string{ProblemVersionMismatch}) {
		t.Fatalf("got %v", h.Problems)
	}
	ff.Syncing, ff.LastSyncError = false, "download failed"
	if h := s.Snapshot(t0, ff, ""); !slices.Equal(h.Problems, []string{ProblemVersionSyncFailed}) {
		t.Fatalf("got %v", h.Problems)
	}

	s.Probed("8.1.3-1")
	if h := s.Snapshot(t0, okFF(), ""); !slices.Equal(h.Problems, []string{ProblemRestartJellyfin}) {
		t.Fatalf("got %v", h.Problems)
	}
	s.Probed("8.1.3-2")
	if h := s.Snapshot(t0, okFF(), ""); h.Status != "ok" {
		t.Fatalf("got %v", h.Problems)
	}
}

func TestSelftestNoFFmpegAndPaths(t *testing.T) {
	s := NewState(t0)
	s.SetSelftest(t0, errors.New("qsv init failed"))
	s.SetPaths([]PathStatus{{Container: "/media6", OK: false, Error: "target missing"}})
	h := s.Snapshot(t0, FFmpegStatus{Managed: true}.withSync(), "")
	want := []string{ProblemNoFFmpeg, ProblemSelftestFailed, ProblemPathLinks}
	if !slices.Equal(h.Problems, want) {
		t.Fatalf("got %v want %v", h.Problems, want)
	}
}

func TestTailBuffer(t *testing.T) {
	tb := newTail(5)
	tb.Write([]byte("abc"))
	tb.Write([]byte("defg"))
	if tb.String() != "cdefg" {
		t.Fatalf("got %q", tb.String())
	}
}
