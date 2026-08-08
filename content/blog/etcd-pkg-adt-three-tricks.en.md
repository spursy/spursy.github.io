+++
title = "Three Engineering Tricks in etcd pkg/adt's Interval Tree: Encode Overlap in Compare, Kill nil-checks with a sentinel, Reuse Compare via a Virtual Interval"
date = 2026-08-08
description = "The previous post explained how the interval tree's max field cuts overlap queries to O(log n). This one takes a different angle: from a self-contained runnable demo it extracts three tricks that don't change complexity yet make production code elegant and special-case-free — having Compare return 0 to mean 'overlap' rather than 'equal', using a sentinel node to absorb every nil boundary, and packing a subtree's reach into a virtual interval to reuse the same Compare during pruning. The three tricks interlock."
[taxonomies]
tags = ["etcd"]
[extra]
toc = true
+++

The previous post, *"Interval Tree: One max Field Cuts Interval-Overlap Queries to O(log n)"*, was about the **algorithm** — why caching one extra `max` lets you prune whole subtrees that cannot possibly overlap. This post takes a different angle: the **engineering craft**. For the very same `pkg/adt` interval tree, when turning it from textbook pseudocode into production code, etcd uses three "small but clever" tricks.

None of them changes the algorithmic complexity, yet they make the whole thing **elegant, special-case-free, and reusable**. The three tricks also interlock — the payoff of one is cashed in again by the next:

- **Trick 1**: `Interval.Compare` returns `0` to mean **overlap**, not **equal** — encoding a business meaning into the comparison semantics, so the tree's entire traversal framework can be reused verbatim.
- **Trick 2**: the `sentinel` node — one shared dummy absorbs every nil boundary, so the main logic has no nil-check branches at all.
- **Trick 3**: inside `visit`, use **the same Compare** plus `max` to build a "virtual big interval" for pruning — the dividend of Trick 1: even pruning needs no separate comparison logic.

<!-- more -->

> Full runnable source: [golang/etcd/pkg-adt-tricks/main.go](https://github.com/spursy/spursy.github.io/blob/main/golang/etcd/pkg-adt-tricks/main.go). Clone the repo and run `cd golang && go run ./etcd/pkg-adt-tricks`. This demo is carried by a plain BST with a sentinel (no red-black balancing), to focus on the three tricks themselves.

## 1. Trick 1: Compare returning 0 = "overlap", not "equal"

In a plain BST's three-way comparison, `0` means "the two keys are equal." The interval tree **hijacks** this meaning:

```
a.Compare(b) = -1  => a is entirely to the left of b
a.Compare(b) = +1  => a is entirely to the right of b
a.Compare(b) =  0  => a overlaps b   ← not equal!
```

First, the endpoint type's three-way compare — here `0` is still the honest "equal":

```go
type Comparable interface {
	Compare(c Comparable) int
}

type Int64 int64

func (v Int64) Compare(c Comparable) int {
	vc := c.(Int64)
	switch {
	case v < vc:
		return -1
	case v > vc:
		return 1
	default:
		return 0
	}
}
```

The key is the interval's `Compare`. It compares not "who is bigger" but "relative position", and squeezes "overlap" into the return value `0`:

```go
// Interval is the half-open interval [Begin, End).
type Interval struct {
	Begin Comparable
	End   Comparable
}

// Compare — ★ Trick 1: returns 0 when the two intervals overlap, not when equal.
func (ivl *Interval) Compare(c Comparable) int {
	ivl2 := c.(*Interval)
	ivbCmpBegin := ivl.Begin.Compare(ivl2.Begin) // my begin vs its begin
	ivbCmpEnd := ivl.Begin.Compare(ivl2.End)     // my begin vs its end
	iveCmpBegin := ivl.End.Compare(ivl2.Begin)   // my end   vs its begin

	// I'm entirely on its left: my begin < its begin, and my end <= its begin
	if ivbCmpBegin < 0 && iveCmpBegin <= 0 {
		return -1
	}
	// I'm entirely on its right: my begin >= its end
	if ivbCmpEnd >= 0 {
		return 1
	}
	// otherwise: overlap
	return 0
}
```

Run it and the special meaning of `0` shows up — note the last two lines: two **identical** intervals also return `0`:

```
=== Trick 1: Interval.Compare returns 0 = overlap, not equal ===
  [1,3).Compare([5,8)) = -1  (entirely left)
  [9,12).Compare([5,8)) = +1  (entirely right)
  [6,10).Compare([5,8)) = +0  (★overlap)
  [5,8).Compare([5,8)) = +0  (★overlap)    ← identical also counts as overlap, not "equal"
  [3,6).Compare([5,8)) = +0  (★overlap)
  → note [5,8) vs [5,8) also returns 0: here 0 means "overlap", not "equal".
```

**Why bother?** A plain BST's traversal/lookup logic is "`v<0` go left, `v>0` go right, `v==0` hit." After hijacking the semantics, that framework — **unchanged, not one line touched** — becomes an interval-overlap query: "hit" automatically turns into "found an overlap." No separate traversal for interval overlap is needed. This is the elegance of "encoding a business meaning into a comparison function's return value."

As a bonus, point queries ride this logic for free too: turn a single key `p` into a point interval `[p, p+1)`, and the overlap test answers "which intervals does this point fall inside":

```go
func point(p int64) Interval { return Interval{Int64(p), Int64(p + 1)} }
```

## 2. Trick 2: the sentinel node

Every "nil leaf" and "the root's parent" points to **one and the same** shared dummy node, `sentinel`. So the countless `if x == nil` checks in traversal/insertion/rotation all become `if x == sentinel`.

```go
type node struct {
	iv          Interval
	max         Comparable
	left, right *node
}

type Tree struct {
	root     *node
	sentinel *node // ★ Trick 2: all nil leaves share this one dummy
	visited  int   // demo only: counts nodes visited by the last query
}

func New() *Tree {
	s := &node{} // sentinel: a real object whose fields are readable/writable
	return &Tree{root: s, sentinel: s}
}
```

The difference: `sentinel` is a **real, field-readable/writable** object — it has a valid `max`, can be safely assigned via `x.left = ...`, so boundary nodes take part in the unified logic without special-casing. Look at insertion: every nil-check is `== t.sentinel`, and a new node's empty children hang off `t.sentinel` rather than `nil`:

```go
func (t *Tree) Insert(iv Interval) {
	z := &node{iv: iv, max: iv.End, left: t.sentinel, right: t.sentinel}
	if t.root == t.sentinel { // not t.root == nil
		t.root = z
		return
	}
	x := t.root
	for {
		if x.max.Compare(iv.End) < 0 {
			x.max = iv.End
		}
		if iv.Begin.Compare(x.iv.Begin) < 0 {
			if x.left == t.sentinel { // not x.left == nil
				x.left = z
				return
			}
			x = x.left
		} else {
			if x.right == t.sentinel {
				x.right = z
				return
			}
			x = x.right
		}
	}
}
```

The demo verifies "all empty children really are the same object":

```
=== Trick 2: sentinel — all nil leaves share one dummy object ===
  Do leaf [0,3)'s left/right both point to the shared sentinel: left=true right=true
  Are different leaves' empty children the same object: true
  → every nil-check is written as `x == t.sentinel`, no `x == nil` special case; boundaries unified.
```

This is a classic application of the **Null Object pattern** to a pointer-based data structure: use one "dummy object" to absorb all boundary cases so the trunk code stays branch-free. Red-black tree rotation and deletion have a great many branches, so shaving off half the `if x == nil` checks matters even more there.

## 3. Trick 3: build a "virtual big interval" with the same Compare + max to prune

Now cash in the dividends of Tricks 1 and 2 together. Here is the traversal function `visit` — it visits, in ascending Begin order, every node overlapping `iv`; returning `false` from `nv` stops early:

```go
func (t *Tree) visit(x *node, iv *Interval, nv func(*node) bool) bool {
	if x == t.sentinel { // Trick 2: sentinel as recursion terminator, no nil-check
		return true
	}
	t.visited++
	v := iv.Compare(&x.iv) // Trick 1: v==0 IS "overlap"
	switch {
	case v < 0:
		// query is to the left of this node → can only be in the left subtree
		return t.visit(x.left, iv, nv)
	case v > 0:
		// query is to the right → use virtual big interval [Begin, max] to test whether the subtree can still reach it
		maxiv := Interval{x.iv.Begin, x.max}
		if maxiv.Compare(iv) == 0 { // Trick 3: reuse the SAME Compare
			if !t.visit(x.left, iv, nv) {
				return false
			}
			return t.visit(x.right, iv, nv)
		}
		return true // even the virtual big interval doesn't overlap → prune the whole subtree
	default: // v == 0: this node overlaps the query. In-order: left → self → right
		if !t.visit(x.left, iv, nv) {
			return false
		}
		if !nv(x) {
			return false
		}
		return t.visit(x.right, iv, nv)
	}
}
```

All three tricks appear in this single function:

1. **Trick 2**: the first line `x == t.sentinel` is the recursion terminator — no `nil` check.
2. **Trick 1**: `v := iv.Compare(&x.iv)`, where `v==0` directly means "overlap hit"; the whole `switch` reuses the plain BST's "left/right/hit" three-way framework.
3. **Trick 3**: the pruning in the `v>0` branch.

Focus on Trick 3. When the query `iv` lies to the right of the current node (`v>0`), you cannot rashly go only right — **the left subtree might hide an interval whose end reaches far to the right**. How to decide "is there still a possible overlap in this subtree"? The interval tree builds a **virtual interval**:

```go
maxiv := Interval{x.iv.Begin, x.max}
```

`maxiv` stretches from "this node's begin" all the way to "the subtree's maximum end `max`" — it "represents" the entire range that the subtree rooted at `x` can cover. Then it **reuses the Compare from Trick 1**:

```go
if maxiv.Compare(iv) == 0 { // does the virtual big interval overlap the query?
```

- If the virtual big interval **overlaps** the query (`==0`) → the subtree may hold answers, so descend into both sides.
- If even this "farthest-reaching" virtual interval doesn't overlap → not a single interval in the subtree can overlap, so `return true` and **prune it whole**.

The beauty: the pruning decision writes **not one extra line of interval-comparison logic**; it packs "the subtree's spatial range" into an interval and hands it to the same `Compare`. The abstraction defined in Trick 1 pays off a second time here.

Run `Stab` (find all overlapping intervals) and see how much pruning saved:

```
=== Trick 3: visit uses the same Compare + max virtual-interval pruning ===
  Stab([8,18)) hits (ascending Begin): [8,9), [15,23), [17,19)
  nodes visited = 6 / 7 total (maxiv pruned the out-of-reach subtree)
  point Stab(point(6)=[6,7)) hits: [5,8)
  → pruning still uses that same Compare from Trick 1; no interval comparison logic was written anew.
```

`Stab` itself just wraps `visit` with a result-collecting visitor — note it too reuses the same traversal and contains no comparison logic of its own:

```go
func (t *Tree) Stab(iv Interval) []Interval {
	t.visited = 0
	var res []Interval
	t.visit(t.root, &iv, func(n *node) bool {
		res = append(res, n.iv)
		return true // return false to stop the traversal early
	})
	return res
}
```

### Aside: why isn't the right-subtree return negated?

The in-order traversal in the `default` branch deserves a note. It is the standard "return early if an intermediate step fails; on the last step just return the final value" pattern:

```go
if !t.visit(x.left, iv, nv) { // left subtree: more work follows, must bail out on failure
    return false
}
if !nv(x) {                    // this node: the right subtree still follows, must bail out on failure
    return false
}
return t.visit(x.right, iv, nv) // right subtree: the last step, its return value IS the final value — just return
```

The left subtree and this node are **not the last step**, so they use `if !... { return false }` to do the "bail out on failure" check, propagating the stop signal upward; the right subtree **is the last step** — nothing to protect afterward, so its return value equals the function's return value, and we just `return` without negation. The equivalent form is `if !right { return false }; return true`, written more crisply as one line.

## 4. How the three tricks interlock

```
Trick 1 (Compare 0 = overlap)
   │  defines the "interval comparison" abstraction
   ├─► lets the traversal reuse the BST "left/right/hit" three-way structure (the switch in visit)
   └─► lets pruning reuse the same Compare (maxiv.Compare(iv) in Trick 3)

Trick 2 (sentinel)
   └─► makes the boundaries in visit / Insert uniformly `== sentinel`; the trunk has no nil-check branch

Trick 3 (virtual big interval pruning)
   └─► stands on Trick 1's shoulders: treat "subtree range" as an interval, reuse Compare for free
```

In one sentence: **Trick 1 is the foundation** (encode the business meaning into the comparison function), Trick 2 keeps the trunk **clean** (no nil-checks), and Trick 3 is Trick 1's **interest** (pruning needs no new comparison).

## 5. Mapping to the real etcd source

This demo is a trimmed-down version; the real implementation is a red-black tree (CLRS ch. 13 & 14.3). But these three tricks appear, unchanged, in the etcd source:

| Trick | Demo counterpart | etcd source location |
|-------|------------------|----------------------|
| Compare returns 0 = overlap | `Interval.Compare` | `pkg/adt/interval_tree.go` (`Interval.Compare`) |
| sentinel absorbs nil | `Tree.sentinel` | `pkg/adt/interval_tree.go` (`sentinel` node definition and use) |
| virtual big interval + same Compare for pruning | `maxiv` in `visit` | `pkg/adt/interval_tree.go` (`visit` / `IntervalVisitor`) |

And the tree still serves the same "interval overlap/coverage" scenarios in etcd: transaction conflict detection (`v3rpc/key.go`), permission checks (`auth/range_perm_cache.go`), range watcher routing (`mvcc/watcher_group.go`), and proxy range cache invalidation (`grpcproxy/cache/store.go`). The previous post covered their shared `max` pruning; this one covers the three little knives that, in the very same code, make it writable and pleasant to read.

## 6. Summary

The interval tree's **algorithmic** core is the `max` field (previous post), but turning it into production code rests on three **engineering** tricks:

- **Encode a business meaning in the comparison function** (`0` = overlap): one abstraction, reused in both traversal and pruning.
- **Absorb boundaries with a dummy object** (sentinel): the Null Object pattern, so the trunk code has no nil-check branch.
- **Pack a range into an object and reuse an existing abstraction** (virtual big interval): pruning needs no new wheel.

These three don't belong only to the interval tree. "Let the return value carry business semantics," "eliminate special cases with a dummy object," and "pack state into an existing type to reuse its logic" — they transfer to any pointer-based data structure and any code that traverses and prunes.

## Key source index

| Content | Location |
|---------|----------|
| Interval tree impl (red-black + max + Compare + sentinel + visit) | `pkg/adt/interval_tree.go` |
| Endpoint types (Int64 / String / BytesAffine, empty string = +∞) | `pkg/adt/interval_tree.go` |
| Transaction conflict detection `checkIntervals` | `server/etcdserver/api/v3rpc/key.go:236` |
| Permission checks Contains/Intersects | `server/auth/range_perm_cache.go:88, 101` |
| range watcher routing | `server/storage/mvcc/watcher_group.go:153, 184, 190` |
| proxy range cache | `server/proxy/grpcproxy/cache/store.go:71` |
| This post's demo | `golang/etcd/pkg-adt-tricks/main.go` |
