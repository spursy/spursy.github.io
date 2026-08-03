+++
title = "etcd pkg/idutil: Lock-Free, Coordination-Free Unique IDs"
date = 2026-08-03
description = "Starting from a self-contained runnable demo, this post explains how etcd's idutil generates lock-free, cross-node-unique, restart-unique IDs with a bit layout plus a single atomic increment plus controlled overflow, and how it closes the loop with pkg/wait on the write path."
[taxonomies]
tags = ["etcd"]
[extra]
toc = true
+++

The previous posts covered the `pkg/wait` family — `Wait` that wakes a single waiter precisely by ID, `WaitTime` that releases in batches by threshold, and `notify` that broadcasts. They all share one premise: **every write request needs a unique ID first**, so it can register on `wait.Register(id)` and be woken precisely once apply completes.

Where does that ID come from? It is `pkg/idutil`, the star of this post. It is only 40 lines, yet it satisfies a seemingly contradictory list of requirements — **lock-free, coordination-free, cross-node-unique, restart-unique, monotonic, and blazing fast** — with **a single uint64 bit layout plus one atomic increment**. This post starts from a self-contained runnable demo, then maps it back to the write path.

<!-- more -->

## 1. What idutil solves

Every etcd write request needs a unique ID, under harsh constraints:

- **Within a node**: monotonic, non-repeating, extremely fast to generate — it sits on the write hot path and **must not grab a global counter behind a mutex**.
- **Across nodes**: unique by construction — each member issues IDs on its own, with **no coordination** (no central issuer, no RPC).
- **After a restart**: never equal to an ID issued before the restart — otherwise a stale registration left on `wait` would be mistakenly woken by a new ID.

In one sentence: a **locally generated, lock-free, coordination-free, yet globally unique** number. idutil's answer is to split "uniqueness" into three orthogonal dimensions, each packed into a distinct bit region of a 64-bit integer.

## 2. The 64-bit layout

```
| prefix   | suffix                |
| 2 bytes  | 5 bytes    | 1 byte   |
| memberID | timestamp  | cnt      |
 high 2 bytes = memberID (node isolation)
 middle 5 bytes = boot-time millisecond timestamp (restart isolation)
 low 1 byte = counter (per-node increment)
```

Each segment has one job:

1. **memberID prefix (high 2B)** → solves **cross-node uniqueness**: different nodes have different prefixes, so their value ranges never overlap.
2. **millisecond timestamp (middle 5B)** → solves **restart uniqueness**: after a restart time has advanced, so the starting point differs. 5 bytes of milliseconds ≈ 2⁴⁰ ms ≈ a **35-year** window, plenty for one process's lifetime.
3. **counter (low 1B)** → solves **per-node increment**: each `Next()` just does +1.

## 3. A minimal runnable implementation

> Full runnable source: [golang/etcd/pkg-idutil/main.go](https://github.com/spursy/spursy.github.io/blob/main/golang/etcd/pkg-idutil/main.go). Clone the repo, then `cd golang && go run ./etcd/pkg-idutil`.

Below is a dependency-free equivalent you can `go run` directly; the logic matches `id.go` in `go.etcd.io/etcd/pkg/v3/idutil`. The core is just two methods, `NewGenerator` and `Next`.

```go
const (
	tsLen     = 5 * 8          // timestamp: 5 bytes = 40 bits
	cntLen    = 8              // counter: 1 byte = 8 bits
	suffixLen = tsLen + cntLen // suffix: 6 bytes = 48 bits
)

type Generator struct {
	prefix uint64 // high 2 bytes: memberID << 48, constant after boot
	suffix uint64 // low 6 bytes: timestamp+cnt, atomically incremented by Next()
}

func NewGenerator(memberID uint16, now time.Time) *Generator {
	prefix := uint64(memberID) << suffixLen
	unixMilli := uint64(now.UnixNano()) / uint64(time.Millisecond/time.Nanosecond)
	// Take the low 40 bits of the ms timestamp, shift left 8 to make room for the counter.
	suffix := lowbit(unixMilli, tsLen) << cntLen
	return &Generator{prefix: prefix, suffix: suffix}
}

// Next generates the next unique ID: atomically +1 the whole suffix (a full
// counter carries into the timestamp bits), then OR the prefix onto the low 48 bits. Lock-free.
func (g *Generator) Next() uint64 {
	suffix := atomic.AddUint64(&g.suffix, 1)
	id := g.prefix | lowbit(suffix, suffixLen)
	return id
}

// lowbit takes the low n bits of x.
func lowbit(x uint64, n uint) uint64 {
	return x & (math.MaxUint64 >> (64 - n))
}
```

After `NewGenerator(0x12, ...0x3456ms)`, the first `Next()` yields `0x12000000345601` — which decomposes exactly into `memberID=0x12 | timestamp=0x3456 | cnt=0x01`.

## 4. Key design points

### 1. A bit layout that splits composite uniqueness into orthogonal dimensions

`prefix := uint64(memberID) << suffixLen` pushes the 16-bit memberID up to the top 2 bytes; `suffix := lowbit(unixMilli, tsLen) << cntLen` shifts the low 40 bits of the timestamp left by 8 to make room for the counter in the lowest byte. The three segments are packed non-overlapping into one uint64, each independently guaranteeing one dimension of uniqueness. Compared with designing a complex algorithm, "**pack [node][time][count] into precise bit regions with shifts and masks**" is far clearer.

### 2. A single atomic.AddUint64 instead of a lock

`Next()` advances via `atomic.AddUint64(&g.suffix, 1)` — a single-CPU-instruction atomic increment. `prefix` is constant after boot and never changes, so it needs no protection; only `suffix` needs synchronization, and it is compressed into one atomic add, with **no mutex contention or context switches**. That is why it dares to sit on the write hot path. Compressing "read-modify-write" into one atomic op is a classic lock-free technique.

### 3. The counter "deliberately" overflows and carries into the timestamp bits (the neatest trick)

The counter is only 1 byte (0–255), so at a glance it seems to allow only 256 IDs per millisecond — nowhere near enough. But `Next()`'s increment is `AddUint64` over the **entire 6-byte suffix**, not just the low 8 bits. So once the counter fills past 256, the carry "crawls" into the timestamp bytes:

```
... suffix = 1000:255   (timestamp=1000, cnt=255)
Next()  ->  1001:000    ← counter full, carry pushes into the timestamp segment
```

The designer **deliberately** allows this, effectively borrowing the counter space from 2⁸ up to the entire suffix's 2⁴⁸.

**Why doesn't uniqueness break?** Because the timestamp the carry consumes is a "future millisecond value" — as long as real time hasn't reached there, those values have never been used. And etcd throughput is ≪ 256 req/ms (250k req/s ≈ 0.25 req/ms, three orders of magnitude lower), so the carry's climb can never catch up with real time's advance; the borrowed future space (~35 years) is never exhausted. Meanwhile `lowbit(suffix, suffixLen)` keeps only the low 48 bits, so **no matter how far the carry climbs it cannot cross the 48-bit boundary and pollute the high-2-byte memberID** — cross-node isolation is untouched.

In one sentence: **a "bit layout plus controlled overflow" stretches the lock-free increment window from 2⁸ to 2⁴⁸, using "throughput far below the rate of time's advance" as the premise for uniqueness.** Simpler than Snowflake — it drops all the complexity of clock-rollback detection and sequence-wraparound blocking.

### 4. lowbit: a one-line bit mask

`lowbit(x, n)` = `x & (math.MaxUint64 >> (64-n))`. Right-shift 64 ones by `64-n` to get a mask with the low n bits set, then `&` away x's high bits. Used in two places: truncating the timestamp to 40 bits at construction, and clamping the suffix within 48 bits in `Next()`. The latter is exactly the gate that keeps the carry from overflowing into the prefix.

### Pitfalls

- **Uniqueness is a "probabilistic + premised" guarantee, not a mathematical absolute**: the premise is throughput ≪ 256 req/ms and process lifetime < 35 years. Reality never reaches the tipping point.
- **Relies on the local clock not rolling back much after a restart**: if the machine time is dragged back significantly, a restart may land back in an old time segment. In practice restart intervals > 1ms plus memberID isolation make this a non-issue.
- **The result is a uint64**: some scenarios (like LeaseGrant needing a positive int64) further apply `& ((1<<63)-1)` to strip the sign bit.

## 5. How etcd actually uses it

In one sentence: wherever etcd needs a **locally generated, cross-node-unique, lock-free, blazing-fast** number, it uses `idutil.Generator`. It is the single field named `reqIDGen` in `EtcdServer`.

### Initialization: two args map to two uniqueness guarantees

`server/etcdserver/server.go:266` defines the field, `:327` initializes it:

```go
reqIDGen: idutil.NewGenerator(uint16(b.cluster.nodeID), time.Now()),
```

`nodeID` → prefix → cross-node uniqueness; `time.Now()` → timestamp → restart uniqueness.

### Core scenario: the write request's "issue → register → wake" loop

`processInternalRaftRequestOnce` (`v3_server.go:1058`) stitches idutil and `pkg/wait` together:

```go
r.Header = &pb.RequestHeader{
    ID: s.reqIDGen.Next(),   // 1) idutil issues a unique number
}
// ...
id := r.ID
if id == 0 {
    id = r.Header.ID
}
ch := s.w.Register(id)       // 2) register on wait with that number, get a wait channel

err = s.r.Propose(cctx, data) // 3) hand the request to raft
// ...
select {
case x := <-ch:              // 4) after apply, wait.Trigger(id) wakes this request precisely
    return x.(*apply2.Result), nil
case <-cctx.Done():
    s.w.Trigger(id, nil)     //    timeout: self-Trigger to GC, avoid leaks
    return nil, ...
}
```

idutil's value shows here: `wait` is one-to-one by ID and requires **globally unique keys generated extremely fast**.

- If two concurrent write requests got the same ID, apply results would cross wires (waking the wrong request) — idutil's atomic increment guarantees no duplicates within a node.
- This path is on the write hot path, every request issues a number, so **no lock is allowed** — one `atomic.AddUint64` does it.
- Across nodes, even applying independently, request IDs in the log don't collide (different prefixes), aiding global tracing.

### Other consumers

| Consumer | Location | Purpose |
|----------|----------|---------|
| Internal raft request ID | `v3_server.go:1067` | every write request `r.Header.ID = reqIDGen.Next()` |
| LeaseGrant lease id | `v3_server.go:438` | auto-generated when the user gives none, `& ((1<<63)-1)` to keep it positive |
| ConfChange id | `server.go:1745` | id of a config-change entry |

## 6. Why not the alternatives

| Approach | Problem |
|----------|---------|
| global `mutex + count++` | lock contention on the write hot path; restarting from 0 collides with old IDs |
| UUID | 16 bytes is too large to stuff into raft headers and logs; not monotonic, poor for observability |
| Snowflake | similar idea but heavier: must handle clock rollback and sequence-wraparound blocking; idutil drops all that via controlled overflow |
| central ID issuer | introduces RPC and a single point, betraying the "coordination-free" goal |

idutil satisfies "lock-free, coordination-free, cross-node-unique, restart-unique, monotonic, observable" all at once with **a single uint64 bit layout plus one atomic increment** — which is why it is irreplaceable on etcd's write path. Together with `wait` / `WaitTime` / `notify` from the earlier posts, it forms a complete request-handling chain: **idutil issues → wait registers → raft commits → apply advances (WaitTime) → Trigger wakes precisely**.

## Source location index

| Item | Location |
|------|----------|
| Generator impl (NewGenerator / Next / lowbit / layout comment) | `pkg/idutil/id.go` |
| `reqIDGen` field | `server/etcdserver/server.go:266` |
| init `NewGenerator(nodeID, time.Now())` | `server.go:327` |
| `NextRequestID` wrapper | `server/etcdserver/interface.go:34` |
| write-request issue + hand to wait | `v3_server.go:1067`, `:1106` |
| LeaseGrant auto id | `v3_server.go:438` |
| ConfChange id | `server.go:1745` |
