// Package main 演示 etcd pkg/adt 区间树的「结构变动侧」——旋转后如何维护增强字段 max，
// 以及 updateMax 的「提前终止」优化。这是《区间树 max 剪枝》一篇的续篇：
// 上一篇讲【查询侧】(Insert 埋 max + Intersects/Stab 用 max 剪枝)，
// 本篇讲【维护侧】(红黑树旋转会打乱父子归属，max 必须重算)。
//
// ============================================================================
// 一、问题：为什么旋转后非重算 max 不可
// ============================================================================
//
// 区间树 = 红黑树 + 每节点缓存 max(= 以本节点为根的子树内所有区间 End 的最大值)。
// 红黑树靠 rotateLeft/rotateRight 做自平衡，而**旋转会改变两个节点的子树成员**：
//
//        x                        y
//       / \                      / \
//      a   y     --左旋(x)-->    x   c
//         / \                  / \
//        b   c                a   b
//
// 旋转前 x 的子树 = {a,b,c,x}，旋转后 x 的子树只剩 {a,b,x}；y 的子树反过来变大。
// 两个节点的「子树范围」都变了，缓存的 max 于是过期，必须重算——否则后续查询的
// max 剪枝会基于错误的 max 做判断，把该进的子树剪掉、或该剪的子树进去。
//
// ============================================================================
// 二、核心设计 1：从「下层节点」开始重算，顺序不能反
// ============================================================================
//
// 旋转后 x 落到 y 的下面(x 成了 y 的孩子)。重算必须**自底向上**：先算 x，再算 y。
// 因为 y 的 max 依赖 x 子树的 max——若先算 y，它会用到 x 的【旧】max，算错。
// 本 demo 的做法(对齐 etcd rotateLeft :599/606)：旋转的指针手术做完后，
// 调用 updateMax(x)——它从下层的 x 出发【沿 parent 链向上】重算，
// 天然先 x、再 y、再祖先，顺序自动正确。
//
// ============================================================================
// 三、核心设计 2：updateMax 的「提前终止」——把 O(树高) 压到 O(1~2)
// ============================================================================
//
// updateMax 沿 parent 链向上逐个重算。关键优化：**一旦某节点的 max 重算后没变，
// 就断定它所有祖先也不会变，立即 break**。
//   直觉：max 只会因「子树里冒出一个更大的 End」而增大。如果到某节点这个更大的 End
//   已经被它原有的 max「盖住」(重算后 max 不变)，那再往上，这个 End 更不可能突破
//   祖先们本就 ≥ 当前 max 的值——传播到此收敛。
// 这让「最坏 O(树高) 的向上更新」在多数插入/旋转里只走 1~2 层。
// 这是 augmented tree(增强树)维护派生字段的通用范式：变化沿树上浮，但会自然收敛。
//
// ============================================================================
// 说明：本 demo 为聚焦「旋转 + max 维护 + 提前终止」，用带父指针的 BST 承载，
// 手动触发旋转(不实现完整红黑 fixup 的变色)。max 维护逻辑与真实 etcd 一致。
// 对照源码：updateMax :130、rotateLeft :585、rotateRight :630、Insert :479。
// ============================================================================
package main

import (
	"fmt"
	"math"
)

type node struct {
	begin, end          int64
	max                 int64 // 子树内所有区间 End 的最大值
	left, right, parent *node
}

type Tree struct {
	root *node
	// lastMaxVisits 记录上一次 updateMax 向上走访了几个节点，用来直观展示「提前终止」。
	lastMaxVisits int
}

// updateMax 从 x 出发，沿 parent 链向上重算 max，遇到 max 不变即提前终止。
func (t *Tree) updateMax(x *node) {
	t.lastMaxVisits = 0
	for x != nil {
		t.lastMaxVisits++
		old := x.max
		m := x.end
		if x.left != nil && x.left.max > m {
			m = x.left.max
		}
		if x.right != nil && x.right.max > m {
			m = x.right.max
		}
		if old == m { // ★ 提前终止：本节点 max 没变，祖先必然也不变
			break
		}
		x.max = m
		x = x.parent
	}
}

// Insert 标准 BST 插入(按 begin)，然后从新节点的父节点起向上 updateMax。
func (t *Tree) Insert(begin, end int64) {
	z := &node{begin: begin, end: end, max: end}
	var y *node
	x := t.root
	for x != nil {
		y = x
		if begin < x.begin {
			x = x.left
		} else {
			x = x.right
		}
	}
	z.parent = y
	if y == nil {
		t.root = z
	} else if begin < y.begin {
		y.left = z
	} else {
		y.right = z
	}
	t.updateMax(y) // 新节点自己 max 已是 end；只需向上修正祖先
}

// rotateLeft 左旋 x（x 必须有右孩子）。指针手术后从下层的 x 起 updateMax。
//        x                        y
//       / \                      / \
//      a   y     --左旋(x)-->    x   c
//         / \                  / \
//        b   c                a   b
func (t *Tree) rotateLeft(x *node) {
	y := x.right
	if y == nil {
		return
	}
	x.right = y.left
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
	y.left = x
	x.parent = y
	// ★ 从下层的 x 开始向上：先修 x，再修此刻已成为 x 父节点的 y，再修祖先。
	t.updateMax(x)
}

// rotateRight 右旋 x（x 必须有左孩子）。指针手术后从下层的 x 起 updateMax。
//          x                    y
//         / \                  / \
//        y   c   --右旋(x)-->  a   x
//       / \                       / \
//      a   b                     b   c
// rotateRight 右旋 x（x 必须有左孩子）。镜像 rotateLeft。
func (t *Tree) rotateRight(x *node) {
	y := x.left
	if y == nil {
		return
	}
	x.left = y.right
	if y.right != nil {
		y.right.parent = x
	}
	y.parent = x.parent
	if x.parent == nil {
		t.root = y
	} else if x == x.parent.right {
		x.parent.right = y
	} else {
		x.parent.left = y
	}
	y.right = x
	x.parent = y
	t.updateMax(x)
}

// ---- 校验与打印工具 ----

// subtreeMax 暴力算出子树真实的最大 End（用于校验缓存的 max 是否正确）。
func subtreeMax(x *node) int64 {
	if x == nil {
		return math.MinInt64
	}
	m := x.end
	if l := subtreeMax(x.left); l > m {
		m = l
	}
	if r := subtreeMax(x.right); r > m {
		m = r
	}
	return m
}

// checkMax 逐节点校验 stored max == 真实子树 max。
func checkMax(x *node) bool {
	if x == nil {
		return true
	}
	if want := subtreeMax(x); x.max != want {
		fmt.Printf("  ✗ [%d,%d) max=%d 应为 %d\n", x.begin, x.end, x.max, want)
		return false
	}
	return checkMax(x.left) && checkMax(x.right)
}

// print 横向打印：右子树在上、左子树在下，缩进=深度（头向左歪 90° 看是正常树）。
func (t *Tree) print(x *node, depth int) {
	if x == nil {
		return
	}
	t.print(x.right, depth+1)
	indent := ""
	for i := 0; i < depth; i++ {
		indent += "        "
	}
	fmt.Printf("%s[%d,%d) max=%d\n", indent, x.begin, x.end, x.max)
	t.print(x.left, depth+1)
}

func (t *Tree) Print(title string) {
	fmt.Println(title)
	t.print(t.root, 0)
	if checkMax(t.root) {
		fmt.Println("  ✓ 所有节点 max 正确")
	}
}

func build() *Tree {
	t := &Tree{}
	// 中位数优先，建一棵均衡的 7 节点树
	for _, iv := range [][2]int64{
		{15, 23}, {5, 8}, {19, 20}, {0, 3}, {8, 9}, {17, 19}, {25, 30},
	} {
		t.Insert(iv[0], iv[1])
	}
	return t
}

func main() {
	// ------------------------------------------------------------------
	// demo0：初始树 + max 校验
	// ------------------------------------------------------------------
	t := build()
	t.Print("=== demo0: 初始树（每个节点 [begin,end) max=子树内最大 end）===")

	// ------------------------------------------------------------------
	// demo1：左旋根节点，旋转后 max 依然全部正确
	// ------------------------------------------------------------------
	fmt.Println("\n=== demo1: 左旋根 [15,23)，旋转后 max 自动重算并保持正确 ===")
	t.rotateLeft(t.root)
	t.Print("左旋后：")

	// ------------------------------------------------------------------
	// demo2：updateMax 的「提前终止」——小 end 只走 1 层，大 end 传播到根
	// ------------------------------------------------------------------
	fmt.Println("\n=== demo2: updateMax 提前终止（观察向上走访的节点数）===")
	t2 := build()
	t2.Insert(1, 2) // end=2 很小，父节点 [0,3) 的 max=3 已盖住 → 重算不变，立即终止
	fmt.Printf("插入 [1,2)（end 很小）：updateMax 只走访了 %d 个节点（提前终止）\n", t2.lastMaxVisits)
	t2.Insert(16, 40) // end=40 超过一路祖先的 max → 一直传播到根
	fmt.Printf("插入 [16,40)（end 超大）：updateMax 走访了 %d 个节点（传播到根）\n", t2.lastMaxVisits)

	// ------------------------------------------------------------------
	// demo3：为什么必须从「下层的 x」开始，而不是从 y 开始
	// ------------------------------------------------------------------
	fmt.Println("\n=== demo3: 重算顺序——先下层 x 再上层 y，反了就算错 ===")
	// 手搭一个 y 在上、x 在下的两节点场景，并故意让 x 的 max 过期。
	x := &node{begin: 10, end: 100, max: 20} // 真实应为 100，此刻 max=20 是「过期」值
	y := &node{begin: 5, end: 8, max: 8, left: x}
	x.parent = y
	// 错误顺序：先算上层 y（此时读到 x 的过期 max=20）
	mWrong := y.end
	if y.left.max > mWrong {
		mWrong = y.left.max
	}
	fmt.Printf("先算上层 y：y.max=%d（错！用了 x 的过期 max=20，漏掉 x 真实的 100）\n", mWrong)
	// 正确顺序：先算下层 x，把 x.max 修成 100，再算 y
	xm := x.end
	if x.left != nil && x.left.max > xm {
		xm = x.left.max
	}
	if x.right != nil && x.right.max > xm {
		xm = x.right.max
	}
	x.max = xm // x.max = 100
	ym := y.end
	if y.left.max > ym {
		ym = y.left.max
	}
	fmt.Printf("先算下层 x（x.max=%d）再算 y：y.max=%d（对！）\n", x.max, ym)
	fmt.Println("→ 结论：updateMax 从下层 x 出发向上走，天然保证这个顺序。")
}
