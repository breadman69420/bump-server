# Bump — Production Verification Prompt

> Feed this entire document to an engineer (or a Claude Code agent) tasked with verifying the Bump app is production-ready. It is intentionally exhaustive. The goal is **zero surprises at launch**.

---

## 0. Mission

You are verifying production readiness of **Bump**, a peer-to-peer contact-exchange / mini-game app that pairs two nearby phones over BLE and lets them exchange information, play short games, and rate the encounter. Bump consists of three repos under `/Users/ttalvac/Apps/bump/`:

- `bump/` — Android client (Kotlin, Compose, Hilt, Ktor, Nordic BLE, Tink, Play Billing)
- `bump-server/` — Go backend (net/http, Postgres, Redis, Ed25519, Google Play Developer API), deployed on Fly.io
- `bump-site/` — Static marketing + privacy site

Your job is to systematically walk every section below and produce a **go / no-go report**. For each check: mark it `PASS`, `FAIL`, `WARN`, or `N/A`, cite `file:line` for any finding, and give a one-line reproduction / fix note. Do not gloss over anything. Do not accept "probably fine" — verify.

End with three lists:
1. **Blockers** — must fix before launch.
2. **Warnings** — should fix or document as accepted risk.
3. **Accepted risks** — explicitly okayed by the product owner, written down.

---

## 1. Pre-flight: secrets, env, and config (START HERE — fastest blockers)

### 1.1 Android signing config (`bump/app/build.gradle.kts`)
- [ ] The release signingConfig has **no hardcoded fallback passwords**. The file previously contained a literal like `"bumprelease2026"`. Grep the file: `storePassword`, `keyPassword`, and any literal containing `bumprelease` or `2026`. Release builds must fail if `BUMP_KEYSTORE_PASSWORD` / `BUMP_KEY_PASSWORD` env vars are missing — they must **not** silently use a default.
- [ ] `minifyEnabled = true`, `shrinkResources = true`, ProGuard rules applied in release.
- [ ] No `debuggable true` in the release variant.
- [ ] `versionCode` / `versionName` bumped for release.
- [ ] `applicationId` is `me.getbump.app` (matches Play Console listing).

### 1.2 Android manifest (`bump/app/src/main/AndroidManifest.xml`)
- [ ] `android:allowBackup="false"` and `android:fullBackupContent` not set to a permissive rule.
- [ ] `android:exported` declared explicitly on every Activity / Service / Receiver (required target SDK 31+).
- [ ] `MainActivity` is the only exported activity.
- [ ] `BumpForegroundService` is **not** exported, and uses `foregroundServiceType="connectedDevice"`.
- [ ] No `usesCleartextTraffic="true"` and no `networkSecurityConfig` that weakens TLS.
- [ ] Permissions: `BLUETOOTH_SCAN` / `BLUETOOTH_ADVERTISE` / `BLUETOOTH_CONNECT` on API 31+, `BLUETOOTH` / `BLUETOOTH_ADMIN` with `maxSdkVersion="30"` for legacy, `ACCESS_FINE_LOCATION`, `FOREGROUND_SERVICE`, `FOREGROUND_SERVICE_CONNECTED_DEVICE`, `INTERNET`. Nothing else — no `READ_CONTACTS`, no `READ_PHONE_STATE`, etc.

### 1.3 Android API base URL (`bump/app/src/main/java/me/getbump/app/network/BumpApi.kt`)
- [ ] `BASE_URL` is the production origin (`https://bumpnow.app` or whatever is contracted). No `http://`. No `10.0.2.2`. No localhost. Confirm against Fly app hostname.
- [ ] Ktor client has `connectTimeoutMillis` and `socketTimeoutMillis` set (10s / 15s).
- [ ] Certificate / origin is accessible from a real device (curl `/config` from outside the office network).

### 1.4 Android token verifier public key (`crypto/TokenVerifier.kt`)
- [ ] The hardcoded `SERVER_PUBLIC_KEY` byte array matches the Ed25519 public key derived from the server's `ED25519_PRIVATE_KEY` env var in production. **Compute it on the server and compare byte-for-byte.** Mismatch = every BLE session fails on the client and users will never know why.
- [ ] `setPublicKey()` is not being called with test data anywhere in the release path.

### 1.5 Go server env (`bump-server/config/config.go`, Fly secrets)
Run `fly secrets list -a bump` and verify:
- [ ] `ED25519_PRIVATE_KEY` — set, 64-byte base64, valid. Server exits on startup if missing — confirm it's present.
- [ ] `GOOGLE_PLAY_SERVICE_ACCOUNT_JSON` — set to production service account JSON with `androidpublisher.googleapis.com` scope. **If unset, `/verify` runs in dev mode and accepts any purchase token as valid — this is a launch blocker if left unset.**
- [ ] `BUMP_DEV_DEVICE_HASHES` — **empty or unset**. Dev hashes bypass rate limiting, quotas, and blocklist in `handlers/session.go`, `session_commit.go`, `bumps.go`, and `verify.go`. Grep the startup log for the line that prints the count — it must say `0`.
- [ ] `KILL_SWITCH` — `false` at launch (but verify the switch works before depending on it; see §10).
- [ ] `MAX_SESSIONS_HOUR`, `FREE_BUMPS_PER_DAY`, `TIME_WINDOW_SEC`, `MIN_RSSI`, `MIN_APP_VERSION` — sanity check values match product decisions.
- [ ] `DATABASE_URL`, `REDIS_URL` — point at managed production instances, not local, with TLS where supported.

### 1.6 Fly.toml (`bump-server/fly.toml`)
- [ ] `force_https = true`.
- [ ] Health check path `/health`, 30s interval, reaches a running app after deploy.
- [ ] `min_machines_running` ≥ 1 (or auto-start works within acceptable latency).
- [ ] Memory (256 MB) is sufficient under load — load test in §11.
- [ ] Hard concurrency limit (250) is appropriate for expected peak.

---

## 2. Android app — state machine & UI flow

All references are to `BumpViewModel.kt` unless noted. Test each as instrumented or manual QA. Tests under `app/src/test/.../BumpViewModelTest.kt` cover many of these with virtual time — run them and **confirm they pass**, then also test manually.

### 2.1 State machine correctness
- [ ] `BumpUiState` values exist: `Idle`, `Scanning`, `Found`, `Exchange`, `Received`, `Expired`, `Questions`, `SyncGame`, `Results`, `Done`, `Error`. Every state has a visible, non-stuck UI.
- [ ] `Error` auto-reverts to `Idle` after ~2s (`ERROR_AUTO_DISMISS_MS`). Do not leave users stranded.
- [ ] `Disconnected` from `BleSessionManager` transitions to `Done` and stops the foreground service.

### 2.2 `goLive()` — the "tap to bump" entry
- [ ] Rapid double-tap launches **exactly one** `/session` call. Audit finding C3 is fixed — verify by tapping 5x fast and checking server logs. Expected: one request, not five.
- [ ] Zero bumps → stays on Idle with an explanation, **no network call**.
- [ ] Kill switch active (set `KILL_SWITCH=true` on server, wait 5 min for config cache refresh or clear local DataStore) → stays on Idle with "temporarily unavailable" message.
- [ ] Blocklisted device (add its hash to `blocklist` table) → stays on Idle.
- [ ] Bump is **reserved, not charged**, by `/session`. The count only decrements after `/session/commit` fires on `SessionState.Ready`.
- [ ] Network failure on `/session` → Error, not charged.
- [ ] Malformed 104-byte token response → safely rejected (audit C4 fix — `tokenBytes.size == 104` check in `BumpApi.kt`). Confirm via proxy injection.
- [ ] 429 response → user sees rate-limited error, client respects `Retry-After` (even if minimally). No infinite retry loop.
- [ ] 403 out-of-bumps → parsed correctly even if the body is malformed (audit H5 fix — `parseOutOfBumps` returns `OutOfBumps(0,0)` on parse failure).

### 2.3 Scanning → Found
- [ ] 15s scan timeout with no peer → `NoMatch` → Error("no one found") → Idle.
- [ ] Two phones discover each other and both reach `Found` within 15s.
- [ ] `isInitiator` is consistent — whichever phone has the lexicographically smaller device hash is the initiator on **both** phones.
- [ ] `sessionSeed` (XOR of both nonces) is identical on both phones (verify by logging in debug).
- [ ] Session commit (`/session/commit`) fires exactly once when Ready is observed. Never twice. Never on NoMatch/Error/Cancel. Never after user cancels (verify `pendingSessionId` is cleared by `cancelSession()`).
- [ ] If peer is in blocklist (from cached `/config`), the session is ended **before** commit — verify no charge.

### 2.4 Mode negotiation (Found → Exchange / Questions / SyncGame)
Mode is negotiated over the DATA characteristic with `MOD:propose:X`, `MOD:counter:X`, `MOD:accept:X` messages (not the MODE characteristic — that's deprecated).
- [ ] Initiator proposes → responder sees highlighted suggestion → responder accepts → both transition into that mode.
- [ ] Responder counter-proposes → initiator sees "counter" highlight → initiator accepts → both transition.
- [ ] Responder cannot propose first — UI forbids it.
- [ ] Double-propose is ignored.
- [ ] 30s `NEGOTIATION_TIMEOUT_MS` without agreement → Error → Idle (both sides). No charge beyond the already-reserved bump.

### 2.5 Exchange mode — contact sharing
- [ ] Platform picker shows PHONE, INSTAGRAM, LINKEDIN, SNAPCHAT. Each has correct iconography and input validation.
- [ ] PHONE automatically launches Google Phone Number Hint API (no SIM → graceful failure, user can still type).
- [ ] **Escrow semantics**: if only one side has sent, neither reveals. Only when both have sent does either transition to `Received`. Verify by having A send first and waiting — A must see "waiting for them", not B's data.
- [ ] `EXCHANGE_TIMEOUT_MS` (60s) triggers teardown and error if peer never sends.
- [ ] Info is visible on `ReceivedScreen` for exactly `SELF_DESTRUCT_MS` (15s), then auto-destructs.
- [ ] `FLAG_SECURE` is set on the window **during `Received` state** (screenshots / screen recording blocked). Test with `adb shell screencap -p` — the output should be black for the info area.
- [ ] Tapping a share action (copy / open app / save contact) transitions to Done for the current user but **does not cancel the peer's countdown**.
- [ ] On countdown expiry, both sides send `BYE:expired` and tear down gracefully.
- [ ] **`SecureInfoRenderer`** does not log or persist received info anywhere.

### 2.6 Questions mode (5 rounds)
- [ ] 5 rounds × ANSWER_TIMEOUT_MS (15s) per round.
- [ ] Both phones see the same questions (deterministic from `sessionSeed`).
- [ ] Answer message format: `Q:round:answer`. Out-of-order or wrong-round messages are ignored.
- [ ] Match count on Results matches what both sides actually answered (compare against logs on both devices).
- [ ] Round timeout → either advance (if I already answered) or show "they didn't answer" → Done.
- [ ] Peer disconnects mid-round → graceful Done, no crash.

### 2.7 SyncGame mode (3 rounds)
- [ ] Same as Questions but 3 rounds.
- [ ] Match count correct.

### 2.8 Results → Swap
- [ ] Swap is **mutual-consent**: both users must tap "Swap info" before either transitions to Exchange.
- [ ] If only I tap, `iRequestedSwap = true`, I see "waiting for them to agree".
- [ ] If they tap first and then I tap, we transition together to fresh Exchange.
- [ ] No bump is charged for the swap — it reuses the same BLE session.

### 2.9 Report peer
- [ ] `ReportButton` visible on Found, Results, Done screens (when a peer hash is known).
- [ ] Selecting a reason submits to `/report` with correct `reporter_hash`, `reported_hash`, `reason`.
- [ ] Success/failure surfaces a toast. On failure, nothing is lost and the user can retry.
- [ ] Reporting self is impossible (server rejects, client never tries).

### 2.10 Billing
- [ ] `BillingManager` initializes on app start and reconnects on disconnect.
- [ ] `bump_single` product ID matches the Play Console listing.
- [ ] Unconsumed purchases from a crashed flow are detected on next start and processed (`processPendingPurchases`).
- [ ] Purchase → `/verify` call with `purchase_token`, `product_id`, `device_hash`.
- [ ] On `valid: true`, `paid_balance` in response updates local `bumpsRemaining`.
- [ ] On `valid: false`, **still call `consumePurchase`** so the user can buy again (code does this — verify).
- [ ] Interrupt the purchase flow at various points (cancel dialog, kill app mid-purchase, network off) — app recovers without granting free bumps and without double-charging.

### 2.11 Permissions & system integration
- [ ] First-run on API 31+ prompts for `BLUETOOTH_SCAN`/`ADVERTISE`/`CONNECT` + `ACCESS_FINE_LOCATION`. All-denial path shows an explanatory screen and links to Settings.
- [ ] Bluetooth off → prompts user to enable. Rejecting the enable dialog returns gracefully to Idle.
- [ ] Location off (API 31+ still needs fine location for BLE scan) → graceful error.
- [ ] App backgrounded during scanning → foreground service keeps BLE running, notification is silent/low-priority.
- [ ] Force-stop the app during `Received` — on relaunch, no leaked contact info is visible and no orphaned session exists on the server.
- [ ] Doze mode / battery saver — verify foreground service survives.

### 2.12 Local storage (`data/LocalStore.kt`)
- [ ] Only `cached_bumps_remaining` is persisted. No contacts, no tokens, no nonces, no PII.
- [ ] Uses DataStore Preferences (not plain SharedPreferences).
- [ ] On reinstall, state is fresh (server is source of truth for balances).

### 2.13 Unit tests
Run `./gradlew test` in `bump/`. Every test must pass. Note any `@Ignore` or disabled tests. Read the test source and confirm it actually covers:
- goLive guards (zero bumps, kill switch, blocklist, double-tap)
- commitBumpIfPending fires exactly once on Ready and never on NoMatch/Error/Cancel
- Mode negotiation: propose / counter / accept from both sides
- All Questions rounds complete → Results with correct match count
- All SyncGame rounds complete → Results
- Purchase verify success and failure paths
- cancelSession clears pending session so late Ready does not commit

---

## 3. BLE subsystem — protocol correctness & resilience

Files: `ble/BleConstants.kt`, `BleSessionManager.kt`, `BleAdvertiser.kt`, `BleScanner.kt`, `GattServer.kt`, `GattClient.kt`.

### 3.1 Protocol constants
- [ ] Service UUID is stable and unique (`b0b1b2b3-0000-1000-8000-00805f9b34fb`).
- [ ] Four characteristics exist: TIME (read), READY (write), MODE (read+write, deprecated for negotiation), DATA (write+notify with CCCD).
- [ ] `MIN_RSSI = -85` (or matches server `/config` value).
- [ ] `TOKEN_EXPIRY_MS = 60_000`, `TIME_WINDOW_MS = 15_000`, `SCAN_TIMEOUT_MS = 15_000`, `GATT_OPERATION_TIMEOUT_MS = 10_000`.

### 3.2 Handshake
- [ ] Both sides open GATT server and advertise + scan simultaneously.
- [ ] Client path: scan finds peer → connect → read TIME → validate token → write READY → derive shared key.
- [ ] Server path: `handleReadyReceived` is safe to race with the client path; both can complete, last-write wins, no state corruption.
- [ ] Token validation enforces: length == 104, signature valid, `expiry > now`, `|serverTimestamp - peerServerTimestamp| < TIME_WINDOW_MS`, nonce not in `seenNonces`.
- [ ] MTU 247 is requested. If unavailable, the protocol still works at the default MTU (23). Verify with an older device.

### 3.3 Encrypted data channel
- [ ] Every DATA message is encrypted with `PayloadEncryption.encrypt(plaintext, sharedKey)` before being written/notified.
- [ ] Fresh 12-byte IV per message (AES-256-GCM).
- [ ] Tamper the ciphertext in transit (instrumented test) — decryption fails and the session enters `Error`, does not crash.
- [ ] `GATT_MAX_RETRIES = 2` with `200ms` base backoff. On final failure, Error is surfaced with "Connection lost".
- [ ] Token expiry is re-checked in `sendData()` before every send.

### 3.4 Teardown
- [ ] `stopSession` clears token bytes, clears `seenNonces`, closes GATT server, disconnects client, stops scanner and advertiser.
- [ ] `notifyPeerAndTearDown` sends `BYE:reason` then waits ~250ms for flush before stopping. Verify peer actually receives BYE in the common case.
- [ ] No file descriptor or BT adapter handle is leaked — force 50 sessions in a loop and check `adb shell dumpsys bluetooth_manager` for stale connections.

### 3.5 Adversarial BLE
- [ ] Nearby third device advertising the same service UUID — does the scanner correctly reject after token validation fails?
- [ ] Replay a captured TIME characteristic value (from a real prior session) to a fresh phone — must be rejected by nonce replay or time window.
- [ ] Peer sends junk on DATA — decryption fails, session terminates, no crash.
- [ ] Peer sends oversized payload (> MTU) — handled gracefully (BLE layer fragments, but enforce max plaintext length in app).

---

## 4. Cryptography — correctness review

### 4.1 Session key derivation (`crypto/SessionKeyDerivation.kt`)
- [ ] HKDF-SHA256 (RFC 5869) — extract phase then expand phase.
- [ ] IKM = `ordered.first.toBytes() ‖ ordered.second.toBytes()` (80 bytes).
- [ ] Salt = `ordered.first.nonce ‖ ordered.second.nonce` (16 bytes).
- [ ] Info = `"bump-session-v1"` — **if you ever change the protocol, bump this string**.
- [ ] Output = 32 bytes (AES-256).
- [ ] Ordering is deterministic via lexicographic unsigned compare of `deviceHash`. Both phones derive identical key — verify with two debug builds logging the hex output.
- [ ] Note for threat model documentation: **the server can derive session keys** because it signed both tokens and knows both nonces. This is an architectural trade-off (allows server-side moderation / audit) but means session confidentiality is client↔server↔client, not end-to-end against the server. Document this in the privacy policy if not already done.

### 4.2 Payload encryption (`crypto/PayloadEncryption.kt`)
- [ ] AES-256-GCM with 12-byte IV, 128-bit tag.
- [ ] IV generated fresh per message via `SecureRandom`. **No nonce reuse.** Verify by encrypting the same plaintext twice and confirming ciphertexts differ.
- [ ] Ciphertext framing: `IV ‖ ciphertext+tag`. Minimum length check (≥ 28 bytes) on decrypt.
- [ ] Tampered auth tag → `AEADBadTagException` → surfaces as session Error (not crash).
- [ ] **No AAD is bound**. This means a ciphertext from one direction could, in theory, be replayed the other way within the same session. Document as accepted risk or add direction/sequence AAD before launch.

### 4.3 Token verifier (`crypto/TokenVerifier.kt`)
- [ ] Public key length is 32 bytes.
- [ ] API 33+: native java.security Ed25519. API 26–32: Tink `Ed25519Verify`. Test on both (an API 28 emulator is sufficient).
- [ ] Expiry check uses `System.currentTimeMillis()` — document that device clock skew > 60s will break tokens. Clock sync at `/session` enforces ±2s, but if the clock drifts after token issuance, verification fails.
- [ ] Nonce replay set is in-memory only, cleared on `stopSession`. **Long-running app concern**: set grows unboundedly with sessions. Verify the set is either bounded or time-expired — otherwise add a TODO / memory pressure test.
- [ ] Catch-all `try/catch` around signature verification swallows exceptions and returns `false`. Any weird crypto bug becomes "invalid token". Log errors at WARN level in production so you can detect infrastructure issues.

### 4.4 Server signing (`bump-server/crypto/signing.go`)
- [ ] `GenerateToken` produces exactly 104 bytes: 40-byte payload + 64-byte Ed25519 signature.
- [ ] Payload layout is big-endian: `serverTimestamp(8) ‖ expiry(8) ‖ deviceHash(16) ‖ nonce(8)`.
- [ ] `rand.Read(nonce)` uses `crypto/rand`, not `math/rand`. Grep the whole server: `grep -rn "math/rand"` — must return nothing in the hot path.
- [ ] `deviceHash` from request is decoded to bytes and zero-padded/truncated to 16 bytes consistently — write a unit test if none exists.

### 4.5 `generateTestToken` in `BleSessionManager.kt`
- [ ] This method produces a dummy-signed token for Phase 2 local testing. **Confirm it is unreachable in release builds** — grep all call sites and verify they're behind a debug flag or removed. If a release build can call it, the session would fail silently when the signature is verified by the peer (or worse, succeed if verification is ever bypassed).

---

## 5. Go server — per-endpoint verification

For each endpoint: hit it with `curl` against a staging instance, verify input validation, auth, rate limit, response shape, and error paths. Run `session_commit_test.go` and any other tests: `cd bump-server && go test ./...` — all must pass.

### 5.1 `POST /session` (`handlers/session.go`)
- [ ] Body > 1 KB → 400.
- [ ] `device_hash` not matching `^[a-f0-9]{32}$` → 400.
- [ ] `client_time` more than 2000ms off from server → 400 "clock out of sync".
- [ ] `integrity_token` is accepted but not validated. **Decision**: either validate with Play Integrity API before launch, or remove the field to stop advertising false security. Currently it's a no-op.
- [ ] Hourly rate limit (`MAX_SESSIONS_HOUR`, default 10): 11th request within an hour is progressively throttled; 21st is hard-blocked with 429. Test with a single device hash.
- [ ] Rate-limit key is per-device: different device hashes are not coupled.
- [ ] Dev allowlist device bypasses rate limiting **entirely**. Confirm `BUMP_DEV_DEVICE_HASHES` is empty in prod.
- [ ] Blocked device gets a 5-second sleep and then **a valid token that clients will ignore** (via cached blocklist in `/config`). Decide whether this honeypot behavior is intentional; if not, return 403. **This is an ambiguity — resolve before launch.**
- [ ] Free bump reservation: Lua script atomically checks daily counter < `FREE_BUMPS_PER_DAY` (default 3), sets `reserve:{session_id}` key with 60s TTL. Verify with `redis-cli MONITOR`.
- [ ] Daily free bumps reset at UTC midnight — verify `secondsUntilMidnightUTC` math is correct across DST transitions (there are none in UTC, but confirm).
- [ ] Paid bump fallback: if daily free is exhausted, reserve a paid slot (no decrement yet). TOCTOU on the paid balance read is **acceptable** — results in 409 at commit time, not over-debit.
- [ ] 403 out-of-bumps response always includes `free_remaining` and `paid_balance` fields (client parses them; must not be omitted).
- [ ] Session ID is 32 hex chars from `crypto/rand` (16 bytes). Confirm in code.
- [ ] 500 responses do **not** leak stack traces or DB error strings to the client — only generic messages. Recovery middleware logs the panic but returns a plain 500.

### 5.2 `POST /session/commit` (`handlers/session_commit.go`)
- [ ] Requires matching `device_hash` + `session_id`. Missing reservation → 410 Gone. Wrong owner → 403.
- [ ] Free bump commit: Lua script increments daily counter atomically and writes committed sentinel.
- [ ] Second commit of the same `session_id` is **idempotent** — returns same balances, does not double-increment. Verify by calling `/session/commit` twice in rapid succession.
- [ ] Paid bump commit: acquires `commitlock:{session_id}` via SetNX (30s TTL), decrements `device_bumps.paid_balance` in Postgres via `UPDATE ... WHERE paid_balance > 0` (atomic, cannot go negative — schema `CHECK` also enforces this), then marks committed sentinel.
- [ ] Concurrent paid commits for the same session_id → only one decrements. Test with two goroutines.
- [ ] If balance was 0 at commit time → 409 Conflict, clean failure.
- [ ] Partial failure path (DB decrement succeeds, sentinel write fails): within 10s, a retry can see the still-active reservation and attempt again; the `UPDATE WHERE > 0` guard prevents double-decrement. Accept as known risk, ensure the 10s sentinel TTL is correct.
- [ ] Failure of `/session/commit` should **not** charge the user — verify by killing Redis between `/session` and `/session/commit` and confirming the free counter never increments.

### 5.3 `GET /bumps` (`handlers/bumps.go`)
- [ ] Requires `device_hash` query param, validated.
- [ ] Fails **closed** on Redis error (503, not fake-full). Verify.
- [ ] Dev allowlist returns full quota.
- [ ] Response shape: `free_remaining`, `paid_balance`.

### 5.4 `POST /verify` (`handlers/verify.go`)
- [ ] `product_id == "bump_single"` strictly; other product IDs → 400.
- [ ] `purchase_token` is non-empty, ≤ 512 chars.
- [ ] Per-device rate limit (key `verify:{device_hash}`): 5/hr soft, 10/hr hard.
- [ ] Replay protection: `verified_purchases.purchase_token` is the primary key. Second `/verify` with the same token → `{ valid: false, bumps_granted: 0 }`. **Test this explicitly — it is the single most important fraud control.**
- [ ] Google Play Developer API call uses `GOOGLE_PLAY_SERVICE_ACCOUNT_JSON`. Without it, **all tokens are accepted** (dev mode). Fail the launch if the service account is not set.
- [ ] Purchase state must equal 0 (Purchased). Cancelled, Pending, Refunded → rejected.
- [ ] Atomic credit: single Postgres transaction does `INSERT ... ON CONFLICT DO NOTHING` into `verified_purchases` + `INSERT ... ON CONFLICT DO UPDATE SET paid_balance = paid_balance + EXCLUDED.paid_balance RETURNING paid_balance`. Concurrent `/verify` with the same token → only one wins.
- [ ] Audit findings H1 (tx rollback), H2 (atomic balance read), C1 (double-credit) all confirmed fixed.
- [ ] Google Play API timeout → returns `{ valid: false }` gracefully, does not hang the request.
- [ ] Test with an intentionally invalid token, a valid token for a different product, an expired token, a refunded purchase, a subscription token.

### 5.5 `POST /report` (`handlers/report.go`)
- [ ] Body limited to 1 KB. Both hashes validated. Reason ∈ {`harassment`, `spam`, `safety`, `other`}.
- [ ] Self-report rejected.
- [ ] `timestamp` field is accepted but **not stored** anywhere. Either remove or use it — current state is confusing dead code.
- [ ] Dedup on `(reporter_hash, reported_hash)` via unique index.
- [ ] **3-report auto-blocklist is a known abuse vector.** No per-reporter rate limit. A bad actor with 3 devices can blocklist any target. **Decide one of:**
  - Raise the threshold (e.g., 10 unique reporters over a longer window).
  - Add a rate limit on `/report` per reporter (e.g., 5/day).
  - Require human review before adding to blocklist.
  - Accept the risk and document it, along with a monitoring dashboard.
  - **This must not ship unresolved.**
- [ ] Blocklist add is racy (no transaction) — low impact but verify no crash on duplicate insert.

### 5.6 `GET /config` (`handlers/config.go`)
- [ ] Returns `time_window_sec`, `min_rssi`, `min_app_version`, `kill_switch`, `blocklist`, `max_sessions_per_hour`.
- [ ] `Cache-Control: public, max-age=300` header present.
- [ ] `blocklist` is the full list — verify growth strategy. If it could exceed a few MB, implement pagination or a signed blob.
- [ ] Kill switch toggle: set `KILL_SWITCH=true` via fly secrets → redeploy or wait for cache → app refuses new bumps within 5 min.
- [ ] `min_app_version` is respected: client should refuse to start if it's below this. **Verify the client actually checks this** — code review suggests it is fetched but not enforced. Fix before launch if you need the kill switch ability for old versions.

### 5.7 `GET /health`
- [ ] Returns 200 OK unconditionally (stateless). Fly health check works.

### 5.8 `GET /` and `GET /privacy`
- [ ] Serves `static/index.html` and `static/privacy/index.html` respectively.
- [ ] `http.ServeFile` — no path traversal, only mapped paths.
- [ ] Unknown paths return 404, not a directory listing.

### 5.9 Middleware
- [ ] `Recovery` catches panics, logs without formatting the value with `%+v` (no stack traces to clients), returns 500.
- [ ] `Logger` logs method, path, duration — does **not** log request bodies or headers (avoid PII leakage).
- [ ] No CORS middleware present (intentional — no browser clients).

### 5.10 Database (`db/schema.sql`, `db/queries.go`, `db/db.go`)
- [ ] All queries parameterized (grep `fmt.Sprintf` in `db/queries.go` — must not be used for values).
- [ ] `device_bumps.paid_balance` has `CHECK (paid_balance >= 0)` at the schema level.
- [ ] Indexes exist on `reports(reported_hash)`, `session_log(device_hash, requested_at)`.
- [ ] Schema is created with `IF NOT EXISTS` — safe to re-run.
- [ ] Connection pool: `MaxOpenConns = 10`, `MaxIdleConns = 5`. Under Fly 256 MB limit with 250 concurrent requests, pool saturation is possible — load test in §11.
- [ ] Daily `CleanupOldSessions` goroutine prunes `session_log` rows > 7 days old. Verify the goroutine is still running 24h after deploy.

### 5.11 Redis (`cache/redis.go`)
- [ ] Lua scripts for free reserve and free commit are correct and idempotent. Read `session_commit_test.go` — it asserts this.
- [ ] TTLs: reservations 60s, committed sentinel 10s, commit lock 30s, rate limit 1h, daily counter (to next UTC midnight).
- [ ] Fail-open on `CheckRateLimit` Redis error is intentional but risky — document and add a metric so you notice if Redis is down.
- [ ] Redis persistence (AOF or RDB) is enabled in production — reserve state survives restart.

---

## 6. Security — cross-cutting verification

### 6.1 Transport
- [ ] All client → server traffic is HTTPS (enforced by Fly + Ktor defaults). No plaintext fallback.
- [ ] Consider certificate pinning for `/verify` and `/session` (currently not implemented — decide pre-launch).

### 6.2 Secrets
- [ ] Private Ed25519 key is not in git, not in logs, not in error messages. Grep the whole repo for fragments of the key.
- [ ] Google Play service account JSON is not in git, not echoed in logs.
- [ ] No `TODO: remove before prod` or `DEV_KEY` or similar left in the code.

### 6.3 PII
- [ ] Server stores **no** contact info, names, phone numbers, emails. Confirm by reading `schema.sql`.
- [ ] Client does not persist received contact info to DataStore, SharedPreferences, Room, or files. Confirm by instrumenting `LocalStore` and `PayloadEncryption.decrypt` call sites.
- [ ] Logs do not include device hashes at higher than DEBUG level, or if they do, Android logging is at DEBUG-off in release (verify ProGuard strips `Log.d`).
- [ ] Ktor `Logger` level is `NONE` or `INFO` in release, not `ALL`.

### 6.4 Abuse & anti-fraud
- [ ] Mass-report DoS — see §5.5. Blocker.
- [ ] Purchase replay — covered by `verified_purchases` PK.
- [ ] Rate-limit bypass via Redis outage — document.
- [ ] Device hash forgery — anyone can claim any hash, but without a matching signed token from the server they can't do anything useful. Still, verify `deviceHash` in tokens is used for the user it was issued to (server binds at `/session`).

### 6.5 BLE-specific
- [ ] Three-phone MITM scenario: nearby attacker relaying tokens. The 15s time window + signed tokens + nonce replay protection makes this hard but not impossible if the attacker is fast. Document in threat model.
- [ ] A malicious peer can harvest the counterparty's signed token (it's broadcast on TIME characteristic). That token is useless without the peer being in range and the nonce being unseen — but document.

---

## 7. Observability & operational readiness

- [ ] Structured logs (JSON) or at least grep-able patterns for: bump reserved, bump committed, verify success, verify failure, report filed, blocklist update, rate limit hit.
- [ ] Fly metrics (`fly status`, `fly logs`) are being collected. CPU / memory / request rate dashboards exist.
- [ ] Alerting on: 5xx rate spike, `verify` failure rate, Google Play API error rate, Redis errors, Postgres errors, kill switch enabled, `/health` failure.
- [ ] Deploy rollback plan: `fly releases list`, `fly releases rollback <version>` works against a previous good version.
- [ ] Database backups: automated daily, tested restore.
- [ ] Redis durability: AOF appendfsync everysec or RDB with acceptable RPO.
- [ ] Runbook exists for: key rotation, service account rotation, kill switch activation, mass blocklist reversal.

---

## 8. Privacy, legal, store listing

- [ ] `bump-site/privacy/` privacy policy is **current** and reflects actual data practices: device hash, no contacts stored server-side, BLE-only exchange, Google Play Billing, auto-deleting encounter data.
- [ ] Data Safety form in Play Console matches the privacy policy.
- [ ] Target SDK ≥ 34 (Play Store 2024+ requirement — confirm current Play requirement at launch).
- [ ] 64-bit native libs included (via Tink / BoringSSL — verify APK has `arm64-v8a` and `x86_64`).
- [ ] App content rating matches usage (user-generated text in contact exchange → rating likely Teen or higher).
- [ ] Play Integrity API enabled in Play Console for the package.
- [ ] In-app product `bump_single` is created in Play Console, priced, activated in all target regions.

---

## 9. Device / OS coverage

Run the full happy path on:
- [ ] Pixel, API 34 (current flagship)
- [ ] Samsung Galaxy (One UI BLE stack quirks are notorious)
- [ ] API 28 or 29 (lowest realistic target, API 26 minSdk but most users are API 28+)
- [ ] A low-end device (2 GB RAM)
- [ ] Two identical devices simultaneously (stress the initiator election)
- [ ] With Bluetooth Multi-Advertiser off (some older chipsets)
- [ ] With 10+ other BLE peripherals in range (crowded café test)

---

## 10. Kill-switch fire drill

Before launch, practice the emergency stop:
- [ ] Set `KILL_SWITCH=true` on Fly.
- [ ] Verify existing client instances stop initiating new bumps within 5 minutes (config cache TTL).
- [ ] Verify no partial charges or stuck sessions.
- [ ] Set `KILL_SWITCH=false` — service resumes.
- [ ] Document the procedure in the runbook.

---

## 11. Load & soak tests

### 11.1 Load
- [ ] Generate 100 concurrent `/session` → `/session/commit` flows against staging. Expect: all 200, daily counter increments exactly N times.
- [ ] 250 concurrent long-polling clients hitting `/config` — verify Fly machine handles the hard limit.
- [ ] Burst 500 `/session` requests from a single device — verify 429s kick in and throttle correctly.
- [ ] 100 concurrent `/verify` calls with distinct valid (mock) tokens — all credited once, no double-credit.
- [ ] 100 concurrent `/verify` calls with the same token — only one credit recorded.

### 11.2 Soak
- [ ] Run the staging backend at 20 rps for 6 hours. Check memory (256 MB is tight), goroutine count (`pprof` if enabled), Redis key count (daily + reservation keys expire correctly), Postgres connection count.
- [ ] Verify the daily `CleanupOldSessions` goroutine fires after 24h.

---

## 12. Reporting template (your output)

Produce a single summary document at the end with these sections:

### Blockers (must fix before launch)
> `PATH:LINE` — One-sentence description. Impact. Reproduction.

### Warnings (should fix soon)
> `PATH:LINE` — Description. Severity. Owner.

### Accepted risks (signed off by product)
> Description. Mitigating factor. Monitoring/rollback plan.

### Passing
> Brief list of major areas that are green.

### Test coverage gaps (new tests you should add)
> What's untested, in order of impact.

---

## 13. Known issues / decisions surfaced during the audit (address before launch)

These came out of static review and are listed here so you don't lose them:

1. **Hardcoded fallback signing passwords** in `build.gradle.kts` (historical — verify removed). Blocker if still present.
2. **`generateTestToken` in `BleSessionManager.kt`** is a phase-2 dev helper with a dummy signature. Verify no release code path reaches it.
3. **`integrity_token` in `/session`** is accepted but never validated. Either wire up Play Integrity verification server-side or remove the field. Shipping a field named "integrity_token" that does nothing is worse than not having it.
4. **`/report` has no per-reporter rate limit**, and the 3-report auto-blocklist threshold is low. Coordinated abuse vector. Resolve before launch (raise threshold, add rate limit, or require review).
5. **Blocklisted `/session` callers get a valid token after a 5-second delay** instead of an error. Decide if this honeypot behavior is intended; either document it or return 403.
6. **Server can derive session keys** (not E2E against the server). Not a bug, but must be reflected in the privacy policy and threat model.
7. **AES-GCM has no AAD** in `PayloadEncryption`. Consider binding direction + sequence to AAD to close the same-session ciphertext-swap vector.
8. **TokenVerifier `seenNonces` is unbounded** and in-memory. Bound or time-expire it to avoid slow memory growth on long-running sessions.
9. **Server public key hardcoded in `TokenVerifier.kt`** — rotating requires an app update. Acceptable, but document the procedure.
10. **`min_app_version` from `/config`** may not be enforced in the client. Verify; if not, add enforcement before launch (it's your only lever for forcing upgrades).
11. **`timestamp` field in `/report`** is accepted but unused. Remove or use.
12. **Google Play API verification fail-open** in dev mode: absence of `GOOGLE_PLAY_SERVICE_ACCOUNT_JSON` disables fraud checking entirely. Blocker if missing in prod.
13. **`BUMP_DEV_DEVICE_HASHES` bypasses everything.** Blocker if set in prod.
14. **No TLS pinning** on client → server. Decide if acceptable given HTTPS + signed tokens.
15. **Reports `timestamp` is client-supplied** if it ever starts being used — prefer server time.

---

## 14. How to run this verification

1. Check out the three repos. Confirm you're on the release commit.
2. Work through sections 1 → 13 in order. Do not skip.
3. For each check: **run the actual command or test**, don't pattern-match from memory.
4. When you find something, cite `file:line` and a reproduction.
5. Produce the final report as described in §12.
6. Do not ship until the Blockers list is empty.

Good luck. Be skeptical. Assume anything untested is broken.
