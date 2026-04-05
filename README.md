# bump-server

Backend for the [bump](https://github.com/breadman69420/bump) Android app. Issues short-lived signed session tokens over HTTPS, handles abuse reports, verifies Google Play purchases, and serves the static marketing pages at `bumpnow.app`. Deployed to Fly.io.

**Stack:** Go · SQLite · Fly.io · Cloudflare (in front)
**Hostname:** `bumpnow.app` (primary) · `bump.fly.dev` (Fly direct)
**Region:** `iad` (Ashburn)

## What it serves

| Endpoint | Purpose |
|---|---|
| `POST /session` | Reserve a signed Ed25519 session token (rate-limited per device hash) |
| `POST /session/commit` | Commit a reserved session, decrementing the user's daily bump quota |
| `POST /verify` | Verify a Google Play purchase token and grant IAP entitlement |
| `POST /report` | Submit an abuse report (reasons: harassment, child_safety, spam, safety, other) |
| `GET /health` | Health check (Fly monitors this) |
| `GET /` | Landing page |
| `GET /privacy` | Privacy policy |
| `GET /safety` | Child safety standards (Play Console CSAE policy) |
| `GET /videos/*` | Public video assets (e.g., permission-declaration demos) |

## Local dev

```sh
cp .env.example .env                  # fill in the values
go run .                              # starts on :8080
curl http://localhost:8080/health     # "ok"
go test ./...                         # before any handler change
```

## Deploy

No CI auto-deploy. Deploys are manual from this directory:

```sh
fly deploy --remote-only
fly logs -a bump                      # tail output
fly status -a bump                    # machine health
```

## Required Fly secrets

| Secret | Purpose |
|---|---|
| `ED25519_PRIVATE_KEY` | Signs session tokens. Public key is hardcoded in the Android client. **Never commit, never log.** Rotation: see `OPERATIONS.md` §1. |
| `GOOGLE_PLAY_SERVICE_ACCOUNT_JSON` | Verifies Play purchase tokens against the Android Publisher API. Required for production; can be deferred to `BUMP_DEV_MODE=true` during closed test. Setup: `docs/GCP_PLAY_SETUP.md`. |
| `DATABASE_URL` | SQLite file path or Postgres connection string. |

Optional: `BUMP_DEV_MODE=true` makes `/verify` accept any purchase token (bootstrap for closed test only; **must be cleared before Production promotion**).

## Docs

- `docs/GCP_PLAY_SETUP.md` — Google Cloud service account setup for Play Billing verification
- `OPERATIONS.md` — runbooks for key rotation, incident response, backups

## Companion repos

- **[bump](https://github.com/breadman69420/bump)** — Android client
- **[bump-site](https://github.com/breadman69420/bump-site)** — mirror of `static/` (this repo is the source of truth)
