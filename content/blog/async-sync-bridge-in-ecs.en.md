+++
title = "Doing Async Gracefully Inside a Sync Framework: the ECS async→sync Bridge"
date = 2026-09-12
description = "Bevy ECS systems are synchronous functions that must never await, yet quote pushes and REST calls are asynchronous. Using the RT / STOCKS / UPDATE_TX trio in tui-bevy-ecs, this post shows how the 'sync world' cleanly drives the 'async world' and gets results back safely — a pattern portable to any sync-framework + async-IO scenario."
[taxonomies]
tags = ["rust", "bevy", "ecs", "tokio", "async"]
+++

> Source: [github.com/spursy/tui-bevy-ecs](https://github.com/spursy/tui-bevy-ecs). This post focuses on the hardest part: bridging synchronous ECS with an async SDK.

## 1. The tension: ECS systems can't `await`

Bevy ECS systems are **plain synchronous functions**. The scheduler runs them synchronously per frame; the signature can't be `async`, and the body can't `.await`:

```rust
pub fn render_watchlist(/* ... */) {
    // this is synchronous; you cannot await anything here
}
```

But real-world data sources are all asynchronous:

- Quotes arrive via a WebSocket **push**.
- Refreshing hits a **REST** endpoint via `ctx.quote(..).await`.

**The two worlds can't call each other directly:** a sync system can't `await` an async request, and an async task can't freely touch the ECS World (it isn't a `Send`/`Sync`-friendly shared structure, and mutating the World concurrently would violate the scheduler's borrow assumptions).

This is the core tension every "sync framework + async IO" runs into. tui-bevy-ecs bridges it with **three global singletons**.

## 2. The trio: three pillars of the bridge

| Singleton | Type | Role |
| --- | --- | --- |
| `RT` | `OnceLock<tokio::runtime::Handle>` | Lets sync systems `RT.get().spawn(..)` to dispatch async background tasks |
| `STOCKS` | `LazyLock<StockStore>` | Global quote cache: background tasks write, render systems read |
| `UPDATE_TX` | `OnceLock<UnboundedSender<CommandQueue>>` | Background tasks push batched ops / redraw signals back to the main loop |

In one line: **`RT` dispatches async work, `STOCKS` shares data, `UPDATE_TX` sends results back to ECS.**

```
render system (sync) ──RT.spawn──▶ tokio async task ──write──▶ STOCKS cache
                                        │
                                        └──UPDATE_TX──▶ main loop ──apply──▶ ECS world (mark dirty)
```

The three pillars are wired up at startup:

```rust
// at the main loop's startup
let (update_tx, mut update_rx) = mpsc::unbounded_channel();
UPDATE_TX.set(update_tx).ok();
RT.set(tokio::runtime::Handle::current()).ok();   // stash the current tokio runtime handle in RT
```

## 3. The core: `refresh_stock_debounced`

This function walks the full path "sync system → async task → write cache → notify ECS":

```rust
pub fn refresh_stock_debounced(counter: Counter) {   // note: no `async`, a sync function
    let Some(rt) = RT.get() else { return };         // 1) grab the entry to the async world
    rt.spawn(async move {                            // 2) dispatch an async task (the key leap)
        // 3) debounce: stocks flicked past during fast navigation get dropped
        tokio::time::sleep(Duration::from_millis(150)).await;

        // 4) simulate a REST request, 5) write the global cache
        let (last_done, change_rate) = fake_rest_quote(&counter).await;
        STOCKS.modify(counter.clone(), |s| s.update_from_push_quote(last_done, change_rate));

        // 6) tell the main loop "there's an update, please redraw"
        if let Some(tx) = UPDATE_TX.get() {
            let queue = bevy_ecs::system::CommandQueue::default();
            let _ = tx.send(queue);
        }
    });
}
```

Why each part matters:

**1) The sync world grabs the async world's entry point**

```rust
let Some(rt) = RT.get() else { return };
```

`let ... else` binds the runtime handle if present, and `return`s early if not yet initialized. This is the sync side's first contact with the async side.

**2) Dispatch an async task — the key leap**

```rust
rt.spawn(async move { ... });
```

`rt.spawn` throws `async move { ... }` onto the tokio background to **run independently and returns immediately**. So the calling ECS system is **never blocked**; the render loop keeps flowing. `move` transfers ownership of `counter` into the async block.

**This hop is the heart of the bridge:** a sync function with no `await` still launches async work—the trade-off is "fire-and-forget"; it doesn't wait for a result, which flows back through the other two pillars.

**3) Debounce**

```rust
tokio::time::sleep(Duration::from_millis(150)).await;
```

The `.await` happens only inside the background task, never touching the main thread. Effect: when the user rapidly scrolls through stocks, waiting 150ms before the real request lets the "flicked past" ones get dropped—only the one you land on actually fetches. **Putting debounce inside the async task is something the sync side can't do—a sync function can't "wait a bit then act" without blocking.**

**4)+5) Fetch + write shared cache**

```rust
let (last_done, change_rate) = fake_rest_quote(&counter).await;
STOCKS.modify(counter.clone(), |s| s.update_from_push_quote(last_done, change_rate));
```

Once written into `STOCKS`, the render system sees the new value on its next read. **This is the first path for results flowing back: pure data goes through the shared cache.** `STOCKS` is a `Mutex<HashMap>` inside (the real project uses `DashMap`), ensuring safe async-write / sync-read concurrency.

**6) Notify ECS: send the "redraw" signal back**

```rust
if let Some(tx) = UPDATE_TX.get() {
    let queue = bevy_ecs::system::CommandQueue::default();
    let _ = tx.send(queue);
}
```

`CommandQueue` is Bevy's batched-ops queue, capable of carrying structural changes like "insert/remove resources/entities in the World." Here we only touched the cache and don't need structural changes, so we send an **empty queue** purely as a "there's an update, please redraw a frame" signal.

## 4. How results get back into the ECS World safely

The main loop's `tokio::select!` has a dedicated branch to receive this signal:

```rust
Some(mut cmd) = update_rx.recv() => {
    cmd.apply(&mut app.world);                 // apply the CommandQueue to the world
    render_state.mark_dirty(DirtyFlags::ALL);  // mark dirty → next tick triggers a redraw
}
```

**This is the most important safety guarantee of the whole design:** the async task never touches the World directly. It only packages "the changes I want" into a `CommandQueue` and sends it back to the **main loop** via mpsc; the main loop then `cmd.apply(&mut world)` in a **single-threaded, contention-free** context. Thus:

- The World is only ever mutated by the one main-loop thread—**no data races**.
- Changes apply **atomically as a batch**, never half-interrupted.

So "results flowing back" has two paths, each with a job:

| What flows back | Which path | Who applies it |
| --- | --- | --- |
| **Pure data** (new price) | Write the `STOCKS` shared cache | No apply needed; render systems read directly |
| **Structural changes** (insert/remove resources/entities) + redraw signal | `UPDATE_TX` sends a `CommandQueue` | Main loop `cmd.apply(&mut world)` |

## 5. Full data flow

```
[sync ECS system] enter_stock / Refresh action
      │ calls
      ▼
refresh_stock_debounced(counter)   ← sync function
      │ 1) RT.get() grabs the async entry
      │ 2) rt.spawn dispatches an async task ─────────┐
      └─ (returns immediately, system not blocked)    │
                                                      ▼
                           [tokio background task] async move {
                              3) sleep 150ms debounce
                              4) fake_rest_quote().await fetch
                              5) STOCKS.modify() write cache   ← data path 1
                              6) UPDATE_TX.send() notify loop   ← signal path 2
                           }
                                                      │
                                                      ▼
                     [main loop] update_rx.recv()
                              7) cmd.apply(world) + mark dirty  ← single-thread safe apply
                              8) next render tick → render system reads STOCKS → redraw
```

## 6. Where this pattern ports to

This "trio" isn't Bevy-specific; it's a general pattern for **any sync framework + async IO**:

- **`RT` (runtime handle)**: the entry for sync code to dispatch async work. Anywhere holding a tokio `Handle` can `spawn`.
- **`STOCKS` (shared cache)**: a concurrency-safe container for async-write / sync-read (`Mutex`/`RwLock`/`DashMap`). Great when "eventual consistency of data is fine."
- **`UPDATE_TX` (return channel)**: async tasks package "things that must happen in the main context" and send them back for the main thread to apply serially. Great for "state that must be mutated on a specific thread/context" (GUI main thread, ECS World, DB transaction…).

The core idea in one line: **the sync side only "dispatches" and "reads cache," never `await`s; the async side only "does work" and "sends back," never touches protected state directly; the two are decoupled via a shared cache (data) and a channel (signals/structural changes).**

## Wrap-up

`refresh_stock_debounced` is the bridge in concrete form: it uses `RT` to fling slow async work to the background (no render stall), writes results into the `STOCKS` shared cache there, then uses `UPDATE_TX` to knock on the main loop for a safe World apply and redraw—**all three bridging singletons appear in one function, completing the full loop from "async SDK world" to "sync ECS render world."**

Next post, on performance: how this demo uses **dirty flags + ratatui's double-buffer diff** as two-level throttling to save both CPU and terminal IO.

---

*Full source: [github.com/spursy/tui-bevy-ecs](https://github.com/spursy/tui-bevy-ecs)*
