+++
title = "etcd pkg/adt Interval Tree (Part 2): Maintaining max After Rotations, and updateMax's Early Termination"
date = 2026-08-06
description = "Part 1 covered the interval tree's query side — Insert plants max, Intersects/Stab prune with it. This part adds the maintenance side: red-black rotations reshuffle subtree membership, so the cached max must be recomputed. We dissect when rotateLeft/rotateRight call updateMax, why it must start from the lower node, and how the one line if old==m break compresses O(height) to O(1~2)."
[taxonomies]
tags = ["etcd"]
[extra]
toc = true
+++

[Part 1](/en/blog/etcd-pkg-adt-interval-tree/) covered the interval tree's **query side**: `Insert` plants `max` along the descent path, and `Intersects` / `Stab` use `max` to cut whole non-overlapping subtrees. But that demo was an **unbalanced plain BST** that deliberately omitted red-black rotations — leaving a loose end: real etcd self-balances via rotations, and **rotations change a node's subtree membership, so the cached `max` goes stale**.

This part adds the **maintenance side**: how to recompute `max` after a rotation, why it must start from the lower node, and how one unremarkable line of early termination in `updateMax` compresses a worst-case O(height) update to O(1~2). The two parts together make a complete interval tree.

<!-- more -->

## 1. The problem: why max must be recomputed after a rotation

Recall: an interval tree = a red-black tree + each node caches `max` (the largest End among all intervals in the subtree rooted at that node). The red-black tree self-balances via `rotateLeft` / `rotateRight`, and **a rotation changes the subtree membership of two nodes**:

```
       x                        y
      / \                      / \
     a   y     --left-rotate(x)--> x   c
        / \                      / \
       b   c                    a   b
```

Before the rotation `x`'s subtree = `{a, b, c, x}`; after it, `x`'s subtree is only `{a, b, x}`, while `y`'s subtree grows. Both nodes' coverage changed, so their cached `max` goes stale.

If not recomputed, later max-pruning in queries would decide based on a wrong `max` — **pruning a subtree it should have entered (missed hits), or entering one it should have pruned (wasted work)**. So the pointer surgery of a rotation must be immediately followed by a max recompute.

## 2. The rotation code: pointer surgery + one line of updateMax

The demo's `rotateLeft` (the mirror `rotateRight` swaps left/right):

```go
func (t *Tree) rotateLeft(x *node) {
	y := x.right
	if y == nil {
		return
	}
	x.right = y.left          // middle subtree b: from y's left, re-hung onto x's right
	if y.left != nil {
		y.left.parent = x
	}
	y.parent = x.parent
	if x.parent == nil {
		t.root = y
	} else if x == x.parent.left {
		x.parent.left = y
	} else {
		x.parent.right = y
	}
	y.left = x                // x drops to y's left
	x.parent = y
	// ★ Start from the lower x: fix x first, then y (now x's parent), then ancestors.
	t.updateMax(x)
}
```

The dozen lines before are standard red-black rotation pointer surgery (preserving ordering, only reshaping). **The only interval-tree-specific part is the last line `t.updateMax(x)`** — the extra step a plain red-black tree doesn't have but an interval tree must do. Matching real etcd's `interval_tree.go` `rotateLeft` (`:599`, `:606`), which even calls it once mid-rotation and once at the end.

## 3. Design 1: must start from the lower node x, order can't be reversed

That `updateMax(x)` line has an easily-missed subtlety: **why is the argument the lower `x`, not the risen `y`?**

Because after the rotation `x` drops below `y` (`x` becomes `y`'s child). The recompute must go **bottom-up**: `x` first, then `y`. The reason: **`y`'s max depends on `x`'s subtree max** — if you compute `y` first, it reads `x`'s **stale** max and gets it wrong.

The demo's `demo3` shows this plainly with an extreme case:

```
compute upper y first: y.max=20 (WRONG! used x's stale max=20, missed x's real 100)
compute lower x first (x.max=100) then y: y.max=100 (correct!)
→ conclusion: updateMax starts from the lower x and walks up, guaranteeing this order.
```

The scenario: `x`'s real subtree max End is 100, but its cached `max` is still the stale 20. Then:

- **Compute upper `y` first**: `y` reads `x.max=20` and computes itself as 20 — missing the real 100 in `x`'s subtree.
- **Compute lower `x` first**: fix `x.max` to 100, then `y` reads the correct 100.

And `updateMax` is implemented to **start from the given node and walk up the parent chain**, so passing the lower `x` naturally guarantees the "x, then y, then ancestors" order — no extra coordination needed.

## 4. Design 2: updateMax's early termination

`updateMax` itself:

```go
func (t *Tree) updateMax(x *node) {
	for x != nil {
		old := x.max
		m := x.end
		if x.left != nil && x.left.max > m {
			m = x.left.max
		}
		if x.right != nil && x.right.max > m {
			m = x.right.max
		}
		if old == m { // ★ early termination: this node's max unchanged → ancestors unchanged too
			break
		}
		x.max = m
		x = x.parent
	}
}
```

It recomputes up the parent chain. **The key optimization is `if old == m { break }`: once a node's recomputed max is unchanged, conclude that all its ancestors are unchanged too, and stop immediately.**

### Why "I'm unchanged" implies "all ancestors unchanged"

This rests on `max`'s **monotonicity** — the higher up (closer to root), the larger a node's max (ancestors cover larger subtrees; a parent's max is the max over its subtrees' maxes).

Let the End introduced by this change be `E`. Walking up to some node `A` and finding max unchanged means `E ≤ A.max` (E didn't break A). Now A's parent `P`:

```
P.max ≥ A.max ≥ E
```

If `E` couldn't break `A`, it certainly can't break the already-larger `P` — so P is unchanged. The same holds all the way to the root. **So we can safely break at A; the conclusion is locked in by transitivity, no need to verify each ancestor.**

Intuitively, the new End is like ripples from a stone dropped in water: it only affects nodes whose max was smaller than it, and once it meets the first ancestor whose `max ≥ it`, the ripple is absorbed and travels no further — **changes float up the tree but converge naturally**.

### demo2: small End walks 1 level, large End reaches the root

```
insert [1,2) (small end): updateMax visited only 1 node (early termination)
insert [16,40) (huge end): updateMax visited 3 nodes (propagated to root)
```

- Inserting `[1,2)`, E=2: the landing parent `[0,3)` already has max=3 covering it (2 ≤ 3), recompute unchanged → **only 1 level**. Above, `[5,8).max=9` and root `[15,23).max=30` cover it too, so logically no need to look.
- Inserting `[16,40)`, E=40: breaks every ancestor's max in turn, `break` never fires → **propagates to root, 3 levels**.

That is the value of early termination: it compresses the worst-case O(height) up-update to 1~2 levels in most inserts/rotations.

## 5. Verification: max stays correct after rotations

After each operation the demo verifies the cached value node-by-node by brute-force recomputing each subtree's real max. After left-rotating the root `[15,23)`:

```
after left rotate:
        [25,30) max=30
[19,20) max=30
                [17,19) max=19
        [15,23) max=23
                        [8,9) max=9
                [5,8) max=9
                        [0,3) max=3
  ✓ all node max correct
```

(Sideways print: right subtree on top, left on bottom, indentation = depth — tilt your head 90° left for a normal tree.)

Note that after the rotation `[15,23)`'s subtree shrank, and its `max` **correctly dropped from 30 to 23** — showing `updateMax` doesn't only "raise" values but can also compute a smaller correct value when a subtree shrinks (the recompute is an overall max, not a one-way accumulation). The whole tree reads `✓ all node max correct`.

## 6. This is a general augmented-tree pattern

The maintenance logic for interval-tree max applies to any **augmented tree** caching a "subtree aggregate" on nodes — as long as that aggregate satisfies "a parent's value is determined by its children's":

| Augmented field | Aggregation | Early termination |
|-----------------|-------------|-------------------|
| Interval tree `max` | subtree max End | stop when unchanged |
| Subtree sum `sum` | left + right + self | stop when delta is 0 |
| Subtree count `size` | left + right + 1 | usually changes every time, hard to stop early |

Interval-tree `max` is especially suited to early termination because "take the max" naturally satisfies "a small value is absorbed by a larger one." The pattern in one sentence:

> **An augmented red-black tree = a red-black tree + "after every structural change (insert/delete/rotate), recompute the augmented field starting from the affected lower node, walking up the parent chain, terminating early once the field stabilizes."**

## 7. Summary

- **Rotations necessarily stale max**: a rotation changes parent-child subtree membership; the two rotated nodes' coverage changes, so cached max must be recomputed.
- **Recompute starts from the lower node**: `updateMax(x)` takes the lower `x` and walks up the parent chain, naturally guaranteeing "child before parent" — computing the parent first would read the child's stale value and err.
- **Early termination is the finishing touch**: `if old == m { break }` exploits max's upward monotonicity — once the new End is covered by some ancestor, propagation stops, compressing O(height) to O(1~2).

Together with [Part 1](/en/blog/etcd-pkg-adt-interval-tree/)'s query side, we can now fully answer why the interval tree is steadily O(log n): **red-black rotations keep the height from degenerating → updateMax (with early termination) maintains max efficiently after rotations → queries prune with max**. All three interlock; none can be dropped.

## Key source locations

| Content | Location |
|---------|----------|
| `updateMax` (up-recompute + early termination `if oldmax==max break`) | `pkg/adt/interval_tree.go:130` |
| `rotateLeft` (updateMax mid-rotation and at the end) | `interval_tree.go:585` (`:599`, `:606`) |
| `rotateRight` (mirror) | `interval_tree.go:630` (`:644`, `:651`) |
| `replaceParent` (updateMax when reconnecting parent) | `interval_tree.go:655` (`:665`) |
| `Insert` (`y.updateMax` after insert) | `interval_tree.go:479` |
| `Delete` (`updateMax` after delete) | `interval_tree.go:303`, `:307` |
