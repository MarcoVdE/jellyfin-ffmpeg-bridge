package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regenerate with: UPDATE_EXAMPLES=1 go test -run TestExampleOverride ./cmd/ffagent
func TestExampleOverrideMatchesGenerated(t *testing.T) {
	file := filepath.Join("..", "..", "examples", "docker-compose.override.yml")
	got := renderOverride(overrideOpts{Service: "jellyfin", Image: defaultShimImage})
	if os.Getenv("UPDATE_EXAMPLES") != "" {
		if err := os.WriteFile(file, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, overrideMarker) {
		t.Fatal("the example must not carry the generated marker")
	}
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

func TestGeneratedOverrideCarriesMarker(t *testing.T) {
	got := renderOverride(overrideOpts{Service: "jellyfin", Image: defaultShimImage, Generated: true})
	if !strings.HasPrefix(got, overrideMarker+"\n") {
		t.Fatalf("missing marker:\n%s", got)
	}
}

func TestRemoveEnvVar(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, ".env")
	os.WriteFile(f, []byte("TZ=Europe/Amsterdam\r\nFFSHIM_TOKEN=abc\r\n"), 0o644)
	if ok, err := removeEnvVar(f, "FFSHIM_TOKEN"); !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	if b, _ := os.ReadFile(f); string(b) != "TZ=Europe/Amsterdam\r\n" {
		t.Fatalf("got %q", b)
	}
	if ok, _ := removeEnvVar(f, "FFSHIM_TOKEN"); ok {
		t.Fatal("nothing left to remove")
	}
	// A .env holding only our line is deleted entirely.
	g := filepath.Join(dir, "only.env")
	os.WriteFile(g, []byte("FFSHIM_TOKEN=abc\n"), 0o644)
	if ok, err := removeEnvVar(g, "FFSHIM_TOKEN"); !ok || err != nil || fileExistsAt(g) {
		t.Fatalf("expected file removed: %v %v", ok, err)
	}
	if ok, err := removeEnvVar(filepath.Join(dir, "missing.env"), "X"); ok || err != nil {
		t.Fatalf("missing file: %v %v", ok, err)
	}
}

func TestFindComposeFileAndGeneratedOverride(t *testing.T) {
	dir := t.TempDir()
	if findComposeFile(dir) != "" {
		t.Fatal("empty dir")
	}
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("services: {}\n"), 0o644)
	if got := findComposeFile(dir); filepath.Base(got) != "docker-compose.yml" {
		t.Fatalf("got %q", got)
	}
	gen := filepath.Join(dir, "docker-compose.override.yml")
	os.WriteFile(gen, []byte(renderOverride(overrideOpts{Service: "jellyfin", Image: defaultShimImage, Generated: true})), 0o644)
	hand := filepath.Join(dir, "hand.yml")
	os.WriteFile(hand, []byte(renderOverride(overrideOpts{Service: "jellyfin", Image: defaultShimImage})), 0o644)
	if !isGeneratedOverride(gen) || isGeneratedOverride(hand) || isGeneratedOverride(filepath.Join(dir, "nope")) {
		t.Fatal("isGeneratedOverride wrong")
	}
}

func TestOverrideWithTokenSecret(t *testing.T) {
	got := renderOverride(overrideOpts{Service: "jellyfin", Image: defaultShimImage, Generated: true, TokenFile: "./ffagent/ffshim-token"})
	for _, s := range []string{"    secrets:\n      - ffshim_token\n", "secrets:\n  ffshim_token:\n    file: ./ffagent/ffshim-token\n"} {
		if !strings.Contains(got, s) {
			t.Errorf("missing %q in:\n%s", s, got)
		}
	}
	if strings.Contains(got, "FFSHIM_TOKEN:") || strings.Contains(got, ".env") {
		t.Errorf("secret mode must not use the environment/.env:\n%s", got)
	}
}

func TestComposeRelPath(t *testing.T) {
	base := filepath.Join(t.TempDir(), "jellyfin")
	if got := composeRelPath(base, filepath.Join(base, "ffagent", "ffshim-token")); got != "./ffagent/ffshim-token" {
		t.Fatalf("got %q", got)
	}
	outside := filepath.Join(filepath.Dir(base), "elsewhere", "ffshim-token")
	if got := composeRelPath(base, outside); got != outside {
		t.Fatalf("got %q", got)
	}
}

func TestLegacyGeneratedOverrideIsRecognised(t *testing.T) {
	dir := t.TempDir()
	// What v1.0.0-alpha.2..4 wrote: no marker, env token, legacy first line.
	legacy := filepath.Join(dir, "docker-compose.override.yml")
	os.WriteFile(legacy, []byte(legacyOverrideHeader+"\n# `docker compose up -d` merges it...\nservices:\n  ffshim:\n"), 0o644)
	if !isGeneratedOverride(legacy) {
		t.Fatal("legacy generated override not recognised")
	}
	// The repository example is not "generated".
	example := filepath.Join(dir, "example.yml")
	os.WriteFile(example, []byte(renderOverride(overrideOpts{Service: "jellyfin", Image: defaultShimImage})), 0o644)
	if isGeneratedOverride(example) {
		t.Fatal("example treated as generated")
	}
}
