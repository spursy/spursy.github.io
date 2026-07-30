+++
title = "etcd pkg/notify: One-to-Many Broadcast by Channel"
date = 2026-07-30
description = "Starting from a self-contained runnable demo, this post explains how etcd's pkg/notify uses close(channel) for repeatable one-to-many broadcast, and how it serves linearizable reads."
[taxonomies]
tags = ["etcd"]
+++

The previous post covered `pkg/wait`, which is **one-to-one**: one ID maps to exactly one waiter, used to wrap "async consensus" into a "sync call". But etcd has another kind of need inside — **a global event happens and must wake up a batch of waiters of unknown count at once, without caring who they are**. For that etcd uses a smaller package: `pkg/notify`.

It is only a few dozen lines, and its core idea is a single sentence: **closing a channel is, by nature, a broadcast**. This post starts from a self-contained runnable demo, explains its design, and then maps it back to the etcd source to see how it serves linearizable reads.

<!-- more -->

## 1. What pkg/notify is

`wait` is one-to-one (one id per waiter); `notify` is **one-to-many broadcast**: one event happens and simultaneously wakes every goroutine currently waiting, without caring who they are or how many.

The core mechanism exploits a Go property — **`close(ch)` makes every `<-ch` return immediately**:

- `Receive()` returns the current channel; a consumer blocks on `<-ch`;
- `Notify()` **first creates a new channel to replace the old one, then `close`s the old channel** — all goroutines waiting on the old channel are woken at once; the next round of waiters receive the new channel, so the broadcast is repeatable.

In one sentence: **closing a channel is the broadcast; swap in a fresh one after each use to make it repeatable.**

## 2. A minimal runnable implementation

> Full runnable source: [golang/etcd/pkg-notify/main.go](https://github.com/spursy/spursy.github.io/blob/main/golang/etcd/pkg-notify/main.go). Clone the repo, then `cd golang && go run ./etcd/pkg-notify`.

Below is a dependency-free equivalent you can `go run` directly; the logic matches `go.etcd.io/etcd/pkg/v3/notify`. The core is just two methods, `Receive` and `Notify`.

```go
// Notifier is a thread-safe struct used to broadcast the occurrence of an event to multiple consumers.
type Notifier struct {
	mu      sync.RWMutex
	channel chan struct{}
}

func NewNotifier() *Notifier {
	return &Notifier{channel: make(chan struct{})}
}

// Receive returns a channel that can be used to wait for a notification.
// Consumers are notified by the "channel being closed" event.
func (n *Notifier) Receive() <-chan struct{} {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.channel
}

// Notify closes the channel currently handed out to consumers (broadcasting to all waiters),
// and creates a new channel for the next notification.
func (n *Notifier) Notify() {
	newChannel := make(chan struct{})
	n.mu.Lock()
	channelToClose := n.channel
	n.channel = newChannel
	n.mu.Unlock()
	close(channelToClose) // close the old channel: every <-ch returns at once
}
```

Use it to "wake all consumers with one broadcast":

```go
n := NewNotifier()

var wg sync.WaitGroup
for i := 0; i < 3; i++ {
	wg.Add(1)
	go func(id int) {
		defer wg.Done()
		<-n.Receive() // block until woken by the broadcast
		fmt.Printf("consumer %d awakened\n", id)
	}(i)
}

time.Sleep(100 * time.Millisecond) // let all consumers start waiting
n.Notify()                         // one call, 3 consumers wake at once
wg.Wait()
```

A single `Notify()` wakes all 3 consumers at once — **without knowing how many there are**. That is the value of one-to-many broadcast.

For repeated periodic broadcasts, the consumer must call `Receive()` again each round:

```go
go func() {
	for i := 0; i < 3; i++ {
		<-n2.Receive() // must Receive again each round to get the new channel
		fmt.Printf("tick %d\n", i+1)
	}
}()

for i := 0; i < 3; i++ {
	time.Sleep(200 * time.Millisecond)
	n2.Notify() // broadcast every 200ms
}
```

## 3. A few key design points

### 1. One-to-many broadcast

A single `Notify()` wakes all waiters at once, with no need to know how many consumers there are. Simpler than messaging each consumer by hand or `Broadcast`-ing on a `sync.Cond` — because `close(channel)` is inherently "one operation, everyone receives it".

### 2. Repeatable notification — swap in the new one before closing the old

`Notify()` always **installs a new channel into `n.channel` first, then `close`s the old one**. The order matters: new waiters get the new channel before the old one is closed, so this round's close only affects "those already waiting on the old channel" and never harms the next round. That is what lets it broadcast round after round (periodic release, repeatedly changing state).

### 3. Zero-memory signal

It uses `chan struct{}`, carrying only the "the event happened" signal itself with no memory footprint. `context.Done()` is built on the same idea.

### 4. Thread safety

An `RWMutex` protects reading and replacing the channel: `Receive()` takes the read lock (concurrent), `Notify()` takes the write lock (swaps the pointer). Both `Receive` and `Notify` may be called from multiple goroutines concurrently.

### Gotchas

- **Consumers must `Receive()` again each round**: `Notify` swaps in a new channel; anyone still holding the old one will miss the next broadcast.
- **Carries no data**: it delivers only a signal; to pass data you need a separate lock-protected shared variable.
- **Edge-triggered, not level-triggered**: if nobody is in `Receive` at the moment of `Notify`, that broadcast is "missed". It suits "periodic release", not "guaranteed delivery".

## 4. How etcd actually uses it

In one sentence: whenever "a **global state event** happens and a batch of waiters of **unknown count** must be woken at once, without distinguishing who they are", etcd uses `pkg/notify` for **one-to-many broadcast**. Its core purpose is serving **linearizable reads**.

`server/etcdserver/server.go` creates 3 `notify.Notifier`s:

| Notifier | When `Notify()` (broadcast) | Who `Receive()`s (waits) | Purpose |
|----------|-----------------------------|--------------------------|---------|
| `leaderChanged`(:236) | when this node becomes new leader(:789) | linearizable read loop | leader changed → **drop stale read requests** |
| `firstCommitInTerm`(:282) | on first commit in a new term(:1949) | ReadIndex logic(:2207) | may **re-send ReadIndex** now |
| `clusterVersionChanged`(:283) | when cluster version changes | version-monitor loop(:2231) | cluster version changed |

### Scenario 1: leaderChanged — serving linearizable reads

Linearizable reads use the ReadIndex mechanism: before reading, confirm with the leader "how far commit has progressed", then wait for local apply to catch up before reading; multiple concurrent reads **wait on the same confirmation together**.

The problem: if the **leader switches** while waiting, every confirmation based on the old leader is void, and this batch of stale reads must be **woken immediately and made to fail and retry** — otherwise they could return inconsistent data.

The consumer at `server/etcdserver/read/read.go:98`:

```go
leaderChangedNotifier := r.server.LeaderChanged() // = s.leaderChanged.Receive()
select {
case <-leaderChangedNotifier: // woken by broadcast
	continue                  // drop this round, start over
// ...
}
```

`server.go:789` broadcasts on becoming new leader:

```go
if newLeader {
	s.leaderChanged.Notify() // one broadcast wakes all waiting reads
}
```

A single `Notify()` wakes every blocked read at once, with no need to know how many — exactly the value of design point 1 in a real system.

### Scenario 2: firstCommitInTerm — triggering a ReadIndex re-send

Before a new leader commits its first log entry in the term, the ReadIndex return value is unreliable. The read logic must wait for the "first commit in term" event before re-sending ReadIndex.

`read.go:194`:

```go
case <-firstCommitInTermNotifier:
	firstCommitInTermNotifier = r.server.FirstCommitInTermNotify() // Receive again to get the new channel
	// ... re-send ReadIndex
```

`server.go:1949` broadcasts with `s.firstCommitInTerm.Notify()` on the first commit. This perfectly demonstrates the **repeatable notification** of design point 2: the consumer, once woken, immediately `Receive()`s again to be ready for the next term.

### An important contrast: the custom error-carrying notifier in the read package

`server/etcdserver/read/util.go` defines its **own** notifier:

```go
type notifier struct {
	c   chan struct{}
	err error // ← one more err field than pkg/notify
}
func (nc *notifier) notify(err error) { nc.err = err; close(nc.c) }
```

It is used for "read completion result" notifications, because it needs to **carry an err along**; `pkg/notify` is used for the **pure-signal, data-free** broadcasts like leaderChanged / firstCommitInTerm. Both share the exact same idea (broadcast via `close(channel)`); the only difference is whether data is carried.

## 5. Why notify instead of wait

| | Write request (wait) | These internal events (notify) |
|--|----------------------|--------------------------------|
| Topology | one-to-one, precise wake by ID | one-to-many, broadcast to all |
| Data | carries the apply result | signal only |
| Reason | each write must get back **its own** result | leader change / new term are global events; all reads must know, no need to tell them apart |

The boundary is clear: to route precisely to a single one and return data, use `wait`; to broadcast a pure signal to everyone at once, use `notify`.

## Key source locations

| Item | Location |
|------|----------|
| notify implementation | `pkg/notify/notify.go` (Receive / Notify) |
| the 3 Notifier definitions | `server/etcdserver/server.go:236, 282, 283` |
| `leaderChanged.Notify()` | `server.go:789` |
| `firstCommitInTerm.Notify()` | `server.go:1949` |
| linearizable read loop consumer | `server/etcdserver/read/read.go:98` |
| ReadIndex re-send consumer | `server/etcdserver/read/read.go:194` |
| custom error-carrying notifier | `server/etcdserver/read/util.go` |
