# scan-hub

Scan over Wi-Fi from a **Fujitsu/Ricoh ScanSnap iX500** (proprietary VENS
protocol) and an **Epson ET-2750** (eSCL), and send the documents
automatically to **Paperless-ngx**.

- Go backend with a mobile-friendly web UI (`html/template` + HTMX 4 +
  Alpine.js, vendored with no front-end build).
- The ScanSnap's physical Scan button sends a scan straight to Paperless.
- Multi-page sessions: add pages, preview them, then send one merged PDF.
- Runs in Docker (Linux host networking): a 14 MB distroless, non-root image.

**Status:** early but working, tested against real devices: ScanSnap
ownership and crash recovery, the physical Scan button, Epson flatbed scans,
sessions on disk, PDF building, a durable Paperless outbox (Paperless-ngx 3.3,
API v10) and a mobile-first web UI. Hardware observations are in
[`docs/protocol-notes.md`](docs/protocol-notes.md).

## Web UI

Three screens:

- **Desk:** start a session and see device status.
- **Session:** scan with either scanner, preview and delete pages, title the
  document, and send it.
- **Outbox:** follow delivery; retry or discard failed documents.

How it's built:

- Server-rendered `html/template`, with htmx 4.0.1 for partial updates and
  Alpine.js 3.17.5 (CSP build) for local state.
- Vendored and embedded: no CDN and no front-end build. The SHA-256s are in
  `internal/web/static/SHA256SUMS` and checked by a test.
- Scans run as background jobs that the page polls, so a long batch never hits
  the 60 s htmx request timeout.

Security:

- Host allowlist; only the listen address and `SCANHUB_ALLOWED_HOSTS` are
  answered.
- Every POST/DELETE needs a same-origin `Origin` and a CSRF token bound to a
  `SameSite=Strict` cookie.
- Strict Content-Security-Policy: same-origin scripts and styles only, with no
  `unsafe-inline` and no `eval`.

To work on the UI without hardware, run it against fake devices (a ScanSnap,
an Epson and a Paperless on loopback):

```bash
go run ./cmd/scan-hub-demo -addr 127.0.0.1:8098
```

## Run locally

```bash
cp .env.example .env   # then set SCANHUB_SCANSNAP_PASSWORD and SCANHUB_DATA_DIR
make run               # real scanners, settings from .env → http://127.0.0.1:8097
make demo              # same UI against fake devices → http://127.0.0.1:8098
make help              # all targets: test, e2e, docker-build, docker-up, ...
```

Commands (load `.env` first: `set -a && . ./.env && set +a`):

```bash
./bin/scan-hub scan-epson -dpi 300 -color color   # one flatbed pass into a new session
./bin/scan-hub pdf <session-id> -o out.pdf        # merge a session into one PDF
./bin/scan-hub send <session-id>                  # send to Paperless (server must be stopped)
```

The process needs UDP 55264 and 55265 free on the host, and the ScanSnap must
be able to reach it with UDP (button events).

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `SCANHUB_DATA_DIR` | `/data` | Absolute path for sessions and the ScanSnap ownership journal |
| `SCANHUB_LISTEN_ADDR` | `127.0.0.1:8097` | HTTP listener (UI, `/healthz`, `/readyz`, `/status`); use an explicit LAN IPv4 in production |
| `SCANHUB_ALLOWED_HOSTS` | none | Extra `Host` values the UI answers to (comma-separated), e.g. `scan.example.com`; the listen address and `localhost:<port>` are always allowed |
| `SCANHUB_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `SCANHUB_SESSION_MAX_AGE` | `24h` | Unsent sessions untouched for this long are deleted (minimum `1h`) |
| `SCANHUB_SESSION_MAX_PAGES` | `200` | Pages per session (1–1000) |
| `SCANHUB_MIN_FREE_MB` | `512` | Refuse new pages below this much free disk space (`0` disables) |
| `SCANHUB_SCANSNAP_IP` | empty (disabled) | ScanSnap IPv4 (fixed DHCP reservation) |
| `SCANHUB_SCANSNAP_PASSWORD` / `_FILE` | none | ScanSnap Wi-Fi password; required with the IP |
| `SCANHUB_BUTTON_COLOR` | `auto` | Button profile: `auto`, `color`, `gray`, `bw` |
| `SCANHUB_BUTTON_DPI` | `300` | `auto`, `150`, `200`, `300` |
| `SCANHUB_BUTTON_DUPLEX` | `true` | Duplex |
| `SCANHUB_BUTTON_PAPER` | `auto` | `auto`, `a4`, `a5`, `business-card`, `postcard` |
| `SCANHUB_BUTTON_BLANK_PAGE_REMOVAL` | `true` | Drop blank pages |
| `SCANHUB_BUTTON_MULTIFEED` | `true` | Multi-feed detection |
| `SCANHUB_PAPERLESS_URL` | empty (disabled) | Paperless base URL, e.g. `https://paperless.example.com` |
| `SCANHUB_PAPERLESS_TOKEN` / `_FILE` | none | API token of a dedicated Paperless user that has only the `add_document` and `view_paperlesstask` permissions; required with the URL |
| `SCANHUB_PAPERLESS_TAGS` | none | Comma-separated tag IDs added to every upload |
| `SCANHUB_OUTBOX_MAX_MB` | `2048` | Maximum size of PDFs waiting in the outbox |
| `SCANHUB_EPSON_URL` | empty (disabled) | Epson eSCL base URL, e.g. `https://192.168.1.60` |
| `SCANHUB_EPSON_TLS_PIN` | none | Hex SHA-256 of the Epson certificate's SPKI (or of the whole certificate); required for `https` |
| `SCANHUB_EPSON_TLS_INSECURE` | `false` | Skip TLS verification (explicit fallback only) |
| `SCANHUB_EPSON_COLOR` | `color` | Flatbed default: `color`, `gray`, `bw` |
| `SCANHUB_EPSON_DPI` | `300` | `100`, `200`, `300`, `600` |
| `SCANHUB_EPSON_REGION` | `a4` | `a4`, `letter`, `max` (full platen) |

The ScanSnap protocol work builds on
[mzyy94/AirScap](https://github.com/mzyy94/AirScap) (MIT); see
[`THIRD_PARTY_NOTICES`](THIRD_PARTY_NOTICES). scan-hub is MIT-licensed
([`LICENSE`](LICENSE)).
