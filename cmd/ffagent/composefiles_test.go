package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExampleOverrideMatchesGenerated(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("..", "..", "examples", "docker-compose.override.yml"))
	if err != nil {
		t.Fatal(err)
	}
	got := renderOverride(overrideOpts{Service: "jellyfin", Build: "./jellyfin-ffmpeg-bridge"})
	if got != strings.ReplaceAll(string(want), "\r\n", "\n") {
		t.Fatalf("examples/docker-compose.override.yml is out of date; regenerate it:\n%s", got)
	}
}

func TestRenderOverrideVariants(t *testing.T) {
	got := renderOverride(overrideOpts{Service: "media", Image: "ghcr.io/me/shim:1.0.0", Agent: "10.0.0.5:7878"})
	for _, s := range []string{"  media:\n", "    image: ghcr.io/me/shim:1.0.0\n", "      FFSHIM_AGENT: 10.0.0.5:7878\n"} {
		if !strings.Contains(got, s) {
			t.Errorf("missing %q", s)
		}
	}
	if strings.Contains(got, "build:") {
		t.Error("unexpected build: with image")
	}
}

func TestOverridePath(t *testing.T) {
	cases := map[string]string{
		filepath.Join("x", "docker-compose.yml"): filepath.Join("x", "docker-compose.override.yml"),
		filepath.Join("x", "compose.yaml"):       filepath.Join("x", "compose.override.yaml"),
		filepath.Join("x", "jellyfin.yml"):       "",
	}
	for in, want := range cases {
		if got := overridePath(in); got != want {
			t.Errorf("overridePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRepoBuildContext(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "jellyfin-ffmpeg-bridge")
	os.MkdirAll(filepath.Join(repo, "cmd", "ffshim"), 0o755)
	os.WriteFile(filepath.Join(repo, "Dockerfile"), nil, 0o644)
	os.WriteFile(filepath.Join(repo, "cmd", "ffshim", "main.go"), nil, 0o644)

	if got := repoBuildContext(repo, root); got != "./jellyfin-ffmpeg-bridge" {
		t.Fatalf("got %q", got)
	}
	if got := repoBuildContext(repo, filepath.Join(root, "other")); got != "../jellyfin-ffmpeg-bridge" {
		t.Fatalf("got %q", got)
	}
	if got := repoBuildContext(root, root); got != "" {
		t.Fatalf("not a repo, got %q", got)
	}
}

func TestSetEnvVar(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, ".env")

	if err := setEnvVar(f, "FFSHIM_TOKEN", "aaa"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(f); string(b) != "FFSHIM_TOKEN=aaa\n" {
		t.Fatalf("new file: %q", b)
	}

	os.WriteFile(f, []byte("\ufeffTZ=Europe/Amsterdam\r\nFFSHIM_TOKEN=old\r\n# keep me\r\n"), 0o644)
	if err := setEnvVar(f, "FFSHIM_TOKEN", "bbb"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(f); string(b) != "TZ=Europe/Amsterdam\r\nFFSHIM_TOKEN=bbb\r\n# keep me\r\n" {
		t.Fatalf("update: %q", b)
	}
}
