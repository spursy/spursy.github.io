+++
title = "etcd pkg/adt 区间树：用一个 max 字段把区间重叠查询降到 O(log n)"
date = 2026-08-05
description = "从二叉搜索树讲起，说清区间树在红黑树上多缓存的那个 max 字段如何实现『整片剪掉不可能重叠的子树』，再从一个可独立运行的 demo 拆解 Insert / Intersects / Stab 三个操作，最后看它在 etcd 的事务冲突、权限校验、watcher 路由里的真实用法。"
[taxonomies]
tags = ["etcd"]
[extra]
toc = true
+++

前几篇讲的是 etcd 的并发原语(`wait` / `WaitTime` / `notify`)、无锁 ID(`idutil`)和页对齐写(`PageWriter`)。这篇转向数据结构:`pkg/adt` 里的**区间树(Interval Tree)**——它是一棵红黑树,但每个节点多缓存了一个 `max` 字段,就能在 O(log n) 内回答「哪些区间和 `[begin, end)` 有交集」。

etcd 里凡是「一堆 key 区间,要快速判断某个 key/区间是否与它们重叠或被覆盖」的场景,底座都是它:事务冲突检测、权限校验、range watcher 路由、proxy 缓存失效。

要讲清它,得先从二叉搜索树的有序性说起,再看区间树在它之上加的那一个字段。然后从一个可独立运行的 demo 拆解实现,最后看 etcd 里的真实用法。

<!-- more -->

## 一、从二叉搜索树说起

**二叉搜索树(BST)** 的核心是一条有序性:对任意节点,**左子树所有 key < 本节点 key < 右子树所有 key**。这条性质递归地对每棵子树都成立。

```
        15
       /  \
      6    23
     / \   / \
    3   8 19  25
```

因为有序,查找就像猜数字——每下降一层排除一半,是 O(树高)。树平衡时高度 ≈ log n。

但 BST 擅长的是「**单点**」查询(key=8 在不在)。很多场景要回答的却是「**区间重叠**」查询:给一个区间 `[begin, end)`,树里有哪些区间和它有交集?朴素做法遍历所有区间是 O(n)。区间树把它降到 O(log n)——靠的就是在 BST 有序骨架上加的一个字段。

## 二、核心设计：红黑树 + 增强字段 max

区间树的构造分三步:

**第一步:选排序键**。以区间的**左端点 Begin** 作为 BST 的 key(Begin 相同再比 End)。于是首先是一棵按左端点有序、可自平衡的树。

**第二步:每个节点缓存 max**。`max = 以本节点为根的整棵子树中,所有区间 End 的最大值`,即 `max = max(本节点 End, 左子树 max, 右子树 max)`。插入/删除/旋转后沿路径向上维护。

**第三步:用 max 做查询剪枝**——这是整个数据结构最巧妙的地方。查重叠时,若某棵子树的 `max <= 查询.Begin`,说明这棵子树里**每一个**区间的右端点都 ≤ 查询左端点,全部落在查询区间左侧、不可能重叠 → **整棵子树一次性剪掉**。

> 直觉:BST 的 key 告诉你「往哪边找更大/更小」;区间树的 max 额外告诉你「哪边根本不可能有重叠、别浪费时间进去」。

层次关系是这样叠上来的:

```
二叉树 → 二叉搜索树(BST，加有序性)
        → 红黑树(BST + 自平衡，保证最坏 O(log n))
          → 区间树(红黑树 + max 字段，支持区间重叠查询)
```

真实 etcd 实现是红黑树(CLRS 第 13 章)+ 区间树增强(第 14.3 章),保证最坏 O(log n),并用 sentinel 哨兵节点统一 nil 边界、用 `Comparable` 接口泛化端点类型。下面的 demo 为聚焦原理,用**普通 BST** 承载——max 剪枝的正确性与是否红黑平衡无关,平衡只影响最坏树高。

## 三、最小可运行实现

> 完整可运行源码：[golang/etcd/pkg-adt/main.go](https://github.com/spursy/spursy.github.io/blob/main/golang/etcd/pkg-adt/main.go)，克隆仓库后 `cd golang && go run ./etcd/pkg-adt` 即可执行。

先看区间和节点定义。区间是半开区间 `[Begin, End)`,重叠判断是经典的 `b1 < e2 && e1 > b2`:

```go
type Interval struct {
	Begin int64
	End   int64
}

// 两个半开区间有交集：b1 < e2 且 e1 > b2
func (iv Interval) overlaps(o Interval) bool {
	return iv.Begin < o.End && iv.End > o.Begin
}

type node struct {
	iv          Interval
	val         any
	max         int64 // ★ 子树内所有区间 End 的最大值
	left, right *node
}
```

### Insert：BST 插入 + 沿途维护 max

```go
func (t *IntervalTree) Insert(iv Interval, val any) {
	t.count++
	if t.root == nil {
		t.root = &node{iv: iv, val: val, max: iv.End}
		return
	}
	x := t.root
	for {
		if iv.End > x.max { // ★ 沿途更新祖先的 max
			x.max = iv.End
		}
		if iv.Begin < x.iv.Begin {
			if x.left == nil {
				x.left = &node{iv: iv, val: val, max: iv.End}
				return
			}
			x = x.left
		} else { // Begin 相等也走右边：固定一个方向，不丢数据
			if x.right == nil {
				x.right = &node{iv: iv, val: val, max: iv.End}
				return
			}
			x = x.right
		}
	}
}
```

两个要点:

1. **`if iv.End > x.max` 这行是区间树的灵魂**。新区间会成为下降路径上每个节点的子树成员,所以这条路径正好覆盖了所有需要抬高 max 的祖先——一趟下降就全部维护好,不用回溯。
2. **`else` 分支同时兜住「大于」和「等于」**。`if` 的条件是严格 `<`,取反后 `else` 覆盖 `>=`。Begin 相同的两个区间(如 `[5,8)` 和 `[5,20)`)约定都放右子树——只要固定一个方向,就不会丢数据、不破坏有序性。这是 BST 处理重复 key 的标准写法。

### Intersects：是否存在重叠（单路径 O(log n)）

```go
func (t *IntervalTree) Intersects(iv Interval) bool {
	x := t.root
	for x != nil {
		if x.iv.overlaps(iv) {
			return true
		}
		// ★ max 剪枝：左子树 max > iv.Begin 时左子树才「可能」有重叠
		if x.left != nil && x.left.max > iv.Begin {
			x = x.left
		} else {
			x = x.right // 整棵左子树被剪掉
		}
	}
	return false
}
```

它只走**一条从根往下的路径**,不遍历整棵树——这正是 O(log n) 而非 O(n) 的原因。每到一个节点先看自己是否重叠(命中即返回),没中就用左子树的 max 决定走向:

- `x.left.max > iv.Begin` → 左子树可能有重叠,往左。
- 否则 → 左子树里每个区间右端点都 ≤ 查询左端点,不可能重叠,**整棵左子树剪掉**,拐向右边。

正确性靠区间树理论(CLRS 14.3)保证:被放弃的那半边一定没有答案,所以单路径下降就能给出正确的存在性结论。

### Stab：找出所有重叠（中序遍历 + 两层剪枝）

`Intersects` 找一个即停,`Stab` 要找**全部**,所以不能只走一条路,而是递归遍历 + 剪枝:

```go
func (t *IntervalTree) stab(x *node, iv Interval, res *[]Interval) {
	if x == nil {
		return
	}
	// ★ 剪枝 1：整棵子树最大右端点 <= 查询左端点，子树内无任何重叠
	if x.max <= iv.Begin {
		return
	}
	// 先递归左边（保证结果按 Begin 升序）
	t.stab(x.left, iv, res)
	if x.iv.overlaps(iv) {
		*res = append(*res, x.iv)
	}
	// ★ 剪枝 2：查询右端点 <= 本节点左端点时，右子树 Begin 更大、更不可能重叠
	if iv.End > x.iv.Begin {
		t.stab(x.right, iv, res)
	}
}
```

它巧妙地用了**两个方向的剪枝**:

| 剪枝 | 条件 | 剪掉谁 |
|------|------|--------|
| **max 剪枝** | `x.max <= iv.Begin` | 整棵子树(子树最大右端点够不到查询左端点)|
| **Begin 剪枝** | `iv.End <= x.iv.Begin` | 整棵右子树(右子树 Begin 都太大)|

三步顺序 **左 → 根 → 右** 就是中序遍历,在 BST 上天然产生**按 Begin 升序**的结果,不需额外排序。复杂度 O(log n + k),k 是命中数。

## 四、横向打印：一眼看懂树形与 max

demo 里的 `Print` 用「右子树在上、左子树在下、缩进表示深度」的横向画法(把头向左歪 90° 看就是正常的树)。跑一次插入 9 个区间后的输出:

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

顶格的 `[15,23)` 是根,上方一坨是右子树、下方一坨是左子树。还原成正常方向:

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

逐个核对 max(= 子树内所有 End 的最大值):`[8,9)` 的子树含 `[6,10)`,所以 max=10;`[5,8)` 的子树含 `{8,3,9,10}`,max=10;根 `[15,23)` 覆盖全树,max=30。全部正确。这种横向打印虽然要歪头看,但缩进唯一决定父子关系、**无歧义**,是文本画树的常用手法。

## 五、max 剪枝到底省了多少

demo 的 `demo3` 直观展示剪枝价值。查询 `[1,2)` 落在最左侧:

```
query [1,2) -> [[0 3)]; visited only 3 of 9 nodes thanks to max-pruning
```

9 个节点只访问了 3 个——右侧那些 Begin 很大的子树全被 `x.max <= iv.Begin` 整棵剪掉。这就是那个 max 字段的价值:**把「找重叠」从 O(n) 稳定压到 O(log n)**。

⚠️ 一个前提:这个 demo 是**未平衡的普通 BST**。若按 Begin 递增顺序插入(1,2,3,4...),树会退化成右偏链(等于链表),剪枝失效、退回 O(n)。这恰好印证了真实 etcd 为何**必须用红黑树保证平衡**——demo 里特意用「中位数优先」的插入顺序,让树有左右子树可供剪枝演示。

## 六、在 etcd 中的真实用法

区间树在 etcd 里服务四个对象,全是「区间重叠/覆盖」查询:

| 场景 | 位置 | 树里存什么 | 查询问什么 |
|------|------|-----------|-----------|
| **事务冲突检测** | `v3rpc/key.go:236` | 一个 Txn 内所有 delete 的 key range | put 的 key 是否落进某个 delete range |
| **权限校验** | `auth/range_perm_cache.go` | 用户被授权的读/写 key range | 请求的 key/range 是否被授权覆盖 |
| **range watcher 路由** | `mvcc/watcher_group.go` | 所有监听一段 range 的 watcher | 某个变更 key 命中了哪些 range watcher |
| **proxy range 缓存** | `grpcproxy/cache/store.go` | 已缓存的 range 响应 | 写操作要让哪些重叠缓存失效 |

### 场景 1：事务冲突检测（Intersects 的真实用途）

`checkIntervals`(`key.go:236`)判断一个 Txn 里的 put/delete 有没有互相冲突:

```go
dels := adt.NewIntervalTree()
// ① 把本层所有 DeleteRange 作为区间插入
for _, req := range reqs {
	iv := adt.NewStringAffineInterval(dreq.Key, dreq.RangeEnd)
	dels.Insert(iv, struct{}{})
}
// ② 每个 put 的 key 检查是否落进任何 delete range
if dels.Intersects(adt.NewStringAffinePoint(k)) {
	return rpctypes.ErrGRPCDuplicateKey // 冲突！
}
```

`NewStringAffinePoint(k)` 把单个 key 变成一个「点区间」,复用同一套重叠判断。这正是 demo 里 `Intersects` 的真实用途:**不关心和谁重叠,只关心有没有重叠**。

### 场景 2：权限校验（Intersects vs Contains）

`range_perm_cache.go` 给每个用户建两棵区间树 `readPerms` / `writePerms`,把授权的 key range 插进去。校验时分两种语义:

```go
// 请求是一个 range：授权范围必须【完全覆盖】它 → 用 Contains
cachedPerms.readPerms.Contains(ivl)

// 请求是单个 key：只要有任一授权区间【包含】这个点 → 用 Intersects
cachedPerms.readPerms.Intersects(pt)
```

- **`Intersects`**:有交集即可(单点是否被任一授权段命中)。
- **`Contains`**:查询区间必须被**完整覆盖**(range 请求不能超出授权边界)。

这里 `BytesAffine` 端点类型很关键:空 `[]byte{}` 被当作 **+∞**,用来表达「从某 key 到末尾」的开放授权区间。

### 场景 3 & 4：watcher 路由与缓存失效

`watcher_group.go` 用区间树存所有 range watcher,一次 key 写入要快速找出「所有覆盖了这个 key 的 range watcher」,把匹配从 O(watcher 数) 降到 O(log n)。proxy 的 `cache/store.go` 则用它缓存 range 响应,写操作时让所有重叠的缓存 range 失效——概念上对应 demo 的 `Stab`(批量找出所有重叠)。

## 七、为什么不用普通 map / 有序表

- 普通 **map** 只能精确匹配单个 key,答不了「区间重叠」。
- **有序表**能二分单点,但一个查询区间可能横跨多个已存区间,仍需 O(k) 扫描。
- **区间树**靠每节点缓存的 `max` 整片剪掉不可能重叠的子树,把重叠查询稳定压到 O(log n)。

## 八、总结

区间树 = **BST 的有序骨架 + 红黑树的自平衡 + 一个 max 增强字段**。三者缺一不可:

- **有序性**(按 Begin):决定往哪边查。
- **红黑平衡**:保证最坏树高 O(log n),避免退化成链。
- **max 字段**:查询时整片剪掉不可能重叠的子树,是把区间重叠查询降到 O(log n) 的关键。

理解「BST 有序性 → max 缓存子树最大右端点 → 查询时按 max 剪枝 → 红黑树保证不退化」这条线,就抓住了 etcd 事务冲突、权限校验、watcher 路由背后共同的那把钥匙。

## 关键源码位置索引

| 内容 | 位置 |
|------|------|
| 区间树实现（红黑树 + max + Intersects/Contains/Find） | `pkg/adt/interval_tree.go` |
| 端点类型（Int64 / String / BytesAffine，空串视为 +∞） | `pkg/adt/interval_tree.go` |
| 事务冲突检测 `checkIntervals` | `server/etcdserver/api/v3rpc/key.go:236` |
| 权限树构建 `getMergedPerms` | `server/auth/range_perm_cache.go:30` |
| 权限校验 Contains/Intersects | `server/auth/range_perm_cache.go:88, 101` |
| range watcher 路由 | `server/storage/mvcc/watcher_group.go:153, 184, 190` |
| proxy range 缓存 | `server/proxy/grpcproxy/cache/store.go:71` |
