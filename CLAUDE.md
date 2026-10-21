# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

`AGENTS.md` is the authoritative project instruction file (authority/scope, safety, delivery rules); read it and the repo-tracked `.agents/skills/ponytail/SKILL.md` (smallest-correct-change guidance) before coding. This file only adds commands and architecture.

## What Piik is

Private screen sharing for one Host and up to 20 authenticated Viewers. Web Host/Viewer/relay is the current product surface; a cross-platform Go "App" wraps the same core. Media is P2P-first with a bounded embedded SFU/UDP fallback (Pion/LiveKit). Do not add custom quality scores/ladders/probes/rebalancing — WebRTC and LiveKit own adaptation.

## Commands

Toolchain: Node `.node-version` (>=24), npm 11, Go 1.26. Run from repo root.

```sh
npm ci
npm run dev                  # Vite dev server on :8787 (needs a server, see below)
npm run typecheck            # tsc -b (src/ and scripts/ projects)
npm test                     # vitest run (tests/**/*.test.ts)
npx vitest run tests/quality.test.ts            # single TS test file
npx vitest run -t "name fragment"               # single test by name
npm run build                # build:web → internal/server/webassets/dist (embedded by Go binaries)
npm run check                # typecheck + test + build (the main TS gate)
npm run check:go             # gofmt, go test, go vet, cross-builds of app/peer-gate/server
npm run check:go-core | check:go-race | check:native
go test ./internal/server/route/ -run TestName  # single Go test (build:web first: Go embeds dist)
node scripts/check-docs.mjs  # docs hygiene; sh scripts/check-project-state.sh = pre-commit check
```

Run the Server from source (second terminal beside `npm run dev`):

```sh
PORT=8788 PUBLIC_BASE_URL=http://localhost:8787 go run ./cmd/piik-server
```

On Windows build to a fixed path (`go build -o build/dev/piik-server.exe ./cmd/piik-server`) to avoid repeated firewall prompts; on Windows `check-go.mjs` also runs Go tests from `build/go-check`, never from Go's temp dir. The App: `npm run build:web` then `go run ./cmd/piik-app` (see `cmd/piik-app/README.md`). Config is via env vars; see `.env.example` and `docs/standards/configuration.md`.

`gate:*` / `probe:*` npm scripts are heavy browser/network acceptance gates (batch them at acceptance, not per change). Ordinary scoped work uses the lean path in `CONTRIBUTING.md`: focused checks only, no `npm run check`/PRs/status edits unless a release or material contract change.

## Architecture

One shared Go backend core (ADR-0012) serves two entry points, `cmd/piik-server` (Hosted) and `cmd/piik-app` (Local / temporary public link / saved Site), both embedding the single Vite-built Browser bundle (`internal/server/webassets`). Node/Vite are build tooling only, not a runtime. Browser UI lives in `src/client`; wire types shared with Go live in `src/shared/protocol.ts` ↔ `internal/server/protocol`.

Server-side layering (`internal/server/`):
- `app` — HTTP, site access, static assets; room mutations go through `signal`.
- `signal` — authenticated command/effect owner; serializes all room, session and route mutations.
- `room` — room authority, credentials, SQLite persistence (durable write precedes in-memory commit; caller holds the signal lock).
- `route` — synchronous room graph + single serial child operation decisions; no locks/timers, returns resource effects to `signal`.
- `sfu` + `internal/app/mediaedge` over `internal/media/forwarding` — bounded embedded forwarding.

App-side (`internal/app/`): launcher, loopback service, native control sessions, `nativecapture` (Go contract) ↔ `native/capture/*` (platform capture adapters). The App never contains a second room backend.

Browser (`src/client`): `pages/HostPage.tsx` / `ViewerPage.tsx` orchestrate; `media/`, `webrtc/`, `sfu/`, `native/` are endpoint adapters; `locales/` holds en/zh copy. Every runtime follows acquire → authoritative commit → retire; revalidate identity after each await (see the lifecycle table in `docs/standards/engineering.md`).

Key invariants (details in ADR-0005 and `docs/standards/routing-transport.md`): one committed route graph per room controller, one reconcile loop, one serial child operation; one steady outbound-copy cap (default 2, range 1..3) for every endpoint; STUN only — no TURN or TCP media.

Wire/contract changes: edit both ends together plus `tests/fixtures/wire-samples.json`, which the TS and Go fixture tests both consume. Until a public release is declared, keep one current internal contract — delete replaced surfaces instead of adding compat aliases.

## Docs and ownership

Each durable fact has one owner under `docs/standards/` (rooms-access, routing-transport, media-quality, presentation-lifecycle, engineering, visual-language, versioning, configuration, naming). Read `docs/project-memory.md`, `docs/status.md`, `docs/todo.md` before substantial work; `docs/todo.md` is the only work ledger. UI/copy changes follow `docs/standards/visual-language.md` and `naming.md`; product copy is bilingual (en/zh).

`err.txt`/`out.txt` at the root are stray local output, not source.
