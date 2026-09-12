+++
title = "How a Frame Gets Saved: Two-Level Throttling with Dirty Flags + ratatui's Double-Buffer Diff"
date = 2026-09-13
description = "Terminal apps care about performance too. tui-bevy-ecs uses two-level throttling to minimize CPU and terminal IO: an outer layer of dirty flags (a DirtyFlags bitset) decides 'should this frame run the whole ECS update', and an inner layer of ratatui's double-buffer diff decides 'which cells to paint'. This post dissects the pull-based, doubly-frugal rendering design."
[taxonomies]
tags = ["rust", "ratatui", "tui", "performance"]
+++

> Source: [github.com/spursy/tui-bevy-ecs](https://github.com/spursy/tui-bevy-ecs). This post focuses on render performance: how two-level throttling avoids wasted computation and terminal writes.

## 1. Why terminal apps care about performance too

You might think a terminal program renders no 3D, so how much CPU can it burn? But a live-quote TUI has two hidden costs:

1. **The recompute-every-frame cost**: if every 33ms (30 FPS) you blindly run "read data → lay out → assemble widgets," you burn CPU even when the screen hasn't changed at all.
2. **The terminal-write cost**: terminal IO isn't cheap. Re-sending every character on screen produces a flood of ANSI escapes into stdout—painful over SSH / slow terminals, and it flickers.

tui-bevy-ecs uses **two-level throttling** to kill each cost:

| Layer | Solves | Mechanism | Decides |
| --- | --- | --- | --- |
| **Outer** | Recompute-every-frame cost | Dirty flags (`DirtyFlags` bitset) | Whether to run the whole `app.update()` this frame |
| **Inner** | Terminal-write cost | ratatui double-buffer diff | If painting, which cells to paint |

In one line: **the outer layer decides "paint or not," the inner layer decides "where to paint."**

## 2. First, the premise: pull-based rendering

A key design premise: **a data change never gets "pushed" to the terminal.** Instead, "when it's time to paint, the render system actively reads a data snapshot and paints." This decouples "data update" from "screen redraw": data can change frequently, but its latest value is read only on "the frame we actually paint."

Because they're decoupled, there's room for outer throttling: since redraw is an active pull, we can ask "is a pull even needed?" beforehand—if not, skip the whole frame.

## 3. Outer layer: dirty flags decide "paint this frame or not"

### The bitset: one u32 records "which regions are dirty"

```rust
bitflags! {
    pub struct DirtyFlags: u32 {
        const NONE         = 0;
        const WATCHLIST    = 0b0000_0001;
        const STOCK_DETAIL = 0b0000_0010;
        const PORTFOLIO    = 0b0000_0100;
        const INDEXES      = 0b0000_1000;
        const POPUP_HELP   = 0b0001_0000;
        const STATUS_BAR   = 0b1000_0000_0000;
        const ALL          = 0xFFFF_FFFF;
    }
}
```

Each bit represents a UI region. The benefit of a bitset: **marking, testing, and merging are all single bitwise ops, essentially free**, and it can express "several regions dirty at once."

### Precise marking: a quote push only dirties the "price regions"

The most important method: a quote push should **not** dirty the whole screen; it only affects the regions showing live prices:

```rust
pub fn mark_quote_update(mut self) -> Self {
    // mark only price-related regions, not ALL
    self.insert(Self::WATCHLIST | Self::STOCK_DETAIL | Self::INDEXES | Self::STATUS_BAR);
    self
}
```

> The source comment nails it: *"A quote push only touches the regions that show live prices — not the whole screen. This is the key to a high skip rate."*

Compare the dirty scope of different events:

| Event | Dirty scope | Code |
| --- | --- | --- |
| Quote push | Price regions only | `mark_quote_update()` |
| Move cursor up/down | `WATCHLIST \| STOCK_DETAIL` | `input.rs` |
| Switch screen | `ALL` (whole screen must repaint) | `mark_all_dirty()` |
| Background refresh returns | `ALL` | main loop |

### Main loop: run if dirty, skip if not

The outer throttle lands in the main loop's render branch:

```rust
_ = render_tick.tick() => {            // every 33ms
    if render_state.needs_render() {   // any dirty flags?
        app.update();                  // yes → run a full ECS frame
        render_state.clear();          // clear dirty after painting
    } else {
        render_state.skip();           // no → skip the whole frame, don't even call app.update()
    }
}
```

**Note that "skip" skips the entire `app.update()`**—all systems' data reads, layout, and widget assembly are saved. For a TUI that's mostly "no one touching it, no new pushes," this skip rate can be very high.

### Bonus: quantify "how much was saved"

`RenderState` also has a built-in efficiency readout that computes the skip rate directly:

```rust
pub fn efficiency(&self) -> f64 {
    let total = self.render_count + self.skip_count;
    if total == 0 { 0.0 }
    else { (self.skip_count as f64 / total as f64) * 100.0 }
}
// stats() → "renders: 12, skips: 88, efficiency: 88.0%"
```

`efficiency` is **the fraction of frames skipped**—exactly the win from dirty tracking, and you can print it on the status bar as self-proof.

## 4. Inner layer: ratatui's double-buffer diff decides "which cells to paint"

Suppose the outer layer rules "paint this frame" and we enter `app.update()`. The render system assembles widgets inside the `terminal.draw(|frame| ...)` closure. **But the assembled widgets aren't written straight to the terminal**—they first go into an in-memory `Buffer` (a 2D array where each cell holds "char + fg + bg + modifier"):

```rust
frame.render_widget(list, area);   // only writes into the in-memory Buffer, not the real terminal yet
```

When it actually reaches the terminal, **ratatui diffs "this frame's new Buffer" against "last frame's old Buffer" cell by cell**, picks out only the **changed cells**, and hands them to `CrosstermBackend` to turn into ANSI escapes into stdout:

```
new Buffer ─┐
            ├─ cell-by-cell diff ─▶ only changed cells ─▶ CrosstermBackend ─▶ ANSI ─▶ stdout
old Buffer ─┘
```

So even if the outer layer greenlights a whole frame, the inner layer sends only the **truly changed characters** to the terminal. If a quote push changed a single price digit, what's written to the terminal may be just those few cells, not the whole screen.

> This logic lives inside the ratatui crate (this project only calls `terminal.draw`, so you won't see the implementation). The principle is simple: `Terminal` holds **two Buffers** (current frame / previous frame), and on finishing `draw` it does two things—

The inner layer is really **two-level throttling** too, saving along two dimensions:

```rust
// 1) Spatial: Buffer::diff compares cell by cell, collecting only changed cells
pub fn diff(&self, other) -> Vec<(x, y, &Cell)> {
    for (cur, prev) in other.zip(self) {
        if cur != prev { updates.push((x, y, cur)); }  // collect only if changed
    }
}
// 2) Output: the backend emits ANSI only for diff cells, with incremental color/cursor
for (x, y, cell) in updates {
    if !adjacent_to_prev { MoveTo(x, y) }        // move cursor only when non-contiguous
    if cell.fg != prev.fg { SetFg(..) }          // emit color code only when it changed
    Print(cell.symbol);
}
// finally, swap the two Buffers: this frame becomes "previous," the spare is cleared
```

- **1) diff (spatial saving)**: a screen has thousands of cells; if only one price changed, `updates` has just those few entries.
- **2) backend increments (escape-sequence saving)**: a run of same-colored cells won't re-emit the color code, and the cursor only moves when non-contiguous.

Stacked together, these two steps shrink the "expensive terminal write" to a minimum.

## 5. The two layers together: a quote push's full journey

```
A quote push arrives (every 200ms)
  │
  ├─ [Outer] mark_quote_update() → dirty only price regions (not ALL)
  │
  └─ next render tick (33ms)
        │ needs_render()? → dirty → app.update()
        │                   clean → skip() (whole frame saved)
        ▼
     render system terminal.draw: read STOCKS, assemble widgets → write in-memory Buffer
        │
        └─ [Inner] new-vs-old Buffer diff → only that price digit changed
                    → write just a few cells' ANSI into stdout
```

Each layer blocks one kind of waste:

- **Outer** blocks "recompute when the screen hasn't changed"—if clean, don't even run `app.update()`.
- **Inner** blocks "recompute then rewrite the whole screen"—only write changed cells.

Even if the outer layer occasionally over-greenlights (marks `ALL` when only one cell changed), **the inner diff still backstops**, ensuring terminal writes only ever carry real differences. It's a "double safety net": if either layer slips, the other still saves.

## 6. Portable takeaways

This two-level throttling isn't TUI-only; it fits any "frame/tick-based rendering + a clear source of changes":

1. **Pull-based, not push-based**: decouple "data update" from "redraw" so you can judge "needed or not" before redrawing.
2. **Fine-grained dirty flags via a bitset**: different events dirty different regions, so "unrelated actions" avoid triggering a redraw; marking/testing are free bitwise ops.
3. **Ask "is it needed?" before redrawing**: skipping the whole recompute is the biggest single saving.
4. **Diff at the bottom as a backstop**: even when the upper layer greenlights, only commit real differences to the expensive output (terminal / network / DOM).
5. **Quantify the skip rate**: a readout like `efficiency()` makes optimization visible and helps you notice regressions ("why did the skip rate suddenly drop?").

## Wrap-up

tui-bevy-ecs's rendering is a **pull-based, two-level-throttled** pipeline: the outer layer uses a `DirtyFlags` bitset to decide "run the whole `app.update()` this frame or not" (a quote push dirties only price regions—the key to a high skip rate), and the inner layer uses ratatui's double-buffer diff to decide "which cells to paint." **Bevy handles scheduling and data injection; ratatui + crossterm handle getting characters onto the terminal efficiently**—each layer saves one level, with the other as a backstop.

This wraps up the series. The three posts covered the **architectural novelty of ECS for TUIs**, the **async difficulty of the async→sync bridge**, and the **performance win of two-level throttling**—the three most worthwhile pieces of design in this little demo.

---

*Full source: [github.com/spursy/tui-bevy-ecs](https://github.com/spursy/tui-bevy-ecs)*
