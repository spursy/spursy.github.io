+++
title = "cli-terminal: A Few Careful Designs in an OAuth Device-Flow CLI"
date = 2026-08-17
description = "Starting from cli-terminal, a small Rust CLI that demonstrates RFC 8628 device-code login, this post distills four reusable engineering designs: encrypted machine-bound token storage, bearer-as-default-header, a lazily-refilled token-bucket limiter, and transparent pre-call token refresh."
[taxonomies]
tags = ["rust", "oauth"]
+++

[cli-terminal](https://github.com/spursy/cli-terminal) is a small Rust command-line tool that demonstrates the **OAuth 2.0 Device Authorization Grant (RFC 8628)**, a.k.a. "Device Flow" / device-code login. Its surface is tiny — `auth login` / `logout` / `status` plus a `whoami` — but several choices it makes around *credential safety* and *client self-discipline* are worth pulling out on their own.

This post isn't about the device-flow protocol itself (that's the RFC's job); it focuses on four engineering design points you can lift straight into other projects.

<!-- more -->

## 1. One sentence of background

The device-code grant solves "how does an **input-constrained device** (TV, CLI, IoT) log in": the device gets a `device_code`, hands the user a short URL, the user authorizes in a browser on their phone/laptop, and the device polls `POST /token` until the token arrives.

cli-terminal stores the resulting token locally (`~/.cli-terminal/openapi/cli-auth`), and every subsequent protected call reuses it automatically. Around the four verbs — *how to store, how to carry, how to use, how to renew* — the code hides four careful choices.

## 2. Design #1: Encrypted token storage + machine binding (the one to steal)

A CLI stores its token in a file; the naive approach is plaintext JSON. cli-terminal doesn't do that — it **encrypts at rest**, and it binds the key to **the current machine**: a stolen credential file simply won't decrypt on another box.

The on-disk format is self-describing and minimal:

```
MAGIC(b"AG\x01") || NONCE[12] || CIPHERTEXT+TAG
```

The key isn't hard-coded; it's **derived from a machine-unique identifier via HKDF-SHA256** into an AES-256 key (`src/token.rs`):

```rust
/// HKDF `info` string; versioned so the derivation scheme can evolve.
const HKDF_INFO: &[u8] = b"cli-token-v1";

/// Derive the AES-256 key from the machine ID via HKDF-SHA256.
fn machine_derived_key() -> Key<Aes256Gcm> {
    let id = machine_id();
    let hk = Hkdf::<sha2::Sha256>::new(None, id.as_bytes());
    let mut key_bytes = [0u8; 32];
    // HKDF expand is infallible for output lengths <= 255 * HashLen.
    let _ = hk.expand(HKDF_INFO, &mut key_bytes);
    *Key::<Aes256Gcm>::from_slice(&key_bytes)
}
```

Encryption uses AES-256-GCM with a random nonce, concatenating `MAGIC || NONCE || CIPHERTEXT`:

```rust
fn encrypt(plaintext: &[u8]) -> Result<Vec<u8>> {
    let key = machine_derived_key();
    let cipher = Aes256Gcm::new(&key);
    let nonce = Aes256Gcm::generate_nonce(&mut OsRng);
    let ciphertext = cipher
        .encrypt(&nonce, plaintext)
        .map_err(|e| anyhow::anyhow!("AES-GCM encrypt failed: {e}"))?;

    let mut out = Vec::with_capacity(MAGIC.len() + nonce.len() + ciphertext.len());
    out.extend_from_slice(MAGIC);
    out.extend_from_slice(&nonce);
    out.extend_from_slice(&ciphertext);
    Ok(out)
}
```

A few details deserve a pause:

### 1. Machine binding = defense in depth

The input to key derivation is `machine_id()`. That means the ciphertext **can only be decrypted on the machine that produced it**. Even if the credential file is copied off and uploaded elsewhere, an attacker can't recover the plaintext token. It's a layer of defense *on top of* file permissions.

### 2. A versioned `HKDF_INFO`

The `v1` in `b"cli-token-v1"` isn't decoration. HKDF's `info` parameter participates in derivation, so versioning it means that **when the scheme changes later** (add a salt, swap the algorithm), old ciphertext naturally stops decrypting and can be migrated cleanly — no accidental key collision with the new scheme. A cheap bit of foresight.

### 3. GCM brings integrity for free

AES-**GCM** is an AEAD; the ciphertext carries an authentication tag. Any tampering (even a single flipped bit) makes decryption *fail* rather than silently yield garbage. The repo tests exactly this:

```rust
#[test]
fn decrypt_fails_on_tampered_ciphertext() {
    let mut encrypted = encrypt(b"some token data").expect("encrypt failed");
    // Flip a byte in the ciphertext region (after MAGIC + NONCE).
    let idx = MAGIC.len() + 12;
    encrypted[idx] ^= 0xFF;
    assert!(decrypt(&encrypted).is_err(), "tampered data must not decrypt");
}
```

### 4. Atomic write + 0600 perms

Persistence uses the classic "**write a temp file, then rename**" atomic swap, so a crash mid-write never leaves a half-corrupted file; on Unix it also tightens the mode to `0600`:

```rust
let tmp = path.with_extension("tmp");
fs::write(&tmp, &encrypted)?;
fs::rename(&tmp, &path)?;   // rename is atomic within one filesystem
harden_file_permissions(&path); // Unix: chmod 0600
```

### 5. Backward-compatible migrate-on-read

If some historical version once wrote plaintext JSON, `load()` detects the old format by the **absence of the MAGIC header**, parses it, and **immediately rewrites it in the encrypted format** — the next read is ciphertext, and the user never notices the upgrade:

```rust
if bytes.starts_with(MAGIC) {
    // New format: decrypt
} else {
    // Legacy plaintext JSON: parse, then re-save encrypted
    let token: StoredToken = serde_json::from_slice(&bytes)?;
    let _ = save(&token);
    Ok(Some(token))
}
```

> A pragmatic trade-off: when the machine ID is unavailable (e.g. a minimal container) it falls back to an empty string. Encryption still works; it just loses the machine-binding layer — degrade, don't error. "Works" comes before "maximally secure."

## 3. Design #2: Bearer as a default header + sensitive values kept out of logs

Once you have the token, how do you make **every** API call carry it? One way is `.header("Authorization", ...)` at each call site — repetitive and easy to forget. cli-terminal instead sets the bearer as the HTTP client's **default header**: build once, and every later request carries it automatically (`src/api/client.rs`):

```rust
pub fn build_authenticated_client(access_token: &str) -> Result<reqwest::Client> {
    let mut bearer = HeaderValue::from_str(&format!("Bearer {access_token}"))?;
    bearer.set_sensitive(true); // key: keep it out of debug logs

    let mut headers = HeaderMap::new();
    headers.insert(AUTHORIZATION, bearer);

    reqwest::Client::builder()
        .timeout(Duration::from_secs(15))
        .default_headers(headers)  // every request now carries it
        .build()
        .context("failed to build authenticated HTTP client")
}
```

Two points:

- **`default_headers` injects the credential once and applies it everywhere**, so a call site like `whoami` collapses to a single request that doesn't have to think about auth.
- **`set_sensitive(true)`** is the easily-missed but crucial step: it flags the header as sensitive, so when reqwest prints a `HeaderMap` via `Debug` it masks the value as `Sensitive`, preventing the token from being accidentally `{:?}`-logged. Credentials should be *unobservable by default* — a good habit.

## 4. Design #3: A token-bucket limiter with lazy refill (no background timer)

A well-behaved client of an open platform should **rate-limit itself** rather than wait for a 429 to back off. cli-terminal ships a **token-bucket limiter** (`src/api/rate_limiter.rs`), and the neat part is that it has **no background timer** — refill is driven *lazily* by the requests themselves:

```rust
pub async fn acquire(&self) {
    loop {
        self.refill_tokens().await;          // top up based on elapsed time

        if let Ok(permit) = self.semaphore.try_acquire() {
            permit.forget();                 // consume: don't return it on drop
            return;
        }

        // No token yet: compute when the next one is due, sleep, retry.
        let elapsed = Instant::now().duration_since(*self.last_refill.lock().await);
        let time_per_token = Duration::from_secs_f64(1.0 / f64::from(self.tokens_per_second));
        let wait_time = time_per_token.saturating_sub(elapsed).max(Duration::from_millis(1));
        sleep(wait_time).await;
    }
}
```

The clever bits:

- **`Semaphore` permits *are* the tokens.** A successful `try_acquire()` takes one token, and `permit.forget()` **prevents it from being returned on drop** — so tokens are only ever replenished explicitly by `refill_tokens`, never "self-healing." That's what makes the bucket semantics correct.
- **Lazy refill:** `refill_tokens` adds tokens proportional to the *real* time since the last refill × the rate, capped at `max_tokens` (allowing bursts). No background tokio task spinning; nothing to clean up when the CLI exits.
- **`last_refill` only advances when tokens are actually added**, so a sub-token sliver of elapsed time isn't repeatedly discarded (which would make the limiter drift slow over time).

On top of that, `execute()` layers **exponential backoff-retry on server-side 429s**, uniting self-discipline with a safety net:

```rust
if is_rate_limit && retry_count < MAX_RETRIES {
    retry_count += 1;
    sleep(backoff).await;
    backoff *= 2;   // 1s → 2s → 4s
    continue;
}
```

So: **stay under the cap with the token bucket first**, and if you still hit server-side throttling, **yield with exponential backoff**. Two layers of client-side rate politeness.

## 5. Design #4: Transparent pre-call refresh + slow_down backoff

The last two small-but-tidy points are both about *time*.

### 1. Transparent refresh before protected calls

Every protected request goes through `request_json_body`, whose first step is always `refresh_if_expired()` — **before the request is actually sent**, it checks whether the access token has expired and, if so, silently swaps in a new one using the refresh token. The user notices nothing:

```rust
pub async fn request_json_body(/* ... */) -> Result<serde_json::Value> {
    crate::auth::refresh_if_expired().await?;   // ① renew if expired
    let stored = require_token()?;               // ② token is now guaranteed fresh
    let http = build_authenticated_client(&stored.access_token)?;
    // ③ send the request with a fresh token…
}
```

The "is it expired?" check also leaves a **60-second safety margin**, so the token can't expire in the network latency between "check passed" and "request reaches the server":

```rust
pub fn is_expired(&self) -> bool {
    let now = /* unix now */;
    self.expires_at <= now + 60   // treat as expired 60s early
}
```

### 2. Honoring slow_down while polling (RFC 8628 §3.5)

While polling `POST /token`, the server may return `slow_down` asking the client to ease off. cli-terminal dutifully complies — each time it receives one, it **adds 5 seconds** to the poll interval (`src/auth.rs`):

```rust
match err_resp["error"].as_str() {
    Some("authorization_pending") => {}          // not authorized yet, keep polling
    Some("slow_down") => interval_ms += 5000,    // §3.5: add at least 5 seconds
    Some(other) => anyhow::bail!("Authorization failed: {other}"), // terminal error
    None => anyhow::bail!("Unexpected token poll response"),
}
```

Each sleep is also clamped with `interval.min(remaining)`, guaranteeing it **never overshoots the `expires_in` deadline** — no "slept a bit too long and missed the window" footgun.

## 6. Wrap-up

Abstracted, the four design points are four general lessons:

| Design point | General lesson |
|--------------|----------------|
| Encrypted + machine-bound token | Don't leave local credentials naked; binding the key to the environment is cheap defense-in-depth; a self-describing, versioned format eases evolution |
| Bearer default header + `set_sensitive` | Inject the credential once, apply it everywhere; keep sensitive values unobservable by default |
| Token bucket + lazy refill + 429 backoff | Self-discipline first, safety net second; counting permits removes the need for a background timer |
| Pre-call refresh + safety margin + slow_down | Hide "renewal" inside the request path; leave slack on time boundaries and obey server signals verbatim |

None of them is complex on its own, but together they make a few-hundred-line demo CLI genuinely solid on both *credential safety* and *client politeness*. Full code at [github.com/spursy/cli-terminal](https://github.com/spursy/cli-terminal).

## Key source locations

| Topic | Location |
|-------|----------|
| Encrypted storage / machine-derived key / migrate-on-read | [`src/token.rs`](https://github.com/spursy/cli-terminal/blob/main/src/token.rs) |
| Bearer default header + `set_sensitive` | [`src/api/client.rs`](https://github.com/spursy/cli-terminal/blob/main/src/api/client.rs) |
| Token-bucket limiter + 429 backoff | [`src/api/rate_limiter.rs`](https://github.com/spursy/cli-terminal/blob/main/src/api/rate_limiter.rs) |
| Device flow / transparent refresh / slow_down | [`src/auth.rs`](https://github.com/spursy/cli-terminal/blob/main/src/auth.rs) |
