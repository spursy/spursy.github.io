+++
title = "etcd pkg/wait.WaitTime: Threshold-Based Batch Release by Logical Time"
date = 2026-07-31
description = "Starting from a self-contained runnable demo, this post explains how etcd's WaitTime waits for the apply index to catch up via a monotonic progress line plus threshold release, and how it serves linearizable reads."
[taxonomies]
tags = ["etcd"]
+++

The previous two posts covered `pkg/wait` (one-to-one by ID, carries data) and `pkg/notify` (broadcast, signal only). Beyond those two, `pkg/wait` hides a third waiting primitive — `WaitTime` (`wait_time.go`): **batch release keyed on a monotonically increasing "logical time" threshold**.

It serves a very specific problem: **wait for a monotonically increasing progress to catch up to some mark**. In etcd that progress is the **applied index**, and its main consumer is exactly the **linearizable read** from the last post. This post starts from a self-contained runnable demo to explain its design, then maps it back to the source.

<!-- more -->

## 1. What WaitTime is

`pkg/wait` actually contains two things:

- **`Wait` (`wait.go`)**: one-to-one by "ID", `Trigger(id, result)` precisely wakes one waiter and **carries data**.
- **`WaitTime` (`wait_time.go`, our focus)**: one-to-many by "logical time", `Trigger(deadline)` wakes at once every waiter whose target is `<=` deadline, **signal only**.

Typical scenario: there is a monotonically increasing progress counter (like applied index).

- Consumer: "wait until progress reaches mark N" → `Wait(N)`, blocks on the returned channel;
- Producer: "already advanced to mark M" → `Trigger(M)`, releases every waiter with `N <= M` at once.

In one sentence: **on a single monotonically increasing progress line, whoever wants to wait for a mark registers a channel; when progress advances to X, one call closes all channels `<= X` and releases them all.**

## 2. A minimal runnable implementation

> Full runnable source: [golang/etcd/pkg-wait-time/main.go](https://github.com/spursy/spursy.github.io/blob/main/golang/etcd/pkg-wait-time/main.go). Clone the repo, then `cd golang && go run ./etcd/pkg-wait-time`.

Below is a dependency-free equivalent you can `go run` directly; the logic matches `wait_time.go` in `go.etcd.io/etcd/pkg/v3/wait`. The core is just two methods, `Wait` and `Trigger`.

```go
// closec is a channel that is closed the moment it is born,
// used to give callers waiting on an already-passed deadline a zero-cost immediate release.
var closec chan struct{}

func init() { closec = make(chan struct{}); close(closec) }

type timeList struct {
	l                   sync.Mutex
	lastTriggerDeadline uint64                   // the largest deadline triggered so far
	m                   map[uint64]chan struct{} // deadline -> waiting channel
}

func NewTimeList() *timeList {
	return &timeList{m: make(map[uint64]chan struct{})}
}

// Wait returns a channel that waits on the given deadline;
// the channel is closed (released) when Trigger's deadline >= this deadline.
func (tl *timeList) Wait(deadline uint64) <-chan struct{} {
	tl.l.Lock()
	defer tl.l.Unlock()
	// already passed: return the pre-closed global channel for an immediate release (removes the race).
	if tl.lastTriggerDeadline >= deadline {
		return closec
	}
	ch := tl.m[deadline]
	if ch == nil {
		ch = make(chan struct{})
		tl.m[deadline] = ch
	}
	return ch
}

// Trigger advances progress to deadline and releases every waiter whose target is <= deadline.
func (tl *timeList) Trigger(deadline uint64) {
	tl.l.Lock()
	defer tl.l.Unlock()
	tl.lastTriggerDeadline = deadline
	for t, ch := range tl.m {
		if t <= deadline {
			delete(tl.m, t)
			close(ch) // close means release: goroutines waiting on it all return
		}
	}
}
```

Use it to simulate "wait for the applied index to catch up": three consumers wait for progress 5 / 8 / 12.

```go
tl := NewTimeList()

var wg sync.WaitGroup
for _, d := range []uint64{5, 8, 12} {
	wg.Add(1)
	go func(deadline uint64) {
		defer wg.Done()
		<-tl.Wait(deadline) // block until progress reaches deadline
		fmt.Printf("waiter(deadline=%d) released\n", deadline)
	}(d)
}

time.Sleep(100 * time.Millisecond) // let all consumers register

tl.Trigger(8)  // advance to 8: release 5 and 8; 12 keeps waiting
tl.Trigger(20) // advance to 20: release the remaining 12
wg.Wait()
```

A single `Trigger(8)` releases both waiters for deadlines 5 and 8 — **"release everything past the line", not "exactly equal"**.

And waiting on an already-passed deadline returns immediately:

```go
// now lastTriggerDeadline = 20
<-tl.Wait(10) // 10 <= 20, already passed, returns at once via the global closec, no blocking
```

## 3. A few key design points

### 1. Release via "closing a channel"

Each not-yet-reached deadline maps to a `chan struct{}`; on `Trigger`, all channels `<=` deadline are `close`d — the goroutines waiting on them return at once. Same root as `notify`: **`close` is broadcast**. The difference here is the added "threshold filter".

### 2. lastTriggerDeadline — removing the "wait for an already-happened event" race (the subtlest part)

Progress is monotonic: once `Trigger(M)` has happened, no later `Wait(N<=M)` should block anymore. `timeList` records `lastTriggerDeadline`; if the `Wait`'s deadline has already been triggered, it returns the **global `closec` that was `close`d back in `init()`** — released immediately, zero allocation.

This solves a real race: callers often "peek at whether progress is enough, and wait only if not". Between the check and the `Wait` there is a window; if progress catches up and `Trigger`s right after the check, waiting on a bare channel would **block forever**. `lastTriggerDeadline` makes "an already-happened event still retrievable when you come to wait for it later" — that is where it beats a bare channel.

### 3. One Trigger folds up a whole batch of waiters

`Trigger(deadline)` walks the map and closes all channels `<= deadline`. A later, larger deadline **incidentally releases all the smaller ones before it**, with no need to wake each one precisely. This is extremely efficient when a single progress line has many waiters at different marks.

### Gotchas

- **deadline must come from a monotonically increasing quantity** (like applied index), otherwise the "already-passed ⇒ instant release" semantics of `lastTriggerDeadline` break down.
- **Signal only, no data**: whatever progress value you want to read afterward must be fetched separately.
- **It is "release all `<=`", not "exactly equal"**: it cannot express "wait for exactly this mark".

## 4. How etcd actually uses it

In one sentence: whenever you need to **wait for a monotonically increasing progress (applied index) to catch up to some mark**, etcd uses `WaitTime` (`wait.NewTimeList()`). It is the single field named `applyWait` in `EtcdServer`.

### The only producer: the apply loop

`server/etcdserver/server.go:248` defines the field, `:567` initializes it. Every time `applyAll()` finishes applying a batch of entries, it advances the progress (`server.go:978`):

```go
func (s *EtcdServer) applyAll(ep *etcdProgress, apply *toApply) {
	s.applySnapshot(ep, apply)
	s.applyEntries(ep, apply)
	// ...
	s.applyWait.Trigger(ep.appliedi) // advance to appliedi, release everything <= it
}
```

Apply is a **globally single, serial** loop, so `appliedi` is naturally **monotonically increasing** — exactly satisfying `WaitTime`'s premise that deadlines are monotonic.

### The core consumer: the "second wait" of a linearizable read

The linearizable read from the last post has two steps; `WaitTime` handles the second:

1. **Confirm step** (uses `pkg/notify` + ReadState): ask the leader for a ReadIndex and get a reliable `confirmedIndex` — "at the moment of this read, the cluster has committed at least up to here".
2. **Catch-up step** (uses `WaitTime`): the local apply may not have reached `confirmedIndex` yet. `server/etcdserver/read/read.go:133`:

```go
appliedIndex := r.server.AppliedIndex()
if appliedIndex < confirmedIndex {
	select {
	case <-r.server.ApplyWait(confirmedIndex): // wait until apply catches up to confirmedIndex
	case <-r.server.Stopping():
		return
	}
}
// apply is now >= confirmedIndex, so what we read is guaranteed linearizable
nr.notify(nil)
```

Multiple concurrent reads wait on different `confirmedIndex` values; a single `Trigger(appliedi)` from the apply loop releases every read whose target has been caught up — exactly the value of **threshold-based one-to-many release**. The `lastTriggerDeadline` of §3.2 saves the day in the real system too: the window between the `AppliedIndex() < confirmedIndex` check and the following `ApplyWait` is backstopped by it, so it never blocks forever.

### Other consumers

| Consumer | Location | Waits for |
|----------|----------|-----------|
| linearizable read catch-up | `read/read.go:135` | `ApplyWait(confirmedIndex)` |
| auth token validation | `server.go:352` | `applyWait.Wait(index)`, for auth state to be applied |
| wait local catch-up to commit | `v3_server.go:458` `waitAppliedIndex()` | `ApplyWaitCommit()` |
| Lease HTTP handler | `server.go:646` | `ApplyWaitCommit`, a follower catches up before handling |

`ApplyWaitCommit()` (`server.go:632`) = `applyWait.Wait(s.getCommittedIndex())`, meaning "wait until local apply catches up to the current committed index".

## 5. wait / notify / WaitTime side by side

| | wait (by ID) | notify (broadcast) | WaitTime (by threshold) |
|--|--------------|--------------------|-------------------------|
| Topology | one-to-one | one-to-many | one-to-many |
| Wake condition | exact ID match | any event happens | deadline `<=` trigger value |
| Data | carries apply result | signal only | signal only |
| Fits | a write gets back **its own** result | global events like leader change | wait for progress to **reach a mark** |

"Wait for apply to reach mark N" is inherently a **threshold semantic**: it does not care who or how many are waiting; as long as progress crosses the line, they are all released. `wait` would require minting an ID per index; `notify` cannot express "release only past the line"; only `WaitTime` fits. Together the three cover etcd's internal needs for "precise routing / global broadcast / threshold release".

## Key source locations

| Item | Location |
|------|----------|
| WaitTime implementation | `pkg/wait/wait_time.go` (Wait / Trigger / lastTriggerDeadline / global closec) |
| `applyWait` field definition | `server/etcdserver/server.go:248` |
| initialization `NewTimeList()` | `server.go:567` |
| the only producer `Trigger(appliedi)` | `server.go:978` (`applyAll`) |
| `ApplyWait` / `ApplyWaitCommit` wrappers | `interface.go:22`, `server.go:632` |
| linearizable read catch-up consumer | `server/etcdserver/read/read.go:135` |
| `waitAppliedIndex` consumer | `server/etcdserver/v3_server.go:458` |
