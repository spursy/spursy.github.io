+++
title = "Building a Terminal App with Bevy ECS: Not Just for Games"
date = 2026-09-11
description = "ECS is often seen as a game-engine thing, but its essence is 'separating data from behavior'. Using a runnable terminal stock-quote demo (tui-bevy-ecs), this post shows why a TUI is a great fit for ECS: how state, resources, events, and systems each play their part, and the magic of Systems-as-dependency-injection."
[taxonomies]
tags = ["rust", "bevy", "ecs", "tui"]
+++

> Source: [github.com/spursy/tui-bevy-ecs](https://github.com/spursy/tui-bevy-ecs). A self-contained, `cargo run`-able program that distills longbridge-terminal's TUI layer into a ~300-line binary.

## 1. ECS is not a game-only thing

Say "ECS" (Entity-Component-System) and most people picture a game engine juggling thousands of sprites and particles. But the **core of ECS has nothing to do with games**. It's an architectural pattern:

> **Separate data (Components/Resources) from behavior (Systems) completely.** Data lives in one big store (the World); behavior is a set of independent functions that read/write that store on demand.

Compare it to object-orientation:

| | OOP | ECS |
| --- | --- | --- |
| Data & methods | Bundled together (`player.move()`) | Separated (`move_system` queries everything that can move) |
| Extension | Inheritance, polymorphism | Compose data + add systems |
| Mental model | "objects carry abilities" | "data is data, logic is logic" |

Once you accept the premise "data vs behavior are separate," you realize **plenty of non-game programs fit ECS too**—especially programs that have several "modes/screens," switch between them, and run different logic in each. A terminal UI is exactly that.

## 2. What the demo looks like

`tui-bevy-ecs` is a stock-quote panel in your terminal:

```
cargo run     # launch the TUI
```

Keys: `1` Watchlist · `2` Portfolio · `3` Orders · `j`/`k` or `↑`/`↓` move · `Enter` open · `Esc` back · `r` refresh · `q` quit.

It has multiple "screens" (Loading / Watchlist / Stock detail / Portfolio / Orders), switched via a state machine, each running its own render logic—**precisely where an ECS state machine shines**.

## 3. The five building blocks of Bevy ECS

Classic ECS has just Entity / Component / System. Bevy adds **Resource** and **State** on top, making it fit full applications. In this demo, the division of labor is:

| Block | Essence | Data or Behavior | Example in the demo |
| --- | --- | --- | --- |
| **State** | Which screen we're on (a special Resource under the hood) | Data | `AppState` (Loading/Watchlist/Stock…) |
| **Event** | A one-shot message, consumed once read | Data (transient) | `Key` (Up/Down/Enter nav intents) |
| **Resource** | Globally unique, persistent data | Data | `Terminal`, `Selection`, `StockDetail`, `WsState` |
| **System** | A logic function | **Behavior** | `render_watchlist`, `enter_stock`… |
| Entity/Component | Data attached to entities | Data | Barely used here (a TUI has no swarm of entities) |

One-line mnemonic: **State = "which screen now," Event = "what just happened," Resource = "persistent data," System = "the logic that does the work." The first three are data; the System is behavior.**

> Note: this demo **barely uses Entity/Component**. A TUI has no thousands of entities with position/HP to batch-process; it needs "globally unique data" (the selected row, the current stock, the terminal handle), which naturally maps to Resource. This is the classic "use ECS but only the Resource part" minimal usage.

## 4. State machine: multi-screen routing

Bevy models "which screen we're on" as a state machine. Registering it is one line:

```rust
app.add_state::<AppState>()
```

That auto-inserts two resources: `State<AppState>` (current) and `NextState<AppState>` (the pending target). Switching screens isn't immediate—it's **queued first**:

```rust
// Want to switch to the Stock screen: queue it; it takes effect next frame
app.world.insert_resource(NextState(Some(AppState::Stock)));
```

On the next `app.update()`, Bevy's transition schedule reads `NextState`, updates `State<AppState>`, and fires the matching `OnEnter`/`OnExit` hooks.

**Gating systems** is the prettiest part. Each screen registers its own render system, limited to its state via `run_if(in_state(..))`:

```rust
.add_systems(Update, systems::render_watchlist.run_if(in_state(AppState::Watchlist)))
.add_systems(Update, systems::render_stock.run_if(in_state(AppState::Stock)))
// One-shot hooks for entering/leaving the Stock screen
.add_systems(OnEnter(AppState::Stock), systems::enter_stock)
.add_systems(OnExit(AppState::Stock), systems::exit_stock);
```

Mechanically, **every frame runs all systems in the `Update` schedule**, but `run_if(in_state)` lets only the "current screen" `render_*` actually execute—the rest are skipped by the condition. It's like front-end routing: the URL changes, only the matching page component renders.

## 5. Systems as dependency injection: the signature is the declaration

This is ECS's biggest "aha." Look at `render_watchlist`'s signature:

```rust
pub fn render_watchlist(
    mut terminal: ResMut<Terminal>,   // want a resource (read-write)
    mut events: EventReader<Key>,     // want to read an event
    mut selection: ResMut<Selection>, // want a resource (read-write)
    (state, ws): NavFooter,           // want two read-only resources (grouped)
    mut frames: Local<u64>,           // system-private, kept across frames
) { /* ... */ }
```

**A system never constructs its dependencies; it "declares" what it needs in the parameters, and Bevy injects them by type.** Key rules:

- Every parameter type must implement `SystemParam` (`Res`/`ResMut`/`EventReader`/`Local`/`Commands`/tuples… all do).
- **Matched by type, not by order**—reorder the params freely, behavior is unchanged.
- **Take only what you need**—`render_watchlist` doesn't touch `StockDetail`, so it doesn't declare it; `render_stock` uses `Res<StockDetail>`.
- Tuples can group params (`NavFooter` is a type alias for `(Res<State<AppState>>, Res<WsState>)`) to shorten signatures.

`Local<T>` is especially neat: it doesn't come from the World; it's **system-private state kept across frames**. Here `frames: Local<u64>` is this system's own frame counter—`*frames += 1` each frame, no global pollution.

The payoff: **from a single signature you know exactly which data a system reads/writes**—dependencies are explicit, and swapping them in tests is easy.

## 6. Tying it together: what happens in one frame

```
User presses ↓
  → input layer looks up the keymap → ActionId::Down
  → send an Event(Key::Down) into the World  + mark dirty
  → next render tick, app.update() runs a frame
      → run_if(in_state(Watchlist)) picks render_watchlist
          → EventReader<Key> reads Down, mutates ResMut<Selection>
          → terminal.draw(...) paints the list to the terminal
```

Data (Event, Selection) and behavior (render_watchlist) stay separate throughout: the input layer only **sends events and mutates resources**; the render system only **reads events, reads resources, and paints**. Neither calls the other directly—**that's the decoupling power of ECS**.

## 7. When to use ECS in a non-game project

ECS is no silver bullet. Its sweet spot:

- The program has **several distinct modes/states** with transitions (multi-screen TUIs, multi-mode editors, state-machine-driven services).
- There's **a batch of globally shared data** read/written in many places, and you want to avoid threading references everywhere.
- You want **logic (systems) to be fully independent, composable, and conditionally enabled**.

If your program is just a linear flow with little shared state, ECS is over-engineering. But for a "multi-screen TUI," the clarity it buys is well worth it.

## Wrap-up

Bevy ECS's core is "separate data from behavior," game-agnostic. This demo uses its **State (multi-screen routing) + Resource (global data) + Event (transient messages) + System (dependency-injected logic)** quartet to organize a terminal quote panel cleanly and extensibly—**a System's parameters are its dependency declaration, injected by type by Bevy**, the most elegant piece of the whole design.

Next post: the hardest part of this demo—**how synchronous ECS systems gracefully talk to an async SDK**—the async→sync bridge built from the `RT` / `STOCKS` / `UPDATE_TX` trio.

---

*Full source: [github.com/spursy/tui-bevy-ecs](https://github.com/spursy/tui-bevy-ecs)*
