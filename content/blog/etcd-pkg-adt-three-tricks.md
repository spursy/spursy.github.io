+++
title = "etcd pkg/adt 区间树里的三个工程小技巧：Compare 编码重叠、sentinel 消灭判空、虚拟区间复用比较"
date = 2026-08-08
description = "上一篇讲清了区间树靠 max 字段把重叠查询降到 O(log n)。这篇换个视角，从一个可独立运行的 demo 里抠出三个『不改变复杂度、却让生产代码优雅无特判』的手艺：用 Compare 返回 0 编码『重叠』而非『相等』、用 sentinel 哨兵吸收所有 nil 边界、在剪枝时把子树范围打包成虚拟区间复用同一个 Compare。三个技巧环环相扣。"
[taxonomies]
tags = ["etcd"]
[extra]
toc = true
+++

上一篇《区间树：用一个 max 字段把区间重叠查询降到 O(log n)》讲的是**算法**——为什么多缓存一个 `max` 就能整片剪掉不可能重叠的子树。这篇换个视角讲**工程手艺**：同一个 `pkg/adt` 区间树，把它从教科书伪码落成生产代码时，etcd 用了三个「小而妙」的技巧。

它们都不改变算法复杂度，却让整份代码**优雅、无特判、可复用**。三个技巧还环环相扣，一个的红利会在下一个里再次兑现：

- **技巧一**：`Interval.Compare` 返回 `0` 表示【重叠】而非【相等】——用比较语义编码业务含义，让整棵树的遍历框架原样复用。
- **技巧二**：`sentinel` 哨兵节点——用一个共享 dummy 吸收所有 nil 边界，主逻辑再无判空分支。
- **技巧三**：`visit` 里用【同一个 Compare】+ `max` 构造「虚拟大区间」做剪枝——技巧一的红利，连剪枝都不必另写比较逻辑。

<!-- more -->

> 完整可运行源码：[golang/etcd/pkg-adt-tricks/main.go](https://github.com/spursy/spursy.github.io/blob/main/golang/etcd/pkg-adt-tricks/main.go)，克隆仓库后 `cd golang && go run ./etcd/pkg-adt-tricks` 即可执行。本 demo 用带 sentinel 的普通 BST（未做红黑平衡）承载，聚焦这三个技巧本身。

## 一、技巧一：Compare 返回 0 = 「重叠」而非「相等」

普通 BST 的三路比较里，`0` 表示「两个 key 相等」。区间树把这个语义**偷换**了：

```
a.Compare(b) = -1  => a 完全在 b 左边
a.Compare(b) = +1  => a 完全在 b 右边
a.Compare(b) =  0  => a 与 b 【重叠】   ← 不是相等！
```

先看端点类型的三路比较——这里 `0` 还是老实的「相等」：

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

关键在于区间的 `Compare`。它比较的不是「谁大谁小」，而是「相对位置」，并把「重叠」压进返回值 `0`：

```go
// Interval 是半开区间 [Begin, End)。
type Interval struct {
	Begin Comparable
	End   Comparable
}

// Compare —— ★ 技巧一：返回 0 表示两区间【重叠】，而非相等。
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
```

跑一遍就能看出 `0` 的特殊含义——注意最后一行，两个**完全相同**的区间也返回 `0`：

```
=== 技巧一：Interval.Compare 返回 0 表示【重叠】而非相等 ===
  [1,3).Compare([5,8)) = -1  (完全在左)
  [9,12).Compare([5,8)) = +1  (完全在右)
  [6,10).Compare([5,8)) = +0  (★重叠)
  [5,8).Compare([5,8)) = +0  (★重叠)    ← 相同也算重叠，不是「相等」
  [3,6).Compare([5,8)) = +0  (★重叠)
  → 注意 [5,8) vs [5,8) 也返回 0：这里的 0 是「重叠」，不是「相等」。
```

**为什么值得这么做？** 普通 BST 的遍历/查找逻辑是「`v<0` 往左、`v>0` 往右、`v==0` 命中」。语义偷换之后，这套框架**一字不改**就能拿来做区间重叠查询——「命中」自动变成「找到一个重叠」。不必为区间重叠单独写一套遍历。这就是「用比较函数的返回值编码业务含义」的巧思。

顺带一提，单点查询也能白嫖这套逻辑：把一个 key `p` 造成点区间 `[p, p+1)`，重叠判断就是「这个点落在哪些区间里」：

```go
func point(p int64) Interval { return Interval{Int64(p), Int64(p + 1)} }
```

## 二、技巧二：sentinel 哨兵节点

树里所有「nil 叶子」和「根的父节点」，都指向**同一个**共享的 dummy 节点 `sentinel`。于是遍历/插入/旋转里大量的 `if x == nil` 判空，全变成 `if x == sentinel`。

```go
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
```

区别在于：`sentinel` 是一个**真实存在、字段可读可写**的对象——它有合法的 `max`、可以被安全地 `x.left = ...`，所以边界节点无需特判就能参与统一逻辑。看插入，所有判空都是 `== t.sentinel`，新节点的空孩子也一律挂 `t.sentinel` 而非 `nil`：

```go
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
```

demo 里验证了「所有空孩子确实是同一个对象」：

```
=== 技巧二：sentinel —— 所有 nil 叶子共享一个 dummy 对象 ===
  叶子 [0,3) 的 left/right 是否都指向共享 sentinel：left=true right=true
  不同叶子的空孩子是同一个对象吗：true
  → 代码里所有判空写成 `x == t.sentinel`，无需 `x == nil` 特判，边界统一。
```

这是 **Null Object（空对象）模式**在指针数据结构上的经典应用：用一个「哑元对象」吸收所有边界情况，让主干代码无分支。红黑树的旋转、删除逻辑分支极多，少写一半 `if x == nil` 的价值就更明显了。

## 三、技巧三：用同一个 Compare + max 构造「虚拟大区间」剪枝

现在把技巧一和技巧二的红利一起兑现。这是遍历函数 `visit`——按 Begin 升序访问所有与 `iv` 重叠的节点，`nv` 返回 `false` 可提前停止：

```go
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
```

三个技巧在这一个函数里全部现身：

1. **技巧二**：第一行 `x == t.sentinel` 就是递归终止，没有 `nil` 判断。
2. **技巧一**：`v := iv.Compare(&x.iv)`，`v==0` 直接就是「重叠命中」，整个 `switch` 沿用了普通 BST「左/右/命中」的三路框架。
3. **技巧三**：`v>0` 分支里的剪枝。

重点看技巧三。当查询 `iv` 落在当前节点右边（`v>0`）时，不能武断地只走右子树——**左子树里可能藏着一个右端点很长、伸到右边来的区间**。怎么判断「这棵子树里还有没有可能重叠的区间」？区间树的做法是构造一个**虚拟区间**：

```go
maxiv := Interval{x.iv.Begin, x.max}
```

`maxiv` 从「本节点左端点」一直延伸到「整棵子树的最大右端点 `max`」——它「代表」了以 `x` 为根的子树能覆盖到的整个范围。然后**复用技巧一的那个 Compare**：

```go
if maxiv.Compare(iv) == 0 { // 虚拟大区间和查询重叠吗？
```

- 若虚拟大区间和查询**重叠**（`==0`）→ 子树里可能有答案，左右都进去找。
- 若连这个「最远够得着」的虚拟大区间都不重叠 → 整棵子树一个都不会重叠，`return true` **直接剪掉**。

妙处在于：剪枝判断**没有另写一行区间比较逻辑**，而是把「子树的空间范围」打包成一个区间、丢给同一个 `Compare`。技巧一定义的抽象，在这里第二次收获红利。

跑一次 `Stab`（找出所有重叠区间），看剪枝省了多少：

```
=== 技巧三：visit 用同一个 Compare + max 虚拟区间剪枝 ===
  Stab([8,18)) 命中(按 Begin 升序)：[8,9), [15,23), [17,19)
  访问节点数 = 6 / 共 7 个(maxiv 剪掉了够不着的子树)
  单点 Stab(point(6)=[6,7)) 命中：[5,8)
  → 剪枝用的仍是技巧一那个 Compare，未另写任何区间比较逻辑。
```

`Stab` 本身只是给 `visit` 套了个收集结果的 visitor——注意它也复用了同一套遍历，自己不含任何比较逻辑：

```go
func (t *Tree) Stab(iv Interval) []Interval {
	t.visited = 0
	var res []Interval
	t.visit(t.root, &iv, func(n *node) bool {
		res = append(res, n.iv)
		return true // 返回 false 即可提前停止遍历
	})
	return res
}
```

### 顺带：为什么右子树的 return 不用取反？

`default` 分支里的中序遍历值得单独说一句。它是标准的「中间步骤失败就提前 return，最后一步直接 return 终值」写法：

```go
if !t.visit(x.left, iv, nv) { // 左子树：后面还有事，失败要提前中止
    return false
}
if !nv(x) {                    // 本节点：后面还有右子树，失败要提前中止
    return false
}
return t.visit(x.right, iv, nv) // 右子树：最后一步，它的返回值就是终值，直接 return
```

左子树和本节点都**不是最后一步**，所以要 `if !... { return false }` 做「失败即中止」的判断，把中止信号一路向上传播；右子树是**最后一步**，后面没有要保护的操作，它的返回值就等于整个函数的返回值，于是直接 `return` 而无需取反。等价写法是 `if !右 { return false }; return true`，简写成一行更清爽。

## 四、三个技巧如何环环相扣

```
技巧一（Compare 0=重叠）
   │  定义了「区间比较」这个抽象
   ├─► 让遍历框架复用 BST 的「左/右/命中」三路结构（visit 的 switch）
   └─► 让剪枝复用同一个 Compare（技巧三的 maxiv.Compare(iv)）

技巧二（sentinel）
   └─► 让 visit / Insert 的边界统一成 == sentinel，主干无判空分支

技巧三（虚拟大区间剪枝）
   └─► 站在技巧一肩上：把「子树范围」当成一个区间，白嫖 Compare 完成剪枝
```

一句话：**技巧一是地基**（把业务含义编码进比较函数），技巧二让主干**干净**（无判空），技巧三则是技巧一的**利息**（剪枝不必另写比较）。

## 五、在真实 etcd 源码里的对照

本 demo 是精简版，真实实现是红黑树（CLRS 第 13、14.3 章）。但这三个技巧原封不动地出现在 etcd 源码里：

| 技巧 | demo 对应 | etcd 源码位置 |
|------|-----------|---------------|
| Compare 返回 0 = 重叠 | `Interval.Compare` | `pkg/adt/interval_tree.go`（`Interval.Compare`）|
| sentinel 哨兵吸收 nil | `Tree.sentinel` | `pkg/adt/interval_tree.go`（`sentinel` 节点定义与使用）|
| 虚拟大区间 + 同一 Compare 剪枝 | `visit` 的 `maxiv` | `pkg/adt/interval_tree.go`（`visit` / `IntervalVisitor`）|

而这棵树在 etcd 里服务的仍是那几个「区间重叠/覆盖」场景：事务冲突检测（`v3rpc/key.go`）、权限校验（`auth/range_perm_cache.go`）、range watcher 路由（`mvcc/watcher_group.go`）、proxy range 缓存失效（`grpcproxy/cache/store.go`）。上一篇讲的是它们背后共同的 `max` 剪枝；这一篇讲的是同一份代码里，让它写得下去、读得舒服的三把小刀。

## 六、总结

区间树的**算法**核心是 `max` 字段（上一篇），但把它落成生产代码，靠的是三个**工程**技巧：

- **用比较函数编码业务含义**（`0`=重叠）：一次抽象，遍历和剪枝两处复用。
- **用哨兵对象吸收边界**（sentinel）：Null Object 模式，主干代码无判空分支。
- **把范围打包成对象、复用已有抽象**（虚拟大区间）：剪枝不必另造轮子。

这三条不只属于区间树。「让返回值携带业务语义」「用哑元对象消除特判」「把状态打包成已有类型复用逻辑」——是任何指针数据结构、任何需要遍历+剪枝的代码里都能迁移的手艺。

## 关键源码位置索引

| 内容 | 位置 |
|------|------|
| 区间树实现（红黑树 + max + Compare + sentinel + visit） | `pkg/adt/interval_tree.go` |
| 端点类型（Int64 / String / BytesAffine，空串视为 +∞） | `pkg/adt/interval_tree.go` |
| 事务冲突检测 `checkIntervals` | `server/etcdserver/api/v3rpc/key.go:236` |
| 权限校验 Contains/Intersects | `server/auth/range_perm_cache.go:88, 101` |
| range watcher 路由 | `server/storage/mvcc/watcher_group.go:153, 184, 190` |
| proxy range 缓存 | `server/proxy/grpcproxy/cache/store.go:71` |
| 本文 demo | `golang/etcd/pkg-adt-tricks/main.go` |
