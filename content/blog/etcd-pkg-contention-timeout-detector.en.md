+++
title = "etcd pkg/contention.TimeoutDetector: Distilling Monitoring into a Few Dozen Reusable Lines"
date = 2026-08-04
description = "Starting from raft's classic warning 'leader is overloaded likely from slow disk', we dissect etcd's tiny, general-purpose stall detector TimeoutDetector: how a single map answers 'is something that should happen periodically running late', and how it buckets by follower in the heartbeat loop to pinpoint the slow node."
[taxonomies]
tags = ["etcd"]
[extra]
toc = true
+++

The earlier posts dissected etcd's concurrency primitives (`wait` / `WaitTime` / `notify`), its lock-free ID (`idutil`), and page-aligned IO (`PageWriter`). This one picks an even smaller module: the **70-line** `TimeoutDetector` in `pkg/contention`. It solves no business logic at all — it is a pure **observability tool** that answers a deceptively simple, highly general question: **is something that should happen periodically running late?**

If you have run etcd in production, you have probably seen this warning:

```
leader failed to send out heartbeat on time; took too long,
leader is overloaded likely from slow disk
```

Behind that log line sits `TimeoutDetector`. Let's start there.

<!-- more -->

## 1. What problem does it solve

Many systems have things that **should happen at a fixed interval**:

- A raft leader should send a heartbeat to every follower once per `heartbeat` interval;
- Some background task should tick every N seconds;
- Some reporting loop should push metrics every minute…

We want to know: **are these things running late?** Especially the raft heartbeat — if the leader is stalled by a **slow disk fsync, CPU starvation, or a GC pause**, the heartbeat loop misses its schedule; followers stop hearing from the leader and start an election, causing leader churn and reduced availability. A "late heartbeat" is an **early symptom** of this class of failures, worth detecting and alerting on.

`TimeoutDetector` is the general-purpose tool that answers this. Its design makes two key trade-offs:

1. **It has no idea what a "heartbeat" is.** It only deals with the abstract notion of "a periodic event carrying an id" — anything that should happen at a fixed interval can use it. This decoupling from any concrete business is exactly why it can be a reusable tool.
2. **Zero background overhead.** No goroutine, no timer — it does a single subtraction at the moment it is observed. Its state is just one map.

## 2. The whole implementation: one map + one subtraction

The entire type has just three fields:

```go
type TimeoutDetector struct {
	mu          sync.Mutex           // protects everything below
	maxDuration time.Duration        // the expected maximum interval (threshold)
	records     map[uint64]time.Time // id → last-seen time of that id
}
```

All the essence lives in a single `Observe` method:

```go
func (td *TimeoutDetector) Observe(id uint64) (bool, time.Duration) {
	td.mu.Lock()
	defer td.mu.Unlock()

	ok := true
	now := time.Now()
	exceed := time.Duration(0)

	if pt, found := td.records[id]; found {
		exceed = now.Sub(pt) - td.maxDuration // actual interval - threshold
		if exceed > 0 {
			ok = false
		}
	}
	td.records[id] = now // update "last time" for the next comparison
	return ok, exceed
}
```

Step by step:

1. Look up the "last time" `pt` for this `id`. **If this id has never been seen (first time)**, `found == false`, so we skip the comparison and return `ok=true` — with no "previous time" there is nothing to judge lateness against, so we let it pass.
2. If seen before, compute `now - pt` (the real interval between two successive events of the same id), then subtract the threshold `maxDuration`. **`exceed > 0` means it's late**, so return `ok=false` plus the amount by which it's late.
3. Either way, update `records[id]` to `now`, establishing a new baseline for the next comparison.

There is also a `Reset` that clears all history:

```go
func (td *TimeoutDetector) Reset() {
	td.mu.Lock()
	defer td.mu.Unlock()
	td.records = make(map[uint64]time.Time)
}
```

When do you need to clear it? When the "periodicity assumption" no longer holds — as we'll see next, once the raft leader role changes, the old timestamps become meaningless and you must `Reset` to avoid false alarms.

> Full runnable source: [golang/etcd/pkg-contention/main.go](https://github.com/spursy/spursy.github.io/blob/main/golang/etcd/pkg-contention/main.go). After cloning the repo, run `cd golang && go run ./etcd/pkg-contention`.

## 3. A few easy traps

**① The first `Observe` always returns `ok=true`.** This is not a bug, it's by design — with no "previous time" there's nothing to compare. So when monitoring with it, the first period never alerts; judgment begins from the second period onward.

**② It measures "the interval between two successive events of the same id," not "how long an action took."** If you want to measure "how long one fsync took," this tool is not the right fit (that would require recording time before and after the action). It is naturally suited to the "**is a periodic event late**" class of scenarios.

**③ `records` only grows.** Every id ever seen stays in the map. If the id space is large and the program runs long, the map keeps growing — you must rely on `Reset()` or an external bound to control memory. In etcd, ids are cluster member counts (usually single digits), so there is nothing to worry about.

**④ Concurrency safety is guaranteed by the internal `mu`**, so multiple goroutines can call `Observe` directly.

## 4. Real usage in etcd

Across all of etcd, `TimeoutDetector` has exactly one user — the raft heartbeat loop in `server/etcdserver/raft.go`.

### Construction: threshold = 2 heartbeat intervals

```go
// server/etcdserver/raft.go:143
// set up contention detectors for raft heartbeat message.
// expect to send a heartbeat within 2 heartbeat intervals.
td: contention.NewTimeoutDetector(2 * cfg.heartbeat),
```

Note the threshold is `2*heartbeat`, not `1*heartbeat` — a **"one-interval grace"** that leaves room for normal scheduling jitter so it doesn't alert at the slightest wobble. This threshold semantics is decided by the **caller**; the tool hardcodes no number, which is another sign of its generality.

### Observation: use "who the heartbeat goes to" as the event id

```go
// server/etcdserver/raft.go:385
if m.GetType() == raftpb.MsgHeartbeat {
	ok, exceed := r.td.Observe(m.GetTo())   // the follower's id is the event id
	if !ok {
		r.lg.Warn(
			"leader failed to send out heartbeat on time; took too long, "+
			"leader is overloaded likely from slow disk",
			zap.String("to", fmt.Sprintf("%x", m.GetTo())),
			zap.Duration("heartbeat-interval", r.heartbeat),
		)
	}
}
```

The key is using `m.GetTo()` (the target follower of the heartbeat) as the id, so each follower gets its own independent timeline. That lets the alert **pinpoint "which node's heartbeat is slow"** instead of vaguely saying "heartbeats are slow." That is the value of "bucketing by id."

### Reset: clear on leadership change

```go
// server/etcdserver/raft.go:206
rh.updateLeadership(newLeader)
r.td.Reset()   // role/leader changed; old timestamps are meaningless, start over
```

When this node stops being leader, or has just become leader, the recorded "last heartbeat time" is no longer meaningful (it could be long ago, or may never have sent one at all). `Reset()` clears the history and restarts the clock to avoid false alarms.

## 5. Run the demo

The accompanying demo reproduces the behavior above with an equivalent implementation. The second demo directly simulates the raft heartbeat scenario: after establishing a baseline for three followers (A/B/C), it keeps A and C on time while B stalls due to a "slow disk," and observes who gets named:

```
################ demo2: simulate raft heartbeat, per-follower bucketed detection ################
baseline: established first heartbeat time for followers [0xa 0xb 0xc]
next round: A/C on time, B stalled by slow disk ...
  heartbeat to follower 0xa: on time ✓
  heartbeat to follower 0xc: on time ✓
  heartbeat to follower 0xb: LATE ✗ (exceeded 32ms) → leader overloaded likely from slow disk

Conclusion: per-id bucketing pinpoints "which follower's heartbeat is slow."
```

Only B gets named; A/C are fine — precisely the effect of "bucketing by id" in a real alert.

## 6. Design highlights

| Technique | Explanation |
|------|------|
| **Monitoring distilled into a reusable tool** | Has no idea what a "heartbeat" is; only handles the abstract "periodic event with an id" — any fixed-interval thing can use it |
| **Bucketing by id** | Each follower is timed independently, pinpointing which node's heartbeat is slow |
| **Relative judgment + returns the overshoot** | Returns not just a bool but also `exceed` (how late), making logs quantifiable |
| **Zero background overhead** | No goroutine, no timer; computes once only when `Observe`d; state is a single map |
| **Threshold decided by the caller** | The `2*heartbeat` "one-interval grace" lives on the raft side; the tool hardcodes nothing |

Seventy lines, yet it wraps a general monitoring need cleanly. It reminds us that **"monitoring" itself can be abstracted into a small tool independent of any concrete business** — the next time you want to detect "whether some periodic thing is running late," recall this map + one-subtraction pattern.

## Key source locations

| Content | Location |
|------|------|
| `TimeoutDetector` implementation (Observe / Reset) | `pkg/contention/contention.go` |
| Package doc | `pkg/contention/doc.go` |
| `td *contention.TimeoutDetector` field | `server/etcdserver/raft.go:100` |
| Construction `NewTimeoutDetector(2*heartbeat)` | `raft.go:143` |
| `Observe(m.GetTo())` + warning log | `raft.go:385` |
| `td.Reset()` on leadership change | `raft.go:206` |
