// Package main 演示 etcd pkg/adt 的「区间树（Interval Tree）」核心设计原理。
//
// ============================================================================
// 一、区间树是什么 / 解决什么问题
// ============================================================================
//
// 普通有序树擅长「单点」查询（key=5 在不在），但很多场景要回答「区间重叠」查询：
// 给一个区间 [begin, end)，树里有哪些区间和它有交集？
//   - etcd 事务判断两个 key range 是否冲突
//   - 权限校验：授权范围是否覆盖请求的 key
//   - 网络：IP 段 / 端口段是否与已有规则重叠
//
// 朴素做法遍历所有区间 O(n)，区间树把它降到 O(log n)。
//
// ============================================================================
// 二、核心设计原理：红黑树 + 增强字段 max
// ============================================================================
//
//  1. 按区间左端点 Begin 排序（作为 BST/红黑树的 key），左端点相同再比右端点 End。
//     -> 首先是一棵合法的、按左端点有序、可自平衡的树。
//
//  2. ★ 每个节点缓存 max = 「以本节点为根的整棵子树中，所有区间 End 的最大值」。
//     max = 三者取最大(本节点 End, 左子树 max, 右子树 max)，
//     插入/删除/旋转后沿路径向上更新。
//
//  3. ★ max 用于查询剪枝（整个数据结构最巧妙处）：
//     查重叠时，若 左子树.max <= 查询.Begin，说明左子树里每个区间的右端点都
//     <= 查询左端点 -> 全部落在查询区间左侧、不可能重叠 -> 整棵左子树一次性剪掉。
//     直觉：BST 的 key 告诉你往哪边找更大/更小；区间树的 max 额外告诉你
//     哪边根本不可能有重叠、别浪费时间进去。
//
//  4. 重叠判断编码成三分比较：Interval.Compare 返回 0 表示「重叠」(而非相等)，
//     -1/1 表示完全在左/右，从而复用树的遍历框架。
//
// 工程细节（真实 etcd 实现，本 demo 为聚焦原理做了简化）：
//   - 真实实现是红黑树（CLRS 第13章）+ 区间树（第14.3章），保证最坏 O(log n)；
//   - 用 sentinel 哨兵节点统一 nil 边界，简化旋转/删除;
//   - 用 Comparable 接口泛化端点类型（Int64/String/BytesAffine，空 []byte 视为 +∞）。
//
// 本 demo 用普通 BST 承载：max 剪枝的正确性与是否红黑平衡无关，
// 平衡只影响最坏树高，不影响本 demo 要演示的「重叠查询 + max 剪枝」原理。
//
// ============================================================================
package main

import "fmt"

// Interval 表示半开区间 [Begin, End)。
type Interval struct {
	Begin int64
	End   int64
}

// overlaps 报告两个半开区间是否有交集：b1 < e2 且 e1 > b2。
func (iv Interval) overlaps(o Interval) bool {
	return iv.Begin < o.End && iv.End > o.Begin
}

type node struct {
	iv          Interval
	val         any
	max         int64 // 子树内所有区间 End 的最大值
	left, right *node
}

// IntervalTree 是一棵（简化的、未做红黑平衡的）区间树。
type IntervalTree struct {
	root  *node
	count int
	// visited 统计上一次查询访问过的节点数，用于直观展示 max 剪枝效果。
	visited int
}

func NewIntervalTree() *IntervalTree { return &IntervalTree{} }

func (t *IntervalTree) Len() int { return t.count }

// Insert 插入一个区间。按 Begin 排序，插入路径上向下维护 max。
func (t *IntervalTree) Insert(iv Interval, val any) {
	t.count++
	if t.root == nil {
		t.root = &node{iv: iv, val: val, max: iv.End}
		return
	}
	x := t.root
	for {
		if iv.End > x.max { // 沿途更新祖先的 max
			x.max = iv.End
		}
		if iv.Begin < x.iv.Begin {
			if x.left == nil {
				x.left = &node{iv: iv, val: val, max: iv.End}
				return
			}
			x = x.left
		} else {
			if x.right == nil {
				x.right = &node{iv: iv, val: val, max: iv.End}
				return
			}
			x = x.right
		}
	}
}

// Intersects 报告是否存在任一区间与 iv 重叠（找到一个即返回）。O(log n)。
func (t *IntervalTree) Intersects(iv Interval) bool {
	t.visited = 0
	x := t.root
	for x != nil {
		t.visited++
		if x.iv.overlaps(iv) {
			return true
		}
		// ★ max 剪枝：左子树 max > iv.Begin 时左子树才「可能」有重叠。
		if x.left != nil && x.left.max > iv.Begin {
			x = x.left
		} else {
			x = x.right // 整棵左子树被剪掉
		}
	}
	return false
}

// Print 把树竖着打印出来（最直观的理解方式）。
// 每个节点显示 [Begin,End) max=子树最大右端点。右子树在上、左子树在下，
// 把头向左歪 90 度看就是一棵正常的树。
func (t *IntervalTree) Print() {
	fmt.Println("tree structure (每个节点: [Begin,End) max=子树内最大End):")
	t.print(t.root, 0)
}

func (t *IntervalTree) print(x *node, depth int) {
	if x == nil {
		return
	}
	t.print(x.right, depth+1) // 右子树先打印（在上方）
	indent := ""
	for i := 0; i < depth; i++ {
		indent += "        "
	}
	fmt.Printf("%s[%d,%d) max=%d\n", indent, x.iv.Begin, x.iv.End, x.max)
	t.print(x.left, depth+1) // 左子树后打印（在下方）
}

// IntersectsVerbose 和 Intersects 逻辑完全一样，只是把查找路径上每一步的
// 「决策理由」打印出来，方便你对照树结构看懂算法怎么走。
func (t *IntervalTree) IntersectsVerbose(iv Interval) bool {
	fmt.Printf("query [%d,%d): 从根开始查找有没有重叠区间\n", iv.Begin, iv.End)
	x := t.root
	step := 1
	for x != nil {
		fmt.Printf("  step%d: 到节点 [%d,%d) (max=%d)\n", step, x.iv.Begin, x.iv.End, x.max)
		step++
		if x.iv.overlaps(iv) {
			fmt.Printf("         -> 本节点和查询重叠，命中！返回 true\n")
			return true
		}
		if x.left != nil && x.left.max > iv.Begin {
			fmt.Printf("         -> 左孩子 max=%d > 查询.Begin=%d，左子树可能有重叠，往左走\n",
				x.left.max, iv.Begin)
			x = x.left
		} else {
			if x.left == nil {
				fmt.Printf("         -> 没有左孩子，往右走\n")
			} else {
				fmt.Printf("         -> 左孩子 max=%d <= 查询.Begin=%d，整棵左子树不可能重叠，剪掉！往右走\n",
					x.left.max, iv.Begin)
			}
			x = x.right
		}
	}
	fmt.Printf("  走到空节点，没有任何重叠，返回 false\n")
	return false
}

// Stab 返回所有与 iv 重叠的区间。O(log n + k)，k 为命中数。
func (t *IntervalTree) Stab(iv Interval) []Interval {
	t.visited = 0
	var res []Interval
	t.stab(t.root, iv, &res)
	return res
}

func (t *IntervalTree) stab(x *node, iv Interval, res *[]Interval) {
	if x == nil {
		return
	}
	t.visited++
	// ★ 剪枝：整棵子树的最大右端点都 <= 查询左端点，则子树内无任何重叠，直接返回。
	if x.max <= iv.Begin {
		return
	}
	// 左子树可能有更小 Begin 的重叠区间，先递归左边（保证结果按 Begin 升序）。
	t.stab(x.left, iv, res)
	if x.iv.overlaps(iv) {
		*res = append(*res, x.iv)
	}
	// 只有当查询右端点 > 本节点左端点时，右子树才可能有重叠（右子树 Begin 都 >= 本节点）。
	if iv.End > x.iv.Begin {
		t.stab(x.right, iv, res)
	}
}

func main() {
	t := NewIntervalTree()
	// 插入一批区间。注意：本 demo 是未平衡的普通 BST，若按 Begin 递增顺序插入会
	// 退化成右偏链（等于链表），剪枝失效——这恰好印证真实 etcd 为何要红黑平衡。
	// 这里用「近似均衡」的插入顺序（中位数优先），让树有左右子树可供剪枝演示。
	intervals := []Interval{
		{15, 23}, // 根（Begin 中位数）
		{5, 8}, {19, 20},
		{0, 3}, {8, 9}, {17, 19}, {25, 30},
		{6, 10}, {16, 21},
	}
	for i, iv := range intervals {
		t.Insert(iv, fmt.Sprintf("v%d", i))
	}
	fmt.Printf("inserted %d intervals\n\n", t.Len())

	// ------------------------------------------------------------------
	// 演示 0：把树画出来（最直观），并逐步走一次查找
	// ------------------------------------------------------------------
	fmt.Println("=== demo0: visualize the tree + walk one query ===")
	t.Print()
	fmt.Println()
	t.IntersectsVerbose(Interval{1, 2}) // 会命中最左边的 [0,3)
	fmt.Println()
	t.IntersectsVerbose(Interval{11, 14}) // 谁都不重叠，看剪枝

	// ------------------------------------------------------------------
	// 演示 1：Intersects —— 是否存在重叠
	// ------------------------------------------------------------------
	fmt.Println("\n=== demo1: Intersects (single overlap) ===")
	for _, q := range []Interval{{1, 2}, {11, 14}, {21, 23}, {30, 40}} {
		hit := t.Intersects(q)
		fmt.Printf("query %-9v -> intersects=%-5v (visited %d nodes)\n",
			fmt.Sprintf("[%d,%d)", q.Begin, q.End), hit, t.visited)
	}

	// ------------------------------------------------------------------
	// 演示 2：Stab —— 找出所有重叠区间（按 Begin 升序）
	// ------------------------------------------------------------------
	fmt.Println("\n=== demo2: Stab (all overlaps, sorted by Begin) ===")
	for _, q := range []Interval{{8, 18}, {0, 100}, {24, 26}} {
		res := t.Stab(q)
		fmt.Printf("query [%d,%d) -> %v\n", q.Begin, q.End, res)
	}

	// ------------------------------------------------------------------
	// 演示 3：直观展示 max 剪枝的价值
	// ------------------------------------------------------------------
	fmt.Println("\n=== demo3: max-pruning skips whole subtrees ===")
	// 查询落在最左侧，右侧那些 Begin 很大的子树会被 x.max <= iv.Begin 整棵剪掉。
	q := Interval{1, 2}
	res := t.Stab(q)
	fmt.Printf("query [%d,%d) -> %v; visited only %d of %d nodes thanks to max-pruning\n",
		q.Begin, q.End, res, t.visited, t.Len())
}
