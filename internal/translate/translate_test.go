package translate

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// A trimmed 4K HDR -> 1080p SDR command as Jellyfin generates it on Linux with
// QSV, native decoders off, VPP tone mapping off and OpenCL tone mapping on.
var linuxHdrCommand = []string{
	"-analyzeduration", "200M", "-probesize", "1G",
	"-init_hw_device", "vaapi=va:,vendor_id=0x8086,driver=iHD",
	"-init_hw_device", "qsv=qs@va",
	"-init_hw_device", "opencl=ocl@va",
	"-filter_hw_device", "qs",
	"-hwaccel", "qsv", "-hwaccel_output_format", "qsv", "-noautorotate",
	"-c:v", "hevc_qsv",
	"-i", "file:/media6/Film (2020)/Film.mkv",
	"-codec:v:0", "h264_qsv",
	"-vf", "setparams=color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc," +
		"vpp_qsv=w=1920:h=1080:format=p010," +
		"hwmap=derive_device=opencl:mode=read," +
		"tonemap_opencl=format=nv12:p=bt709:t=bt709:m=bt709:tonemap=bt2390:peak=100:desat=0," +
		"hwmap=derive_device=qsv:mode=write:reverse=1:extra_hw_frames=16,format=qsv",
	"-f", "hls", "-hls_segment_filename", "/cache/transcodes/abc%d.mp4",
	"/cache/transcodes/abc.m3u8",
}

func TestTranslateHdrPipeline(t *testing.T) {
	res, err := Translate(linuxHdrCommand)
	if err != nil {
		t.Fatalf("unexpected reject: %v", err)
	}
	want := slices.Clone(linuxHdrCommand)
	want[5] = "d3d11va=va:,vendor=0x8086"
	if !slices.Equal(res.Args, want) {
		t.Fatalf("args differ:\n got %q\nwant %q", res.Args, want)
	}
	if res.ExtraHwaccels != nil {
		t.Fatalf("unexpected hwaccels injection")
	}
	// The input slice must not be modified.
	if linuxHdrCommand[5] != "vaapi=va:,vendor_id=0x8086,driver=iHD" {
		t.Fatal("input was mutated")
	}
}

func TestTranslateDeviceVariants(t *testing.T) {
	for _, v := range []string{
		"vaapi=va",
		"vaapi=va:,kernel_driver=i915,driver=iHD",
		"vaapi=va:/dev/dri/renderD128,driver=iHD",
	} {
		res, err := Translate([]string{"-init_hw_device", v, "-init_hw_device", "qsv=qs@va"})
		if err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		if res.Args[1] != "d3d11va=va:,vendor=0x8086" || res.Args[3] != "qsv=qs@va" {
			t.Fatalf("%s: got %q", v, res.Args)
		}
	}
}

func TestTranslateRejects(t *testing.T) {
	cases := []struct {
		name string
		args []string
		hint string
	}{
		{"native decoder", []string{"-init_hw_device", "vaapi=va:,driver=iHD", "-hwaccel", "vaapi", "-hwaccel_output_format", "vaapi"}, hintNativeDecoder},
		{"vpp tonemap", []string{"-vf", "vpp_qsv=w=1920,hwmap=derive_device=vaapi,format=vaapi,tonemap_vaapi=format=nv12"}, hintVppTonemap},
		{"vaapi mode", []string{"-vf", "scale_vaapi=w=1920:h=1080", "-c:v", "h264_vaapi"}, hintUseQsv},
		{"drm device", []string{"-init_hw_device", "drm=dr:/dev/dri/renderD128"}, hintUseQsv},
	}
	for _, c := range cases {
		_, err := Translate(c.args)
		var rej *RejectError
		if !errors.As(err, &rej) {
			t.Fatalf("%s: expected RejectError, got %v", c.name, err)
		}
		if rej.Hint != c.hint {
			t.Fatalf("%s: hint %q, want %q", c.name, rej.Hint, c.hint)
		}
	}
}

func TestTranslatePassesThroughUnrelatedCommands(t *testing.T) {
	for _, args := range [][]string{
		{"-version"},
		{"-h", "filter=tonemap_opencl"},
		{"-hide_banner", "-re", "-f", "lavfi", "-i", "nullsrc=s=1x1:r=1:d=5", "-f", "null", "-"},
		{"-i", "/media6/x.mkv", "-c:v", "libx264", "-f", "null", "-"},
	} {
		res, err := Translate(args)
		if err != nil || !slices.Equal(res.Args, args) {
			t.Fatalf("%q: got %q, %v", args, res.Args, err)
		}
	}
}

func TestHwaccelsInjection(t *testing.T) {
	res, err := Translate([]string{"-hwaccels"})
	if err != nil || !slices.Equal(res.ExtraHwaccels, []string{"vaapi"}) {
		t.Fatalf("got %v %v", res.ExtraHwaccels, err)
	}
	winOut := "Hardware acceleration methods:\r\ncuda\r\ndxva2\r\nqsv\r\nd3d11va\r\nopencl\r\n\r\n"
	got := string(AppendHwaccels([]byte(winOut), res.ExtraHwaccels))
	if !strings.Contains(got, "\nvaapi\n") || !strings.Contains(got, "qsv") {
		t.Fatalf("unexpected output %q", got)
	}
	// Idempotent when already present.
	again := string(AppendHwaccels([]byte(got), res.ExtraHwaccels))
	if strings.Count(again, "vaapi") != 1 {
		t.Fatalf("duplicated entry: %q", again)
	}
}

func TestIsVersionProbe(t *testing.T) {
	if !IsVersionProbe([]string{"-version"}) || IsVersionProbe([]string{"-version", "x"}) || IsVersionProbe(nil) {
		t.Fatal("IsVersionProbe wrong")
	}
}

func TestForProfiles(t *testing.T) {
	nvenc := []string{"-init_hw_device", "cuda=cu:0", "-filter_hw_device", "cu", "-hwaccel", "cuda",
		"-vf", "scale_cuda=w=1920:h=1080,tonemap_cuda=format=nv12", "-c:v", "h264_nvenc"}
	res, err := For(ProfileNone, nvenc)
	if err != nil || !slices.Equal(res.Args, nvenc) || res.ExtraHwaccels != nil {
		t.Fatalf("none: %q %v", res.Args, err)
	}
	// Linux VA-API commands pass unchanged through "none" (Linux agent).
	vaapi := []string{"-init_hw_device", "vaapi=va:/dev/dri/renderD128", "-hwaccel", "vaapi", "-hwaccels"}
	if res, err := For(ProfileNone, vaapi); err != nil || !slices.Equal(res.Args, vaapi) || res.ExtraHwaccels != nil {
		t.Fatalf("none must not touch vaapi: %q %v", res.Args, err)
	}
	if _, err := For(ProfileQSVWindows, vaapi); err == nil {
		t.Fatal("qsv-windows must reject -hwaccel vaapi")
	}
	if !ValidProfile("none") || !ValidProfile("qsv-windows") || ValidProfile("auto") {
		t.Fatal("ValidProfile wrong")
	}
}
