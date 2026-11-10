// Package translate turns the ffmpeg arguments Jellyfin generates when it
// runs on Linux with Intel QSV into arguments that work with jellyfin-ffmpeg
// on Windows.
//
// With the required Jellyfin settings (QSV, OS-native decoders off, VPP tone
// mapping off, OpenCL tone mapping on) the Linux and Windows pipelines only
// differ in how the GPU device is opened:
//
//	Linux:   -init_hw_device vaapi=va:,vendor_id=0x8086,driver=iHD
//	Windows: -init_hw_device d3d11va=va:,vendor=0x8086
//
// The device keeps its name ("va"), so derived devices such as
// "qsv=qs@va" and "opencl=ocl@va" keep working unchanged.
//
// Anything else that is VA-API specific has no Windows equivalent and is
// rejected with a hint pointing at the Jellyfin setting that caused it.
package translate

import (
	"bytes"
	"regexp"
	"slices"
	"strings"
)

// IntelVendorID is the PCI vendor id used to select the Intel GPU.
const IntelVendorID = "0x8086"

// Result is a translated command line.
type Result struct {
	Args []string
	// ExtraHwaccels must be appended to the output of `ffmpeg -hwaccels`.
	// Jellyfin only builds its full-GPU QSV pipeline on Linux when it
	// believes "vaapi" is available.
	ExtraHwaccels []string
}

// RejectError explains why a command line cannot run on Windows.
type RejectError struct {
	Arg  string
	Hint string
}

func (e *RejectError) Error() string {
	return "cannot run on Windows: argument " + quote(e.Arg) + " is VA-API only. " + e.Hint
}

const (
	hintNativeDecoder = "In Jellyfin > Dashboard > Playback > Transcoding, turn OFF " +
		"'Prefer OS native DXVA or VA-API hardware decoders'."
	hintVppTonemap = "In Jellyfin > Dashboard > Playback > Transcoding, turn OFF " +
		"'Enable VPP Tone mapping' (keep 'Enable Tone mapping' on)."
	hintUseQsv = "In Jellyfin > Dashboard > Playback > Transcoding, set Hardware " +
		"acceleration to 'Intel QuickSync (QSV)' (not VA-API)."
)

var vaapiDevice = regexp.MustCompile(`^vaapi=([A-Za-z0-9_]+)(:.*)?$`)

// Translation profiles.
const (
	// ProfileNone passes arguments through unchanged. Used when Jellyfin's
	// commands already fit the agent's machine: NVENC (identical on Linux and
	// Windows) and agents running on Linux.
	ProfileNone = "none"
	// ProfileQSVWindows turns Jellyfin's Linux QSV commands into Windows ones.
	ProfileQSVWindows = "qsv-windows"
)

// ValidProfile reports whether p names a profile.
func ValidProfile(p string) bool { return p == ProfileNone || p == ProfileQSVWindows }

// For translates args with the given profile.
func For(profile string, args []string) (Result, error) {
	if profile == ProfileQSVWindows {
		return Translate(args)
	}
	return Result{Args: slices.Clone(args)}, nil
}

// Translate rewrites Linux QSV args for Windows or returns a *RejectError.
func Translate(args []string) (Result, error) {
	out := slices.Clone(args)

	for i := 0; i < len(out)-1; i++ {
		if out[i] != "-init_hw_device" {
			continue
		}
		v := out[i+1]
		if m := vaapiDevice.FindStringSubmatch(v); m != nil {
			out[i+1] = "d3d11va=" + m[1] + ":,vendor=" + IntelVendorID
			i++
			continue
		}
		if strings.HasPrefix(v, "drm=") {
			return Result{}, &RejectError{Arg: v, Hint: hintUseQsv}
		}
	}

	// Specific causes first, so the hint names the right setting.
	for i := 0; i < len(out)-1; i++ {
		if (out[i] == "-hwaccel" || out[i] == "-hwaccel_output_format") && out[i+1] == "vaapi" {
			return Result{}, &RejectError{Arg: out[i] + " vaapi", Hint: hintNativeDecoder}
		}
	}
	for _, a := range out {
		if strings.Contains(a, "tonemap_vaapi") || strings.Contains(a, "derive_device=vaapi") {
			return Result{}, &RejectError{Arg: a, Hint: hintVppTonemap}
		}
	}
	for _, a := range out {
		if strings.Contains(a, "vaapi") || strings.Contains(a, "/dev/dri") {
			return Result{}, &RejectError{Arg: a, Hint: hintUseQsv}
		}
	}

	res := Result{Args: out}
	if slices.Contains(out, "-hwaccels") {
		res.ExtraHwaccels = []string{"vaapi"}
	}
	return res, nil
}

// AppendHwaccels adds names to the output of `ffmpeg -hwaccels` if missing.
func AppendHwaccels(output []byte, names []string) []byte {
	have := map[string]bool{}
	for _, line := range strings.Split(string(output), "\n") {
		have[strings.TrimSpace(line)] = true
	}
	out := bytes.TrimRight(output, "\r\n")
	out = append(slices.Clone(out), '\n')
	for _, n := range names {
		if !have[n] {
			out = append(out, n...)
			out = append(out, '\n')
		}
	}
	return append(out, '\n')
}

// IsVersionProbe reports whether args is Jellyfin's startup `ffmpeg -version`.
func IsVersionProbe(args []string) bool {
	return len(args) == 1 && args[0] == "-version"
}

func quote(s string) string {
	if len(s) > 120 {
		s = s[:117] + "..."
	}
	return "\"" + s + "\""
}
