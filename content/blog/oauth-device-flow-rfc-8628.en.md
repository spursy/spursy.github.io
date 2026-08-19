+++
title = "How OAuth 2.0 Device Flow (RFC 8628) Actually Works"
date = 2026-08-19
description = "Starting from 'how does an input-constrained device log in', a walkthrough of OAuth 2.0's Device Authorization Grant (RFC 8628): its motivation, how it differs from other grant types, the division of labor between device_code and user_code, and the three-state convergence of client polling — illustrated with cli-terminal's Rust code."
[taxonomies]
tags = ["rust", "oauth"]
+++

`gh auth login`, `aws sso login`, logging into YouTube/Netflix on a TV — these login experiences share one thing: the device itself never lets you type a password. Instead it shows you a short code and a URL, and you complete authorization in a browser on your phone or laptop. Behind all of them is the same standard: **OAuth 2.0's Device Authorization Grant (RFC 8628)**, a.k.a. Device Flow / device code mode.

This post isn't about any one tool; it explains the flow itself: what problem it solves, how it differs from the other OAuth grants, and what's exchanged at each step. I'll illustrate each part with code from [cli-terminal](https://github.com/spursy/cli-terminal), a small Rust CLI that demonstrates the flow.

<!-- more -->

## 1. The problem it solves: how does an input-constrained device log in

Imagine logging in on these devices: a command-line tool, a smart TV, a router, a headless server, an IoT device. Their shared predicament is being **input-constrained / lacking a (usable) browser**:

- Typing a password with a TV remote is torture;
- Making a user enter credentials in a CLI — plus handling SSO, 2FA, QR codes — can hardly be done gracefully;
- A headless server has no browser at all.

The core idea of device flow is a single sentence:

> **Move the act of "authorizing" off the constrained device and onto the browser-capable device already in the user's hand.**

The constrained device does just two lightweight things — **display a short code and a URL**, then **poll for the result**. The real login (which can involve SSO, enterprise single sign-on, 2FA, QR codes) happens in the browser on the user's phone or laptop.

## 2. What makes it special among OAuth grants

OAuth 2.0 has several grant types; which one you pick depends on "who is logging in, and does the device have a browser":

| Grant | Fits | Does the device touch the password | Needs a browser callback |
|-------|------|-------------------------------------|--------------------------|
| Authorization Code | Web / desktop apps with a browser | No | Yes (callback URL / port) |
| **Device Flow (this post)** | **Input-constrained devices / CLIs** | **No** | **No (polling instead)** |
| Resource Owner Password | Highly trusted first-party apps | **Yes** (discouraged) | No |
| Client Credentials | Service-to-service (no user) | N/A | No |

The two most important properties of device flow:

1. **The device never gets the user's password.** The password only travels between "browser ↔ authorization server"; the device ends up with just an access token. Even if the device is compromised, the exposure is limited to that revocable token.
2. **No callback port / public address needed.** The Authorization Code flow has to run a callback server on `localhost` to receive the result; device flow instead uses **client-driven polling**, so it works fine behind firewalls and NAT — especially friendly for CLIs and server environments.

## 3. The two "codes" and their roles: device_code vs user_code

At the start of the flow the server returns two codes — don't mix them up:

- **`user_code`** — **for the human**. Short, easy to read and type (e.g. `MOCK-0042`), shown on the constrained device; the user enters it into the browser.
- **`device_code`** — **for the machine**. Long, opaque; it's the device's identity when it later polls `POST /token`. The user never needs to see it.

Plus a `verification_uri` (the URL the user opens). Many servers also provide a `verification_uri_complete` — **the user_code already embedded in the URL's query string** — so the user can just click the link without typing the code, a nicer experience.

## 4. The flow, end to end

The whole flow has two phases: "get the codes" and "poll".

### Phase 1: get the codes (POST /device/authorize)

The device requests authorization with its `client_id`, and the server returns four things: `device_code`, `verification_uri_complete`, the poll `interval`, and the lifetime `expires_in`. cli-terminal carries them in a struct:

```rust
struct DeviceAuthorization {
    device_code: String,
    verification_uri_complete: String,
    interval: u64,
    expires_in: u64,
}
```

Parsing the response has a small robustness touch — prefer `verification_uri_complete` (the full link with the code), and fall back to the bare `verification_uri` if the server didn't send one:

```rust
let verification_uri_complete = resp["verification_uri_complete"]
    .as_str()
    .or_else(|| resp["verification_uri"].as_str())
    .unwrap_or_default()
    .to_owned();
```

Then the device shows the URL to the user and makes a best-effort attempt to open the browser directly (if it can't, no harm — the user opens it manually):

```rust
let opened = open_browser(verification_url);
println!("Open the following URL in your browser to authorize:");
println!("{verification_url}");
if opened {
    println!("Browser opened. Waiting for authorization...");
} else {
    println!("Waiting for authorization...");
}
```

### Phase 2: poll (POST /token)

Now the device waits: every `interval` seconds it asks the server, "authorized yet?", carrying the `device_code`. The key part of the request is that long, standardized `grant_type`:

```rust
let raw = http_client
    .post(&url)
    .form(&[
        ("client_id", client_id),
        ("grant_type", "urn:ietf:params:oauth:grant-type:device_code"),
        ("device_code", device_code),
    ])
    .send()
    .await?;
```

Meanwhile the user logs in, completes 2FA, and clicks "Authorize" in the browser. The device sees none of this; it's just polling.

## 5. The three-state convergence of polling: the soul of device flow

Polling isn't "dumbly wait for success". Each `POST /token` response pushes a state machine toward one of three kinds of outcome. Understand these three states and you understand how to write an RFC 8628 client:

| Server returns | Meaning | Client action |
|----------------|---------|---------------|
| HTTP 2xx (with token) | User authorized | Parse and save the token, **finish successfully** |
| `authorization_pending` | User hasn't authorized yet | **Keep** polling at the interval |
| `slow_down` | You're polling too fast | Increase interval by **at least 5s** (§3.5), then continue |
| `access_denied` / `expired_token` / … | User denied / code expired, etc. | **Terminate immediately** with an error |

In code, a single `match` cleanly separates "continue" from "terminate":

```rust
let err_resp = raw.json::<serde_json::Value>().await.unwrap_or_default();
match err_resp["error"].as_str() {
    // User hasn't finished authorizing — keep polling
    Some("authorization_pending") => {}
    // RFC 8628 §3.5: polling too fast, add at least 5 seconds
    Some("slow_down") => interval_ms += 5000,
    // Any other error code (access_denied, expired_token, ...) is terminal
    Some(other) => anyhow::bail!("Authorization failed: {other}"),
    None => anyhow::bail!("Unexpected token poll response"),
}
```

Three takeaways:

1. **`authorization_pending` is the normal case, not an error.** It maps to an empty match arm — do nothing, let the loop proceed to the next round. The vast majority of responses while polling are this.
2. **`slow_down` is the server's rate-limit signal, and must be obeyed.** The RFC requires adding at least 5 seconds to the interval on receipt (`interval_ms += 5000`). This is a protocol-level "be polite to the server" contract.
3. **Other error codes are terminal and must bubble up immediately.** If the user denied (`access_denied`) or the code expired (`expired_token`), there's no point polling further — error out right away.

## 6. Don't forget the deadline: expires_in

The `device_code` isn't valid forever — the server gives an `expires_in` (cli-terminal defaults to 300 seconds as a fallback). The poll loop must respect this deadline, or it's an infinite wait.

cli-terminal handles it carefully: compute an absolute `deadline` up front, check the remaining time each round, and **clamp every sleep with `min(remaining)`** so it never "oversleeps past the deadline":

```rust
let deadline = Instant::now() + Duration::from_secs(expires_in);
loop {
    let remaining = deadline.saturating_duration_since(Instant::now());
    if remaining.is_zero() {
        anyhow::bail!("Device authorization timed out");
    }
    tokio::time::sleep(Duration::from_millis(interval_ms).min(remaining)).await;
    // ... POST /token, then handle the three states from the previous section
}
```

`saturating_duration_since` also quietly avoids underflow if the clock goes backwards — small, unglamorous details that make the poll loop genuinely robust.

## 7. After success

Eventually one poll returns HTTP 2xx with an `access_token` in the body, usually along with a `refresh_token` and `expires_in`. The device stores them together with an **absolute expiry timestamp**:

```rust
let now = SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs();
return Ok(StoredToken {
    client_id: client_id.to_string(),
    access_token: access_token.to_string(),
    refresh_token,
    expires_at: now + expires_in,   // store absolute time; checking expiry is just a compare with now
});
```

Login is now complete. Every subsequent protected call carries this access token; when it's near expiry, the refresh_token renews it silently so the user doesn't have to log in again.

## 8. Wrap-up

The essence of device flow is really three sentences:

- **Move authorization onto a browser-capable device**; the constrained device only displays "short code + URL" and polls for the result;
- **The two codes have distinct jobs**: `user_code` for the human to enter, `device_code` for the machine to poll with;
- **Polling is a three-state converging machine**: `authorization_pending` continues, `slow_down` backs off, terminal errors exit immediately, all backstopped by an `expires_in` deadline.

Precisely because it never touches the user's password, needs no callback port, and can reuse the browser's full login capabilities, it became the de facto standard for CLI, TV, and IoT login. For a minimal implementation you can run end-to-end locally, see [github.com/spursy/cli-terminal](https://github.com/spursy/cli-terminal).

## Key source locations

| What | Where |
|------|-------|
| Device authorization request / polling three states / expires_in deadline | [`src/auth.rs`](https://github.com/spursy/cli-terminal/blob/main/src/auth.rs) |
| Minimal local mock OAuth server (incl. /device/authorize, /token) | [`src/bin/oauth/authorization.rs`](https://github.com/spursy/cli-terminal/blob/main/src/bin/oauth/authorization.rs) |
