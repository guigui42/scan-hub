# Copilot instructions

- Never send the VENS commands `UPDATE_PSW 0x15`, `SET_WIFI_MODE 0x31`,
  `FIRM_UPDATE 0x51`, or `SET_DEV_INFO 0x14`, nor RELEASE with action 1
  (ENFORCE). Never send PIXELSIZE during a scan session.
  `internal/vens/guard.go` enforces this; keep it that way.
- Record hardware observations in `docs/protocol-notes.md`.
- Ask the user before any step that talks to the real scanners or changes live
  services (Paperless users or tokens, DNS, deployments).
- Keep secrets out of Git and logs: the ScanSnap password and its derived
  identity, and the Paperless token. Locally they live in the gitignored
  repo-root `.env` (see `.env.example`).
- Keep the repository free of anything specific to one installation: no real
  IPs, MACs, domains, TLS pins, serials or tokens. Use placeholders such as
  `192.168.1.50` and `example.com`.
- Image tags are exact versions only; no `:latest`.
