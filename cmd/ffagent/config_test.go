package main

import (
	"bufio"
	"path/filepath"
	"strings"
	"testing"
)

// testToken is a dummy value, long enough to pass validation.
var testToken = strings.Repeat("t", 20)

func parseString(t *testing.T, s string) *Config {
	t.Helper()
	c, err := parseConfig(bufio.NewScanner(strings.NewReader(s)))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestParseConfig(t *testing.T) {
	c := parseString(t, "\ufefftoken = "+testToken+"\r\n"+
		"# comment\r\n"+
		"drive = d:\\\r\n"+
		"[paths]\r\n"+
		"/media6 = D:\\Videos\\Movies\\\r\n"+
		"/cache = \"\\\\jellyfin-host\\cache\"\r\n"+
		"[credentials]\r\n"+
		"\\\\jellyfin-host\\cache = jelly:pa:ss word\r\n")
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Token != testToken || c.Drive != "D:" || c.Listen != defaultListen {
		t.Fatalf("bad root values: %+v", c)
	}
	if len(c.Paths) != 2 || c.Paths[0].Host != `D:\Videos\Movies` || c.Paths[1].Host != `\\jellyfin-host\cache` {
		t.Fatalf("bad paths: %+v", c.Paths)
	}
	if len(c.Credentials) != 1 || c.Credentials[0].User != "jelly" || c.Credentials[0].Password != "pa:ss word" {
		t.Fatalf("bad credentials: %+v", c.Credentials)
	}
}

func TestValidate(t *testing.T) {
	head := "token = " + testToken + "\n"
	bad := []string{
		"token = short",
		head + "[paths]\nmedia = D:\\x",
		head + "[paths]\n/media/ = D:\\x",
		head + "[paths]\n/a = D:\\x\n/A = D:\\y",
		head + "drive = CC",
	}
	for _, s := range bad {
		c, err := parseConfig(bufio.NewScanner(strings.NewReader(s)))
		if err == nil {
			err = c.Validate()
		}
		if err == nil {
			t.Errorf("expected error for %q", s)
		}
	}
	if _, err := parseConfig(bufio.NewScanner(strings.NewReader("[nope]"))); err == nil {
		t.Error("unknown section accepted")
	}
	if _, err := parseConfig(bufio.NewScanner(strings.NewReader("tokn = x"))); err == nil {
		t.Error("unknown key accepted")
	}
}

func TestRenderRoundTrip(t *testing.T) {
	c := &Config{
		Token:       newToken(),
		Paths:       []PathMap{{"/media6", `D:\Videos\Movies`}, {"/config", `C:\docker\jellyfin\config`}},
		Credentials: []Credential{{Share: `\\nas\media`, User: "u", Password: "p"}},
	}
	c.applyDefaults()
	file := filepath.Join(t.TempDir(), "ffagent.conf")
	if err := c.Save(file); err != nil {
		t.Fatal(err)
	}
	back, err := LoadConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	if back.Token != c.Token || len(back.Paths) != 2 || back.Paths[0].Container != "/config" ||
		len(back.Credentials) != 1 || back.Drive != "C:" || back.Listen != defaultListen {
		t.Fatalf("round trip mismatch: %+v", back)
	}
}

func TestGPUAndTranslateSettings(t *testing.T) {
	c := parseString(t, "token = "+testToken+"\ngpu = NVIDIA\ntranslate = auto\n")
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.GPU != GPUNvidia || c.TranslateProfile() != "none" {
		t.Fatalf("gpu %q profile %q", c.GPU, c.TranslateProfile())
	}
	c = parseString(t, "token = "+testToken+"\ngpu = intel\n")
	want := "none"
	if isWindows {
		want = "qsv-windows"
	}
	if got := c.TranslateProfile(); got != want {
		t.Fatalf("intel profile %q, want %q", got, want)
	}
	c = parseString(t, "token = "+testToken+"\ngpu = intel\ntranslate = none\n")
	if c.TranslateProfile() != "none" {
		t.Fatal("explicit translate must win")
	}
	for _, bad := range []string{"gpu = amd", "translate = magic"} {
		c := parseString(t, "token = "+testToken+"\n"+bad+"\n")
		if c.Validate() == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestEffectiveConfigFromEnv(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "none.conf")
	env := map[string]string{"FFAGENT_TOKEN": testToken, "FFAGENT_GPU": "nvidia", "FFAGENT_LISTEN": ":9000"}
	c, err := loadEffectiveConfig(missing, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != testToken || c.GPU != GPUNvidia || c.Listen != ":9000" || c.HealthListen != defaultHealthListen {
		t.Fatalf("%+v", c)
	}
	if _, err := loadEffectiveConfig(missing, func(string) string { return "" }); err == nil {
		t.Fatal("expected error without file and env")
	}

	// File + env override.
	file := filepath.Join(t.TempDir(), "ffagent.conf")
	base := &Config{Token: newToken(), GPU: GPUIntel}
	base.applyDefaults()
	if err := base.Save(file); err != nil {
		t.Fatal(err)
	}
	c, err = loadEffectiveConfig(file, func(k string) string {
		if k == "FFAGENT_GPU" {
			return "none"
		}
		return ""
	})
	if err != nil || c.Token != base.Token || c.GPU != GPUNone {
		t.Fatalf("%+v %v", c, err)
	}
}
