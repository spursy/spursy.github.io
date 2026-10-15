+++
title = "OIDC Single Sign-On in Rust: From Auth Principles to CSRF / SSRF Defense"
date = 2026-10-15
description = "Starting from 'what is OIDC', a walkthrough of how OIDC builds single sign-on (SSO) on top of OAuth 2.0's Authorization Code flow — with a sequence diagram covering the four auth steps (discovery, state, code→token, userinfo). Then, grounded in the oidc-poc Rust code, a deep dive into CSRF and SSRF: what they are, how the attacks work, and how oidc-poc defends against them — closing with the traits of implementing RESTful endpoints in axum."
[taxonomies]
tags = ["oidc", "sso", "rust", "csrf", "security"]
+++

Every time you click "Sign in with Google" or "Log in with your company account," you are walking the same standard: **OpenID Connect (OIDC)**. It moves "who are you" out of each individual app and consolidates it into a single identity provider (IdP) — that is the foundation of single sign-on (SSO).

This article uses a minimal example project, [oidc-poc](https://github.com/spursy/oidc-poc) (Rust + axum + React), to make the whole chain concrete: what OIDC is, how it implements SSO, how the code defends against CSRF and SSRF, and what's distinctive about writing this kind of RESTful endpoint in Rust.

## 1. What is OIDC

OIDC is an **authentication** layer built on top of **OAuth 2.0**. First, separate two things that are often conflated:

- **OAuth 2.0 solves authorization**: with your consent, it lets app A obtain a **token** to access resource B. It doesn't itself care "who you are."
- **OIDC solves authentication**: on top of OAuth 2.0's Authorization Code flow, it adds a standardized `id_token` (a JWT) and a `userinfo` endpoint, so an app can reliably learn **the identity of the person logging in** (email, sub, etc.).

In one line: **OAuth governs "can you get it," OIDC governs "who you are."** OIDC = OAuth 2.0 Authorization Code flow + an identity layer.

OIDC defines three key endpoints, all discoverable from a single **discovery document**:

| Endpoint | Purpose |
| -------- | ------- |
| `authorization_endpoint` | the user is redirected here to log in + consent |
| `token_endpoint` | the backend exchanges a one-time `code` for `access_token` / `id_token` |
| `userinfo_endpoint` | exchange the `access_token` for the user's identity (email, etc.) |

The discovery document lives at a fixed path: `{issuer}/.well-known/openid-configuration`. This means an app needs to **hardcode no endpoints** — just configure `OIDC_ISSUER`, and the endpoints are discovered at runtime. Swapping IdPs (say from self-hosted Keycloak to another) is a config change, not a code change. That's exactly what oidc-poc does:

```rust
#[derive(Clone, Deserialize)]
struct OidcMetadata {
    authorization_endpoint: String,
    token_endpoint: String,
    userinfo_endpoint: String,
}

async fn oidc_metadata(state: &AppState, issuer: &str) -> Option<OidcMetadata> {
    if let Some(meta) = state.oidc_meta.lock().unwrap().clone() {
        return Some(meta); // cache hit
    }
    let url = format!(
        "{}/.well-known/openid-configuration",
        issuer.trim_end_matches('/')
    );
    let resp = reqwest::Client::new().get(&url).send().await.ok()?;
    // ...parse and cache into AppState
}
```

## 2. How OIDC implements SSO

SSO boils down to: **multiple apps share one session at the IdP**. You log in at the IdP once; afterward each participating app only needs to redirect you to the IdP, which — seeing an existing session — hands your identity straight back without asking for a password again.

oidc-poc implements the standard **Authorization Code Flow**. The whole auth chain has four steps:

```text
Browser                   oidc-poc (backend)        IdP (Keycloak)
   │                          │                          │
   │ (1) GET /login/oidc      │                          │
   ├─────────────────────────>│                          │
   │                          │ read discovery (cached)   │
   │                          │ mint state, set cookie    │
   │ (2) 302 to authorize + Set-Cookie(state)            │
   │<─────────────────────────┤                          │
   │                          │                          │
   │ (3) follow 302 (carrying the IdP session)           │
   ├────────────────────────────────────────────────────>│
   │                          │      user logs in /       │
   │                          │      already in + consent │
   │ (4) 302 back to /login/oidc/callback?code=&state=    │
   │<────────────────────────────────────────────────────┤
   │ (5) GET /callback (browser auto-attaches state cookie)
   ├─────────────────────────>│                          │
   │                          │ (6) verify state == cookie│
   │                          │ (7) code → token ────────>│
   │                          │   (backend-to-backend, client_secret)
   │                          │<──────────── access_token │
   │                          │ (8) access_token → userinfo>
   │                          │<──────────────────── email│
   │ (9) 302 /login?signed_in=<email> + clear cookie      │
   │<─────────────────────────┤                          │
```

The trust chain of the four steps closes like this:

1. **The IdP has authenticated the user** → only then does it issue a `code`;
2. **The `code` can only be exchanged for a token by a backend holding `client_secret`** → proving the exchanging party is a legitimate client; and the `code` is single-use and short-lived, so it's hard to reuse even if leaked;
3. **`state == cookie`** → proving this callback is bound to the browser that started the flow (see the CSRF section);
4. **The email returned by `userinfo`** → a trusted identity the backend itself fetched from the IdP, not any plaintext in the URL.

All four together is what makes a login valid. The key security point: **oidc-poc never treats any unverified URL parameter as identity** — `email` always comes from the backend's call to `userinfo_endpoint`.

## 3. CSRF: Cross-Site Request Forgery

### What it is

CSRF exploits the fact that **browsers automatically attach cookies**. Once you're logged into site A, an attacker lures you to a malicious page that fires a request at A; the browser auto-attaches your A cookies, so the request "looks like" it came from you.

In the OIDC context there's a special variant called **Login CSRF**: the attacker doesn't steal your identity — they quietly **log you in as the attacker's account**.

### How the attack works

1. The attacker walks the Authorization Code flow partway at the IdP and obtains a `code` **belonging to the attacker's account**;
2. The attacker crafts a link: `https://victim-app/login/oidc/callback?code=<attacker's code>`;
3. They lure the victim into clicking it;
4. If the callback **doesn't verify that this callback was initiated by the user themselves**, the victim's browser completes the login with the attacker's `code` — and from then on everything the victim does in that app (uploading files, saving notes, linking a payment method) lands in the attacker's account, which the attacker later logs into to harvest.

### How oidc-poc defends

The core weapon is OAuth 2.0's `state` parameter, which oidc-poc **binds to a browser cookie**:

**At start** (`/login/oidc`) it mints a random `state` and sets it as a cookie:

```rust
fn new_state() -> String {
    let mut bytes = [0u8; 16];
    rand::rngs::OsRng.fill_bytes(&mut bytes); // 128 bits from the OS CSPRNG
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

fn set_state_cookie(state: &str) -> String {
    let secure = if oidc_redirect_uri().starts_with("https") { "; Secure" } else { "" };
    format!(
        "{OIDC_STATE_COOKIE}={state}; Path=/login/oidc; HttpOnly; SameSite=Lax; Max-Age=600{secure}"
    )
}
```

**At callback** (`/login/oidc/callback`) it checks that the `state` in the URL exactly equals the `state` in the cookie:

```rust
let csrf = query.state.unwrap_or_default();
let cookie_state = cookie_value(&headers, OIDC_STATE_COOKIE).unwrap_or_default();
if csrf.is_empty() || csrf != cookie_state {
    println!("/login/oidc/callback -> unknown or missing state; rejecting");
    return Redirect::to("/login?error=oidc_state").into_response();
}
```

Why does this stop Login CSRF? In the attacker's forged callback link, `state` is the value from the attacker's own flow; whereas the victim's browser either **has no** `oidc_state` cookie (they never started a flow) or has **a different** value. They don't match, so the callback is rejected outright. **The attacker cannot plant a matching cookie in the victim's browser** — that's the root of why the defense holds.

Each cookie attribute serves security:

| Attribute | Purpose |
| --------- | ------- |
| `HttpOnly` | page JS can't read it, blocking XSS theft of state |
| `SameSite=Lax` | blocks most cross-site auto-submits, yet survives the top-level GET navigation back from the IdP |
| `Path=/login/oidc` | sent only on the callback path, shrinking exposure |
| `Max-Age=600` | 10-minute expiry, so an unfinished login doesn't stay valid |
| `Secure` (on HTTPS) | travels only over an encrypted channel |

There's a bonus: putting state in a cookie rather than server memory makes validation **stateless** — multi-replica deployments and pod restarts don't matter; any instance can validate any callback. (This was the direct motivation for migrating from an in-memory `HashSet` to a cookie.)

## 4. SSRF: Server-Side Request Forgery

### What it is

SSRF makes the **server** issue requests on the attacker's behalf. The danger: servers often sit inside a private network and can reach addresses the outside world can't — cloud metadata services (`169.254.169.254`, which can hand out temporary credentials), internal admin consoles, database ports, and so on. Once an attacker controls "which URL the server fetches," they may pivot laterally using the server's identity.

### How the attack works

1. The app has a "fetch some URL based on a parameter" code path;
2. The attacker points that parameter at an internal address, e.g. `http://169.254.169.254/latest/meta-data/iam/security-credentials/`;
3. The server dutifully fires the request and returns or stores the internal response (such as cloud credentials);
4. The attacker uses those credentials to attack further.

In OIDC there's a natural "server fetches a URL" path — the **discovery request**: the backend does `GET {issuer}/.well-known/openid-configuration`. If `issuer` can be controlled by an attacker, that's a textbook SSRF entry point.

### How oidc-poc defends

oidc-poc's defense **eliminates the attack surface by design**: `issuer` **comes from no request parameter, only from the trusted environment variable** `OIDC_ISSUER`:

```rust
fn oidc_issuer() -> Option<String> {
    std::env::var("OIDC_ISSUER").ok().filter(|s| !s.is_empty())
}
```

Both places that use discovery — start and callback — take `issuer` from this function throughout, with no way for the user to inject it:

```rust
let issuer = match oidc_issuer() {   // from env, not from query / body
    Some(i) => i,
    None => return Redirect::to("/login?error=oidc_unconfigured").into_response(),
};
let meta = match oidc_metadata(&state, &issuer).await { /* ... */ };
```

This is the most robust SSRF defense: **keep user input out of the decision of "which address the server requests."** The config is set by operators at deploy time and lives inside the trusted boundary.

If some business genuinely needs a user/tenant-supplied issuer (multi-tenant SSO), you must stack defense-in-depth:

- **Allowlist**: permit only pre-registered issuer domains;
- **Scheme and address checks**: force `https`, resolve to an IP, and reject private ranges (`10/8`, `172.16/12`, `192.168/16`, `127/8`, `169.254/16`, `::1`, etc.);
- **Disable redirect following**: to avoid `https://evil → 302 → http://169.254.169.254` bypasses;
- **Egress proxy / policy**: restrict, at the network layer, where the server can connect.

By locking issuer into config, oidc-poc simply stands on the safest side.

## 5. Traits of the RESTful code in oidc-poc (Rust)

Dropping this OIDC chain onto axum, a few engineering traits are worth calling out:

**1. A handler is an async function that declares its dependencies via "extractors."** axum maps a URL to an `async fn`; whatever parameters you put in the signature, the framework extracts from the request. oidc-poc's callback uses three extractors at once:

```rust
async fn oidc_callback(
    State(state): State<AppState>,   // shared state (discovery cache)
    headers: HeaderMap,              // raw request headers (to read the cookie)
    Query(query): Query<OidcCallbackQuery>, // parse the query string into a typed struct
) -> Response { /* ... */ }
```

The ordering convention matters: extractors like `State`, `Query`, and `HeaderMap` that don't consume the body implement `FromRequestParts` — you can have many, in any order; one that consumes the body (like `Json<T>`) implements `FromRequest` — there can be only one, and it must come last.

**2. Type requests/responses with `#[derive(Deserialize)]`.** Query strings, the token response, and the userinfo response are each a struct; serde handles deserialization, missing fields fall back to `Option`, and extra fields are ignored. The code declares only "the fields I care about":

```rust
#[derive(Deserialize)]
struct OidcCallbackQuery { code: Option<String>, state: Option<String>, error: Option<String> }

#[derive(Deserialize)]
struct OidcTokenResponse { access_token: String }

#[derive(Deserialize)]
struct OidcUserinfo { email: Option<String> }
```

**3. Shared state via `State<AppState>` + `Arc<Mutex<…>>`.** The discovery document is fetched once and cached; `AppState` uses `Arc` so many requests share one copy and `Mutex` for concurrency safety. `#[derive(Clone)]` lets axum inject it into every handler — cloning only copies the `Arc` pointer, not the data:

```rust
#[derive(Clone, Default)]
pub struct AppState {
    oidc_meta: Arc<Mutex<Option<OidcMetadata>>>,
}
```

**4. Return a unified `Response`, expressing "errors" as "redirects."** This is a web login flow, not a JSON API, so every branch — success and every kind of failure — returns a `Response`, with failures 302-ing back to `/login?error=<code>`, which the frontend maps to a localized message. An error isn't a thrown exception; it's a normal exit of the flow:

```rust
return Redirect::to("/login?error=oidc_state").into_response();
```

**5. Self-contained, few dependencies.** Even URL encoding is a hand-rolled minimal implementation (passing through only RFC 3986 unreserved characters), avoiding an extra dependency for one small feature; the frontend SPA is embedded into the binary at compile time via `rust-embed`, so deployment is a single file. The overall style is "just enough, controllable, no magic."

## 6. Summary

- **OIDC = OAuth 2.0 Authorization Code flow + an identity layer**: OAuth for authorization, OIDC for authentication.
- **SSO** works by sharing the IdP session plus a discovery-driven Authorization Code flow: discovery → state → code→token → userinfo, a four-step closed trust chain where identity is only taken from `userinfo`.
- **CSRF (including Login CSRF)** is defended by "binding `state` to a browser cookie" — an attacker can't plant a matching cookie in the victim's browser; cookie-stored state also buys statelessness and horizontal scalability.
- **SSRF** is designed out: issuer comes only from a trusted env var, keeping user input out of "whom the server requests"; if user-supplied issuers are required, stack allowlisting + private-range blocking + no redirect following.
- **Traits of the Rust/axum RESTful code**: dependencies declared via extractors, typed with serde, shared state via `State + Arc<Mutex>`, errors as redirects, self-contained with few dependencies.

Full code at [github.com/spursy/oidc-poc](https://github.com/spursy/oidc-poc).
