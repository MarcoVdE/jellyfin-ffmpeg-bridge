package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSelftestArgsPerGPU(t *testing.T) {
	cases := []struct {
		gpu     string
		windows bool
		want    []string
	}{
		{GPUIntel, true, []string{"d3d11va=dx11:,vendor=0x8086", "qsv=qs@dx11", "opencl=ocl@dx11", "h264_qsv"}},
		{GPUIntel, false, []string{"vaapi=va:/dev/dri/renderD129,driver=iHD", "qsv=qs@va", "opencl=ocl@va", "h264_qsv"}},
		{GPUVaapi, false, []string{"vaapi=va:/dev/dri/renderD129", "h264_vaapi"}},
		{GPUNvidia, true, []string{"cuda=cu:0", "h264_nvenc"}},
		{GPUNvidia, false, []string{"cuda=cu:0", "h264_nvenc"}},
	}
	for _, c := range cases {
		args := selftestArgs(c.gpu, "/dev/dri/renderD129", c.windows)
		for _, w := range c.want {
			if !slices.Contains(args, w) {
				t.Errorf("%s/windows=%v: missing %q in %q", c.gpu, c.windows, w, args)
			}
		}
		if args[len(args)-1] != "-" || !slices.Contains(args, "null") {
			t.Errorf("%s: must write to the null muxer: %q", c.gpu, args)
		}
		vf := args[slices.Index(args, "-vf")+1]
		if !strings.HasPrefix(vf, "format=nv12,hwupload") {
			t.Errorf("%s: unexpected filter %q", c.gpu, vf)
		}
	}
	if selftestArgs(GPUNone, defaultDevice, true) != nil {
		t.Error("gpu none must not test")
	}
}

func TestDetectFixedVersion(t *testing.T) {
	status := filepath.Join(t.TempDir(), "status")
	os.WriteFile(status, []byte("Package: jellyfin-ffmpeg8\nStatus: install ok installed\nVersion: 8.1.3-2-bookworm\n"), 0o644)
	old := dpkgStatusPath
	dpkgStatusPath = status
	defer func() { dpkgStatusPath = old }()

	if v := detectFixedVersion("/usr/lib/jellyfin-ffmpeg/ffmpeg"); v != "8.1.3-2" {
		t.Fatalf("got %q", v)
	}
	if v := detectFixedVersion(`C:\tools\ffmpeg.exe`); v != "custom" {
		t.Fatalf("got %q", v)
	}
	if v := detectFixedVersion(""); v != "" {
		t.Fatalf("got %q", v)
	}

	// A fixed jellyfin-ffmpeg that differs from the Jellyfin image is reported.
	f := NewFFmpeg(t.TempDir(), "/usr/lib/jellyfin-ffmpeg/ffmpeg", GPUNone, defaultDevice, log.New(io.Discard, "", 0))
	f.SetExpected("8.1.4-1")
	st := f.Status()
	if st.Version != "8.1.3-2" || st.InSync || st.Managed {
		t.Fatalf("status %+v", st)
	}
	h := NewState(t0).Snapshot(t0, st, "")
	if !slices.Equal(h.Problems, []string{ProblemVersionMismatch}) {
		t.Fatalf("problems %v", h.Problems)
	}
	// "custom" builds can't be compared and must not alarm.
	f2 := NewFFmpeg(t.TempDir(), "/opt/ffmpeg/ffmpeg", GPUNone, defaultDevice, log.New(io.Discard, "", 0))
	f2.SetExpected("8.1.4-1")
	if h := NewState(t0).Snapshot(t0, f2.Status(), ""); h.Status != "ok" {
		t.Fatalf("custom ffmpeg flagged: %v", h.Problems)
	}
}
