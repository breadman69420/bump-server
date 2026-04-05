# Bump — Google Play service account setup

One-time task, ~10 minutes of clicking. This wires Google Cloud Platform so the Bump server can call the Google Play Android Developer API to verify purchase tokens submitted to `POST /verify`. Without this, the server has to run with `BUMP_DEV_MODE=true` as a bootstrap, which accepts every purchase token without contacting Google — safe for a closed invite-only test track, unsafe for a public launch.

## Cost

**$0.** The only real cost in this pipeline is the $25 one-time Google Play developer registration fee, which you've already paid if your `bump` app exists in Play Console.

- GCP account: free
- GCP project: free
- Service account: free
- Enabling Google Play Android Developer API: free
- API calls to verify purchases: free (quota ~200k/day, you'll use dozens)
- JSON keys: free

Google will likely ask for a credit card when you first create a GCP billing account. That's for identity verification; as long as you only enable the Android Publisher API and don't touch compute/storage/other services, your monthly bill is literally $0.00. Set a budget alert at $1/mo as a safety net (free feature).

---

## Part 1 — Create the service account in GCP (5 min)

1. Go to <https://console.cloud.google.com> and sign in with the same Google account that owns your Play Console.
2. Top bar → project dropdown → **New Project**.
   - Name: `bump-play`
   - No organization (unless you have one)
   - Click **Create**
3. Switch to the new project via the top-bar dropdown. Every subsequent click needs to happen inside `bump-play`, not whatever project you were in before.
4. Left nav → **APIs & Services → Library**.
5. Search: **Google Play Android Developer API**. Click it → **Enable**.
   - Note: if it asks for a billing account here, that's a one-time prompt for the GCP account as a whole. Add a card for verification, accept the free-tier terms, and continue. This specific API has no usage charges regardless of whether billing is "enabled" on the project.
6. Left nav → **IAM & Admin → Service Accounts**.
7. Click **Create Service Account** at the top.
   - Service account name: `bump-play-verifier`
   - Service account ID: auto-generated from the name; leave it
   - Description: `Verifies Google Play purchase tokens for the bump app`
   - Click **Create and Continue**
8. **Grant this service account access to project** step — **skip it**. Click **Continue** without selecting any role. You don't need any project-level IAM role for this use case; the actual grant happens in Play Console in Part 2. Older docs say "Service Account User" but that role is for one SA impersonating another, which isn't what's happening here.
9. **Grant users access to this service account** step — skip it too. Click **Done**.
10. Back on the Service Accounts list, click on the `bump-play-verifier` row.
11. Go to the **Keys** tab → **Add Key → Create new key → JSON → Create**.
12. Your browser downloads `bump-play-XXXXXXXXXX.json`. **Move it out of `Downloads` immediately** — into 1Password as a file attachment, or an encrypted disk image, or anywhere outside a cloud-synced folder.
    - Do NOT commit it to git
    - Do NOT email it or paste it into Slack
    - Do NOT leave it in `~/Downloads` where cloud backup services or Spotlight indexers will grab it

## Part 2 — Link GCP to Play Console (3 min)

1. Go to <https://play.google.com/console>.
2. Left nav at the **top level** (NOT inside an app) → **Setup → API access**.
3. If prompted, click **Link project** → select the GCP project you just created (`bump-play`) → accept the terms.
4. After linking, scroll down to the **Service accounts** section. `bump-play-verifier` should be listed.
5. Click **Grant access** on that row.
6. In the **App permissions** tab:
   - Click **Add app** → select **Bump**
   - Check these boxes, and **only** these boxes:
     - ✅ View app information and download bulk reports (read-only)
     - ✅ View financial data, orders, and cancellation survey responses
   - Uncheck everything else. Minimum privilege = smaller blast radius if the JSON key ever leaks.
7. In the **Account permissions** tab: leave everything unchecked.
8. Click **Invite user** (or **Apply** — UI wording varies).

## Part 3 — Wire the JSON into Fly (30 sec)

In your Terminal (not through Claude — the JSON contains a private key that shouldn't land in a shared log):

```bash
fly secrets set GOOGLE_PLAY_SERVICE_ACCOUNT_JSON="$(cat /path/to/bump-play-XXXXXXXXXX.json)" -a bump
```

Fly will stage the secret and restart machines automatically. Wait ~10 seconds, then clear the dev-mode bootstrap:

```bash
fly secrets unset BUMP_DEV_MODE -a bump
```

Ping Claude back and we'll verify via `fly logs`:
- The `WARNING: BUMP_DEV_MODE` line should be **gone**
- No `Failed to parse service account JSON` line
- `Dev device allowlist: 0 entries (production mode)` still present
- `Server public key (...)` still matches the Kotlin constant
- A test `POST /verify` with a known-invalid purchase token now returns `{"valid": false}` (because it's hitting the real Google Play API) instead of the previous dev-mode always-true behavior

---

## Important caveat: propagation delay

Play Console takes **up to 24 hours** to propagate a new service account permission grant to the Android Publisher API. If you do this and immediately test `/verify`, you may get `permission denied` errors for the first few hours even though every click was correct. This is normal. Wait it out.

For the closed testing timeline, this doesn't block anything — keep `BUMP_DEV_MODE=true` during the 14-day internal test, then switch to the real service account when you're ready to promote to production track. **Set a day-13 calendar reminder** so you don't forget.

---

## Rotation

If this JSON key is ever leaked, compromised, or suspected compromised:

1. GCP Console → IAM & Admin → Service Accounts → `bump-play-verifier` → Keys → **Add key → Create new key → JSON**. Download.
2. `fly secrets set GOOGLE_PLAY_SERVICE_ACCOUNT_JSON="$(cat new-key.json)" -a bump` — Fly restarts machines automatically.
3. Verify `fly logs -a bump` shows no warnings.
4. Back in GCP Console → Keys tab → **delete** the old key.
5. Shred the old JSON from your local disk.

Full procedure also documented in `OPERATIONS.md §2`.

---

## If you ever need to revoke access entirely (e.g., shutting down the project)

1. Play Console → Setup → API access → find `bump-play-verifier` → **Remove access**.
2. GCP Console → IAM & Admin → Service Accounts → delete `bump-play-verifier`.
3. GCP Console → APIs & Services → Library → Google Play Android Developer API → **Disable**.
4. `fly secrets unset GOOGLE_PLAY_SERVICE_ACCOUNT_JSON -a bump` — then either set `BUMP_DEV_MODE=true` for graceful degradation or let the server crash-loop on restart (B4 guardrail fires).
