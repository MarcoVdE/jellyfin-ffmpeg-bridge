//go:build !windows

package main

import (
	"testing"
)

func TestLinuxPathsAreVerifiedNotLinked(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{Paths: []PathMap{
		{Container: dir, Host: "nas:/media"},
		{Container: dir + "/missing", Host: "nas:/cache"},
	}}
	st := ensureLinks(cfg)
	if len(st) != 2 || !st[0].OK || st[1].OK || st[1].Error == "" {
		t.Fatalf("got %+v", st)
	}
}
