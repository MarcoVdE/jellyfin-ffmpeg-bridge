package main

import (
	"errors"
	"testing"
)

func TestReadToken(t *testing.T) {
	files := map[string]string{secretTokenPath: "from-secret\n", "/custom": " from-file "}
	read := func(p string) ([]byte, error) {
		if v, ok := files[p]; ok {
			return []byte(v), nil
		}
		return nil, errors.New("missing")
	}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	if got := readToken(env(map[string]string{"FFSHIM_TOKEN": "from-env"}), read); got != "from-env" {
		t.Fatalf("env: %q", got)
	}
	if got := readToken(env(map[string]string{"FFSHIM_TOKEN_FILE": "/custom"}), read); got != "from-file" {
		t.Fatalf("file: %q", got)
	}
	if got := readToken(env(nil), read); got != "from-secret" {
		t.Fatalf("secret: %q", got)
	}
	if got := readToken(env(nil), func(string) ([]byte, error) { return nil, errors.New("x") }); got != "" {
		t.Fatalf("none: %q", got)
	}
}
