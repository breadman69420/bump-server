# Bump — Operations Runbook

Audience: whoever is on call for Bump. This document covers the operational tasks that cannot be done from a code change — rotations, kill-switch fire drills, and incident response. Keep it in sync with reality. If you had to do something not listed here, add it.

All commands assume you have `fly` CLI authenticated against the `bump` app and shell access to where the Android keystore lives.

---

## 1. Ed25519 signing key rotation

The server signs short-lived (60 s) session tokens with an Ed25519 private key. The Android client holds the matching public key hardcoded in `bump/app/src/main/java/me/getbump/app/crypto/TokenVerifier.kt:71-76`. A rotation is a coordinated client+server change; you cannot do it server-only without breaking every live session.

### When to rotate
- The current private key is believed to be compromised.
- Annual precautionary rotation (recommended).
- A departing team member had access to `fly secrets`.

### Procedure
1. **Generate a new keypair** locally (never commit):
   ```
   cd bump-server
   go run ./cmd/keygen
   ```
   Output includes both the base64 private key and the Kotlin `byteArrayOf(...)` public key literal.

2. **Prepare the Android client for dual-verification.** Edit `TokenVerifier.kt` to accept both the OLD and NEW public keys during the transition window. Concretely: add a second `byteArrayOf` and have `verifySignature()` try the new key first, fall back to the old key. Ship this as a release through Play Console **before** touching the server. Wait for adoption (at least a few days of the 14-day closed-test cycle, or longer for production).

3. **Deploy the new private key to the server** once client adoption is high enough:
   ```
   fly secrets set ED25519_PRIVATE_KEY='<new-base64-private-key>' -a bump
   fly deploy -a bump
   ```
   On deploy, `fly logs -a bump` will show the new public key in Kotlin format (see §6 below). Byte-compare against the NEW literal in `TokenVerifier.kt`.

4. **Drop the old public key** from the client in the next release after you've confirmed the server is signing with the new key and there are no signature-verification failures in the logs.

### Rollback
If something is wrong after step 3, `fly secrets set ED25519_PRIVATE_KEY='<old-base64-private-key>' -a bump` and redeploy. The old key stays valid as long as clients still trust both keys.

### What NOT to do
- Do not rotate the server key without first shipping a dual-verification client release. Every live session will fail signature verification within 60 s.
- Do not commit the private key to git, paste it in Slack, or print it to any log. The server intentionally logs only the public key.

---

## 2. Google Play service account rotation

The server uses a Google Cloud service account with Play Console access to verify purchase tokens via the Android Publisher API. Credentials are stored in the Fly secret `GOOGLE_PLAY_SERVICE_ACCOUNT_JSON`.

### When to rotate
- Suspected leak.
- A team member with GCP IAM access leaves.
- The key is older than your rotation policy (recommended: annual).

### Procedure
1. **Create a new key** for the existing service account in GCP Console → IAM & Admin → Service Accounts → bump-play-verifier → Keys → Add Key → Create new key → JSON.
2. **Apply to Fly**:
   ```
   fly secrets set GOOGLE_PLAY_SERVICE_ACCOUNT_JSON="$(cat new-sa.json)" -a bump
   ```
   (Fly automatically redeploys with the new secret.)
3. **Verify** via `fly logs -a bump` that the server started without a `Failed to parse service account JSON` warning.
4. **Delete the old key** in GCP Console.
5. **Shred the JSON files** locally: `shred -u new-sa.json` (or equivalent).

### Safety
- If `GOOGLE_PLAY_SERVICE_ACCOUNT_JSON` is empty or malformed, the server **refuses to start** in production mode (`BUMP_DEV_MODE` unset). This is the B4 guardrail from the pre-launch audit. Don't remove it.
- If rotation fails partway (new key in Fly, old key deleted in GCP, new key somehow doesn't work), the server won't start on next deploy. You have as much time as the running machine lives to roll back. Don't delete the old key until you've verified the new one works.

---

## 3. Android release keystore password rotation

The Android release keystore is a `.jks` file; its password is required at release build time via the `BUMP_KEYSTORE_PASSWORD` and `BUMP_KEY_PASSWORD` environment variables. There are NO fallbacks in `build.gradle.kts` — release builds fail loudly if either is missing.

### When to rotate
- The password was previously committed to git as a default fallback (this was the case in a pre-launch version — do this rotation **once** before launching).
- Suspected leak.
- Team member with access leaves.

### Procedure
1. **Change the store and key passwords** with `keytool`:
   ```
   keytool -storepasswd -keystore bump-release.jks
   keytool -keypasswd -keystore bump-release.jks -alias bump
   ```
   Pick strong, unique passwords. Store them in 1Password (or whatever ops password vault you use), never in env files or config files.
2. **Update CI secrets**: wherever you run `./gradlew bundleRelease`, update `BUMP_KEYSTORE_PASSWORD` and `BUMP_KEY_PASSWORD` to the new values.
3. **Update your local ops shell** if you ever build release APKs locally.
4. **Cut a clean test release** to confirm the new passwords work end-to-end, upload it to Play Console internal testing track, and verify it installs.

### Safety
- **Never** edit `bump/app/build.gradle.kts` to add a password fallback. The `verifyReleaseSigning` gradle task will still catch a missing env var, but the committed fallback itself is the risk. If you find yourself reaching for a fallback, fix your env var plumbing instead.
- **Never** commit `bump-release.jks` to git. It should live in the ops vault only.

---

## 4. Kill switch fire drill

The server's `KILL_SWITCH` env var disables all new bumps globally. Clients check the flag via the 5-minute-cached `/config` endpoint and refuse new `goLive()` calls when it's true. This is the "oh god something's wrong, stop everything" lever.

### When to use it
- Critical bug discovered post-deploy that can't wait for a rollback.
- Payment processor incident affecting `/verify`.
- Active abuse campaign where rate limits aren't keeping up.

### Procedure
1. **Enable the kill switch**:
   ```
   fly secrets set KILL_SWITCH=true -a bump
   ```
2. **Wait up to 5 minutes** for clients to refresh their cached config. Users currently in a session finish it normally; new `goLive()` taps see "bump is temporarily unavailable".
3. **Fix the problem** (deploy, rollback, etc.).
4. **Disable the kill switch**:
   ```
   fly secrets unset KILL_SWITCH -a bump
   ```
   (Or set it to any string other than `"true"`.)
5. **Announce restoration** if anyone outside the team was notified.

### Fire drill (do this BEFORE launching to production)
Once, in a staging environment or during a low-traffic window:
1. Enable the kill switch as above.
2. On a test device, tap bump. Confirm it shows "bump is temporarily unavailable".
3. Disable the kill switch.
4. Confirm bumps work again after 5 min + a tap on the test device.
5. Check that none of the steps accidentally charged a paid bump.

Document the date of the most recent successful drill here:
```
Last drill: <date>    On call: <name>    Outcome: PASS/FAIL
```

---

## 5. Mass-report abuse response

If `/report` volume spikes or blocklist additions per hour exceed normal baseline, you are likely being targeted by a coordinated mass-report campaign. The per-reporter rate limit (5/hr, hard block at 10/hr — see `handlers/report.go:29`) and the raised auto-block threshold (5 distinct reporters — same file) slow this down, but they don't stop it cold.

### Detection
Set up an alert on `blocklist` table growth rate in Postgres, or on `INSERT` into the `blocklist` table. More than N auto-blocks per hour (pick N from baseline) is an incident.

### Response
1. **Identify the reported hashes** from the recent `blocklist` inserts.
2. **Look at their report sources** — query `reports` WHERE `reported_hash IN (...)` — if you see a small pool of reporter hashes driving many reports, those are the attackers.
3. **Reverse the wrongful blocks** by `DELETE FROM blocklist WHERE device_hash IN (...)`.
4. **Block the attacker hashes instead** by `INSERT INTO blocklist (device_hash) VALUES (...)`.
5. **Consider raising `autoBlocklistThreshold`** in `handlers/report.go` if the current value is clearly inadequate. Requires a deploy.
6. **Consider lowering `reportRateLimit`** in the same file for the same reason.

### Future work
The mass-report vector is not fully closed. A determined attacker with enough distinct devices can still force auto-blocks. Consider:
- Requiring N *reputable* reporters (reporters who have themselves completed ≥M successful bumps) instead of N total reporters.
- Adding a human-review queue before auto-blocks take effect.
- Requiring confirmation-by-second-report (same pair) before a block counts.

---

## 6. Verifying the hardcoded client public key matches the live server

This is the single most dangerous configuration mismatch in the system: if the Android client's hardcoded `SERVER_PUBLIC_KEY` doesn't match the server's signing key, every BLE session silently fails signature verification, and users see "can't connect" errors with no useful diagnostics.

### Procedure (do this on every server deploy)
1. **After `fly deploy -a bump`**, run:
   ```
   fly logs -a bump | grep -A 10 "Server public key"
   ```
2. **Expected output** looks like:
   ```
   Server public key (Kotlin byteArrayOf format for TokenVerifier.SERVER_PUBLIC_KEY):
   byteArrayOf(
       22, 45, -85, 50, 116, 119, -103, 51,
       -52, 105, -118, -11, -71, 25, -68, -97,
       85, 96, -89, -84, 1, 54, 70, 11,
       -7, -35, -74, 44, -33, 84, -108, 110
   )
   ```
3. **Byte-compare** against `bump/app/src/main/java/me/getbump/app/crypto/TokenVerifier.kt:71-76`. Every number must match exactly.
4. **If they match**: done.
5. **If they don't match**: STOP. This is why you should have a dual-verification client release ready (§1) before you rotate. If they don't match and the client in production only trusts the OLD key, roll back `fly secrets` to the old private key and redeploy.

### Expected startup log lines (every deploy)
All three of these should appear in `fly logs` within a few seconds of server start:
- `Bump server starting on :8080`
- `Dev device allowlist: 0 entries (production mode)` — if this says anything else, B5 is live in prod.
- `Server public key (Kotlin byteArrayOf format for TokenVerifier.SERVER_PUBLIC_KEY):` followed by the literal above.

And NONE of these:
- `WARNING: BUMP_DEV_MODE=true and GOOGLE_PLAY_SERVICE_ACCOUNT_JSON is empty` — if present, B4 is live in prod. Remove `BUMP_DEV_MODE=true` from Fly secrets immediately.
- `PANIC:` anything.

---

## 7. Expected Fly secret inventory

Run `fly secrets list -a bump` before every production deploy. The list should be exactly:

| Name | Required | Notes |
|---|---|---|
| `ED25519_PRIVATE_KEY` | YES | 64-byte base64 Ed25519 private key. Server refuses to start without it. |
| `GOOGLE_PLAY_SERVICE_ACCOUNT_JSON` | YES (in prod) | GCP service account JSON with Play Developer API scope. Server refuses to start without it unless `BUMP_DEV_MODE=true`. |
| `DATABASE_URL` | YES | Managed Postgres URL. |
| `REDIS_URL` | YES | Managed Redis URL. |

| Name | Must be ABSENT in prod |
|---|---|
| `BUMP_DEV_MODE` | Enables `/verify` fail-open. |
| `BUMP_DEV_DEVICE_HASHES` | Bypasses all quotas. |
| `KILL_SWITCH` | Enables kill switch — only set during an incident. |

| Name | Optional tuning |
|---|---|
| `MAX_SESSIONS_HOUR` | Default 10. |
| `FREE_BUMPS_PER_DAY` | Default 3. |
| `TIME_WINDOW_SEC` | Default 15 (BLE peer token time window, sent to client). |
| `MIN_RSSI` | Default -75 (sent to client). |
| `MIN_APP_VERSION` | Default 1 (sent to client; enforced in `BumpViewModel.goLive()`). |
| `PORT` | Default 8080. |

---

## 8. Incident triage checklist

When something is obviously broken in production, work this list top to bottom. Don't skip.

1. **Is the server up?** `curl https://bumpnow.app/health` should return 200. If not, `fly status -a bump` and `fly logs -a bump`.
2. **Are secrets set?** `fly secrets list -a bump` — compare against §7.
3. **Are the startup lines right?** `fly logs -a bump` — see §6 expected output.
4. **Is the public key in sync with the client?** See §6.
5. **Are DB and Redis reachable?** `fly logs` will show connection errors if not.
6. **Is it an abuse incident?** Check Postgres for a spike in `reports` or `blocklist` inserts.
7. **Is it a Google Play Integrity / API outage?** Check https://status.cloud.google.com and `fly logs` for `Google Play verification error`.
8. **When in doubt**: enable the kill switch (§4). It's cheap to enable, cheap to disable, and it stops the bleeding while you figure out what's happening.
