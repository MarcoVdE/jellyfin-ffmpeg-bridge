package compose

import (
	"slices"
	"strings"
	"testing"
)

const userCompose = `# Jellyfin is a media server.
services:
  jellyfin:
    image: jellyfin/jellyfin
    container_name: jellyfin
    # user: uid:gid
    network_mode: bridge
    ports: # Ignored in host most
      - 8096:8096 # Port for selecting content / web interface
      - 1900:1900 # DLNA port.
    volumes: # Note: Autopulse on the host OS
      - ./config:/config
      - ./cache:/cache
      - ./media:/media
      - D:/Videos/Movies:/media6:ro
      - D:/Videos/Series:/media7:ro
      - "D:/Videos/Exercise:/media8:ro"
      - D:/Videos/YouTube:/YouTube:ro
      - ffshim:/opt/ffshim:ro
    restart: 'unless-stopped'

  other:
    image: something
    volumes:
      - ./other-config:/config
`

func TestVolumesUserCompose(t *testing.T) {
	got, warnings, err := Volumes(userCompose, "jellyfin", `C:\docker\jellyfin`)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	want := []Mount{
		{"/config", `C:\docker\jellyfin\config`},
		{"/cache", `C:\docker\jellyfin\cache`},
		{"/media", `C:\docker\jellyfin\media`},
		{"/media6", `D:\Videos\Movies`},
		{"/media7", `D:\Videos\Series`},
		{"/media8", `D:\Videos\Exercise`},
		{"/YouTube", `D:\Videos\YouTube`},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
}

func TestVolumesLongSyntaxAndWarnings(t *testing.T) {
	c := strings.Join([]string{
		"services:",
		"  jellyfin:",
		"    volumes:",
		"      - type: bind",
		"        source: 'E:\\Media\\Music'",
		"        target: /music",
		"        read_only: true",
		"      - type: volume",
		"        source: cachevol",
		"        target: /cache",
		"      - ${MEDIA_DIR}:/media",
		"      - /mnt/wsl/data:/data",
		"      - ../shared:/shared",
		"volumes:",
		"  cachevol:",
	}, "\n")
	got, warnings, err := Volumes(c, "jellyfin", `C:\docker\jellyfin`)
	if err != nil {
		t.Fatal(err)
	}
	want := []Mount{
		{"/music", `E:\Media\Music`},
		{"/shared", `C:\docker\shared`},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
	if len(warnings) != 2 {
		t.Fatalf("want 2 warnings (variable, WSL path), got %v", warnings)
	}
}

func TestVolumesErrors(t *testing.T) {
	if _, _, err := Volumes("services:\n  emby:\n    image: x\n", "jellyfin", `C:\x`); err == nil {
		t.Fatal("expected missing service error")
	}
	if _, _, err := Volumes("services:\n  jellyfin:\n    image: x\n", "jellyfin", `C:\x`); err == nil {
		t.Fatal("expected missing volumes error")
	}
}

func TestSplitShort(t *testing.T) {
	cases := map[string][2]string{
		"D:/Videos/Movies:/media6:ro": {"D:/Videos/Movies", "/media6"},
		"./config:/config":            {"./config", "/config"},
		`C:\data:/data:rw`:            {`C:\data`, "/data"},
	}
	for in, want := range cases {
		src, dst, ok := splitShort(in)
		if !ok || src != want[0] || dst != want[1] {
			t.Errorf("splitShort(%q) = %q %q %v", in, src, dst, ok)
		}
	}
}
