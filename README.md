# plexiptv

**Version 1.0**

plexiptv makes your IPTV provider show up in Plex as one or more HDHomeRun network tuners, the same idea as xTeVe. It is built to stay fast with very large playlists (50k+ entries) and to make filtering simple. It is a single binary with no dependencies, and runs on Windows (including as a service) and Linux.

- **Sources:** Xtream Codes accounts (server, username, password), M3U playlists (URL or local file) and extra XMLTV guides.
- **VOD and series are skipped at import.** Xtream sources only request the live list. M3U entries pointing at `/movie/`, `/series/` or video files are dropped while the file is parsed. A 60k-channel import takes about 3 s.
- **Filtering:**
  - Tick whole groups on the Groups page.
  - Write ordered include/exclude rules (contains, equals, starts with, regex) on the Rules page. Each rule shows a live match count and sample matches while you type.
  - Override single channels on the Channels page: on/off, number, name, logo, EPG id.
- **Stable channel numbers.** Numbers are stored per channel, keyed by the provider's stream id. They do not move when the provider reorders or briefly drops channels, so Plex's guide mapping doesn't break.
- **Multiple tuners.** Plex handles about 480 channels per DVR device. Each tuner has a channel limit (default 480): only the lowest-numbered channels up to the limit are sent to Plex, so an over-broad rule can't stall Plex setup. Add more tuners (each on its own port) and send groups to them by rule.
- **Stream proxy:**
  - One provider connection is shared by everyone watching the same channel.
  - The provider connection limit is enforced, and a clean "all tuners busy" is returned when it's reached.
  - Stalled streams reconnect automatically.
  - HLS (`.m3u8`) streams are remuxed to MPEG-TS with ffmpeg.
- **Looks like a TV to your provider.** Playlist, guide and stream requests go out with the User-Agent and headers of a Samsung Smart TV by default (not `Go-http-client` or ffmpeg's `Lavf`). Choose LG webOS, Fire TV, VLC or a custom string under **Settings → Provider identity**, or per source.
- **Guide:** XMLTV generated for only your lineup, with "auto-match EPG by name" and placeholder entries so Plex doesn't hide channels that have no guide data.

## Quick start (Windows)

1. Download or build `plexiptv-windows-amd64.exe` and put it in its own folder, e.g. `C:\plexiptv\`.
2. Run it. It creates `C:\plexiptv\data\` (config, database, guides, log).
3. Open `http://localhost:34500/` and choose a panel password.
4. **Sources** → `+ Xtream account` (or `+ M3U playlist`). The import runs in the background.
5. **Groups** → tick the groups you want. Fine-tune them on **Rules** and **Channels**.
6. In Plex: **Settings → Live TV & DVR → Set up Plex DVR**.
   - If the tuner isn't listed, type the address shown on the dashboard, e.g. `192.168.1.20:34500`.
   - When asked for a guide, choose **"Have an XMLTV guide on your server?"** and enter `http://192.168.1.20:34500/xmltv.xml`.

The default port is **34500**, so plexiptv can run next to xTeVe (which uses 34400) while you switch over.

### More than ~480 channels: one DVR per tuner

Plex treats each tuner as a separate DVR. For a lineup like this:

| Tuner | Port | Channels |
|---|---|---|
| plexiptv | 34500 | Entertainment, News, Movies |
| plexiptv Sports | 34501 | Sports networks, team channels |
| plexiptv Events | 34502 | ESPN+, Peacock, PPV and other event feeds |

1. Create the extra tuners on the **Tuners** page, each with its own port and start number (e.g. 2000, 3000).
2. Point include rules at them with the rule's *tuner* option.
3. In Plex, add each one as its own DVR at `IP:port`. On the guide step, use **that tuner's** guide, e.g. `http://192.168.1.20:34501/xmltv.xml`. Each tuner's guide holds only its own channels.
4. If you change rules later, click **Renumber…** on the Channels page (filtered to one tuner) to get tidy numbers again.

### Run as a Windows service

From an elevated prompt in the program folder:

```
plexiptv-windows-amd64.exe -service install
plexiptv-windows-amd64.exe -service start
```

`-service stop | restart | uninstall` work too. The service uses the same `data` folder next to the exe.

### Firewall

Plex must be able to reach the web/tuner ports. For automatic discovery it also needs UDP 65001 (HDHomeRun protocol) and UDP 1900 (SSDP):

```powershell
New-NetFirewallRule -DisplayName "plexiptv" -Direction Inbound -Program "C:\plexiptv\plexiptv-windows-amd64.exe" -Action Allow
```

Windows' own *SSDP Discovery* service may already hold UDP 1900. The log says so, and that's harmless: Plex still finds the tuner over UDP 65001, or you can add it by IP:port.

### ffmpeg (optional)

ffmpeg is only needed for HLS sources. Install it and either put it on `PATH` or set `ffmpeg_path` in `data\config.yaml`. The Settings page shows whether it was found.

## Linux

```
./plexiptv-linux-amd64 -data /var/lib/plexiptv
sudo ./plexiptv-linux-amd64 -data /var/lib/plexiptv -service install   # systemd unit
sudo ./plexiptv-linux-amd64 -service start
```

## Configuration

Most settings live in the web panel. `data/config.yaml` holds the few bootstrap settings:

```yaml
listen: :34500          # web panel + default tuner
advertise_host: ""      # force the IP/host put in tuner URLs (auto = the address Plex connected to)
ffmpeg_path: ""         # default: ffmpeg on PATH
ssdp: true              # UDP discovery (HDHomeRun 65001 + SSDP 1900)
log_level: info
```

## How filtering works

1. **Rules** are evaluated top to bottom, and the **last matching rule wins**, like `.gitignore`. Put broad includes first and exceptions after them:
   1. include · group · starts with · `US |`
   2. exclude · name · matches · `\b(4K|UHD|HEVC)\b`
   3. include · group · equals · `US | Sports` → tuner *plexiptv Sports*
   4. exclude · name · contains · `NO EVENT` (empty event placeholders)
2. A **manual on/off** on the Channels page beats every rule. "Follow rules" clears it.
3. Matching ignores case. A rule can be limited to one source. An include rule can also send its channels to a specific tuner.

Prefer several narrow "group equals" rules (the Groups page creates them for you) over one broad "group contains `USA`". Providers often have thousands of event slots and free-TV channels in such groups, and the lowest-numbered ones are what fills a tuner's 480.

## Endpoints

| Path | Purpose |
|---|---|
| `/discover.json`, `/lineup.json`, `/lineup_status.json`, `/device.xml` | HDHomeRun API (per tuner port) |
| `/stream/{id}` | Proxied channel stream |
| `/xmltv.xml` | Guide for the lineup |
| `/lineup.m3u` | Your filtered lineup as M3U (for VLC, Jellyfin, etc.) |

## Troubleshooting

Check the **Logs** page first, or `data/plexiptv.log`. Credentials are redacted from the logs.

| Problem | What to check |
|---|---|
| Plex setup stuck on "Finding the best guide" | The lineup is too big. Keep each tuner under ~480 channels; the Dashboard shows "sent to Plex" per tuner. On the guide step, choose *"Have an XMLTV guide on your server?"* and use `/xmltv.xml`. |
| Plex setup fails right after reading the lineup | Plex test-plays a channel during setup. If the first channels are empty event slots (`NFHS Network 12: NO EVENT`), the provider never answers and Plex gives up. The log shows `upstream interrupted … timeout awaiting response headers`. Exclude them (name contains `NO EVENT`) and redo the setup. |
| Not sure what Plex did | Every Plex request (`discover.json`, `lineup.json`, `xmltv.xml`, …) is logged as `tuner request`, with status and user agent. |
| Plex doesn't find the tuner | Add it by `IP:port` from the dashboard. Allow the exe through the firewall. UDP 65001 must be reachable for auto-discovery. |
| Channels missing in Plex | Plex hides channels that have no guide data: keep *placeholder guide entries* on (Settings). Check the tuner isn't over ~480 channels. |
| "All tuners are in use" | The provider's connection limit was reached. See *Sources → streams x/y*. Set *Max concurrent streams* if the provider reports it wrong. |
| Provider returns 403 / blocks the app | Try another device under *Settings → Provider identity* (e.g. the one your provider's own app or your TV uses). Redirect stream mode bypasses this, since Plex then connects directly. |
| Stream won't start (HLS source) | Install ffmpeg and set `ffmpeg_path`. Settings shows whether it was found. |
| Panel shows old channel list | Click *Refresh* on the source. Imports that return zero channels are rejected and the previous list is kept. |

## Status

Version 1.0. Tested against the bundled fake provider, with 60k live channels, 40k VOD/series entries, a slow provider and a 274 MB guide. A real 39k-channel Xtream account imports in seconds, and Plex Media Server 1.43 discovers the tuners and reads their lineups. Full playback through Plex, the Windows service install and HLS on live channels are not yet confirmed. Reports and logs are welcome.

## Development

```
go test ./...
go run ./tools/fakeprovider -live 60000 -vod 30000 -series 10000   # fake Xtream/M3U provider on :8090 (user u / pass p)
go run ./cmd/plexiptv -data ./data
```

Build release binaries with `./build.sh` or `build.ps1`. The version comes from `cmd/plexiptv/main.go`; pass one to override it (`./build.sh 1.1`). Go 1.23+ is required, and no C compiler is needed.

Layout: `internal/source` (Xtream/M3U import), `internal/catalog` (in-memory lineup, numbering, queries), `internal/filter` (rules), `internal/epg` (XMLTV), `internal/hdhr` (tuner API + discovery), `internal/stream` (proxy), `internal/web` (panel + API), `internal/store` (SQLite).
