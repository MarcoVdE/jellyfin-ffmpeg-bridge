# Troubleshooting

Start with these two commands; most problems show up in one of them:

```powershell
C:\ProgramData\ffagent\ffagent.exe status          # Windows side
docker exec jellyfin /opt/ffshim/ffmpeg -ffshim-ping   # container → agent
```

Logs:

- Agent: `C:\ProgramData\ffagent\ffagent.log`
- Jellyfin: Dashboard → Logs. Each transcode has an `FFmpeg.Transcode-*.log`
  containing the full command and ffmpeg's output, including any `ffshim:`
  error line.

---

### `ffshim: cannot reach ffagent at host.docker.internal:7878`

- Is the agent running? `ffagent status`. If not: Administrator terminal →
  `ffagent restart`, then read `ffagent.log`.
- On separate machines, check `FFSHIM_AGENT` and the
  [firewall rule](separate-machines.md#firewall).
- Docker Desktop normally routes `host.docker.internal` to the Windows host.
  Test from the container: `docker exec jellyfin curl -s http://host.docker.internal:7879/health`.

### `ffshim: authentication failed`

The token in `.env` doesn't match the agent's. Run `ffagent token`, fix `.env`,
and run `docker compose up -d`. Changes to `.env` only apply when the container
is recreated, not on `docker restart`.

### `cannot run on Windows: argument ... is VA-API only`

A Jellyfin transcoding setting is wrong; the message names it. Usually:

- _Prefer OS native DXVA or VA-API hardware decoders_ must be **off**;
- _Enable VPP Tone mapping_ must be **off**;
- Hardware acceleration must be **Intel QuickSync (QSV)**, not VA-API.

If all settings are right, a Jellyfin update changed how it builds commands.
Open an issue with the command line from the `FFmpeg.Transcode-*.log`.

### Jellyfin shows no hardware acceleration, or transcodes on the CPU

Jellyfin detects ffmpeg's capabilities **once at startup**. If the agent was
not running, or ffmpeg was still downloading, it may have detected nothing.

1. `ffagent status` must show `Status: OK` and a jellyfin-ffmpeg version.
2. `docker restart jellyfin`.
3. In the newest Jellyfin log, `Available hwaccel types` must list `qsv`,
   `opencl` **and `vaapi`**. Without `vaapi`, Jellyfin falls back to CPU
   tone mapping; that means it isn't using the shim, so check `JELLYFIN_FFMPEG`
   (`docker exec jellyfin printenv JELLYFIN_FFMPEG` → `/opt/ffshim/ffmpeg`).
4. In the transcode log, look for `tonemap_opencl`. If you see `tonemapx` or
   `zscale` instead, tone mapping runs on the CPU: check that _Enable Tone
   mapping_ is on and the agent's self-test passes.

### GPU self-test fails (`selftest_failed`, `hw_errors`)

```powershell
C:\ProgramData\ffagent\ffagent.exe selftest
```

- Install the latest Intel graphics driver from Intel's site. Windows Update
  drivers sometimes lack OpenCL.
- If the test **passes** from your normal terminal but fails in `status`, the
  GPU is not usable from the background session the task runs in. Run the
  agent in your desktop session instead. Open **Task Scheduler → ffagent →
  Properties**:
    - _General_: **Change User or Group…** → your user, select _Run only when
      user is logged on_, keep _Run with highest privileges_;
    - _Triggers_: replace _At startup_ with _At log on_ of your user.

    Then run `ffagent restart`. The agent now only runs while you're logged in,
    so enable automatic sign-in on the server (for example with Microsoft's
    Autologon tool). `[credentials]` still work for network shares.

### `path_links`: a path shows an error in `ffagent status`

- _Windows folder ... is not reachable_: the folder in `ffagent.conf` doesn't
  exist, or the share is offline or needs `[credentials]`. The agent retries
  every minute.
- _already exists and is a real folder_: something real already exists at, for
  example, `C:\config`. It is never deleted. Rename it, or set `drive = E:` (any
  local drive) in `ffagent.conf` and run `ffagent restart`.
- A path is missing entirely: every volume of the Jellyfin container needs a
  line under `[paths]`. Run `ffagent install --compose ...` again after
  changing your compose file.

### Playback starts, then stalls or errors immediately

- Look at the newest `FFmpeg.Transcode-*.log`. If ffmpeg ran but Jellyfin
  can't see the segments, the `/cache` mapping is wrong: the agent must write
  to the same folder the container mounts as `/cache`. Compare `ffagent status`
  with your compose file.
- If Jellyfin can't delete old transcode files, the folder behind `/cache` has
  unusual permissions. Files are created by SYSTEM; give _Users_ "Modify" on
  that folder.

### Restart notifications (`restart_jellyfin`)

ffmpeg on Windows was updated after Jellyfin last detected its capabilities.
`docker restart jellyfin` clears it. Home Assistant can
[do this automatically](home-assistant.md#3-optional-restart-jellyfin-automatically).

### Library scans are slow

That comes from Docker Desktop sharing Windows folders into WSL2 and is
unrelated to this project.

### NVIDIA: self-test or transcodes fail

- `ffagent selftest` runs a short CUDA scale + NVENC encode. Errors like
  `no capable devices found` or `cannot load libcuda` mean the NVIDIA driver
  isn't installed or is too old for the jellyfin-ffmpeg version. Install the
  current driver from NVIDIA.
- Check that `gpu = nvidia` is set (`ffagent status` prints it). With
  `gpu = intel`, the agent would try to translate commands for Quick Sync.
- Consumer GPUs limit how many NVENC sessions run at once. Extra transcodes
  fail with `OpenEncodeSessionEx failed`.

### Linux agent: GPU test fails or `/dev/dri` permission denied

The agent container needs exactly the same GPU access as a Jellyfin container
on that machine: `devices: /dev/dri:/dev/dri` for Intel or AMD (plus
`group_add` with the render group's id if you run it with `user:`), or the
NVIDIA Container Toolkit with `runtime: nvidia`. Test outside the agent:

```sh
docker exec ffagent /usr/lib/jellyfin-ffmpeg/vainfo     # Intel / AMD
docker exec ffagent nvidia-smi                          # NVIDIA
```

### Linux agent: `not found on this machine: mount ... at ...`

A `[paths]` entry (or a path Jellyfin uses) isn't mounted in the agent
container at the same container path as in Jellyfin's container. Compare the
`volumes:` of both compose files; the right-hand sides must be identical.

### Collecting details for a bug report

```powershell
C:\ProgramData\ffagent\ffagent.exe status --json
Get-Content C:\ProgramData\ffagent\ffagent.log -Tail 100
```

Plus the failing `FFmpeg.Transcode-*.log` from Jellyfin. Remove the token if it
appears anywhere (it shouldn't).
