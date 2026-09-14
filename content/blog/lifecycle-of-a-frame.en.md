+++
title = "The Lifecycle of a Frame: From a Keypress to a Pixel on the Terminal"
date = 2026-09-14
description = "Following a keypress and a quote push, we trace a frame's full journey through tui-bevy-ecs: input/push → mark dirty → 33ms tick → app.update() → run_if picks a system → terminal.draw assembles widgets → double-buffer diff → onto the terminal. It ties the ECS, async→sync bridge, and two-level throttling from earlier posts into one timeline."
[taxonomies]
tags = ["rust", "bevy", "ecs", "ratatui", "tui"]
+++

> Source: [github.com/spursy/tui-bevy-ecs](https://github.com/spursy/tui-bevy-ecs). Earlier posts dissected the ECS architecture, the async→sync bridge, and two-level throttling; this one strings them into a timeline—**tracing a frame's full lifecycle from birth to death**.

## 1. The big picture: one loop feeding four streams

It all starts in the main loop's `tokio::select!` (`app.rs:141`). It watches four streams at once and handles whichever is ready first:

```
                        tokio::select!  (app.rs:141)
   ┌──────────────┬─────────────────┬──────────────────┬─────────────────┐
   │ ① render_tick│ ② update_rx     │ ③ push_stream    │ ④ input_events  │
   │  every 33ms  │  CommandQueue   │  simulated push  │  crossterm keys │
   │  update if   │  back to world  │  write STOCKS +  │  keymap→action  │
   │  dirty       │                 │  mark dirty      │                 │
   └──────┬───────┴────────┬────────┴─────────┬────────┴────────┬────────┘
          │                │                  │                 │
      needs_render?     cmd.apply         STOCKS.modify     handle_global_keys
      → app.update()    +mark_dirty       +mark_quote_update  → switch/emit/act
```

The key division of labor: **②③④ only "mutate data + mark dirty," never paint directly; only ① render_tick actually drives a frame of rendering.** This is "pull-based rendering"—a data change isn't pushed to the terminal; instead, the next tick actively reads it.

Once you see this, "the lifecycle of a frame" splits in two: the **first half** (who marked this frame dirty) and the **second half** (how it's painted when the tick arrives).

## 2. First half: how a frame comes to be "needed"

A frame is born when some stream marks `RenderState` dirty. Take the two most typical triggers.

### Trigger A: the user presses ↓

```
④ input_events.next() receives a key (app.rs:174)
  → handle_global_keys() (input.rs:24)
      → keymap.lookup(&event, ctx) resolves the key to ActionId::Down (keymap.rs:101)
      → send_event(&mut world, Key::Down)  push a nav event into the world (input.rs:49)
      → render_state.mark_dirty(WATCHLIST | STOCK_DETAIL) precise dirty (input.rs:50)
```

Note it **only emits an event and marks dirty**; the screen stays still. The `Key::Down` event sits quietly in the world, waiting for the render system to consume it next frame.

### Trigger B: a quote push arrives

```
③ push_stream.next() receives a push (app.rs:166)
  → STOCKS.modify(counter, |s| s.update_from_push_quote(..)) write cache (app.rs:167)
  → render_state.mark_dirty(NONE.mark_quote_update()) dirty only price regions (app.rs:170)
```

Again just "write cache + mark dirty." `mark_quote_update` is the key to a high skip rate—it doesn't mark `ALL`, only price-related regions (`render.rs:39`).

**State at the end of the first half**: the world may have a new event, `STOCKS` may hold a new price, and a few bits of `RenderState.dirty` are lit. The screen is still the old one.

## 3. Second half: the 33ms tick arrives; paint the frame

### Step 1: dirty check—does this frame need painting at all

```rust
_ = render_tick.tick() => {            // every 33ms (app.rs:143)
    if render_state.needs_render() {   // did the first half mark dirty?
        app.update();                  // yes → run a full ECS frame
        render_state.clear();          // clear dirty after painting
    } else {
        render_state.skip();           // no → skip the whole frame, don't even call app.update()
    }
}
```

This is the **outer throttle**: if clean, `skip()`, and the whole `app.update()` is saved. Most "nobody touching, no pushes" ticks are blocked right here. Suppose we marked dirty in the first half, so we enter `app.update()`.

### Step 2: `app.update()` → run the schedule → `run_if` picks a system

Inside `app.update()`, `world.run_schedule(...)` runs **all** systems in the `Update` schedule, but each `render_*` carries `run_if(in_state(..))` (`app.rs:80-84`). Suppose we're on the Watchlist screen:

```
All 5 render_* systems in Update are "considered"
  → run_if(in_state(Watchlist)) admits only render_watchlist
  → the other 4 are skipped by the condition
```

If this frame also involves a state switch (e.g. Enter was just pressed), Bevy first applies `NextState`, fires the `OnEnter/OnExit` hooks, then runs the new screen's render.

### Step 3: `render_watchlist` consumes events + reads data + assembles widgets

The chosen system gets everything it needs via **parameter injection** (`systems.rs:76`):

```rust
pub fn render_watchlist(
    mut terminal: ResMut<Terminal>,   // the terminal
    mut events: EventReader<Key>,     // the Key events emitted in the first half
    mut selection: ResMut<Selection>, // the selected row
    (state, ws): NavFooter,
    mut frames: Local<u64>,
) {
    // 1) consume the nav events queued in the first half, update the selected row
    for key in events.iter() {
        match key { Key::Down => selection.0 = (selection.0 + 1).min(..), .. }
    }
    // 2) terminal.draw: read STOCKS + selection, format! into colored Span/Line
    terminal.draw(|frame| {
        // Layout splits regions → one line per stock → render_widget writes into in-memory Buffer
    });
}
```

Here "the event emitted in the first half" is finally consumed—`Key::Down` bumps `selection.0`. Then inside `terminal.draw`, the price from `STOCKS` and the highlighted row from `Selection` are `format!`ed into `Span/Line`, and `render_widget` **writes into the in-memory Buffer** (`systems.rs:154`). **Still no contact with the real terminal.**

### Step 4: double-buffer diff—only changed cells reach the terminal

Before `terminal.draw` returns, ratatui diffs the new Buffer against last frame's old Buffer cell by cell, and hands only the **changed cells** to `CrosstermBackend` to turn into ANSI into stdout:

```
new Buffer ─┐
            ├─ cell-by-cell diff ─▶ only changed cells ─▶ CrosstermBackend ─▶ ANSI ─▶ stdout ─▶ terminal
old Buffer ─┘
```

This is the **inner throttle** (implemented inside the ratatui crate). If Trigger B only changed a single price digit, what reaches the terminal may be just those few cells, not the whole screen.

### Step 5: clear dirty, wait for the next frame

```rust
render_state.clear();   // dirty reset to zero, render_count += 1
```

`RenderState` records a "render +1" and clears the dirty bits. **This frame's lifecycle ends here**—it was born from a dirty mark on some stream, painted within a 33ms tick, and finally cleared, waiting to be "needed" again.

## 4. Full timeline: tracing one quote push end to end

Joining the two halves, the full journey of one price update:

```
t=0ms    ③ push_stream receives a new price for 700.HK
         └─ STOCKS.modify writes cache + mark_quote_update() dirties price regions
         (the screen is still old at this point)

t=0~33ms (if more pushes arrive, they just keep marking dirty / writing cache, no repeated render)

t=33ms   ① render_tick fires
         ├─ needs_render()? → dirty → app.update()
         │    └─ run_if(in_state(Watchlist)) → render_watchlist
         │         └─ terminal.draw: read new price from STOCKS → write Buffer
         │              └─ diff: only that price digit changed → a few cells' ANSI → stdout
         └─ clear(): reset dirty, render_count += 1
         (that price digit on the terminal refreshes)
```

Note that **even if 5 pushes arrive between t=0 and t=33ms, rendering happens only once at t=33ms**—that's the saving of "pull-per-tick" over "push-per-event": multiple data changes are naturally coalesced into one frame.

## 5. This one frame ties together all three earlier posts

| Stage | Design used | Post |
| --- | --- | --- |
| `run_if(in_state)` picks a system, parameter injection | ECS: state machine + Systems-as-DI | "Building a Terminal App with Bevy ECS" |
| pushes write `STOCKS`, background refresh returns via `UPDATE_TX` | the async→sync bridge trio | "Doing Async Gracefully Inside a Sync Framework" |
| `needs_render()` frame-skip + diff paints only changes | dirty flags + double-buffer diff, two-level throttling | "How a Frame Gets Saved" |

**A frame's lifecycle is exactly a performance of these three designs together.**

## Wrap-up

In tui-bevy-ecs, a frame lives like this: some stream (key / push / background return) **mutates data and marks dirty** (first half); the next 33ms tick arrives, the main loop **runs `app.update()` only if dirty**, Bevy uses `run_if(in_state)` to pick the current screen's render system, the system **consumes events, reads data, and `terminal.draw` assembles widgets into the in-memory Buffer**, ratatui's **double-buffer diff commits only the changed cells to the terminal**, and finally **clears dirty to wait for the next frame** (second half). **Decoupled data and render flows, pull-per-tick, two-level throttling**—this pipeline keeps a live-quote TUI both responsive and frugal.

---

*Full source: [github.com/spursy/tui-bevy-ecs](https://github.com/spursy/tui-bevy-ecs)*
