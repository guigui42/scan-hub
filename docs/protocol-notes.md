# ScanSnap iX500 and Epson ET-2750: hardware notes

Observations from real devices on a home LAN, recorded while building
scan-hub. The first part comes from a throwaway probe built on AirScap
`internal/vens` @ `177fdae` plus a write-side command allowlist (the same
guard as `internal/vens/guard.go`), run on 2026-11-07 from a Linux desktop on
the same Wi-Fi network as the scanner. Only session commands were sent; the
forbidden commands and PIXELSIZE were blocked.

## Device

| Item | Value |
|---|---|
| Model / firmware | `FUJITSU ScanSnap iX500`, firmware revision **`0U00`** (INQUIRY) |
| Discovery protocol version | **`0x0006`** (offset 8; AirScap documents `0x0004`) |
| RESERVE response protocol | `0x00040000` (same as AirScap) |
| Ports | data 53218, control 53219 (from discovery) |
| Serial format | `iX500-` + 10 characters (16 total) |
| Scan params (INQUIRY VPD F0) | 50–600 dpi, colour modes `0x11`, max width `0x28D0`, max height `0xA1D0` (1/1200 in) |
| Wi-Fi status | `GET_WIFI_STATUS` state 3 (strong) |

The AirScap code worked unchanged on protocol `0x0006`.

## 1. Pairing

- The identity algorithm (AirScap §4.5) is **correct** for this firmware.
- **The password is not the factory default.** The serial-derived default was
  rejected with RESERVE status `-3` (`0xFFFFFFFD`, unknown pairing key). The
  password set on the scanner was accepted with status `0`. Configure it
  with `SCANHUB_SCANSNAP_PASSWORD`.
- A rejected RESERVE left the scanner free (state 1). No lockout was seen
  after a single failure; do not retry blindly.
- The claim sequence that works:
  1. discovery;
  2. heartbeat (0.5 s);
  3. wait 300 ms;
  4. `RESERVE`;
  5. INQUIRY (device info);
  6. `GET_WIFI_STATUS`;
  7. INQUIRY VPD F0;
  8. `EB` config.

  The `RELEASE` action=1 ("ENFORCE"/register) step from the official pairing
  flow was **not needed**.

## 2. Software-started scan (no button)

- `D8 → D4 → D5 → C2 → E0` with one sheet loaded and **no button press**: `E0`
  returned **status 0 after about 1.0 s**, and the sheet fed by itself.
- **Decision:** the UI starts ScanSnap scans in software. It needs **no
  "Press Scan on the scanner" waiting state**. Paper presence is checked with
  `C2` first (bit `0x80` = ADF empty), and the "no paper" state is shown before
  sending `E0`.
- Simplex with default settings (auto colour, auto quality, auto size) gave one
  baseline JPEG, 1648×2324, 3 components (A4 at about 200 dpi), 456 KB, in 3
  chunks (the first chunk was 0 bytes).
- **End of batch:** `C2` showed `0x00000080` (ADF empty), then `E0` returned
  `waitStatus=2`. That is a clean end, followed by `D6`. AirScap issue #1
  (`0x2000` + ADF empty) did **not** occur in this single-sheet simplex run; the
  port keeps the fix anyway.
- Not tested yet: duplex and multi-sheet batches, B&W (TIFF). Cover them in
  the first scan-hub hardware run below.

## 3. Button events (UDP 55265)

- Each physical press sends **3 identical 48-byte datagrams, about 500 ms
  apart**, from one ephemeral source port per press. The port differs between
  presses.
- Payload: magic `VENS` at offset 4, **type `0x00000001`** at 8, **data at 16:
  `0x02000000`, then `0x03000000`** on the next press. The high byte works as a
  per-press **sequence counter**.
- **Decision:** accept only 48-byte `VENS` type-1 packets from the configured
  scanner IP, and debounce on the counter value. As a fallback, use a 2 s
  window if the counter repeats after a scanner power cycle.
- Events arrive while claimed and idle, with no paper loaded. A press with no
  paper still sends the event, so the button flow must check `C2` and report
  "no paper".

Example:
`0000003056454e530000000100000000020000000000…` (48 bytes).

## 3b. Power-on announcement (UDP 53220, 2026-11-07)

Passive capture on the host (nothing sent to the scanner) while the paper
chute was closed (power off) and opened again (power on):

- On power-on, once on Wi-Fi, the scanner broadcast **6 identical 48-byte
  datagrams to UDP 53220 within about 1 s**, from two ephemeral source ports.
- Payload: magic `VENS` at offset 4, **command `0x21`** at 8, `0x01000000` at
  16, **its IPv4 at 20**, **its MAC at 24** (matches
  `vens.ParseBroadcastAdvertisement`).
- Nothing was received on 53220, 55264 or 55265 when powering off. While idle
  and powered, no broadcast came within 22 s (earlier check).
- **Decision:** treat an announcement from a configured ScanSnap's IP as "it
  just started": reclaim at once (reusing the journal token) instead of
  waiting for the retry backoff, and debounce the burst.

Example (IP and MAC redacted):
`0000003056454e53000000210000000001000000c0a80132aabbccddeeff0000…` (48 bytes).

## 4. Ownership, restart and crash recovery

| Scenario | Observed |
|---|---|
| Discovery while free | state 1, `paired=false`, holder none |
| Discovery (throwaway token) while we hold it | state 3, holder = the client's IP. `paired` was `true` on the first claim and `false` after re-claims, so **do not rely on `paired`** |
| Discovery with **our own** session token while claimed | **no reply** (all retries time out) |
| Discovery (throwaway token) during or just after a session | **intermittently no reply** for a few seconds |
| Clean `RELEASE` (action 0) + heartbeat stop | scanner is free (state 1) **at once**; a new process with a fresh token claims with status 0 |
| Crash (no RELEASE, heartbeat stops) | held (state 3, our IP) until **about 20 s after the last heartbeat**, then free on its own (two runs: held at 17–20 s, free at 20.3 s) |
| Fresh token during that stale window | RESERVE **`-4`** (busy/stale) |
| **Adopt the journal token** during the stale window (4 s after the crash) + RESERVE | **status 0**; the session continues normally (C2, heartbeat past 20 s, clean RELEASE) |
| `RELEASE` action 0 with the **journal token** during the stale window | acknowledged; the scanner is **free at once**; a fresh token then claims with 0 |
| Adopt an *expired* journal token (after the timeout) | RESERVE 0 (it behaves like a new session) |
| `-7` (another client holds the scanner) | not tested (no foreign client); deferred with the release feature |

| After a power cycle (chute closed and opened), RESERVE with the dirty **journal token** (2026-11-07) | **`-4`**, and `RELEASE` action 0 with that token: the scanner **closes the connection** (EOF). Who held it is unknown (another client grabbing it at power-on, e.g. ScanSnap Home, is possible). scan-hub then looped on the token until the fix below. |

**Recovery rules for `internal/scansnap`:**

1. Persist the token with `dirty=true` (fsync and rename) **before** RESERVE.
   **Never overwrite a dirty journal with a different token.** The probe lost
   the stale token exactly this way in its first run.
2. On start with a dirty journal, **reuse the journal token**: start the
   heartbeat, then RESERVE. This works both within and after the stale window.
3. If that RESERVE fails with `-4`, send `RELEASE` (action 0) with the journal
   token only, mark the journal clean, then claim with a fresh token. Never use
   ENFORCE, and never release a token that scan-hub didn't create. If that
   RELEASE is refused (connection closed), mark the journal clean anyway and
   go to rule 4: the token proves nothing, and a stale session of ours expires
   on its own.
4. Otherwise (for example `-7`, or `-4` that persists), treat the scanner as
   held by someone else: back off and retry. The stale window is about 20 s.
5. For liveness while claimed, use `GET_WIFI_STATUS` / `C2`, not discovery.
   Use discovery `state` and `holder` only as a hint, and tolerate missing
   replies.
6. Shutdown: send `D6` if a scan is active, then `RELEASE` (action 0), then
   stop the heartbeat, then mark the journal clean.

## Open items

- `-7` behaviour and Windows coexistence (deferred with the release feature).
- B&W (TIFF) and longer multi-sheet batches.
- Who held the scanner after the power cycle that answered `-4` to the
  journal token (now shown as the holder on the card).
- Whether the button counter survives a scanner power cycle (debounce falls
  back to a 5 s window if it repeats).

## scan-hub: ScanSnap owner on hardware (2026-11-07)

The `scan-hub` binary ran from the desktop against the real scanner:

| Check | Result |
|---|---|
| Startup claim (fresh token) | RESERVE 0, `claimed` about 1.5 s after start |
| Button → 2 sheets, duplex, 300 dpi | 4 sides transferred, **3 pages stored**; the blank back of sheet 0 came back as **0 bytes** (blank-page removal, which the client drops). JPEGs of about 2470×3500 at 300 dpi (auto-cropped A4). End of batch: `waitStatus=2`. |
| Button counter | It kept increasing across sessions and processes (`0x04`, `0x05`, `0x06` after the probe's presses), so it **persists on the scanner** |
| SIGINT | RELEASE plus a clean journal in about 30 ms |
| `kill -9`, then restart | **Adopted the journal token**: RESERVE 0 about 3.4 s after start. Own-token discovery gets no reply (expected), so the 3 s discovery timeout is the delay. |
| Button with an empty feeder | `C2` reported no paper, D6 was sent, back to `claimed`. The fix stores nothing for such a press. |

## Epson ET-2750 eSCL (2026-11-07)

| Item | Value |
|---|---|
| TLS | Self-signed `CN=printer, O=SEIKO EPSON CORP.`, valid 2010–2038, SAN `printer`, `printer.local` and the printer's IP; TLS 1.2 `ECDHE-RSA-AES128-GCM-SHA256` |
| Pin (`tls_pin` in the scanner list) | SHA-256 of the certificate's SPKI; it is per device, see `scanners.example.yaml` for the capture command |
| eSCL | Version 2.63, `EPSON ET-2750 Series`, platen only, max 2550×3510 (1/300 in), 100/200/300/600/1200 dpi, `BlackAndWhite1`/`Grayscale8`/`RGB24`, JPEG or PDF |
| Scan | `POST /eSCL/ScanJobs` (A4 2480×3508, RGB24, 300 dpi, JPEG) → `201`, job `/eSCL/ScanJobs/<uuid>`; `NextDocument` blocks until the image is ready: **about 28 s** for A4 colour at 300 dpi; a 2480×3508 JFIF 300-dpi JPEG of about 575 KB; then back to `Idle` |

The client pins the SPKI, never follows the job `Location` host (only its
`/eSCL/ScanJobs/<id>` path, on the configured URL), and deletes the job on
failure or cancellation.

## Web UI on hardware (2026-11-07)

`scan-hub` ran on the desktop with both real scanners and Paperless disabled.
The UI was driven from a browser.

| Check | Result |
|---|---|
| Startup | ScanSnap claimed in about 1.3 s; Epson `Idle`; `/readyz` ready |
| Live status | The desk card switched "Paper: Empty" to "Loaded" when sheets were inserted (5 s poll, `C2`) |
| Feeder from the UI | 2 sheets, duplex, 300 dpi: 4 pages in about 8 s. The page count and thumbnails updated live from the job poll. |
| Glass, same session | A4 colour at 300 dpi: 1 page in about 28 s; that session holds 5 pages from both scanners |
| Physical button with the server running | 2 sheets, 3 pages (one blank back side dropped). Stored as a "ScanSnap button" session because Paperless is off. |
| SIGINT | Released the ScanSnap in about 40 ms; journal clean |

## Container on hardware (2026-11-07)

The `scan-hub:dev` image (14 MB; distroless, UID 65532) ran with
`compose.example.yaml` on the desktop: host networking, `read_only`,
`cap_drop: ALL`, a named volume. Paperless was off.

| Check | Result |
|---|---|
| Start | Healthy; ScanSnap claimed in about 1.3 s; Epson `Idle` |
| Physical button | UDP events reach the container through host networking. 1 sheet, 2 pages, stored in the named volume. |
| `docker restart` (SIGTERM) | Released in about 30 ms, re-claimed with a fresh token about 1.5 s later |
| `docker stop --signal 9` (crash), then start | **Adopted the journal token**: RESERVE 0. The session survived on the volume. |
| `docker compose down` | The scanner is free afterwards (discovery: state 1, no holder) |

## Real Paperless upload (2026-11-07)

The native binary ran against Paperless-ngx 3.3.0 (API v10) with the token of
a dedicated `scanner` user that has only `add_document` and
`view_paperlesstask`.

| Check | Result |
|---|---|
| First button batch (1 sheet, 2 pages) | **Rejected:** `400 {"document":["No file was submitted."]}`. The client streamed the multipart body chunked, with no `Content-Length`, and Django reads such a body as empty. The fake Paperless (Go `net/http`) accepted it, so tests missed it. |
| Fix | The client sends an exact `Content-Length` (multipart head + file size + trailer), still streaming the PDF from disk. The fake now returns the same 400 when `Content-Length` is missing. |
| Retry from the outbox UI | New filename `…-a2.pdf`. Task `success` with a `document_id`, marked delivered about 32 s after upload. |
| Document | 2 pages, owner `scanner`. Paperless consumption workflows (tags, storage path, custom fields) applied as for any other upload. |
| Scanner token scope | `GET /api/tasks/` 200; `GET /api/documents/` 403. |

Title sync (v0.3.0) reads
`GET /api/documents/?id__in=<ids>&fields=id,title` every 5 minutes and right
after each delivery. It needs `view_document` on the `scanner` user; with the
403 above it logs one warning and retries hourly (seen on the real
server, 2026-11-10: one `cannot read document titles` warning at start,
uploads unaffected). Paperless only lists
documents the user owns, was granted, or that have no owner, so a document
whose owner changes during triage keeps its last known title. The grant is
broader than it looks: documents with no owner are visible to every user with
`view_document`. On the test instance (2026-11-10) about 80 % of documents had
no owner, so the permission was not granted and title sync stays off. An untitled
upload is named after its file (`scan-<timestamp>-<id>-a<n>`); scan-hub hides
that name.
