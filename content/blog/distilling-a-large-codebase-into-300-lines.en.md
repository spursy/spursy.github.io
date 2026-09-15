+++
title = "Distilling a Large Project's TUI Layer into 300 Lines: Mirroring Real Architecture with a Minimal Demo"
date = 2026-09-15
description = "Faced with a huge production codebase, how do you quickly grasp its architecture? One effective approach is to 'build a minimal runnable mirror.' Using tui-bevy-ecs as an example, this post shows how to distill longbridge-terminal's entire TUI layer into a ~300-line, cargo run-able demo—keeping the skeleton, cutting the flesh—as a methodology for learning and refactoring."
[taxonomies]
tags = ["rust", "architecture", "refactoring", "learning"]
+++

> Source: [github.com/spursy/tui-bevy-ecs](https://github.com/spursy/tui-bevy-ecs). This post isn't about a specific technique but a **methodology**: how to mirror a large project's architecture with a minimal runnable demo.

## 1. The problem: how to quickly understand a large codebase

You inherit a production project of tens of thousands of lines and want to understand how its TUI layer is organized. Reading the source directly often bogs you down:

- **Low signal-to-noise**: the real architectural skeleton is buried under error handling, logging, edge cases, and business branches.
- **Everything is entangled**: to run even a minimal flow, you must first satisfy a pile of dependencies (SDK, auth, network, config).
- **Afraid to touch it**: you want to verify "what if we removed this layer," but you dare not tinker with production code.

There's an underrated approach: **build a minimal runnable mirror (a distillation)**—lift out the architectural skeleton, reproduce it in a small `cargo run`-able demo, cut all the flesh, keep only the trunk.

`tui-bevy-ecs` is exactly that: it distills longbridge-terminal's entire TUI layer into ~300 lines, with every concept traceable back to the real project.

## 2. The core principle: keep the skeleton, cut the flesh

Distilling isn't "deleting code at random"; it's deliberate retention. The judgment rule is singular:

> **Whatever determines "what the architecture looks like," keep it; whatever only adds "engineering completeness," cut it or replace it with a fake.**

Concretely, in tui-bevy-ecs:

| Keep (architectural skeleton) | Cut / fake (engineering flesh) |
| --- | --- |
| ECS state machine, the roles of Resource/Event/System | Real business fields (keep only the few the render uses) |
| The four streams of the `tokio::select!` main loop | Real WebSocket → a background task `tx.send`s fake pushes |
| The async→sync bridge (`RT`/`STOCKS`/`UPDATE_TX`) | Real REST calls → `fake_rest_quote` returns deterministic data |
| Dirty flags + diff two-level throttling | Real multi-region, popups, depth chart (keep the bit defs, don't implement) |
| Data-driven keymap (one table drives input + hints) | Real auth, config, error reporting, logging |

The key trick is to **hold the interface's place with a "fake"**, rather than deleting it outright. For example:

```rust
/// Stand-in for the real ctx.quote(..).await; returns deterministic pseudo-data
async fn fake_rest_quote(counter: &str) -> (f64, f64) {
    let seed = counter.bytes().map(u64::from).sum::<u64>();
    let base = 50.0 + (seed % 400) as f64;
    let change = ((seed % 700) as f64) / 100.0 - 3.5;
    (base, change)
}
```

This way **the data flow's shape is identical to the real project** (sync system → spawn async → await a "request" → write cache), only "actually hit the network" is swapped for "compute a fake number." What the reader sees is **architecture, not business noise**.

## 3. The method: first draw a "concept → file" map

Before distilling, list a table: for each **architectural concept** in the real project, which demo file will it land in. This table is both the construction blueprint and the reader's tour guide. tui-bevy-ecs's map looks like this:

| Concept (from the architecture write-up) | Where it lives in the demo |
| --- | --- |
| Bevy ECS state machine (`AppState`/`NextState`/`OnEnter`/`OnExit`/`run_if`) | `src/app.rs` |
| Systems as dependency injection (declaring `Res`/`ResMut`/`EventReader`/`Local`) | `src/systems.rs` |
| Resource vs Event (data vs behavior) | `src/systems.rs` |
| Ratatui rendering (`Terminal` resource, enters/leaves alt-screen via `Drop`) | `src/terminal.rs` |
| `tokio::select!` multiplexing main loop | `src/app.rs` |
| Dirty-flag rendering | `src/render.rs` |
| The async→sync bridge (`RT`/`STOCKS`/`UPDATE_TX`) | `src/data.rs` |
| Data-driven keymap | `src/keymap.rs` |

**With this table, "distilling" becomes filling in the blanks line by line**: for each concept, drop a minimal implementation into the corresponding file. The table also ensures you "miss no key concept and mix in no irrelevant one."

## 4. A few tricks that keep the demo "small yet real"

### Trick 1: mirror the real type shapes, even if underused

Some fields/variants in the demo aren't fully used, yet are kept because they're part of the real project's API surface—like the `WsState::Connecting` variant, or unimplemented region bits in `DirtyFlags`. A one-line comment explains it:

```rust
// Some flags/variants aren't exercised by this demo but are kept to mirror
// the real project's full region set.
#![allow(dead_code)]
```

This lets the reader see the **full shape**, not a mangled, unrecognizable version. `#[allow(dead_code)]` is a regular guest in distillation-style demos.

### Trick 2: use deterministic fake data for reproducible behavior

Real data depends on the network and isn't reproducible. All "external data" in the demo is generated by a **deterministic algorithm** (`fake_rest_quote` seeds on the byte-sum of the stock code). The benefit: anyone who `cargo run`s sees the same behavior—stable for explanations, screenshots, and tests.

### Trick 3: one background task simulates the whole upstream

The real project's quotes come from an SDK's WebSocket subscription callback. The demo replaces it with one tokio task that `tx.send`s a fake push every 200ms—**the main loop's `push_stream.next()` branch is identical to when a real WS is wired in**; only "who does the send" is swapped. This keeps the main-loop architecture real and the upstream fake.

### Trick 4: keep it runnable + testable

The endpoint of distilling is "**it runs, it tests**," not a pile of snippets. tui-bevy-ecs keeps `cargo run` (it really launches in the terminal) and `cargo test` (keymap and dirty flags have unit tests). **A runnable demo is a credible one**—it proves the skeleton you extracted is self-consistent, missing no crucial link that makes the system turn.

## 5. What this method buys you

**For learners**: a runnable minimal mirror builds "architectural intuition" faster than reading ten thousand lines of source. You can modify it fearlessly—remove a layer and see what happens, add a system and observe the schedule order—experiments you'd never dare in production code, verified in seconds in the demo.

**For refactorers**: want to refactor a layer of a large project? Distill it into a demo first, experiment with a new architecture there, and only port it back once it's proven. The demo is your "architecture sandbox."

**For doc/blog writers**: every file in a distilled demo is natural material for a post—the earlier posts in this series (ECS architecture, the async→sync bridge, two-level throttling, the lifecycle of a frame) all grew out of this 300-line demo. **Distill first, then explain block by block** is an efficient path to explaining a complex system clearly.

## 6. When you shouldn't do this

Distilling has costs and isn't worth it everywhere:

- **The architecture is already simple**: there's no skeleton to extract, so distilling is just copying.
- **The difficulty is in the details, not the structure**: if the truly hard part is an algorithm or a concurrency bug, read that code directly rather than building a mirror.
- **You'll maintain the demo long-term**: the mirror and the real project drift over time. Either make clear the demo is a "one-off learning tool," or add comments in the real project pointing back to the demo (tui-bevy-ecs's comments frequently say "Mirrors `src/tui/...`", anchoring back to real locations).

## Wrap-up

Facing a huge TUI layer, `tui-bevy-ecs`'s approach is to **build a ~300-line, `cargo run`-able minimal mirror**: first list a "concept → file" map as the blueprint, then retain the architectural skeleton line by line, replace the engineering flesh with fakes (`fake_rest_quote`, a fake push task, full shapes under `#[allow(dead_code)]`), and finally hold the line on "runnable + testable." **Keep the skeleton, cut the flesh, deterministic and reproducible**—this method is both a learning shortcut to quickly understand a large project and a refactoring sandbox to safely experiment with new architecture, and it conveniently produces material for an entire blog series.

---

*Full source: [github.com/spursy/tui-bevy-ecs](https://github.com/spursy/tui-bevy-ecs)*
