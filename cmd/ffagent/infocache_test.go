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
	if string(c.get("b.exe", []string{"-version"}).stdout) != "b.exe" || runs.Load() != 2 {
		t.Fatal("a different ffmpeg must be asked separately")
	}
	fail.Store(true)
	c.get("c.exe", []string{"-version"})
	c.get("c.exe", []string{"-version"})
	if runs.Load() != 4 {
		t.Fatalf("failures must not be cached, runs=%d", runs.Load())
	}
}
