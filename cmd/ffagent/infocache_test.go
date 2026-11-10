package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

func TestIsInfoCommand(t *testing.T) {
	yes := [][]string{
		{"-version"}, {"-hide_banner", "-hwaccels"}, {"-v", "quiet", "-encoders"},
		{"-h", "filter=tonemap_opencl"}, {"-hide_banner", "-h", "encoder=h264_qsv"}, {"-h"},
	}
	no := [][]string{
		nil, {"-hide_banner"}, {"-i", "x.mkv"}, {"-version", "-i", "x"},
		{"-hide_banner", "-re", "-f", "lavfi", "-i", "nullsrc", "-f", "null", "-"},
		{"-init_hw_device", "vaapi=va", "-version"},
	}
	for _, a := range yes {
		if !isInfoCommand(a) {
			t.Errorf("%q should be an info command", a)
		}
	}
	for _, a := range no {
		if isInfoCommand(a) {
			t.Errorf("%q must not be an info command", a)
		}
	}
}

func TestInfoCacheRunsOncePerExeAndRetriesFailures(t *testing.T) {
	var runs atomic.Int32
	fail := atomic.Bool{}
	c := newInfoCache()
	c.run = func(_ context.Context, exe string, args []string) infoResult {
		runs.Add(1)
		if fail.Load() {
			return infoResult{code: 1}
		}
		return infoResult{stdout: []byte(exe)}
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.get("a.exe", []string{"-version"}) }()
	}
	wg.Wait()
	if runs.Load() != 1 {
		t.Fatalf("concurrent gets ran %d times", runs.Load())
	}
	if r, _, _ := c.get("b.exe", []string{"-version"}); string(r.stdout) != "b.exe" || runs.Load() != 2 {
		t.Fatal("a different ffmpeg must be asked separately")
	}
	fail.Store(true)
	c.get("c.exe", []string{"-version"})
	c.get("c.exe", []string{"-version"})
	if runs.Load() != 4 {
		t.Fatalf("failures must not be cached, runs=%d", runs.Load())
	}
}

func TestWarmCommandsAreInfoCommands(t *testing.T) {
	for _, a := range warmInfoCommands {
		if !isInfoCommand(a) {
			t.Errorf("%q is pre-run but would never be answered from the cache", a)
		}
	}
	// What Intro Skipper sends (see FFmpegService.Requirements upstream).
	for _, a := range [][]string{{"-muxers"}, {"-h", "muxer=chromaprint"}, {"-h", "filter=silencedetect"}} {
		if !isInfoCommand(a) {
			t.Errorf("%q not cacheable", a)
		}
	}
}

func TestInfoCacheReportsRunTime(t *testing.T) {
	c := newInfoCache()
	c.run = func(context.Context, string, []string) infoResult { return infoResult{} }
	// The run itself takes ~no time: "ran" must not be derived from the
	// duration (Windows clocks can report 0 for very short runs).
	if _, ran, _ := c.get("x", []string{"-version"}); !ran {
		t.Fatal("first call ran ffmpeg")
	}
	if _, ran, took := c.get("x", []string{"-version"}); ran || took != 0 {
		t.Fatal("second call is from memory")
	}
}
