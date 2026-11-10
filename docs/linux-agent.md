# Agent on a Linux machine

Use this when Jellyfin runs on one machine (for example a NAS without a usable
GPU) and you want transcoding on **another Linux machine** with an Intel, AMD
or NVIDIA GPU.

Both sides run Linux, so **nothing is translated**. Jellyfin's commands run
exactly as they would locally, and every Jellyfin hardware acceleration mode
works, including VA-API for AMD.

```
 Machine A: Jellyfin                                Machine B: GPU
┌────────────────────────────────┐   TCP :7878    ┌──────────────────────────────────────┐
│ jellyfin/jellyfin              │───────────────▶│ jellyfin/jellyfin image running      │
│   JELLYFIN_FFMPEG=ffshim       │                │   ffagent (instead of Jellyfin)      │
│   /cache /config /media        │                │   /cache /config /media (same paths) │
└────────────────────────────────┘                └──────────────────────────────────────┘
          ▲                                                     │
          └──────────────── shared folders (NFS / SMB) ─────────┘
```

**Why the agent runs in the Jellyfin image:** that image already contains
jellyfin-ffmpeg and the Intel, AMD and NVIDIA user-space drivers Jellyfin
needs. Starting it with the agent as the entrypoint means machine B uses
exactly the same ffmpeg and drivers as machine A, and Watchtower updates both
together.

## Requirements

- Machine B: Linux with Docker and a GPU that works for Jellyfin in Docker
  (`/dev/dri` for Intel or AMD, or the NVIDIA Container Toolkit), as described
  in [Jellyfin's hardware acceleration docs](https://jellyfin.org/docs/general/post-install/transcoding/hardware-acceleration/).
- Machine B must see **every folder Jellyfin uses** (`/cache`, `/config`,
  media) and must be able to **write** to Jellyfin's cache, because ffmpeg
  writes the transcoded segments there.
- A wired network between the machines (a 4K remux peaks around 80–150 Mbit/s).

## 1. Share Jellyfin's folders with machine B

Mount Jellyfin's cache, config and media folders on machine B, for example
over NFS:

```sh
# machine B
sudo mkdir -p /mnt/jellyfin/cache /mnt/jellyfin/config /mnt/nas/media
sudo mount -t nfs machine-a:/srv/jellyfin/cache  /mnt/jellyfin/cache
sudo mount -t nfs machine-a:/srv/jellyfin/config /mnt/jellyfin/config
sudo mount -t nfs nas:/volume1/media             /mnt/nas/media
```

Add them to `/etc/fstab` so they come back after a reboot.

**Permissions:** the agent container runs ffmpeg as root, as the Jellyfin image
does by default. With NFS `root_squash`, root becomes `nobody`, so the cache
export must allow that user to write. Either export the cache with
`no_root_squash`, or run both the Jellyfin and the agent container with the
same `user: "uid:gid"` that owns the folders.

## 2. Start the agent (machine B)

```sh
mkdir ~/ffagent && cd ~/ffagent
git clone https://github.com/<you>/jellyfin-ffmpeg-bridge
cp jellyfin-ffmpeg-bridge/examples/linux-agent/docker-compose.yml .
echo "FFSHIM_TOKEN=$(openssl rand -hex 24)" > .env
```

Edit `docker-compose.yml`:

- `FFAGENT_GPU`: `intel` (Quick Sync), `vaapi` (AMD, or Intel through plain
  VA-API), `nvidia`, or `none`. This only selects the self-test.
- `volumes`: one line per volume of your Jellyfin container, with the
  **same container path** on the right-hand side. The left-hand side is where
  machine B mounted it in step 1.
- NVIDIA: replace `devices` with `runtime: nvidia` and the `NVIDIA_*`
  variables (see the comments).
- `image`: the same tag as your Jellyfin (e.g. `jellyfin/jellyfin:10.11.0`) if
  you pin versions.

```sh
docker compose up -d --build
docker exec ffagent /opt/ffshim/ffagent status
```

`status` should show `Status: OK` and `GPU test: OK`. If the GPU test fails,
check the device or runtime configuration exactly as you would for Jellyfin.

## 3. Point Jellyfin at the agent (machine A)

Next to Jellyfin's `docker-compose.yml`:

```sh
git clone https://github.com/<you>/jellyfin-ffmpeg-bridge
cp jellyfin-ffmpeg-bridge/examples/docker-compose.override.yml .
```

In `docker-compose.override.yml`, uncomment and set
`FFSHIM_AGENT: <machine-b-ip>:7878`. Put the **same** token in `.env`:

```sh
echo "FFSHIM_TOKEN=<token from machine B's .env>" >> .env
docker compose up -d --build
docker exec jellyfin /opt/ffshim/ffmpeg -ffshim-ping
```

## 4. Jellyfin settings

Configure hardware acceleration in Jellyfin **for machine B's GPU**, exactly
as Jellyfin's docs describe for Linux: QSV or VA-API for Intel, VA-API for
AMD, NVENC for NVIDIA. Device paths such as `/dev/dri/renderD128` refer to
machine B. Then `docker restart jellyfin` so it re-detects ffmpeg's
capabilities through the agent.

Verify as in the [README](../README.md#6-verify-it-works). `Available hwaccel
types` in Jellyfin's log now shows what machine B's ffmpeg supports.

## Versions

The agent reports its own jellyfin-ffmpeg version (from its Jellyfin image)
and the version Jellyfin's image ships. If they differ, `/health` reports
`version_mismatch`. Keep both containers on the same image tag, or let
Watchtower update both.

## Security

The token never crosses the network (challenge/response), but the connection
isn't encrypted: file paths and ffmpeg output are readable on the wire. Keep
both machines on a trusted LAN, or connect them over WireGuard or Tailscale
and use those addresses. Only publish ports 7878/7879 on the interface
Jellyfin uses, e.g. `"192.168.1.20:7878:7878"`.
