# CLAUDE.md — bump-server

Context for Claude instances working in this repo. Short version: this is a Fly-deployed Go server. It issues signed session tokens to the Android client and serves `bumpnow.app`. Read this before touching anything crypto-adjacent.

## What this is

Go HTTP server on Fly.io, app name `bump`, hostname `bumpnow.app`. Two machines in `iad`, `min_machines_running = 1`, auto-stop enabled. SQLite (or Postgres via `DATABASE_URL`) for session/report storage. Cloudflare sits in front.

## Critical things that will bite you

**1. `ED25519_PRIVATE_KEY` is the crown jewel.** It signs every session token issued to clients. The matching public key is **hardcoded in the Android client** at `bump/app/src/main/java/me/getbump/app/crypto/TokenVerifier.kt:73-78`. If you rotate the private key, every installed client becomes unable to verify tokens and the app bricks for users until they get a new build with the new public key. Rotation procedure is in `OPERATIONS.md §1-2` — it's a coordinated client+server ship, not a solo action. Never commit the key, never log it, never paste it in Slack/chat. Verify with `cmd/verifykey` before any first deploy.

**2. B4 guardrail: /verify refuses to start without auth.** The server will crash-loop on startup unless one of these is set:
- `GOOGLE_PLAY_SERVICE_ACCOUNT_JSON` (production mode, real Play Billing verification)
- `BUMP_DEV_MODE=true` (closed test bootstrap, accepts every purchase token — **must be cleared before production promotion**)

If both are missing, the server intentionally fails rather than running with `/verify` fail-open. This is the April 5 2026 production-hardening guardrail. Do not loosen it.

**3. `BUMP_DEV_MODE` is a time bomb.** It's safe during the 14-day Play Console closed test because the test track is invite-only and uses license-test accounts (no real charges). But it **must be cleared before Production** or anyone can mint free bumps with fake purchase tokens. Day-13 calendar reminder lives in the operator calendar, not in code — do not trust any automation to catch this.

**4. `validReasons` in `handlers/report.go:44` must stay in sync with the client.** The map holds the abuse-report reason codes the server accepts. Adding a new reason requires:
1. Update `validReasons` in this repo
2. Update the `reasons` list in `bump/app/src/main/java/me/getbump/app/ui/components/ReportButton.kt`
3. Ship both together (client needs the new string, server needs to accept it)

Current reasons: `harassment`, `child_safety`, `spam`, `safety`, `other`. The `child_safety` entry is legally load-bearing for the CSAE policy commitment at `bumpnow.app/safety` §6 — do not remove it.

**5. Static files are served from `static/`.** The server registers three specific routes (`/privacy`, `/safety`, `/`) plus a `/videos/` subtree via `http.FileServer`. The catch-all `/` handler returns 404 for any unknown path. If you add a new page, you must add a route in `main.go`, not just drop a file into `static/`. The [bump-site](https://github.com/breadman69420/bump-site) repo is a **mirror for repo consistency only** — this repo is the source of truth for the live site.

## Deploy commands

```sh
go build -o /tmp/bump-check . && rm /tmp/bump-check     # compile check
go test ./handlers/...                                  # run handler tests before any change
fly deploy --remote-only                                # manual deploy, no CI
fly status -a bump                                      # machine health
fly logs -a bump                                        # tail
fly secrets list -a bump                                # what's set (values masked)
```

## Health check

- Code path: `main.go:114` — `mux.HandleFunc("/health", ...)` returns `ok`
- Fly config: `fly.toml:26-30` — `[[checks.health]] path = "/health"`
- External: `curl https://bumpnow.app/health` → `okHTTP: 200`

## Smoke tests (hit production)

```sh
curl -sI https://bumpnow.app/                          # 200 text/html
curl -sI https://bumpnow.app/privacy                   # 200 text/html
curl -sI https://bumpnow.app/safety                    # 200 text/html
curl -sI https://bumpnow.app/videos/bump-fgs-demo.mp4  # 200 video/mp4

# Report endpoint (expect: invalid reason for bogus, 201 {} for valid — but valid will insert a row)
curl -X POST https://bumpnow.app/report -H 'Content-Type: application/json' \
  -d '{"reporter_hash":"aaaa...","reported_hash":"bbbb...","reason":"not_a_reason"}'
```

## Companion repos

- **bump**: `/Users/ttalvac/Apps/bump/bump` — Android client
- **bump-site**: `/Users/ttalvac/Apps/bump/bump-site` — static marketing mirror (not live, sync with `static/` here)
- Umbrella dir: `/Users/ttalvac/Apps/bump/`
