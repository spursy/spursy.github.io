+++
title = "Actors with Channels: Lock-Free Concurrent State in Rust"
date = 2026-10-27
description = "When several tasks need to mutate the same state, the knee-jerk reach is Arc<Mutex<_>>. But there is another road: hand the state to one task that owns it exclusively, and let everyone else only send messages — that is the Actor pattern. Using the actor_counter example from longbridge-ai-acp, this post explains what an actor is, what it solves, and then looks at the real DesktopSession source to see why it has no real alternative."
[taxonomies]
tags = ["rust", "actor", "channel", "tokio", "concurrency"]
+++

> Source comes from [`longbridge-ai-acp`](https://github.com/longbridge/longbridge-ai-acp): the teaching example `examples/actor_counter.rs`, and the real-world usage `src/desktop.rs`. This post centers on one question — **when several tasks share mutable state, what can you do besides locking?**

## 1. The problem: many tasks mutating one counter

The smallest concurrency problem: spawn two tasks, each adds 100 to a counter, and the result should be 200. The intuitive approach is to share the state and protect it with a synchronization primitive. `examples/shared_counter.rs` shows the three standard ways:

```rust
// 1) Atomic integer: lock-free, but only fits a single integer
let count = Arc::new(AtomicI64::new(0));
c1.fetch_add(1, Ordering::Relaxed);

// 2) Mutex: general-purpose, lock then mutate
let mut guard = c1.lock().await;
*guard += 1;

// 3) RwLock: many readers, single writer
let mut guard = c1.write().await;
*guard += 1;
```

All three reach 200. What they share: **the state is shared across tasks (`Arc`), and concurrency safety comes from "taking a lock."** This is the "shared memory + lock" mindset. It works, but as the state grows more complex, you have to watch it yourself: lock granularity, lock ordering (deadlocks), whether you can `.await` while holding the lock, which fields must change together…

The Actor pattern flips the angle: **if sharing is what forces locking, then simply don't share.**

## 2. What an actor is: state belongs to exactly one task

Look at `examples/actor_counter.rs`. It builds an actor out of three things, with no `Arc` and no lock anywhere:

**1) A message enum — the actor's "interface"**

```rust
enum CounterMsg {
    Increment,
    /// Carries a one-shot reply channel so the caller can await the value
    Get(oneshot::Sender<i64>),
}
```

**2) The actor body — a task that owns the state exclusively**

```rust
async fn counter_actor(mut mailbox: mpsc::Receiver<CounterMsg>) {
    let mut count: i64 = 0;                 // count belongs only to this task
    while let Some(msg) = mailbox.recv().await {
        match msg {
            CounterMsg::Increment => count += 1,
            CounterMsg::Get(reply) => { let _ = reply.send(count); }
        }
    }
}
```

`count` is a local variable of this `async fn`, and **nobody but the task itself can touch it**. Since it isn't shared, there is no data race, and therefore no lock is needed.

**3) A handle — exposes only "send a message," never the state**

```rust
#[derive(Clone)]
struct CounterHandle {
    sender: mpsc::Sender<CounterMsg>,
}

impl CounterHandle {
    fn new() -> Self {
        let (tx, rx) = mpsc::channel(32);
        tokio::spawn(counter_actor(rx));   // start the actor task
        Self { sender: tx }
    }

    async fn increment(&self) {
        let _ = self.sender.send(CounterMsg::Increment).await;
    }

    async fn get(&self) -> i64 {
        let (tx, rx) = oneshot::channel();          // temporary reply channel
        let _ = self.sender.send(CounterMsg::Get(tx)).await;
        rx.await.expect("actor dropped the reply channel")
    }
}
```

The caller only gets a `CounterHandle`. It can be `clone`d to any number of tasks, but all they can do is "drop a message into the mailbox." So:

```rust
let counter = CounterHandle::new();
let c1 = counter.clone();
let c2 = counter.clone();
tokio::spawn(async move { for _ in 0..100 { c1.increment().await; } });
tokio::spawn(async move { for _ in 0..100 { c2.increment().await; } });
// ...
println!("final count = {}", counter.get().await); // 200
```

Two tasks concurrently send `Increment`, but every mutation runs **serially** inside the actor task — the mailbox naturally lines concurrent requests up into a queue. No lock, and no data race possible.

There's a key trick here: **use `oneshot` to turn a one-way message into a "request/response."** `Increment` is fire-and-forget, while `Get` needs a return value — so you put a `oneshot::Sender` inside the message and send it along, the actor replies on that channel after handling it, and the caller `await`s it. That is how you simulate a synchronous call on top of message passing.

## 3. Comparing the two mindsets

| | Shared memory + lock | Actor (message passing) |
| --- | --- | --- |
| Where the state lives | `Arc<Mutex<T>>`, shared across tasks | One task owns it, nobody else can reach it |
| How safety is achieved | Runtime mutual exclusion via locking | Not shared at all, no race by construction |
| How concurrent requests are ordered | Race for the lock, order undefined | Mailbox FIFO, order deterministic |
| Easy traps | Deadlock, lock granularity, `.await` while holding | Message design, mailbox backpressure |
| Getting a return value | Read it directly | Reply via `oneshot` |

There's no free lunch. `examples/counter_bench.rs` runs a rough comparison on the same workload (8 tasks × 100k iterations), and the conclusion is spelled out in its comments: **message passing has per-operation overhead (building the message, crossing the channel, scheduling the actor); a bare `AtomicI64` has none.** So:

- A single integer, and you just want speed — use an atomic or a lock.
- Complex state, multiple fields that must stay mutually consistent, and you also want clear lifecycle and serial semantics — the engineering payoff of an actor far outweighs that per-op overhead.

**An actor isn't a performance play to beat locks; it's a structure for making complex, stateful concurrency simple.**

## 4. What actors solve

Abstracting the gains from the example, this is the actor's selling point:

1. **Data races vanish**: state isn't shared, so neither you nor the compiler has to reason about locks.
2. **Serialization**: all mutations happen in one task in message order, so the state machine is consistent by construction; "handle one thing at a time" comes for free.
3. **Encapsulation**: the outside world sees only a few methods on the `Handle`, never the internal state, so you can change the implementation without touching callers.
4. **Clear lifecycle**: an actor is just a task; its start/stop/cancel map cleanly onto "close the mailbox / abort the task."
5. **Easy to pass around**: a `Handle` is just a `Sender`, which is `Clone + Send + 'static`, so you can stash it anywhere.

Point 5 is exactly why `longbridge-ai-acp` has no real alternative to the actor pattern.

## 5. Why ai-acp uses this pattern: `DesktopSession`

`longbridge-ai-acp` is an ACP (Agent Client Protocol) runtime, and what `DesktopSession` has to do is: let a desktop UI (a GPUI / Tauri model) hold a **long-lived** AI session, continuously send prompts and receive streaming events. Open `src/desktop.rs` and you'll see the same three pieces as the counter:

**The message enum (commands the session accepts):**

```rust
enum SessionCommand {
    Prompt {
        blocks: Vec<ContentBlock>,
        accepted: oneshot::Sender<Result<(), SessionControlError>>, // reply: accepted or Busy
    },
    Cancel,
    Shutdown,
}
```

**The handle plus the event side:**

```rust
pub struct DesktopSession {
    commands: mpsc::UnboundedSender<SessionCommand>,       // send commands (the actor's mailbox)
    events: mpsc::UnboundedReceiver<DesktopSessionEvent>,  // receive events
    task: Option<tokio::task::JoinHandle<()>>,             // the actor task itself
    // ...
}
```

**The actor body — `drive_session` loop owning the real SDK session exclusively:**

```rust
async fn drive_session(
    mut session: ActiveSession<'_, Agent>,                 // ← the exclusively-owned state
    mut commands: mpsc::UnboundedReceiver<SessionCommand>,
    events: mpsc::UnboundedSender<DesktopSessionEvent>,
) -> Result<(), agent_client_protocol::Error> {
    while let Some(command) = commands.recv().await {
        match command {
            SessionCommand::Prompt { blocks, accepted } => { /* start a turn */ }
            SessionCommand::Cancel => {}
            SessionCommand::Shutdown => break,
        }
    }
    Ok(())
}
```

Why **must** this be an actor, instead of simply sharing an `Arc<Mutex<ActiveSession>>`? A few hard constraints:

**① The protected state can't be shared at all.** `ActiveSession<'_, Agent>` carries a lifetime: it borrows the underlying ACP connection, and for an external agent (a Codex / Claude subprocess) it indirectly holds that subprocess too. It's neither `'static` nor convenient to share across tasks, so you can't stuff it into an `Arc<Mutex<_>>` that a UI model is supposed to hold long-term and call from everywhere. **The actor locks it inside a task exclusively, and the outside world only ever gets a `Sender`** — and a `Sender` is `Clone + Send + 'static`, exactly the handle a GPUI/Tauri model wants.

**② Request/response, with a precise `oneshot` reply.** `prompt()` needs to know "was the command accepted?", so `Prompt` carries an `accepted` reply channel, identical to the counter's `Get`:

```rust
async fn send_prompt_command(commands: ..., blocks: ...) -> Result<(), SessionControlError> {
    let (accepted_tx, accepted_rx) = oneshot::channel();
    commands.send(SessionCommand::Prompt { blocks, accepted: accepted_tx })
        .map_err(|_| SessionControlError::Closed)?;
    accepted_rx.await.unwrap_or(Err(SessionControlError::Closed))
}
```

**③ Serialization gives a free "only one turn at a time" guarantee.** Send another prompt before a turn finishes, and the actor just replies `Busy` on the reply channel — no lock required:

```rust
Some(SessionCommand::Prompt { accepted, .. }) => {
    let _ = accepted.send(Err(SessionControlError::Busy));
}
```

**④ Ordering.** Streaming updates and the "turn is done" signal travel on different channels, and the actor uses a `biased` `select!` to guarantee: drain the queued content blocks first, then announce the turn's end — otherwise leftover chunks would leak into the next turn. Adjudicating "who goes first" is most natural inside a single actor loop:

```rust
tokio::select! {
    biased;
    message = session.read_update() => { /* forward an Update event */ }
    outcome = &mut stop_rx        => { /* got a stop reason, emit TurnFinished */ }
    command = commands.recv()     => { /* handle Cancel / Shutdown / overlapping Prompt */ }
}
```

**⑤ Lifecycle is the task's lifecycle.** `Cancel` and `Shutdown` are just messages; `Drop` calls `task.abort()` directly — for an external agent, that lets the official SDK terminate the subprocess group along with it. Teardown is thorough and deterministic:

```rust
impl Drop for DesktopSession {
    fn drop(&mut self) {
        if let Some(task) = self.task.take() { task.abort(); }
    }
}
```

**⑥ The command side and event side can be split apart.** `split()` divides the session into a cloneable command handle `DesktopSessionHandle` and an exclusive event side `DesktopSessionEvents` — the UI state layer sends commands, the background task focuses on receiving streaming events, and neither side has to touch the SDK's lifetime. This is the direct payoff of the "a `Handle` is easy to pass around" gain.

## 6. From the counter to DesktopSession

The two pieces of code are the same skeleton:

| Role | `actor_counter.rs` | `desktop.rs` |
| --- | --- | --- |
| Message enum | `CounterMsg` | `SessionCommand` |
| Exclusively-owned state | `count: i64` | `ActiveSession<'_, Agent>` |
| Actor loop | `counter_actor` | `drive_session` / `drive_turn` |
| Mailbox | `mpsc::channel(32)` | `mpsc::unbounded_channel()` |
| Request/response | `Get(oneshot)` | `Prompt { accepted }`, `stop_rx` |
| Handle | `CounterHandle` | `DesktopSession` / `DesktopSessionHandle` |

In the counter, the actor is an elegant choice that "lets you skip the lock"; in `DesktopSession`, it's almost the **only** reasonable structure — because the protected state (a lifetime-bound SDK session wrapping a subprocess) simply can't be shared safely, while the UI needs a lightweight, cloneable, `'static` handle.

## Wrap-up

Remember actors in one line: **don't share state, pass messages instead.** Hand the state to one task that owns it exclusively, and let the outside world send commands through a mailbox and fetch replies via `oneshot` — data races disappear at the source, and serial semantics, encapsulation, and a clear lifecycle all come along for the ride.

It isn't a performance trick to outrun locks (message passing really does have per-op overhead); it's the structure that **makes complex, stateful concurrency maintainable even when it carries unshareable resources — connections, subprocesses, handles.** `longbridge-ai-acp`'s `DesktopSession` is the most concrete footnote to this idea.

---

*Repository: [github.com/longbridge/longbridge-ai-acp](https://github.com/longbridge/longbridge-ai-acp). Examples: `examples/actor_counter.rs`, `examples/shared_counter.rs`, `examples/counter_bench.rs`; real-world usage: `src/desktop.rs`.*
