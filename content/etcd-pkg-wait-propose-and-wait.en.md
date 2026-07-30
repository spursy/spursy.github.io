+++
title = "etcd pkg/wait: Turning Async Consensus into a Sync Call with the Propose-and-Wait Model"
date = 2026-07-30
[taxonomies]
tags = ["etcd"]
+++

In etcd every write request (Put / Delete / Txn / Compaction) looks like an ordinary synchronous call from the outside, yet underneath it is a thoroughly asynchronous process: the request is proposed to Raft, replicated across the cluster to reach consensus, and finally applied to the state machine. In between sits the huge uncertainty of "who handles it, and when is it done".

etcd bridges this gap elegantly with a tiny package of little more than a hundred lines: `pkg/wait`. This post starts from a self-contained runnable demo, explains its design, and then maps it back to the real etcd source to see how it is actually used.

<!-- more -->

## 1. What pkg/wait is

You kick off an asynchronous operation (propose to Raft, push onto a queue, fire an async RPC), but the caller wants the result **synchronously**. `wait` connects the two ends with an `ID -> channel` map:

- the caller does `Register(id)`, gets a channel, and blocks on it;
- whoever finishes the work does `Trigger(id, result)`, dropping the result into the channel under the same `id` to wake the caller.

In one sentence: **it wraps an asynchronous process behind a synchronous API.**

## 2. A minimal runnable implementation

> Full runnable source: [golang/etcd/pkg-wait/main.go](https://github.com/spursy/spursy.github.io/blob/main/golang/etcd/pkg-wait/main.go). After cloning the repo, run `cd golang && go run ./etcd/pkg-wait`.

Below is an equivalent implementation with the etcd dependencies stripped out, runnable directly with `go run`. The core is just two methods: `Register` and `Trigger`.

```go
const defaultListElementLength = 64

type listElement struct {
	l sync.RWMutex
	m map[uint64]chan any
}

// minimalWait provides wait / trigger-by-ID capability.
type minimalWait struct {
	e []listElement // 64 shards to reduce lock contention
}

func newWait() *minimalWait {
	res := &minimalWait{e: make([]listElement, defaultListElementLength)}
	for i := 0; i < len(res.e); i++ {
		res.e[i].m = make(map[uint64]chan any)
	}
	return res
}

// Register returns a channel that waits on the given id.
// Note: the buffer is 1 -- this is the key to decoupling producer and consumer in time.
func (w *minimalWait) Register(id uint64) <-chan any {
	idx := id % defaultListElementLength
	newCh := make(chan any, 1)
	w.e[idx].l.Lock()
	defer w.e[idx].l.Unlock()
	if _, ok := w.e[idx].m[id]; !ok {
		w.e[idx].m[id] = newCh
	} else {
		log.Panicf("dup id %x", id)
	}
	return newCh
}

// Trigger wakes the channel registered under id, delivering result x.
func (w *minimalWait) Trigger(id uint64, x any) {
	idx := id % defaultListElementLength
	w.e[idx].l.Lock()
	ch := w.e[idx].m[id]
	delete(w.e[idx].m, id)
	w.e[idx].l.Unlock()
	if ch != nil {
		ch <- x // buffer of 1: does not block even if nobody is receiving yet
		close(ch)
	}
}
```

Wrap it into a service that is "async inside, sync outside":

```go
// Do is a synchronous API: async under the hood, but synchronous to the caller.
func (s *Server) Do(data string) (Result, error) {
	id := atomic.AddUint64(&s.reqID, 1)

	ch := s.w.Register(id)               // (1) register, get the waiting channel
	s.taskCh <- task{id: id, data: data} // (2) hand the task to the async worker

	select {
	case x := <-ch: // (3) block until woken
		return x.(Result), nil
	case <-time.After(3 * time.Second): // timeout guard to avoid a permanent leak
		return Result{}, fmt.Errorf("request %d timeout", id)
	}
}

// worker is the background processor: after finishing, it wakes the matching waiter by id.
func (s *Server) worker() {
	for t := range s.taskCh {
		result := Result{Value: "processed: " + t.data}
		s.w.Trigger(t.id, result) // (4) return the result under the same id
	}
}
```

With concurrent calls, every request retrieves **exactly its own result** by its unique `id`, with no crosstalk.

## 3. Key design points

### 3.1 Precise one-to-one wakeup

Keyed by request `id`, `Trigger` wakes only the matching waiter -- naturally "one-to-one, exactly once". After `close(ch)` the entry is deleted from the map, so a repeated `Trigger` becomes a no-op.

### 3.2 Buffer of 1 -- decoupling producer and consumer in time (the subtle part)

`Register` creates `make(chan any, 1)`, and `Trigger` does `ch <- x` followed immediately by `close`.

A buffer of 1 guarantees: **even if the caller has not yet reached `<-ch`, `Trigger` does not block and can return immediately.** This means a slow / timed-out / already-departed caller can never block the wakeup-side critical path. In etcd that critical path is the **apply loop** -- a global, single, serial point that must never be stalled by any single client. Buffer-of-1 fundamentally rules out "one slow client dragging down the whole state machine".

### 3.3 Sharded locks to reduce contention

Internally there are 64 `(RWMutex + map)` shards; `id % 64` locates the shard, so lock contention stays low under highly concurrent register / wakeup.

### 3.4 close as a safety net against leaks

After writing, `Trigger` calls `close(ch)`. Even if the caller left early on timeout, the value sits quietly in the buffer, and once the channel has no references it is garbage-collected -- no blocking, no leak.

### Gotchas

- **id must be unique**: registering the same id twice panics.
- **always pair with a timeout**: if the processor never calls `Trigger`, the waiter blocks forever and the channel leaks in the map. etcd relies on context timeouts plus an explicit `Trigger` on propose failure.
- **wakeup only once**: after `close(ch)`, triggering the same id again is a no-op.

## 4. How etcd uses it

In one sentence: etcd implements the **Propose-and-Wait model** with `pkg/wait` -- whenever "a write request needs to get back **its own** result", it uses `wait`.

The entry point is `server/etcdserver/v3_server.go`. The public APIs `Put()` / `Txn` / `Compaction` all funnel into `processInternalRaftRequestOnce()`. The real flow (against the source):

```text
(1) id := s.reqIDGen.Next()        // generate a globally unique request ID (:1067)
(2) ch := s.w.Register(id)         // register the mailbox first, get a buffer-1 channel (:1106)
(3) s.r.Propose(cctx, data)        // propose the request into Raft, return immediately (:1113)
(4) select {                       // block on three cases (:1122)
      case x := <-ch:              //   normal: got the apply result
      case <-cctx.Done():          //   timeout / client cancel
      case <-s.done:               //   server stopping
    }
```

The waker is the apply loop: after applying a log entry to the state machine (going through the UberApplier chain and writing MVCC), it calls `s.w.Trigger(id, result)` to wake the exact originating goroutine by ID.

### Key designs reflected in the source

**Register before Propose (order matters).** If you proposed first, Raft might apply and Trigger extremely quickly -- before Register had run -- and the result would be lost. Build the mailbox first, then propose: race eliminated.

**Buffer of 1 on the waiting channel.** The apply loop is a global, single, serial loop that must not be stalled by any slow / timed-out caller. Buffer-of-1 lets `ch <- x` in `Trigger` drop the result and return immediately, decoupling producer and consumer in time. This is exactly point 3.2 playing out in a real system.

**Timeout plus active cleanup (GC wait).** On propose failure or request timeout, the caller actively calls `s.w.Trigger(id, nil) // GC wait`. The goal is not to wake itself (it is about to return) but to let `Trigger` internally `delete(m, id)` and remove the mailbox from the map, avoiding a permanent leak. **Whoever registers is responsible for cleanup.**

**A three-way select guarantees an exit on any anomaly.** Normal result / timeout-cancel / server-stop -- the goroutine never blocks forever.

## 5. Why wait rather than broadcast (notify)

| | Write request (wait) |
|--|----------------------|
| Topology | one-to-one: each request ID maps to one waiter |
| Carries data | yes, `Trigger(id, result)` carries the apply result |
| Reason | each write must retrieve **its own** result, routed by the unique ID |

For global events like "leader changed" or "first commit in a new term" -- where you need to wake **an unknown number of waiters at once** -- etcd uses a different package, `pkg/notify`, for one-to-many broadcast. The boundary is clear: need to route precisely to one and return data, use `wait`; just broadcast a pure signal to everyone, use `notify`.

## Source index

| Item | Location |
|------|----------|
| wait implementation | `pkg/wait/wait.go` (Register / Trigger / 64 sharded locks) |
| write request entry | `server/etcdserver/v3_server.go:295` `Put()` |
| propose-and-wait core | `server/etcdserver/v3_server.go:1058` `processInternalRaftRequestOnce()` |
| Register the mailbox | `v3_server.go:1106` |
| Propose | `v3_server.go:1113` |
| blocking select | `v3_server.go:1122` |
| GC wait cleanup | `v3_server.go:1116, 1128` |
