+++
title = "Client-Side Rate Limiting: A Lazily-Refilled Token Bucket with Semaphore"
date = 2026-08-26
description = "Starting from real production code, this post explains token-bucket rate limiting: why model tokens as semaphore permits, why refill lazily, and how to layer 429 backoff-retry on top."
[taxonomies]
tags = ["rust", "rate-limiter"]
[extra]
toc = true
+++

Rate limiting is usually thought of as "a server-side concern" — gateways, Nginx, Redis counters. But when writing an API SDK or CLI client, **client-side rate limiting** matters just as much: throttling yourself *before* firing a request protects the server from being hammered, while still allowing short bursts after an idle period.

This post starts from a piece of real client-side rate-limiting code and explains the design of a **token bucket**: why model tokens as `Semaphore` permits, why use "lazy refill" instead of a background timer, and how to layer a 429 backoff-retry on top.

<!-- more -->

## 1. What Is a Token Bucket

An analogy makes it click: picture a **bucket** with a faucet **dripping tokens in at a constant rate**:

- The bucket has a fixed capacity `max_tokens` (overflow is discarded);
- Tokens are refilled at a constant `tokens_per_second`;
- Every incoming request must **take one token** to pass;
- When the bucket is empty, the request **waits** until a new token arrives.

Two parameters fully determine its behavior:

| Parameter | What it controls |
|------|---------|
| **Rate** `tokens_per_second` | Long-term average throughput (steady-state limit) |
| **Capacity** `max_tokens` | Instantaneous burst capacity (how many you can fire at once) |

The key difference from a **leaky bucket** is this: a leaky bucket drains at a constant rate and allows no bursts; a token bucket lets accumulated tokens be spent all at once and **allows bursts** — after an idle spell it can fire a batch of requests instantly. That makes it more flexible and a better fit for the real traffic shape of a client.

## 2. Modeling Tokens as Semaphore Permits

> Full source analyzed in this post: [spursy/cli-terminal · src/api/rate_limiter.rs](https://github.com/spursy/cli-terminal/blob/master/src/api/rate_limiter.rs). The snippets below are lightly trimmed for exposition.

The core idea: use the number of **permits** in a `tokio::sync::Semaphore` to represent "the tokens currently available in the bucket."

```rust
use std::sync::OnceLock;
use std::time::Duration;
use tokio::sync::Semaphore;
use tokio::time::{sleep, Instant};

pub struct RateLimiter {
    /// Semaphore: available permits == tokens currently in the bucket.
    semaphore: Semaphore,
    /// Refill rate: tokens added per second.
    tokens_per_second: u32,
    /// Bucket capacity (burst limit).
    max_tokens: u32,
    /// Timestamp of the last refill.
    last_refill: tokio::sync::Mutex<Instant>,
}

impl RateLimiter {
    pub fn new(tokens_per_second: u32, max_tokens: u32) -> Self {
        Self {
            semaphore: Semaphore::new(max_tokens as usize), // start full
            tokens_per_second,
            max_tokens,
            last_refill: tokio::sync::Mutex::new(Instant::now()),
        }
    }
}
```

At construction, `Semaphore::new(max_tokens)` starts the bucket full — bursts up to the limit are allowed right away.

## 3. Lazy Refill: No Background Timer

The obvious way to refill is to spawn a background task that adds tokens every second. But that means keeping an extra timer task alive and managing its lifecycle. This implementation takes a lighter approach — **lazy refill: instead of pushing tokens proactively, each time a request arrives it computes how many tokens are owed based on "how long has passed since the last refill."**

```rust
impl RateLimiter {
    /// Add tokens proportional to elapsed time, capped at max_tokens.
    async fn refill_tokens(&self) {
        let mut last_refill = self.last_refill.lock().await;
        let now = Instant::now();
        let elapsed = now.duration_since(*last_refill);

        // elapsed time × rate = tokens to add.
        let tokens_to_add = (elapsed.as_secs_f64() * f64::from(self.tokens_per_second)) as u32;

        if tokens_to_add > 0 {
            let current = self.semaphore.available_permits() as u32;
            let room = self.max_tokens.saturating_sub(current); // headroom to full
            let tokens_to_add = tokens_to_add.min(room);        // never exceed capacity

            if tokens_to_add > 0 {
                self.semaphore.add_permits(tokens_to_add as usize);
                // ★ Only advance the clock when tokens were actually added.
                *last_refill = now;
            }
        }
    }
}
```

There's an **easily-missed detail** here (and it's the subtlest part):

> **Only advance the `last_refill` clock when tokens were actually added.**

Suppose the rate is 10 tokens/sec (one every 100ms), but requests come in densely, every 30ms. If you unconditionally set `last_refill = now` on each entry, then `tokens_to_add = 0.03 × 10 = 0` (truncated to 0), yet the clock got advanced — **those 30ms are silently discarded**, and the bucket never refills.

The correct approach: **do not advance the clock for a sub-token elapsed window**; let it keep accumulating until it's worth a whole token, then add it all at once and advance. This is what makes the long-term rate exactly equal to `tokens_per_second`.

## 4. Acquiring a Token: try_acquire + forget

```rust
impl RateLimiter {
    /// Acquire a token; if none, sleep until the next one is due, then retry.
    pub async fn acquire(&self) {
        loop {
            self.refill_tokens().await; // top up by elapsed time first

            // Try to take a token without blocking.
            if let Ok(permit) = self.semaphore.try_acquire() {
                // ★ forget prevents the permit from being returned on drop,
                //   so tokens are only ever replenished by refill_tokens —
                //   this is what makes "taking" a token actually stick.
                permit.forget();
                return;
            }

            // Bucket empty: compute when the next token is due, sleep, retry.
            let last_refill = *self.last_refill.lock().await;
            let elapsed = Instant::now().duration_since(last_refill);
            let time_per_token = Duration::from_secs_f64(1.0 / f64::from(self.tokens_per_second));
            let wait_time = time_per_token
                .saturating_sub(elapsed)
                .max(Duration::from_millis(1)); // at least 1ms to avoid busy-loop
            sleep(wait_time).await;
        }
    }
}
```

The key here is **`permit.forget()`**.

Semaphore permits follow RAII by default: once the `permit` returned by `try_acquire()` goes out of scope and is dropped, the permit is **automatically returned** to the semaphore. If we let it return, the act of "taking a token" is undone — the token count in the bucket never actually drops, which means no rate limiting at all.

`forget()` explicitly "forgets" the permit so it is **never returned**. As a result the token count only ever decreases, and the sole channel that brings it back up is `add_permits` inside `refill_tokens`. **This is precisely the finishing touch that turns a "semaphore" into a proper "token bucket."**

When no token is available, it doesn't spin — it computes exactly "how long until the next token is due" and sleeps for that long before retrying; the `max(1ms)` guards against busy-spinning.

## 5. Layering 429 Backoff-Retry on Top

Client-side limiting controls **your own** send rate, but the server may have its own, stricter limit and return `429 Too Many Requests`. So we wrap another layer of "server-side limit fallback" on top of the token bucket:

```rust
impl RateLimiter {
    pub async fn execute<F, T, E>(&self, _request_name: &str, mut f: F) -> Result<T, E>
    where
        F: FnMut() -> std::pin::Pin<Box<dyn std::future::Future<Output = Result<T, E>> + Send>>,
        E: std::fmt::Display,
    {
        const MAX_RETRIES: u32 = 3;
        let mut retry_count = 0;
        let mut backoff = Duration::from_secs(1);

        loop {
            self.acquire().await; // ① pass our own token bucket first

            match f().await {
                Ok(result) => return Ok(result),
                Err(e) => {
                    let msg = format!("{e}");
                    let is_rate_limit = msg.contains("429")
                        || msg.contains("rate limit")
                        || msg.contains("too many requests");

                    // ② hit a server-side limit → exponential backoff retry.
                    if is_rate_limit && retry_count < MAX_RETRIES {
                        retry_count += 1;
                        sleep(backoff).await;
                        backoff *= 2; // 1s → 2s → 4s
                        continue;
                    }
                    return Err(e); // other errors return immediately
                }
            }
        }
    }
}
```

A few design notes:

- **Double protection**: `acquire()` respects your own limit, the 429 backoff respects the server's limit; the two stack.
- **The closure must be `FnMut`**: because we **retry**, the same request `f` may be invoked multiple times, so it cannot be a `FnOnce` that can only be called once.
- **Returns `Pin<Box<dyn Future + Send>>`**: this makes `execute` a generic wrapper that can accept any async request; `Send` is required so it can be scheduled across threads on Tokio's multi-threaded runtime.
- **Detecting the limit via error text**: `format!("{e}")` matches keywords, hence the `E: Display` bound. It's a pragmatic, duck-typing-ish approach, at the cost of coupling to the wording of the error message.

Callers typically use it like this:

```rust
let quote = limiter
    .execute("get_quote", || {
        Box::pin(async move { client.get_quote("AAPL").await })
    })
    .await?;
```

## 6. A Process-Wide Singleton: OnceLock Lazy Init

Rate limiting is only meaningful when **everyone shares the same bucket** — if every call `new`s a fresh bucket, there's no limiting at all. Use `OnceLock` to build a "process-wide, lazily-initialized, thread-safe" singleton:

```rust
static RATE_LIMITER: OnceLock<RateLimiter> = OnceLock::new();

pub fn global_rate_limiter() -> &'static RateLimiter {
    // Constructed only on first call; concurrent first calls create exactly one.
    RATE_LIMITER.get_or_init(|| RateLimiter::new(10, 20))
}
```

Why `OnceLock` rather than a plain `static`? Because a `static`'s initial value must be a **compile-time constant**, whereas `RateLimiter::new(10, 20)` can only be computed at runtime. `OnceLock` starts empty and only calls `get_or_init` on the first invocation of `global_rate_limiter()`, guaranteeing that concurrent first calls initialize exactly once. What you pass in is a **closure**, not a value — so construction runs only when actually needed (lazy evaluation).

The default `10 req/s, burst 20` is a conservative config: at most 10 requests per second long-term, but after idling and refilling to full it can fire 20 at once.

## 7. Key Design Takeaways

| Design point | How | Why |
|--------|------|--------|
| Tokens = semaphore permits | `Semaphore` + `permit.forget()` | Reuse a battle-tested concurrency primitive; `forget` makes "taking" stick |
| Lazy refill | Request-driven, by elapsed time | Avoids a background timer; lightweight |
| Advance clock only when adding tokens | Don't discard sub-token time | Keeps the long-term rate exactly at the target |
| Wait instead of reject | `sleep` until the next token | Clients usually prefer "a bit slower" over "failed" |
| 429 backoff | Exponential retry, up to 3 times | Respect the server's limit; double protection |
| Global singleton | `OnceLock` lazy init | Share one bucket, or limiting is pointless |

In one sentence: **represent tokens as semaphore permit counts, use `forget()` to consume and `add_permits` to refill lazily, keep the rate precise by "only advancing the clock when tokens are added," then layer 429 exponential backoff on top and collapse it all into a process-wide singleton via `OnceLock`.** That's a sub-140-line client-side token bucket with no background task.
