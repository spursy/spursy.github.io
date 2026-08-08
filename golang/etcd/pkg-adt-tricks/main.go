// Package main 演示 etcd pkg/adt 区间树里三个「小而妙」的工程技巧。
// 它们不改变算法复杂度，却让整份代码优雅、无特判、可复用——是把教科书区间树
// 落成生产代码的关键手艺。三个技巧环环相扣：
//
//   技巧一：Interval.Compare 返回 0 表示【重叠】而非【相等】
//           —— 用比较语义编码业务含义，让整棵树的遍历框架原样复用。
//   技巧二：sentinel 哨兵节点
//           —— 用一个共享 dummy 吸收所有 nil 边界，主逻辑无判空分支。
//   技巧三：visit 里用【同一个 Compare】+ max 构造「虚拟大区间」做剪枝
//           —— 技巧一的红利：连剪枝都不必另写比较逻辑。
//
// ============================================================================
// 技巧一：Compare 返回 0 = 「重叠」而非「相等」
// ============================================================================
//
// 普通 BST 的三路比较里，0 表示「两个 key 相等」。区间树把这个语义【偷换】了：
//
//   a.Compare(b) = -1  => a 完全在 b 左边
//   a.Compare(b) = +1  => a 完全在 b 右边
//   a.Compare(b) =  0  => a 与 b 【重叠】   ← 不是相等！
//
// 好处：普通 BST 的遍历/查找逻辑是「v<0 往左、v>0 往右、v==0 命中」。语义偷换后，
// 这套框架【一字不改】就能用来做区间重叠查询——「命中」自动变成「找到一个重叠」。
// 不必为区间重叠单独写一套遍历。这就是「用比较函数的返回值编码业务含义」的巧思。
//
// ============================================================================
// 技巧二：sentinel 哨兵节点
// ============================================================================
//
// 树里所有「nil 叶子」和「根的父节点」都指向【同一个】共享的 dummy 节点 sentinel。
// 于是遍历/插入/旋转里大量的 `if x == nil` 判空，全变成 `if x == sentinel`。
// 区别在于：sentinel 是一个【真实存在、字段可读可写】的对象——它有合法的 max、
// 可被安全地 `x.left = ...`，所以边界节点无需特判就能参与统一逻辑。
// 这是 Null Object（空对象）模式在指针数据结构上的经典应用：
//   用一个「哑元对象」吸收所有边界情况，让主干代码无分支。
//
// ============================================================================
// 技巧三：用同一个 Compare + max 构造「虚拟大区间」做剪枝
// ============================================================================
//
// 遍历到节点 x、且查询 iv 落在 x 右边(v>0)时，要判断「x 的右/左子树里还有没有可能
// 重叠的区间」。区间树的做法是构造一个【虚拟区间】 maxiv = [x.Begin, x.max]——
// 它「代表」整棵以 x 为根的子树能覆盖到的范围，然后【复用技巧一的同一个 Compare】：
//
//     maxiv.Compare(iv) == 0   // 虚拟大区间和查询重叠吗？
//
// 若这个虚拟大区间都不和查询重叠，整棵子树就一个都不会重叠 → 剪掉。
// 妙处：剪枝判断没有另写一行区间比较逻辑，而是把「子树的空间范围」打包成一个区间、
// 丢给同一个 Compare。技巧一定义的抽象，在这里第二次收获红利。
//
// ============================================================================
// 说明：本 demo 用带 sentinel 的 BST 承载(未做红黑平衡，聚焦这三个技巧本身)。
// 对照源码：Interval.Compare :59、sentinel :214-238、visit(maxiv 剪枝) :151-174。
// ============================================================================
package main

import "fmt"

// ---------- 技巧一：Comparable 三路比较，0 = 重叠 ----------

// Comparable 是端点类型的三路比较接口(泛化 int64 / string / bytes 等)。
type Comparable interface {
	Compare(c Comparable) int
}

// Int64 是最简单的端点类型。
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

// Interval 是半开区间 [Begin, End)。
type Interval struct {
	Begin Comparable
	End   Comparable
}

// Compare —— ★ 技巧一：返回 0 表示两区间【重叠】，而非相等。
//
//	-1 => ivl 完全在 c 左边
//	+1 => ivl 完全在 c 右边
//	 0 => 重叠
func (ivl *Interval) Compare(c Comparable) int {
	ivl2 := c.(*Interval)
	ivbCmpBegin := ivl.Begin.Compare(ivl2.Begin) // 我的左端 vs 它的左端
	ivbCmpEnd := ivl.Begin.Compare(ivl2.End)     // 我的左端 vs 它的右端
	iveCmpBegin := ivl.End.Compare(ivl2.Begin)   // 我的右端 vs 它的左端

	// 我完全在它左边：我的左端 < 它的左端，且我的右端 <= 它的左端
	if ivbCmpBegin < 0 && iveCmpBegin <= 0 {
		return -1
	}
	// 我完全在它右边：我的左端 >= 它的右端
	if ivbCmpEnd >= 0 {
		return 1
	}
	// 否则：重叠
	return 0
}

func ivl(b, e int64) Interval { return Interval{Int64(b), Int64(e)} }

// point 把单个 key 造成一个「点区间」[p, p+1)，从而单点查询复用区间重叠逻辑。
func point(p int64) Interval { return Interval{Int64(p), Int64(p + 1)} }

// ---------- 技巧二：带 sentinel 哨兵的树 ----------

type node struct {
	iv          Interval
	max         Comparable
	left, right *node
}

type Tree struct {
	root     *node
	sentinel *node // ★ 技巧二：所有 nil 叶子共享这一个 dummy
	visited  int   // demo 用：记录上次查询访问的节点数
}

func New() *Tree {
	s := &node{} // 哨兵：一个真实对象，字段可读可写
	return &Tree{root: s, sentinel: s}
}

// Insert 按 Begin 插入(BST)，沿途维护 max。用 sentinel 代替 nil 判断。
func (t *Tree) Insert(iv Interval) {
	z := &node{iv: iv, max: iv.End, left: t.sentinel, right: t.sentinel}
	if t.root == t.sentinel { // 而非 t.root == nil
		t.root = z
		return
	}
	x := t.root
	for {
		if x.max.Compare(iv.End) < 0 {
			x.max = iv.End
		}
		if iv.Begin.Compare(x.iv.Begin) < 0 {
			if x.left == t.sentinel { // 而非 x.left == nil
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

// visit —— ★ 技巧三：用同一个 Compare 驱动遍历 + maxiv 虚拟区间剪枝。
// 按 Begin 升序访问所有与 iv 重叠的节点；nv 返回 false 可提前停止。
func (t *Tree) visit(x *node, iv *Interval, nv func(*node) bool) bool {
	if x == t.sentinel { // 技巧二：哨兵作为递归终止，天然无 nil 判断
		return true
	}
	t.visited++
	v := iv.Compare(&x.iv) // 技巧一：v==0 就是「重叠」
	switch {
	case v < 0:
		// 查询在本节点左边 → 只可能在左子树
		return t.visit(x.left, iv, nv)
	case v > 0:
		// 查询在本节点右边 → 用虚拟大区间 [Begin, max] 判断子树是否还够得着
		maxiv := Interval{x.iv.Begin, x.max}
		if maxiv.Compare(iv) == 0 { // 技巧三：复用同一个 Compare
			if !t.visit(x.left, iv, nv) {
				return false
			}
			return t.visit(x.right, iv, nv)
		}
		return true // 虚拟大区间都不重叠 → 整棵子树剪掉
	default: // v == 0：本节点与查询重叠。中序：左 → 本 → 右
		if !t.visit(x.left, iv, nv) {
			return false
		}
		if !nv(x) {
			return false
		}
		return t.visit(x.right, iv, nv)
	}
}

// Stab 返回所有与 iv 重叠的区间(按 Begin 升序)。
func (t *Tree) Stab(iv Interval) []Interval {
	t.visited = 0
	var res []Interval
	t.visit(t.root, &iv, func(n *node) bool {
		res = append(res, n.iv)
		return true
	})
	return res
}

func fmtIv(iv Interval) string {
	return fmt.Sprintf("[%d,%d)", iv.Begin.(Int64), iv.End.(Int64))
}

func main() {
	// ------------------------------------------------------------------
	// 技巧一：Compare 返回 0 = 重叠(而非相等)
	// ------------------------------------------------------------------
	fmt.Println("=== 技巧一：Interval.Compare 返回 0 表示【重叠】而非相等 ===")
	base := ivl(5, 8)
	cases := []Interval{
		ivl(1, 3),  // 完全在左
		ivl(9, 12), // 完全在右
		ivl(6, 10), // 与 [5,8) 重叠
		ivl(5, 8),  // 恰好相同 —— 也算重叠(0)
		ivl(3, 6),  // 部分重叠
	}
	for _, c := range cases {
		r := c.Compare(&base)
		meaning := map[int]string{-1: "完全在左", 1: "完全在右", 0: "★重叠"}[r]
		fmt.Printf("  %s.Compare(%s) = %+d  (%s)\n", fmtIv(c), fmtIv(base), r, meaning)
	}
	fmt.Println("  → 注意 [5,8) vs [5,8) 也返回 0：这里的 0 是「重叠」，不是「相等」。")

	// ------------------------------------------------------------------
	// 技巧二：sentinel 哨兵节点
	// ------------------------------------------------------------------
	fmt.Println("\n=== 技巧二：sentinel —— 所有 nil 叶子共享一个 dummy 对象 ===")
	t := New()
	for _, iv := range []Interval{
		ivl(15, 23), ivl(5, 8), ivl(19, 20), ivl(0, 3), ivl(8, 9), ivl(17, 19), ivl(25, 30),
	} {
		t.Insert(iv)
	}
	// 找一个叶子，展示它的孩子都是【同一个】sentinel 对象。
	leaf := t.root.left.left // [0,3)
	fmt.Printf("  叶子 %s 的 left/right 是否都指向共享 sentinel：left=%v right=%v\n",
		fmtIv(leaf.iv), leaf.left == t.sentinel, leaf.right == t.sentinel)
	fmt.Printf("  不同叶子的空孩子是同一个对象吗：%v\n",
		t.root.right.right.right == leaf.left) // [25,30).right 与 [0,3).left 同为 sentinel
	fmt.Println("  → 代码里所有判空写成 `x == t.sentinel`，无需 `x == nil` 特判，边界统一。")

	// ------------------------------------------------------------------
	// 技巧三：同一个 Compare + max 构造虚拟大区间做剪枝
	// ------------------------------------------------------------------
	fmt.Println("\n=== 技巧三：visit 用同一个 Compare + max 虚拟区间剪枝 ===")
	q := ivl(8, 18)
	res := t.Stab(q)
	fmt.Printf("  Stab(%s) 命中(按 Begin 升序)：", fmtIv(q))
	for i, r := range res {
		if i > 0 {
			fmt.Print(", ")
		}
		fmt.Print(fmtIv(r))
	}
	fmt.Printf("\n  访问节点数 = %d / 共 7 个(maxiv 剪掉了够不着的子树)\n", t.visited)

	// 单点查询也复用同一套：point(p) = [p, p+1)
	fmt.Printf("  单点 Stab(point(6)=%s) 命中：", fmtIv(point(6)))
	for _, r := range t.Stab(point(6)) {
		fmt.Print(fmtIv(r), " ")
	}
	fmt.Println("\n  → 剪枝用的仍是技巧一那个 Compare，未另写任何区间比较逻辑。")
}
