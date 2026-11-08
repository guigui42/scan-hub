# scan-hub

Scan over Wi-Fi from **Fujitsu/Ricoh ScanSnap iX500** feeders (proprietary
VENS protocol) and the flatbed of **eSCL/AirScan** scanners such as the Epson
ET-2750, and send the documents automatically to **Paperless-ngx**. Scanners
are listed in a YAML file; add or remove one there.

- Go backend with a mobile-friendly web UI (`html/template` + HTMX 4 +
  Alpine.js, vendored with no front-end build).
- A ScanSnap's physical Scan button sends a scan straight to Paperless.
- Multi-page sessions: add pages, preview them, then send one merged PDF.
- Runs in Docker (Linux host networking): a 14 MB distroless, non-root image.

**Status:** early but working, tested against real devices: ScanSnap
ownership and crash recovery, the physical Scan button, Epson flatbed scans,
sessions on disk, PDF building, a durable Paperless outbox (Paperless-ngx 3.3,
API v10) and a mobile-first web UI. Hardware observations are in
[`docs/protocol-notes.md`](docs/protocol-notes.md).

## Web UI

Three screens:

- **Desk:** start a session and see device status. A ScanSnap's card has
  **Release** (tap twice): scan-hub lets go of the scanner so another
  computer (another scan-hub, ScanSnap Home) can claim it, and stays off it,
  even across restarts, until you tap **Take back**.
- **Session:** scan with any configured scanner, preview and delete pages,
  title the document, and send it.
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

To work on the UI without hardware, run it against fake devices (two
ScanSnaps, two eSCL scanners and a Paperless on loopback; the second ScanSnap
needs `127.0.0.2`, which Linux provides and macOS needs as a `lo0` alias):

```bash
go run ./cmd/scan-hub-demo -addr 127.0.0.1:8098
```

## Run locally

```bash
cp .env.example .env   # then set SCANHUB_DATA_DIR, SCANHUB_SCANNERS_FILE and the passwords
cp scanners.example.yaml scanners.yaml   # then list your scanners
make run               # real scanners, settings from .env → http://127.0.0.1:8097
make demo              # same UI against fake devices → http://127.0.0.1:8098
make help              # all targets: test, e2e, docker-build, docker-up, ...
```

Commands (load `.env` first: `set -a && . ./.env && set +a`):

```bash
./bin/scan-hub scan glass -set dpi=300   # one flatbed pass of scanner "glass" into a new session
./bin/scan-hub pdf <session-id> -o out.pdf # merge a session into one PDF
./bin/scan-hub send <session-id>           # send to Paperless (server must be stopped)
```

The process needs UDP 55264 and 55265 free on the host (shared by all
ScanSnaps), and each ScanSnap must be able to reach it with UDP (button
events). It also listens on UDP 53220 for the broadcast a ScanSnap sends when
it powers on (opening its paper chute), and reconnects at once instead of
waiting for its retry delay of up to a minute. If 53220 is taken, it logs a
warning and relies on the retries. `scan` works with eSCL scanners only; ScanSnaps are held by the
server, so scan them from the UI or their Scan button.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `SCANHUB_DATA_DIR` | `/data` | Absolute path for sessions, the outbox and the ScanSnap ownership journals |
| `SCANHUB_LISTEN_ADDR` | `127.0.0.1:8097` | HTTP listener (UI, `/healthz`, `/readyz`, `/status`); use an explicit LAN IPv4 in production |
| `SCANHUB_ALLOWED_HOSTS` | none | Extra `Host` values the UI answers to (comma-separated), e.g. `scan.example.com`; the listen address and `localhost:<port>` are always allowed |
| `SCANHUB_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `SCANHUB_SESSION_MAX_AGE` | `24h` | Unsent sessions untouched for this long are deleted (minimum `1h`) |
| `SCANHUB_SESSION_MAX_PAGES` | `200` | Pages per session (1–1000) |
| `SCANHUB_MIN_FREE_MB` | `512` | Refuse new pages below this much free disk space (`0` disables) |
| `SCANHUB_SCANNERS_FILE` | empty (no scanners) | Absolute path of the scanner list (YAML, see below) |
| `SCANHUB_PAPERLESS_URL` | empty (disabled) | Paperless base URL, e.g. `https://paperless.example.com` |
| `SCANHUB_PAPERLESS_TOKEN` / `_FILE` | none | API token of a dedicated Paperless user that has only the `add_document` and `view_paperlesstask` permissions; required with the URL |
| `SCANHUB_PAPERLESS_TAGS` | none | Comma-separated tag IDs added to every upload |
| `SCANHUB_OUTBOX_MAX_MB` | `2048` | Maximum size of PDFs waiting in the outbox |

Secrets may also be given as `<NAME>_FILE` paths (e.g.
`SCANHUB_PAPERLESS_TOKEN_FILE`).

### Scanner list

`SCANHUB_SCANNERS_FILE` names a YAML file listing the scanners in display
order; [`scanners.example.yaml`](scanners.example.yaml) documents every key.
Each entry has an `id` (1–32 of `a-z`, `0-9`, `-`), a `type` and an optional
display `name`. Unknown keys and invalid values stop startup with a message
that names the entry. In Docker, mount the file read-only; it and any
`password_file` must be readable by UID 65532.

- `type: scansnap`: a ScanSnap iX500 at a fixed IPv4 (`ip`), with its Wi-Fi
  password from `password_env` (the name of an environment variable) or
  `password_file` (an absolute path). `defaults` prefill the UI form and
  `button` is the profile of its Scan button (without it, the button uses
  `defaults`).
- `type: escl`: the flatbed of an eSCL/AirScan scanner at `url`. An `https`
  URL needs exactly one of `tls_pin` and `tls_insecure: true`. `defaults`
  prefill the UI form.

Several ScanSnaps share one button listener (UDP 55265) and power-on listener
(UDP 53220), told apart by their source IP, and take turns for discovery (UDP
55264). So:

- give each ScanSnap a fixed DHCP reservation, reachable without NAT (its
  packets must come from the configured IP);
- run one scan-hub per host;
- each ScanSnap keeps its ownership journal in
  `$SCANHUB_DATA_DIR/scansnap/<id>/journal.json`. After a crash it holds the
  only proof of an unreleased session on that scanner, so scan-hub refuses to
  start if a journal with an unreleased session belongs to an id that is no
  longer listed, or to a different IP. To rename an id or change an IP, stop
  scan-hub cleanly first (the journal is then released), or restore the old
  entry for one start.

Running two real ScanSnaps at once has been tested only against fake
scanners so far.

**Upgrading from the single-scanner release:** the `SCANHUB_SCANSNAP_*`
(except a password read via `password_env`), `SCANHUB_BUTTON_*` and
`SCANHUB_EPSON_*` variables are gone, and scan-hub refuses to start while they
are set. Move them to the scanner list. With exactly one ScanSnap listed, its
old journal (`scansnap/journal.json`) moves to the new location on the first
start. `/status` now lists devices under `scanners`, keyed by id.

The ScanSnap protocol work builds on
[mzyy94/AirScap](https://github.com/mzyy94/AirScap) (MIT); see
[`THIRD_PARTY_NOTICES`](THIRD_PARTY_NOTICES). scan-hub is MIT-licensed
([`LICENSE`](LICENSE)).
