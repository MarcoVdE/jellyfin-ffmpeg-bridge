# Monitoring with Home Assistant

Get a phone notification when the agent is down or transcoding stops working,
and another one when it recovers.

The agent serves a read-only JSON status on `http://<windows-ip>:7879/health`.
Open it in a browser to see what Home Assistant will read.

## What `/health` reports

```json
{
    "status": "degraded",
    "problems": ["translation_rejected"],
    "agent_version": "v1.0.0",
    "uptime_s": 86400,
    "ffmpeg": { "version": "8.1.3-2", "expected": "8.1.3-2", "in_sync": true, "syncing": false, "last_sync": "2026-11-10T03:12:00Z", "last_sync_error": "" },
    "selftest": { "ok": true, "last_run": "2026-11-10T09:00:00Z", "error": "" },
    "shim": { "last_seen": "2026-11-10T09:04:00Z", "last_ping": "2026-11-10T09:04:00Z", "version": "v1.0.0", "jellyfin_probed_ffmpeg": "8.1.3-2" },
    "jobs": { "active": 1, "total": 523, "cancelled": 80, "failed_15m": 0, "rejected_15m": 1, "hw_errors_15m": 0 },
    "paths": [{ "container": "/media6", "link": "C:\\media6", "target": "D:\\Videos\\Movies", "ok": true }],
    "last_error": { "time": "2026-11-10T08:58:10Z", "kind": "rejected", "message": "cannot run on Windows: argument \"-hwaccel vaapi\" is VA-API only. In Jellyfin > ..." }
}
```

- `status` is `ok` when `problems` is empty, otherwise `degraded`.
- **Down** is not a status: it means `/health` doesn't answer. Home Assistant
  marks the sensors `unavailable`, and the automation below reacts to that.

| Problem                | Meaning                                                                                                                                  | What to do                                                                                                                                                    |
| ---------------------- | ---------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `translation_rejected` | **The shim no longer understands Jellyfin's commands**: a transcoding setting changed, or a Jellyfin update builds commands differently. | Check the [Jellyfin settings](../README.md#4-jellyfin-settings). The notification includes the hint. If the settings are right, this project needs an update. |
| `hw_errors`            | ffmpeg failed with a GPU or driver error.                                                                                                | `ffagent selftest`; update or reinstall the Intel driver.                                                                                                     |
| `selftest_failed`      | The periodic GPU self-test (every 15 min) fails.                                                                              | Same as above; often follows a Windows or driver update.                                                                                                      |
| `auth_failed`          | Something connected with the wrong token.                                                                                                | `ffagent token`, fix `.env`, `docker compose up -d`.                                                                                                          |
| `shim_not_seen`        | No heartbeat from the container for 15+ minutes.                                                                                         | The container is stopped, or can't reach the agent: `docker ps`, then `docker exec jellyfin /opt/ffshim/ffmpeg -ffshim-ping`.                                 |
| `version_mismatch`     | Windows ffmpeg differs from the Jellyfin image's (normally only while downloading).                                                      | Wait a few minutes.                                                                                                                                           |
| `version_sync_failed`  | Download or self-test of the new ffmpeg failed; the old version is still used.                                                           | See `last_sync_error`; retried hourly.                                                                                                                        |
| `restart_jellyfin`     | ffmpeg was updated after Jellyfin had detected its capabilities.                                                                         | `docker restart jellyfin` (could be automated).                                                                                                               |
| `path_links`           | A container path can't be mirrored (folder missing, share offline).                                                                      | `ffagent status` shows which path and why.                                                                                                                    |
| `no_ffmpeg`            | No jellyfin-ffmpeg downloaded yet.                                                                                                       | Normal for a minute after install; otherwise check internet access and `last_sync_error`.                                                                     |

Jobs that Jellyfin cancels itself (seeking, stopping, switching quality) are
counted as `cancelled`, not failed. Failures caused by a broken file count in
`failed_15m` but don't degrade the status; only GPU or pipeline errors do.

## 1. Sensors

Add to `configuration.yaml`, replace `192.168.1.50` with the Windows machine's
IP, and restart Home Assistant:

```yaml
rest:
    - resource: http://192.168.1.50:7879/health
      scan_interval: 60
      timeout: 10
      sensor:
          - name: "FFagent status"
            unique_id: ffagent_status
            value_template: "{{ value_json.status }}"
            json_attributes: [problems, ffmpeg, selftest, shim, jobs, last_error]
          - name: "FFagent active transcodes"
            unique_id: ffagent_active_transcodes
            value_template: "{{ value_json.jobs.active }}"
            state_class: measurement
          - name: "FFagent ffmpeg version"
            unique_id: ffagent_ffmpeg_version
            value_template: "{{ value_json.ffmpeg.version }}"
      binary_sensor:
          - name: "FFagent problem"
            unique_id: ffagent_problem
            device_class: problem
            value_template: "{{ value_json.status != 'ok' }}"

    # Optional: tells "Jellyfin is down" apart from "the agent is down".
    - resource: http://192.168.1.50:8096/health
      scan_interval: 60
      sensor:
          - name: "Jellyfin health"
            unique_id: jellyfin_health
            value_template: "{{ value }}" # "Healthy" when OK
```

This creates `sensor.ffagent_status`, `sensor.ffagent_active_transcodes`,
`sensor.ffagent_ffmpeg_version`, `binary_sensor.ffagent_problem` and
`sensor.jellyfin_health`.

The Windows firewall rule created by `ffagent install` allows your local
subnet. If Home Assistant is on another subnet or VPN, see
[separate machines → firewall](separate-machines.md#firewall).

## 2. Notifications

Find your phone's notify action under **Developer Tools → Actions** (search for
`notify.mobile_app_`) and replace `notify.mobile_app_your_phone` below.

Add to `automations.yaml`, or paste each one into a new automation via
**Edit in YAML**:

```yaml
- alias: "FFagent: down"
  triggers:
      - trigger: state
        entity_id: sensor.ffagent_status
        to: unavailable
        for: "00:03:00"
  actions:
      - action: notify.mobile_app_your_phone
        data:
            title: "Jellyfin transcoding DOWN"
            message: >
                The ffmpeg agent is not responding.
                Jellyfin itself: {{ states('sensor.jellyfin_health') }}.

- alias: "FFagent: problem"
  triggers:
      - trigger: state
        entity_id: binary_sensor.ffagent_problem
        to: "on"
        for: "00:02:00"
  actions:
      - action: notify.mobile_app_your_phone
        data:
            title: "Jellyfin transcoding problem"
            message: >
                {% set p = state_attr('sensor.ffagent_status', 'problems') or [] %}
                {% set e = state_attr('sensor.ffagent_status', 'last_error') %}
                {{ p | join(', ') }}{% if e %}: {{ e.message }}{% endif %}

- alias: "FFagent: recovered"
  triggers:
      - trigger: state
        entity_id: sensor.ffagent_status
        to: ok
  conditions:
      - condition: template
        value_template: "{{ trigger.from_state.state in ['unavailable', 'degraded'] }}"
  actions:
      - action: notify.mobile_app_your_phone
        data:
            title: "Jellyfin transcoding OK"
            message: "Working again (jellyfin-ffmpeg {{ states('sensor.ffagent_ffmpeg_version') }})."
```

The `for:` delays avoid alerts during a reboot, a Watchtower update or a short
ffmpeg download.

**Reading the "down" alert:**

| Jellyfin health | Likely cause                              | Fix                                                                                       |
| --------------- | ----------------------------------------- | ----------------------------------------------------------------------------------------- |
| `Healthy`       | Agent stopped, or firewall changed        | Administrator terminal: `C:\ProgramData\ffagent\ffagent.exe restart`; check `ffagent.log` |
| `unavailable`   | Windows machine or Docker Desktop is down | Check the machine                                                                         |

## 3. Optional: restart Jellyfin automatically

To handle `restart_jellyfin` without you, call Jellyfin's restart API from Home
Assistant. Create an API key in Jellyfin (**Dashboard → API Keys**) and put it
in `secrets.yaml` as `jellyfin_api_key`.

`configuration.yaml`:

```yaml
rest_command:
    jellyfin_restart:
        url: http://192.168.1.50:8096/System/Restart
        method: POST
        headers:
            Authorization: !secret jellyfin_auth_header # MediaBrowser Token="<api key>"
```

`secrets.yaml`:

```yaml
jellyfin_auth_header: 'MediaBrowser Token="paste-api-key-here"'
```

Automation:

```yaml
- alias: "FFagent: restart Jellyfin after ffmpeg update"
  triggers:
      - trigger: template
        value_template: >
            {{ 'restart_jellyfin' in (state_attr('sensor.ffagent_status', 'problems') or [])
               and (state_attr('sensor.ffagent_status', 'jobs') or {}).get('active', 0) == 0 }}
        for: "00:05:00"
  actions:
      - action: rest_command.jellyfin_restart
```

It only restarts when no transcode is running, so nobody's playback is interrupted.
