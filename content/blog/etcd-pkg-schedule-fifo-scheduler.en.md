+++
title = "etcd pkg/schedule.FIFO: Collapsing Concurrent Submissions into Serial Execution with a Single-Goroutine Scheduler"
date = 2026-08-04
description = "Starting from a self-contained runnable demo, we dissect the FIFO serial scheduler in etcd's pkg/schedule: how it uses 'a single consumer goroutine + resume edge-triggered wakeup + sync.Cond completion wait + graceful shutdown' to collapse concurrent submissions from many callers into one strictly ordered execution stream that never blocks the submitter — and how it underpins raft apply and MVCC compaction."
[taxonomies]
tags = ["etcd"]
[extra]
toc = true
+++

Earlier posts dissected etcd's concurrency primitives (`wait` / `WaitTime` / `notify`), its lock-free ID (`idutil`), page-aligned IO (`PageWriter`), and the stall detector (`contention`). This one looks at a "scheduling" module: the **FIFO serial scheduler** in `pkg/schedule`.

The problem it solves is very concrete: **a batch of tasks must run strictly in submission order, one after another, yet the submitter must not be blocked by how long execution takes.** Applying raft log entries and MVCC compaction are exactly this kind of "must be serially correct, yet must not stall the upstream" scenario. This post starts from the implementation and shows how it cleanly satisfies these seemingly contradictory demands with a few channels + one lock + one `sync.Cond`.

<!-- more -->

## 1. What it promises

In one line: **collapse concurrent submissions from many callers into serial execution on a single goroutine.**

- **Strict FIFO serial**: all Jobs that come in via `Schedule` execute strictly in submission order, one after another, never concurrently. So **Jobs never have to worry about concurrency safety internally**.
- **Non-blocking submission**: `Schedule` just pushes the Job into the queue and returns immediately; execution happens asynchronously on a background goroutine. "Receiving the next batch" and "executing the current batch" are decoupled in time.
- **Graceful shutdown**: `Stop()` drains and runs the remaining queued tasks, then confirms the background goroutine has truly exited.

## 2. Core data structure

```go
type fifo struct {
	mu sync.Mutex

	resume    chan struct{} // "idle→busy" edge-triggered wakeup signal (buffer 1)
	scheduled int           // number of tasks that have started scheduling (dequeued for execution)
	finished  int           // number of finished tasks
	pendings  []Job         // pending queue (the heart of FIFO)

	ctx    context.Context    // passed to each Job's Do(ctx); cancelled on shutdown
	cancel context.CancelFunc // triggers shutdown; doubles as the "already stopped" sentinel

	finishCond *sync.Cond    // compound-condition wait for completion (WaitFinish)
	donec      chan struct{} // closed when run() exits, letting Stop wait synchronously
}
```

The constructor starts the **one and only** background goroutine — this is the root of serialization:

```go
func NewFIFOScheduler() Scheduler {
	f := &fifo{
		resume: make(chan struct{}, 1),
		donec:  make(chan struct{}, 1),
	}
	f.finishCond = sync.NewCond(&f.mu)          // reuse the same lock
	f.ctx, f.cancel = context.WithCancel(context.Background())
	go f.run()                                  // the sole consumer
	return f
}
```

> Full runnable source: [golang/etcd/pkg-schedule/main.go](https://github.com/spursy/spursy.github.io/blob/main/golang/etcd/pkg-schedule/main.go). After cloning the repo, run `cd golang && go run ./etcd/pkg-schedule`.

## 3. Four core mechanisms

### 1. Single consumer goroutine → the root of serialization

Only one `run()` goroutine ever executes Jobs, so tasks are naturally serial:

```go
func (f *fifo) run() {
	defer func() {
		close(f.donec)
		close(f.resume)
	}()

	for {
		var todo Job
		f.mu.Lock()
		if len(f.pendings) != 0 {
			f.scheduled++
			todo = f.pendings[0] // take the head → FIFO
		}
		f.mu.Unlock()

		if todo == nil {
			select {
			case <-f.resume:     // queue empty → sleep, wait for wakeup
			case <-f.ctx.Done(): // shutdown → drain remaining pending then exit
				// ... see mechanism 4
			}
		} else {
			f.executeJob(todo, false)
		}
	}
}
```

Taking the head with `pendings[0]` while `Schedule` `append`s to the tail — **first-in-first-out** rests entirely on this head-and-tail pair. Because only this one goroutine runs Jobs, "apply order == submission order" is guaranteed for free.

### 2. The resume channel: edge-triggered wakeup, no busy-wait and no blocked producer

When the queue is empty, the consumer must not spin and burn CPU — it must sleep; and it must be woken when work arrives. This "sleep-wakeup" is carried by `resume`, using an elegant **edge-triggered** trick:

```go
func (f *fifo) Schedule(j Job) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.cancel == nil {
		panic("schedule: schedule to stopped scheduler")
	}

	// only fire a wakeup when the queue transitions from empty to non-empty
	if len(f.pendings) == 0 {
		select {
		case f.resume <- struct{}{}:
		default: // a signal is already buffered; don't re-send, and don't block the producer
		}
	}
	f.pendings = append(f.pendings, j)
}
```

Three details together make it complete:

- **Fire the signal only on the "empty→non-empty" edge**: when the queue already has work, `run` is busy running it and needs no waking; sending would be wasted.
- **Buffer of 1**: at most one "pending wakeup" signal, enough to express the single fact "there is work".
- **`select` + `default` non-blocking send**: if the buffer is full (a signal already there), fall through to `default` and skip — **the producer is never blocked by `Schedule`**.

This is the classic "edge-triggered wakeup" pattern: use a capacity-1 channel to represent the single state transition "from idle to busy".

### 3. sync.Cond: waiting for the compound condition "completion count met AND queue empty"

`WaitFinish(n)` waits not for a single event but for a **compound predicate over two variables**: "at least n finished, and no pending left". This kind of compound-condition wait is exactly `sync.Cond`'s home turf:

```go
func (f *fifo) WaitFinish(n int) {
	f.finishCond.L.Lock()
	for f.finished < n || len(f.pendings) != 0 {  // for-loop rechecks the compound condition
		f.finishCond.Wait()
	}
	f.finishCond.L.Unlock()
}
```

Each finished Job `Broadcast`s to wake all waiters to re-evaluate (see `executeJob` in the next section). Using `Cond` rather than a channel is because what we wait on is a combination of two lock-protected variables — "count + queue length" — and `Cond` (lock + `for` recheck) expresses such a predicate most naturally. Note that `sync.NewCond(&f.mu)` **reuses the same `mu`**, ensuring "checking the condition" and "changing the condition" are synchronized under one lock.

### 4. Graceful shutdown: cancel signals, donec confirms exit

`Stop` proceeds in two steps — first send the stop signal, then synchronously wait for the background to truly exit:

```go
func (f *fifo) Stop() {
	f.mu.Lock()
	f.cancel()      // trigger ctx.Done()
	f.cancel = nil  // set nil: a later Schedule will panic (prevents submitting to a stopped scheduler)
	f.mu.Unlock()
	<-f.donec       // block until run() truly exits
}
```

After `run()` receives `ctx.Done()`, it **drains the unfinished queued tasks by running them once with the cancelled ctx**, then exits:

```go
case <-f.ctx.Done():
	f.mu.Lock()
	pendings := f.pendings
	f.pendings = nil
	f.mu.Unlock()
	for _, todo := range pendings {
		f.executeJob(todo, true) // drain remaining pending
	}
	return                       // run exits → defer close(donec)
```

`<-f.donec` is the crucial **synchronization point**: `f.cancel()` only sends the signal asynchronously and `run` does not stop instantly; when `run` exits it `defer close(f.donec)`, and the property that "receiving from a closed channel returns immediately" makes `Stop()` block until `run` truly ends. So **when `Stop()` returns, the background goroutine has definitely exited completely and the remaining tasks have been drained** — upgrading "send a signal" into "confirm it has stopped".

## 4. Two robustness details

**① Dequeue only after execution.** The dequeue action `pendings = pendings[1:]` lives in `executeJob`'s `defer`, not at the moment of taking:

```go
func (f *fifo) executeJob(todo Job, updatedFinishedStats bool) {
	defer func() {
		if !updatedFinishedStats {
			f.finishCond.L.Lock()
			f.finished++
			f.pendings = f.pendings[1:] // ← dequeue only after execution
			f.finishCond.Broadcast()
			f.finishCond.L.Unlock()
		}
		if err := recover(); err != nil {
			fmt.Printf("execute job %q failed: %v\n", todo.Name(), err)
		}
	}()
	todo.Do(f.ctx)
}
```

Why? This way a "**currently executing task still counts as pending**", so the `len(pendings)!=0` check in `WaitFinish` stays accurate — a still-running task is never mistakenly counted as finished and released early.

**② recover as a safety net.** A single Job's `panic` is caught by `recover`, only logged and not re-thrown, so it **won't take down the whole scheduling goroutine**. Otherwise one bad task could keep all subsequent tasks forever unscheduled.

## 5. Run the demo

The demo's two scenarios vividly show "serial" and "graceful shutdown":

```
=== demo1: concurrent submit, FIFO serial execution ===
executed job-0
executed job-1
executed job-2
executed job-3
executed job-4
execution order = [0 1 2 3 4] (== submit order, serial)
pending=0 scheduled=5 finished=5

=== demo2: Stop drains pending jobs ===
before stop: pending=3
slow job done
queued-1 executed (drained on stop)
queued-2 executed (drained on stop)
scheduler stopped
```

In demo1 the 5 tasks **deliberately have decreasing durations** (earlier-submitted ones sleep longer), yet the execution order is still strictly `[0 1 2 3 4]` — proving it is serial by submission order, not by who finishes fastest. In demo2, `Stop()` drains and runs all 3 piled-up pending tasks before returning.

## 6. Two major uses in etcd

Across all of etcd, this FIFO scheduler mainly serves two consumers.

### Scenario 1: applying raft log entries to the state machine (the most important)

The `run()` main loop in `server/etcdserver/server.go`:

```go
sched := schedule.NewFIFOScheduler(lg)          // :764
for {
	select {
	case ap := <-s.r.apply():                   // get a batch of entries to apply from raft
		f := schedule.NewJob("server_applyAll", func(context.Context) {
			s.applyAll(&ep, &ap)
		})
		sched.Schedule(f)                       // :844 enqueue and return, go receive the next batch
	}
}
```

The intent maps exactly onto the four mechanisms above: apply writes bbolt and walks the applier chain — it is **slow**; the main loop only `Schedule`s to enqueue and immediately goes back to receive raft's next batch, never blocked by apply. The actual apply runs on the single goroutine one after another, naturally guaranteeing **apply order == raft log order** — and getting that order wrong is a fatal state-machine-inconsistency bug.

### Scenario 2: MVCC compaction

In `server/storage/mvcc/kvstore.go`, the store holds a `fifoSched` and wraps each compaction into a Job to enqueue:

```go
j := schedule.NewJob("kvstore_compact", func(ctx context.Context) {
	if ctx.Err() != nil {          // scheduler already stopped → go through barrier to finish
		s.compactBarrier(ctx, ch)
		return
	}
	hash, err := s.scheduleCompaction(rev, prevCompactRev) // delete old revisions
	// ...
	close(ch)                      // notify the caller compaction is done
})
s.fifoSched.Schedule(j)
```

- **Compaction must be serial**: concurrently compacting the same MVCC data would interfere with itself; queuing guarantees only one compaction at a time.
- **Non-blocking submission**: `compact()` returns a `ch` right after enqueuing, and the caller `<-ch` waits for the result asynchronously.
- **The cleverness of compactBarrier**: when the scheduler is shutting down (`ctx.Err()!=nil`), compaction cannot run directly, so it **re-`Schedule`s a barrier into the same FIFO queue**, leveraging the "serial + drain on stop" semantics to guarantee the finishing channel is always handled and never leaks.

## 7. Why not just `go func()`

| | plain `go func()` | pkg/schedule FIFO |
|--|-----------------|-------------------|
| Order | unordered, concurrent | strict FIFO serial |
| Submitter | non-blocking but uncontrolled | non-blocking and controlled (enqueue-and-return) |
| Stop | hard to drain uniformly | `Stop()` uniformly drains remaining tasks |
| Crash isolation | one panic may go unhandled | `recover` safety net, doesn't take down the scheduler goroutine |

Both apply and compaction demand "**serially correct AND non-blocking to the upstream**", which is precisely the reason the FIFO scheduler exists: collapse concurrent submissions into one ordered, controllable, gracefully-stoppable execution stream.

## 8. Summary

The FIFO scheduler in `pkg/schedule` satisfies a set of seemingly contradictory demands with four mechanisms:

- **Single consumer goroutine** → serial correctness (order == submission order, Jobs need not care about concurrency);
- **resume edge-triggered wakeup** → the submitter isn't blocked and the consumer doesn't busy-wait;
- **sync.Cond** → cleanly wait for the compound condition "completion count met + queue drained";
- **cancel + donec** → graceful shutdown, signal and synchronously confirm the background has exited.

Add the two details "dequeue only after execution" and "recover as safety net", and it becomes a template-grade implementation of a "must be ordered, serial, gracefully stoppable" background task. Understand it, and you understand how etcd makes the lifeline of raft apply both strictly ordered and non-blocking to the main loop.

## Key source locations

| Content | Location |
|------|------|
| Scheduler implementation (run / Schedule / resume / Stop) | `pkg/schedule/schedule.go` |
| apply scheduler creation | `server/etcdserver/server.go:764` |
| apply Job enqueue | `server/etcdserver/server.go:844` |
| apply scheduler stop | `server/etcdserver/server.go:822` `sched.Stop()` |
| MVCC scheduler field | `server/storage/mvcc/kvstore.go:76` `fifoSched` |
| compaction Job | `server/storage/mvcc/kvstore.go:236` |
| compactBarrier re-enqueue | `server/storage/mvcc/kvstore.go:147, 201` |
