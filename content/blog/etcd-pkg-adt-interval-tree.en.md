+++
title = "etcd pkg/adt Interval Tree: One max Field Cuts Interval-Overlap Queries to O(log n)"
date = 2026-08-05
description = "Starting from the binary search tree, this post explains how the interval tree's one extra max field cached on a red-black tree lets it prune whole subtrees that cannot possibly overlap, then dissects Insert / Intersects / Stab from a self-contained runnable demo, and finally its real uses in etcd's transaction conflicts, permission checks, and watcher routing."
[taxonomies]
tags = ["etcd"]
[extra]
toc = true
+++

The earlier posts covered etcd's concurrency primitives (`wait` / `WaitTime` / `notify`), its lock-free ID (`idutil`), and page-aligned writes (`PageWriter`). This one turns to data structures: the **interval tree** in `pkg/adt` — a red-black tree where each node caches one extra `max` field, which lets it answer "which intervals overlap `[begin, end)`" in O(log n).

Anywhere in etcd that needs "given a pile of key ranges, quickly decide whether some key/range overlaps or is covered by them," this is the foundation: transaction conflict detection, permission checks, range watcher routing, proxy cache invalidation.

To explain it we start from the ordering property of the binary search tree, then look at the one field the interval tree adds on top. Then we dissect the implementation from a self-contained runnable demo, and finally its real uses in etcd.

<!-- more -->

## 1. Starting from the binary search tree

The core of a **binary search tree (BST)** is one ordering property: for any node, **all keys in the left subtree < node key < all keys in the right subtree**. This holds recursively for every subtree.

```
        15
       /  \
      6    23
     / \   / \
    3   8 19  25
```

Because it is ordered, lookup is like a number-guessing game — each level down eliminates half, so it is O(tree height). When balanced, height ≈ log n.

But a BST excels at **point** queries (is key=8 present?). Many scenarios instead need an **interval-overlap** query: given `[begin, end)`, which intervals in the tree overlap it? The naive approach scans all intervals in O(n). The interval tree cuts it to O(log n) — thanks to one field added on top of the BST's ordered skeleton.

## 2. Core design: red-black tree + augmented max field

The interval tree is built in three steps:

**Step 1: pick the sort key.** Use each interval's **left endpoint Begin** as the BST key (ties broken by End). So it is first a tree ordered by left endpoint that can self-balance.

**Step 2: cache max on every node.** `max = the largest End of all intervals in the subtree rooted at this node`, i.e. `max = max(this node's End, left.max, right.max)`. Maintained up the path after insert/delete/rotate.

**Step 3: use max to prune queries** — the cleverest part of the whole structure. When searching for overlaps, if some subtree's `max <= query.Begin`, then **every** interval in that subtree has a right endpoint ≤ the query's left endpoint, all lie to the left of the query, cannot overlap → **the whole subtree is pruned at once**.

> Intuition: the BST key tells you "which way to go for larger/smaller"; the interval tree's max additionally tells you "which side cannot possibly overlap — don't waste time going in."

The layering builds up like this:

```
Binary tree → Binary search tree (BST, adds ordering)
            → Red-black tree (BST + self-balancing, guarantees worst-case O(log n))
              → Interval tree (red-black tree + max field, supports overlap queries)
```

The real etcd implementation is a red-black tree (CLRS ch. 13) + interval-tree augmentation (ch. 14.3), guaranteeing worst-case O(log n), using a sentinel node to unify nil boundaries and a `Comparable` interface to generalize endpoint types. The demo below uses a **plain BST** to focus on the principle — the correctness of max-pruning is independent of red-black balancing; balancing only affects worst-case height.

## 3. A minimal runnable implementation

> Full runnable source: [golang/etcd/pkg-adt/main.go](https://github.com/spursy/spursy.github.io/blob/main/golang/etcd/pkg-adt/main.go). After cloning, run `cd golang && go run ./etcd/pkg-adt`.

First the interval and node definitions. An interval is half-open `[Begin, End)`, and the overlap test is the classic `b1 < e2 && e1 > b2`:

```go
type Interval struct {
	Begin int64
	End   int64
}

// two half-open intervals overlap: b1 < e2 and e1 > b2
func (iv Interval) overlaps(o Interval) bool {
	return iv.Begin < o.End && iv.End > o.Begin
}

type node struct {
	iv          Interval
	val         any
	max         int64 // ★ largest End among all intervals in the subtree
	left, right *node
}
```

### Insert: BST insert + maintain max along the way

```go
func (t *IntervalTree) Insert(iv Interval, val any) {
	t.count++
	if t.root == nil {
		t.root = &node{iv: iv, val: val, max: iv.End}
		return
	}
	x := t.root
	for {
		if iv.End > x.max { // ★ update ancestors' max along the path
			x.max = iv.End
		}
		if iv.Begin < x.iv.Begin {
			if x.left == nil {
				x.left = &node{iv: iv, val: val, max: iv.End}
				return
			}
			x = x.left
		} else { // equal Begin also goes right: fix one direction, lose no data
			if x.right == nil {
				x.right = &node{iv: iv, val: val, max: iv.End}
				return
			}
			x = x.right
		}
	}
}
```

Two key points:

1. **The line `if iv.End > x.max` is the soul of the interval tree.** The new interval becomes a subtree member of every node on the descent path, so that path covers exactly all the ancestors whose max may need raising — one descent maintains them all, no backtracking.
2. **The `else` branch covers both "greater than" and "equal to."** The `if` condition is strict `<`; negated, `else` covers `>=`. Two intervals with equal Begin (e.g. `[5,8)` and `[5,20)`) are conventionally both placed in the right subtree — fixing one direction loses no data and preserves ordering. This is the standard BST way to handle duplicate keys.

### Intersects: does any overlap exist (single path, O(log n))

```go
func (t *IntervalTree) Intersects(iv Interval) bool {
	x := t.root
	for x != nil {
		if x.iv.overlaps(iv) {
			return true
		}
		// ★ max-pruning: go left only if left.max > iv.Begin
		if x.left != nil && x.left.max > iv.Begin {
			x = x.left
		} else {
			x = x.right // the whole left subtree is pruned
		}
	}
	return false
}
```

It walks **a single path from root downward**, not the whole tree — that is why it is O(log n) rather than O(n). At each node it first checks whether it overlaps (return on hit), otherwise uses the left subtree's max to decide direction:

- `x.left.max > iv.Begin` → the left subtree might overlap, go left.
- Otherwise → every interval in the left subtree has its right endpoint ≤ the query's left endpoint, cannot overlap, **prune the whole left subtree**, turn right.

Correctness is guaranteed by interval-tree theory (CLRS 14.3): the abandoned half definitely holds no answer, so a single-path descent yields the correct existence result.

### Stab: find all overlaps (in-order traversal + two-way pruning)

`Intersects` stops at one hit; `Stab` must find **all**, so it cannot walk a single path — it recurses with pruning:

```go
func (t *IntervalTree) stab(x *node, iv Interval, res *[]Interval) {
	if x == nil {
		return
	}
	// ★ prune 1: subtree's max right endpoint <= query left → no overlap in subtree
	if x.max <= iv.Begin {
		return
	}
	// recurse left first (keeps results sorted by Begin)
	t.stab(x.left, iv, res)
	if x.iv.overlaps(iv) {
		*res = append(*res, x.iv)
	}
	// ★ prune 2: if query end <= this node's Begin, right subtree's Begins are even larger
	if iv.End > x.iv.Begin {
		t.stab(x.right, iv, res)
	}
}
```

It cleverly prunes in **two directions**:

| Prune | Condition | What it cuts |
|-------|-----------|--------------|
| **max prune** | `x.max <= iv.Begin` | the whole subtree (its max right endpoint can't reach the query's left) |
| **Begin prune** | `iv.End <= x.iv.Begin` | the whole right subtree (its Begins are all too large) |

The order **left → node → right** is exactly in-order traversal, which on a BST naturally yields results **sorted by Begin** — no extra sort needed. Complexity is O(log n + k), where k is the number of hits.

## 4. Horizontal printing: see the shape and max at a glance

The demo's `Print` uses a sideways layout (right subtree on top, left on bottom, indentation for depth — tilt your head 90° left and it's a normal tree). Output after inserting 9 intervals:

```
                [25,30) max=30
        [19,20) max=30
                [17,19) max=21
                        [16,21) max=21
[15,23) max=30
                [8,9) max=10
                        [6,10) max=10
        [5,8) max=10
                [0,3) max=3
```

The flush-left `[15,23)` is the root; the block above is the right subtree, below is the left. Restored to normal orientation:

```
                [15,23) max=30
              /            \
          [5,8) max=10      [19,20) max=30
          /     \           /        \
    [0,3)      [8,9)    [17,19)     [25,30)
    max=3      max=10    max=21      max=30
                /          /
            [6,10)      [16,21)
            max=10       max=21
```

Verify max node by node (= largest End in the subtree): `[8,9)`'s subtree contains `[6,10)`, so max=10; `[5,8)`'s subtree contains `{8,3,9,10}`, max=10; root `[15,23)` covers the whole tree, max=30. All correct. This sideways print needs a head tilt, but indentation uniquely determines parent-child relations — **unambiguous** — a common technique for drawing trees in text.

## 5. How much does max-pruning save

The demo's `demo3` shows the pruning value directly. Query `[1,2)` lands on the far left:

```
query [1,2) -> [[0 3)]; visited only 3 of 9 nodes thanks to max-pruning
```

Only 3 of 9 nodes visited — the right-side subtrees with large Begins are all cut whole by `x.max <= iv.Begin`. That is the value of the one max field: **it steadily compresses "find overlaps" from O(n) to O(log n)**.

⚠️ One caveat: this demo is an **unbalanced plain BST**. Inserting in increasing Begin order (1,2,3,4...) degenerates the tree into a right-leaning chain (a linked list), pruning fails, and it falls back to O(n). This precisely confirms why real etcd **must use a red-black tree for balance** — the demo deliberately inserts in "median-first" order so there are left and right subtrees to demonstrate pruning.

## 6. Real uses in etcd

The interval tree serves four things in etcd, all "interval overlap/coverage" queries:

| Scenario | Location | What's stored | What's queried |
|----------|----------|---------------|----------------|
| **Txn conflict detection** | `v3rpc/key.go:236` | all delete key ranges in a Txn | does a put's key fall into some delete range |
| **Permission check** | `auth/range_perm_cache.go` | a user's granted read/write key ranges | is the requested key/range covered by a grant |
| **Range watcher routing** | `mvcc/watcher_group.go` | all watchers on a range | which range watchers does a changed key hit |
| **Proxy range cache** | `grpcproxy/cache/store.go` | cached range responses | which overlapping caches must a write invalidate |

### Scenario 1: Txn conflict detection (the real use of Intersects)

`checkIntervals` (`key.go:236`) checks whether the put/delete in a Txn conflict:

```go
dels := adt.NewIntervalTree()
// ① insert every DeleteRange in this layer as an interval
for _, req := range reqs {
	iv := adt.NewStringAffineInterval(dreq.Key, dreq.RangeEnd)
	dels.Insert(iv, struct{}{})
}
// ② check whether each put's key falls into any delete range
if dels.Intersects(adt.NewStringAffinePoint(k)) {
	return rpctypes.ErrGRPCDuplicateKey // conflict!
}
```

`NewStringAffinePoint(k)` turns a single key into a "point interval," reusing the same overlap test. This is the real use of `Intersects` in the demo: **not caring what it overlaps, only whether it overlaps**.

### Scenario 2: Permission check (Intersects vs Contains)

`range_perm_cache.go` builds two interval trees `readPerms` / `writePerms` per user and inserts the granted key ranges. Checks come in two semantics:

```go
// request is a range: the grant must FULLY COVER it → use Contains
cachedPerms.readPerms.Contains(ivl)

// request is a single key: any grant interval CONTAINING the point suffices → use Intersects
cachedPerms.readPerms.Intersects(pt)
```

- **`Intersects`**: any intersection suffices (is a point hit by any granted range).
- **`Contains`**: the query interval must be **fully covered** (a range request cannot exceed grant boundaries).

Here the `BytesAffine` endpoint type matters: an empty `[]byte{}` is treated as **+∞**, expressing an open grant range "from some key to the end."

### Scenarios 3 & 4: watcher routing and cache invalidation

`watcher_group.go` stores all range watchers in an interval tree; each key write must quickly find "all range watchers covering this key," reducing the match from O(number of watchers) to O(log n). The proxy's `cache/store.go` uses it to cache range responses and, on a write, invalidate all overlapping cached ranges — conceptually the demo's `Stab` (find all overlaps at once).

## 7. Why not a plain map / sorted list

- A plain **map** only matches a single exact key; it cannot answer "interval overlap."
- A **sorted list** can binary-search a point, but a query interval may span multiple stored intervals, still needing an O(k) scan.
- An **interval tree** uses each node's cached `max` to cut whole non-overlapping subtrees, steadily compressing overlap queries to O(log n).

## 8. Summary

An interval tree = **the BST's ordered skeleton + the red-black tree's self-balancing + one augmented max field**. None can be dropped:

- **Ordering** (by Begin): decides which way to search.
- **Red-black balance**: guarantees worst-case height O(log n), avoiding degeneration into a chain.
- **max field**: prunes whole non-overlapping subtrees during queries — the key to O(log n) overlap queries.

Understanding the chain "BST ordering → max caches the subtree's largest right endpoint → prune by max during queries → red-black tree prevents degeneration" gives you the shared key behind etcd's transaction conflicts, permission checks, and watcher routing.

## Key source locations

| Content | Location |
|---------|----------|
| Interval tree impl (red-black + max + Intersects/Contains/Find) | `pkg/adt/interval_tree.go` |
| Endpoint types (Int64 / String / BytesAffine, empty = +∞) | `pkg/adt/interval_tree.go` |
| Txn conflict detection `checkIntervals` | `server/etcdserver/api/v3rpc/key.go:236` |
| Permission tree build `getMergedPerms` | `server/auth/range_perm_cache.go:30` |
| Permission check Contains/Intersects | `server/auth/range_perm_cache.go:88, 101` |
| Range watcher routing | `server/storage/mvcc/watcher_group.go:153, 184, 190` |
| Proxy range cache | `server/proxy/grpcproxy/cache/store.go:71` |
