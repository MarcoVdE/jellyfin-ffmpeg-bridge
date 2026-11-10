# jellyfin-ffmpeg-bridge

Run Jellyfin's **ffmpeg on another machine** (or on the Windows host under
Docker Desktop) while Jellyfin itself stays in its official, unmodified Docker
container. Transcoding uses that machine's GPU, including **4K HDR → SDR tone
mapping**.

The main use case: **Jellyfin in Docker on Windows with an Intel GPU.** Docker
Desktop runs containers inside WSL2, which has no usable access to the Intel
GPU. The bridge runs only ffmpeg natively on Windows, where Quick Sync, D3D11
and OpenCL work normally.

It's an add-on for an **existing, working Jellyfin `docker compose` setup**:
one agent where ffmpeg should run, one small file next to your
`docker-compose.yml`, and a few Jellyfin settings. Watchtower keeps updating
Jellyfin as before.

> Unofficial community project, not affiliated with the Jellyfin project.

```
 Jellyfin container (official image, unmodified)          GPU machine (Windows or Linux)
┌─────────────────────────────────────────┐            ┌──────────────────────────────────┐
│ Jellyfin ──runs──▶ /opt/ffshim/ffmpeg   │    TCP     │ ffagent                          │
│                    (ffshim)  ───────────┼── :7878 ──▶│   └─▶ jellyfin-ffmpeg            │
│                    ◀── stderr/stdout ───┼────────────┤        (QSV / NVENC / VA-API)    │
│                         exit code       │            │                                  │
│ /media, /cache, ...  ◀── same files ────┼────────────┼─▶ same paths (links or mounts)   │
└─────────────────────────────────────────┘            │ :7879/health ◀── Home Assistant  │
                                                       └──────────────────────────────────┘
```

> **Status:** the protocol, the shim and the Linux agent are covered by
> end-to-end tests. The Windows-specific parts (scheduled task, links, Job
> Object, GPU self-tests) compile and are unit-tested, but haven't been run on
> real Windows hardware yet. [Verify it works](#6-verify-it-works) walks
> through checking them.

---

## Contents

- [Supported setups](#supported-setups)
- [How it works](#how-it-works)
- [Setup: Windows host](#setup-windows-host) (same machine; about 10 minutes)
- [Jellyfin settings](#5-jellyfin-settings)
- [Agent on another Windows machine](docs/separate-machines.md)
- [Agent on a Linux machine](docs/linux-agent.md)
- [Monitoring with Home Assistant](docs/home-assistant.md)
- [Updating](#updating)
- [Troubleshooting](docs/troubleshooting.md)
- [Reference](#reference)
- [Uninstalling](#uninstalling)
- [Development](#development)
- [License](#license)

---

## Supported setups

Jellyfin always runs in its Linux container, so it always builds **Linux**
ffmpeg commands. What the agent has to do with them depends on where it runs:

| ffmpeg runs on                             | GPU                         | Translation                                          | Notes                                                                                                                                                     |
| ------------------------------------------ | --------------------------- | ---------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Windows** (same host or another machine) | **Intel QSV**               | `qsv-windows`: one argument rewritten                | The main case. Full GPU pipeline incl. 4K HDR tone mapping (OpenCL).                                                                                      |
| Windows                                    | NVIDIA NVENC                | `none`: Jellyfin's NVENC commands are OS independent | Should work as is; not tested on hardware yet.                                                                                                            |
| **Linux** (another machine)                | Intel, AMD (VA-API), NVIDIA | `none`: same OS, commands fit as they are            | Agent runs inside the official Jellyfin image. See [Linux agent](docs/linux-agent.md).                                                                    |
| Windows                                    | AMD                         | —                                                    | **Not supported.** Jellyfin's AMD mode only uses the GPU on Windows, so a Linux Jellyfin would decode and tone-map on the CPU. Use a Linux agent for AMD. |

The agent picks the translation automatically from its `gpu` setting.

---

## How it works

| Program   | Runs                                                                          | Does                                                                                                                            |
| --------- | ----------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------- |
| `ffshim`  | inside the Jellyfin container, as `/opt/ffshim/ffmpeg`                        | Pretends to be ffmpeg. Sends the command line to the agent and relays stdin, stdout, stderr and the exit code back to Jellyfin. |
| `ffagent` | where ffmpeg should run: a boot-time task on Windows, or a container on Linux | Runs jellyfin-ffmpeg with those arguments, keeps it at the same version as the Jellyfin image, and reports health.              |

**Communication.** The shim opens a TCP connection to the agent (port 7878) for
every ffmpeg run:

1. The agent sends a random challenge; the shim answers with an HMAC of it,
   keyed with the shared **token**. The token itself never crosses the network.
2. The shim sends the arguments as a JSON list, so no shell quoting is involved.
3. Output streams back as it is produced, so Jellyfin sees ffmpeg's progress live.
4. When Jellyfin stops a transcode it writes `q` to stdin, which is forwarded,
   or it kills the shim. A dropped connection makes the agent kill ffmpeg
   immediately, so no orphaned ffmpeg is left holding the GPU. ffmpeg also dies
   if the agent dies (Job Object on Windows, parent-death signal on Linux).

On the same Windows machine the container reaches the host through Docker
Desktop's `host.docker.internal`. On another machine, the shim points at that
machine's IP (`FFSHIM_AGENT`). The connection isn't encrypted, so use a trusted
LAN or a VPN such as WireGuard or Tailscale.

**No file transfer.** Video never goes through this connection. Both sides
read and write the _same files_ at the _same paths_:

- **Windows:** every container path (e.g. `/media6`) is mirrored as a link at
  the root of `C:` (`C:\media6` → `D:\Videos\Movies`). Windows resolves the
  `/media6/...` paths in ffmpeg's arguments against the current drive, so
  commands work unchanged.
- **Linux:** the agent container mounts the same folders at the same container
  paths as Jellyfin.

**Translation (Windows + Intel).** With the [Jellyfin settings](#5-jellyfin-settings)
below, Jellyfin's Linux and Windows Quick Sync pipelines differ only in how the
GPU is opened, so the agent rewrites that one argument:

```
-init_hw_device vaapi=va:,vendor_id=0x8086,driver=iHD   →   -init_hw_device d3d11va=va:,vendor=0x8086
```

Decoding (`hevc_qsv`), scaling (`vpp_qsv`), OpenCL tone mapping
(`tonemap_opencl`) and encoding (`h264_qsv`) stay the same. The agent also
reports `vaapi` in `ffmpeg -hwaccels`, because Jellyfin only builds its
full-GPU pipeline on Linux when it believes VA-API exists. Anything VA-API
specific that can't be translated is refused with a message naming the
Jellyfin setting to change, and shows up in [monitoring](docs/home-assistant.md).

**Version sync.** The shim reports which jellyfin-ffmpeg the Jellyfin image
ships. On Windows the agent downloads the matching build from the official
[jellyfin-ffmpeg releases](https://github.com/jellyfin/jellyfin-ffmpeg/releases),
runs a GPU self-test on it, and only then switches over, keeping the previous
version as a fallback. A Linux agent uses the ffmpeg of its own Jellyfin image
and reports when that differs from Jellyfin's.

---

## Setup: Windows host

This covers the most common case: Jellyfin under Docker Desktop and ffmpeg on
the same Windows machine. For other setups see
[another Windows machine](docs/separate-machines.md) or
[a Linux machine](docs/linux-agent.md).

**Requirements**

- Windows 10 or 11 (x64) with an **Intel GPU** (7th gen / Kaby Lake or newer for
  HEVC 10-bit and HDR) or an **NVIDIA GPU**.
- An up-to-date graphics driver from Intel or NVIDIA (not only Windows Update).
- **Docker Desktop** with a working Jellyfin `docker compose` setup using the
  official `jellyfin/jellyfin` image.
- **git** to clone this repository (or download it as a zip).

The examples assume your compose project is in `C:\docker\jellyfin`. Replace it
with your folder.

### 1. Get the code next to your compose file

```powershell
cd C:\docker\jellyfin
git clone https://github.com/MarcoVdE/jellyfin-ffmpeg-bridge
```

### 2. Build the agent

Docker is all you need; no Go install required. In PowerShell:

```powershell
cd C:\docker\jellyfin\jellyfin-ffmpeg-bridge
docker run --rm -v "${PWD}:/src" -w /src -e CGO_ENABLED=0 -e GOOS=windows golang:1-alpine `
  go build -buildvcs=false -trimpath -o ffagent.exe ./cmd/ffagent
```

<details><summary>With Go installed instead</summary>

```powershell
$env:CGO_ENABLED = "0"; go build -trimpath -o ffagent.exe ./cmd/ffagent
```

</details>

### 3. Install the agent

Open **Terminal as Administrator** (right-click Start → Terminal (Admin)):

```powershell
cd C:\docker\jellyfin\jellyfin-ffmpeg-bridge
.\ffagent.exe install --compose ..\docker-compose.yml
```

For an NVIDIA GPU add `--gpu nvidia`. This:

- reads the volumes of the `jellyfin` service from your compose file and
  mirrors them on `C:\` (use `--service name` if your service has another name);
- creates `C:\ProgramData\ffagent\` with the config and a random token;
- writes `FFSHIM_TOKEN=...` into the `.env` file next to your compose file;
- writes `docker-compose.override.yml` next to your compose file. It never
  overwrites an existing one; if you already have one, it tells you what to do;
- registers a boot-time task that runs the agent as SYSTEM, restarts it if it
  crashes, and opens ports 7878/7879 in the firewall for your local network;
- starts the agent, which downloads the matching jellyfin-ffmpeg (about 90 MB).

Check the output for path problems, for example a folder in your compose file
that doesn't exist.

> Windows SmartScreen or Defender may warn about `ffagent.exe` because it isn't
> signed. You built it yourself from this source.

### 4. Restart Jellyfin with the shim

```powershell
cd C:\docker\jellyfin
docker compose up -d --build
```

Compose merges `docker-compose.override.yml` automatically. It adds a one-shot
`ffshim` service that copies the shim into a volume, points `JELLYFIN_FFMPEG`
at it, and adds a 5-minute heartbeat to Jellyfin's healthcheck. See
[`examples/docker-compose.override.yml`](examples/docker-compose.override.yml).

### 5. Jellyfin settings

**Dashboard → Playback → Transcoding.** Use the table for your GPU, save, then
**restart the Jellyfin container once** (`docker restart jellyfin`) so it
re-detects ffmpeg's capabilities through the agent.

**Intel Quick Sync (Windows agent):**

| Setting                                               | Value                                                       | Why                                    |
| ----------------------------------------------------- | ----------------------------------------------------------- | -------------------------------------- |
| Hardware acceleration                                 | **Intel QuickSync (QSV)**                                   |                                        |
| QSV Device                                            | _(leave empty)_                                             |                                        |
| Enable hardware decoding for                          | H264, HEVC, MPEG2, VC1, VP8, VP9, **HEVC 10bit**, VP9 10bit | AV1 only on 11th gen+                  |
| **Prefer OS native DXVA or VA-API hardware decoders** | **Off**                                                     | the VA-API decoder can't be translated |
| Enable hardware encoding                              | On                                                          |                                        |
| Enable Intel Low-Power H.264/HEVC encoder             | Off                                                         | not needed; can fail on older GPUs     |
| **Enable VPP Tone mapping**                           | **Off**                                                     | VA-API only on Linux                   |
| **Enable Tone mapping**                               | **On**                                                      | OpenCL tone mapping works on Windows   |

**NVIDIA NVENC (Windows or Linux agent):**

| Setting                       | Value                                                                                                                                                     |
| ----------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Hardware acceleration         | **NVIDIA NVENC**                                                                                                                                          |
| Enable hardware decoding for  | what your GPU generation supports (see [Jellyfin's NVIDIA docs](https://jellyfin.org/docs/general/post-install/transcoding/hardware-acceleration/nvidia)) |
| Enable enhanced NVDEC decoder | On                                                                                                                                                        |
| Enable hardware encoding      | On                                                                                                                                                        |
| Enable Tone mapping           | On (CUDA)                                                                                                                                                 |

**Linux agent with Intel or AMD:** use Jellyfin's normal Linux settings for
that GPU (QSV for Intel, VA-API for AMD), exactly as if the GPU were in the
Jellyfin machine. No restrictions apply, because nothing is translated.

### 6. Verify it works

1. On Windows: `C:\ProgramData\ffagent\ffagent.exe status` should show
   `Status: OK`, a jellyfin-ffmpeg version, `GPU test: OK` and every path `ok`.
2. From the container:
    ```powershell
    docker exec jellyfin /opt/ffshim/ffmpeg -ffshim-ping
    ```
    should print `ok: ffagent ..., jellyfin-ffmpeg ...`.
3. Jellyfin → Dashboard → Logs → the newest `log_*.log`: the line
   `Available hwaccel types` must include `qsv` and `opencl`, and with Intel on
   Windows also **`vaapi`** (`cuda` for NVIDIA).
4. Play a 4K HDR video and force a lower quality (gear icon → Quality → 1080p).
    - Task Manager on Windows → Performance → GPU should show **Video Decode**
      and **Video Encode** activity from `ffmpeg.exe`.
    - The newest `FFmpeg.Transcode-*.log` (Dashboard → Logs) should contain
      the hardware decoder and encoder (`hevc_qsv`/`h264_qsv` or
      `cuda`/`h264_nvenc`) and `tonemap_opencl` or `tonemap_cuda`.

If anything fails, see [troubleshooting](docs/troubleshooting.md).

### 7. Optional: monitoring

Set up the [Home Assistant notifications](docs/home-assistant.md) to be told
when the agent is down or transcoding stops working, for example after a
Jellyfin update.

---

## Updating

| What                          | How                                                                                                                                                                                                                        |
| ----------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Jellyfin**                  | As before (Watchtower or `docker compose pull`).                                                                                                                                                                           |
| **ffmpeg on a Windows agent** | Automatic: the agent notices the new jellyfin-ffmpeg version on the next connection and installs it before running the job. If that fails, the previous version keeps working and `/health` reports `version_sync_failed`. |
| **ffmpeg on a Linux agent**   | Comes with the agent's Jellyfin image. Let Watchtower update it too (same tag as Jellyfin); `/health` reports `version_mismatch` until both match.                                                                         |
| **This project**              | `git pull`, rebuild `ffagent.exe` (step 2), run `ffagent.exe install --compose ..\docker-compose.yml` again (keeps your token and settings), then `docker compose up -d --build`.                                          |

If Jellyfin was updated and started before the new ffmpeg was ready, `/health`
reports `restart_jellyfin`; run `docker restart jellyfin`.

A Jellyfin update can occasionally change how it builds ffmpeg commands. With
translation (Intel on Windows), the agent then refuses them with a clear error
instead of guessing, and Home Assistant tells you (`translation_rejected`). If
you'd rather update Jellyfin by hand, run Watchtower with `--monitor-only` for
the Jellyfin container.

---

## Reference

### `ffagent` commands

On Windows run them from `C:\ProgramData\ffagent\` (install copies itself
there). `install`, `uninstall` and `restart` are Windows-only and need an
Administrator terminal.

| Command                                                           | Does                                                                                                    |
| ----------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------- |
| `install [--compose FILE] [--service NAME] [--gpu intel\|nvidia]` | Install or update; see [step 3](#3-install-the-agent). Without `--compose` it keeps the existing paths. |
| `status [--json]`                                                 | Show health: versions, GPU test, paths, jobs, last error.                                               |
| `token`                                                           | Print the token for `FFSHIM_TOKEN`.                                                                     |
| `selftest`                                                        | Run the GPU self-test with the installed ffmpeg.                                                        |
| `restart`                                                         | Restart the agent (after editing the config).                                                           |
| `uninstall`                                                       | Remove the task, firewall rule and links. Keeps `C:\ProgramData\ffagent`.                               |
| `run`                                                             | Run in the foreground (what the Windows task and the Linux container run).                              |

### Config file: `C:\ProgramData\ffagent\ffagent.conf`

Generated by `install`. Edit it with Notepad, then run `ffagent restart`.

```ini
token = 4c1f...                  # shared secret, same as FFSHIM_TOKEN
gpu = intel                      # intel | nvidia | none (Linux also: vaapi)
# translate = auto               # auto | none | qsv-windows
# listen = 0.0.0.0:7878          # job port (the shim connects here)
# health_listen = 0.0.0.0:7879   # read-only /health for monitoring
# drive = C:                     # Windows: container paths are mirrored at this drive's root
# ffmpeg = C:\path\ffmpeg.exe    # use a fixed ffmpeg and disable version sync
# device = /dev/dri/renderD128   # Linux: render node for intel / vaapi

[paths]                          # container path = host folder (or \\server\share)
/cache = C:\docker\jellyfin\cache
/config = C:\docker\jellyfin\config
/media6 = D:\Videos\Movies

[credentials]                    # Windows: logins for network shares
# \\server\share = username:password
```

`translate = auto` means `qsv-windows` for `gpu = intel` on Windows, and `none`
everywhere else.

On Windows, **every volume the Jellyfin container uses must be listed**, with
exactly the same container path. A real folder already sitting at a link
location (for example an existing `C:\config`) is never touched; change `drive`
to another drive letter instead. On Linux, `[paths]` is optional and only checks
that the paths exist.

### Agent environment variables

Every setting can also come from the environment. That's how the Linux agent
container is configured; no config file is needed there.

| Variable                                  | Setting                                                                             |
| ----------------------------------------- | ----------------------------------------------------------------------------------- |
| `FFAGENT_TOKEN`                           | `token` (required without a config file)                                            |
| `FFAGENT_GPU`                             | `gpu`                                                                               |
| `FFAGENT_TRANSLATE`                       | `translate`                                                                         |
| `FFAGENT_FFMPEG`                          | `ffmpeg` (Linux default: `/usr/lib/jellyfin-ffmpeg/ffmpeg` from the Jellyfin image) |
| `FFAGENT_DEVICE`                          | `device`                                                                            |
| `FFAGENT_LISTEN`, `FFAGENT_HEALTH_LISTEN` | `listen`, `health_listen`                                                           |
| `FFAGENT_DRIVE`                           | `drive`                                                                             |
| `FFAGENT_HOME`                            | data folder (default `C:\ProgramData\ffagent`, or `/var/lib/ffagent` on Linux)      |

### Shim environment variables (Jellyfin container)

| Variable                | Default                     |                                                                              |
| ----------------------- | --------------------------- | ---------------------------------------------------------------------------- |
| `JELLYFIN_FFMPEG`       |                             | Must be `/opt/ffshim/ffmpeg` (set by the override file).                     |
| `FFSHIM_TOKEN`          |                             | Required. From `.env`.                                                       |
| `FFSHIM_AGENT`          | `host.docker.internal:7878` | Agent address; `<ip>:7878` for another machine.                              |
| `FFSHIM_FFMPEG_VERSION` | detected                    | Override the detected jellyfin-ffmpeg version; `none` disables version sync. |

### Ports

| Port     | Used by             | Exposure                                                  |
| -------- | ------------------- | --------------------------------------------------------- |
| 7878/tcp | shim → agent (jobs) | Local subnet (Windows firewall rule). Requires the token. |
| 7879/tcp | `/health`           | Local subnet. Read-only, no secrets.                      |

### Files on Windows

| Path                                       |                                         |
| ------------------------------------------ | --------------------------------------- |
| `C:\ProgramData\ffagent\ffagent.exe`       | the agent                               |
| `C:\ProgramData\ffagent\ffagent.conf`      | config (contains the token)             |
| `C:\ProgramData\ffagent\ffagent.log`       | log (rotated at 10 MB)                  |
| `C:\ProgramData\ffagent\ffmpeg\<version>\` | downloaded jellyfin-ffmpeg builds       |
| `C:\<container path>`                      | links to your folders, e.g. `C:\media6` |

---

## Uninstalling

1. Delete `docker-compose.override.yml` (or remove the shim parts from it), remove
   `FFSHIM_TOKEN` from `.env`, and run `docker compose up -d`.
2. In Jellyfin, switch hardware acceleration off (or back to what you used before).
3. Windows: in an Administrator terminal run
   `C:\ProgramData\ffagent\ffagent.exe uninstall`, then delete `C:\ProgramData\ffagent`.
   Linux: `docker compose down` in the agent's folder.

---

## Development

```sh
CGO_ENABLED=0 go test ./...                          # unit + end-to-end tests (Linux/macOS)
GOOS=windows go vet ./...                            # check the Windows code
FFAGENT_LIVE=1 go test -run Live ./cmd/ffagent       # query the real GitHub releases
```

Layout:

```
cmd/ffshim/          the shim (runs in the Jellyfin container)
cmd/ffagent/         the agent: server, health, version sync, GPU self-tests,
                     Windows install (install_windows.go, platform_windows.go)
internal/proto/      wire protocol (framing, HMAC challenge)
internal/translate/  translation profiles (none, qsv-windows)
internal/version/    jellyfin-ffmpeg version parsing
internal/compose/    reads volumes from docker-compose.yml
examples/            override file, .env example, Linux agent compose file
docs/                separate machines, Linux agent, Home Assistant, troubleshooting
```

No third-party Go modules: Windows APIs are called through the standard
library's `syscall` package. The Docker image (`Dockerfile`) contains the shim
and the Linux agent. Pushing a `v*` tag runs `.github/workflows/release.yml`,
which publishes `ffagent.exe`, Linux binaries and a multi-arch image to GHCR.
Release builds of `ffagent.exe` reference that image, so users of a release
don't need to clone the repository.

---

## License

Copyright (C) 2026 the jellyfin-ffmpeg-bridge contributors.

This program is free software: you can redistribute it and/or modify it under
the terms of the GNU General Public License, version 2 (GPL-2.0-only), the
same license as Jellyfin. It is distributed WITHOUT ANY WARRANTY. See
[LICENSE](LICENSE) for the full text.

The jellyfin-ffmpeg builds the agent downloads at runtime (Windows) or uses
from the Jellyfin image (Linux) come from the
[jellyfin-ffmpeg](https://github.com/jellyfin/jellyfin-ffmpeg) project under
their own (GPL) license; this project doesn't redistribute them.
