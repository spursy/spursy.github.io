+++
title = "etcd pkg/wait.WaitTime：按「逻辑时间」阈值批量放行的等待"
date = 2026-07-31
description = "从一个可独立运行的 demo 出发，讲清 etcd WaitTime 如何用『单调递增的进度 + 阈值放行』等 apply index 追上，以及它在线性一致读里的真实用途。"
[taxonomies]
tags = ["etcd"]
+++

前两篇讲了 `pkg/wait`（按 ID 一对一、带数据）和 `pkg/notify`（广播、纯信号）。它们俩之外，`pkg/wait` 还藏着第三种等待原语——`WaitTime`（`wait_time.go`）：**按一个单调递增的「逻辑时间」阈值来批量放行**。

它服务的问题非常具体:「**等一个单调递增的进度追上某个刻度**」。在 etcd 里,这个进度就是 **applied index**,而最主要的消费者正是上一篇提到的**线性一致读**。这篇从一个可独立运行的 demo 出发讲清它的设计,再对照源码看它怎么用。

<!-- more -->

## 一、WaitTime 是什么

`pkg/wait` 其实有两套东西:

- **`Wait`（`wait.go`)**:按「ID」一对一,`Trigger(id, result)` 精确唤醒一个等待者,**带数据**。
- **`WaitTime`（`wait_time.go`,本文主角)**:按「逻辑时间」一对多,`Trigger(deadline)` 一次唤醒所有「等待目标 `<=` deadline」的等待者,**只传信号**。

典型场景:有一个单调递增的进度计数(如 applied index)。

- 消费者:「等进度追上第 N 号」→ `Wait(N)`,阻塞在返回的 channel 上;
- 生产者:「已经推进到第 M 号了」→ `Trigger(M)`,一次放行所有 `N <= M` 的等待者。

一句话:**一条单调递增的进度线,谁想等某个刻度就登记一个 channel;进度推进到 X 时,一次关闭 `<= X` 的所有 channel 放行全部。**

## 二、最小可运行实现

> 完整可运行源码：[golang/etcd/pkg-wait-time/main.go](https://github.com/spursy/spursy.github.io/blob/main/golang/etcd/pkg-wait-time/main.go)，克隆仓库后 `cd golang && go run ./etcd/pkg-wait-time` 即可执行。

下面是一个去掉 etcd 依赖、可以直接 `go run` 的等价实现，逻辑与 `go.etcd.io/etcd/pkg/v3/wait` 的 `wait_time.go` 一致。核心就是 `Wait` / `Trigger` 两个方法。

```go
// closec 是一个「一出生就被关闭」的全局 channel，
// 用于给「等待一个已经越过的 deadline」的调用者做零成本的立即放行。
var closec chan struct{}

func init() { closec = make(chan struct{}); close(closec) }

type timeList struct {
	l                   sync.Mutex
	lastTriggerDeadline uint64                   // 已触发到的最大 deadline
	m                   map[uint64]chan struct{} // deadline -> 等待 channel
}

func NewTimeList() *timeList {
	return &timeList{m: make(map[uint64]chan struct{})}
}

// Wait 返回一个在给定 deadline 上等待的 channel；
// 当 Trigger 的 deadline >= 本 deadline 时，该 channel 被关闭（放行）。
func (tl *timeList) Wait(deadline uint64) <-chan struct{} {
	tl.l.Lock()
	defer tl.l.Unlock()
	// 已经越过：直接返回预关闭的全局 channel，立即放行（消除竞态）。
	if tl.lastTriggerDeadline >= deadline {
		return closec
	}
	ch := tl.m[deadline]
	if ch == nil {
		ch = make(chan struct{})
		tl.m[deadline] = ch
	}
	return ch
}

// Trigger 把进度推进到 deadline，并放行所有目标 <= deadline 的等待者。
func (tl *timeList) Trigger(deadline uint64) {
	tl.l.Lock()
	defer tl.l.Unlock()
	tl.lastTriggerDeadline = deadline
	for t, ch := range tl.m {
		if t <= deadline {
			delete(tl.m, t)
			close(ch) // 关闭即放行：等在上面的 goroutine 同时返回
		}
	}
}
```

用它模拟「等 applied index 追上」：3 个消费者分别等进度到 5 / 8 / 12。

```go
tl := NewTimeList()

var wg sync.WaitGroup
for _, d := range []uint64{5, 8, 12} {
	wg.Add(1)
	go func(deadline uint64) {
		defer wg.Done()
		<-tl.Wait(deadline) // 阻塞，直到进度追上 deadline
		fmt.Printf("waiter(deadline=%d) released\n", deadline)
	}(d)
}

time.Sleep(100 * time.Millisecond) // 等消费者都登记好

tl.Trigger(8)  // 进度推进到 8：放行 5、8 两个；12 还得继续等
tl.Trigger(20) // 进度推进到 20：放行剩下的 12
wg.Wait()
```

一次 `Trigger(8)` 同时放行了 deadline 为 5 和 8 的两个等待者——**「过线全放行」而非「精确等于」**。

而「等待一个已经越过的 deadline」会立即返回：

```go
// 此时 lastTriggerDeadline = 20
<-tl.Wait(10) // 10 <= 20，已越过，走全局 closec 秒回，不阻塞
```

## 三、几个关键设计

### 1. 用「关闭 channel」做放行

每个还没到的 deadline 对应一个 `chan struct{}`，`Trigger` 时把所有 `<=` deadline 的 channel `close` 掉——等在上面的 goroutine 同时返回。这和 `notify` 同源：**`close` 即广播**。区别是这里带了「阈值筛选」。

### 2. lastTriggerDeadline —— 消除「等一个已发生事件」的竞态（最精妙处）

进度是单调递增的:一旦 `Trigger(M)` 过了,之后任何 `Wait(N<=M)` 都不该再阻塞。`timeList` 记下 `lastTriggerDeadline`,若 `Wait` 的 deadline 已经被触发过,直接返回那个 **`init()` 时就 `close` 好的全局 `closec`**,立即放行、零分配。

这解决的是一个真实竞态:调用方常常先「看一眼进度够不够、不够再等」。检查和 `Wait` 之间存在窗口,万一刚检查完进度就追上并 `Trigger` 了,再去裸 channel 上等就会**永久阻塞**。`lastTriggerDeadline` 让「已经发生的事件,事后来等也能秒拿到」,这正是它比裸 channel 高明的地方。

### 3. 一次 Trigger 折叠掉一批等待

`Trigger(deadline)` 遍历 map 关闭所有 `<= deadline` 的 channel。晚触发的大 deadline 会**顺带放行之前所有更小的**,无需逐个精确唤醒。这对「一个进度线上挂着大量不同刻度的等待者」的场景极其高效。

### 使用要点 / 坑

- **deadline 必须来自单调递增的量**（如 applied index），否则 `lastTriggerDeadline` 的「已越过即秒放行」语义会失真。
- **只传信号、不带数据**：放行后要读的进度值得自己另外去拿。
- **是「`<=` 全放行」而非「精确等于」**：想要「恰好等于某刻度」的语义,它给不了。

## 四、在 etcd 中的真实用法

一句话概括:凡是「**等一个单调递增的进度(applied index)追上某个刻度**」的场景,etcd 就用 `WaitTime`(`wait.NewTimeList()`)。它是 `EtcdServer` 里名为 `applyWait` 的那唯一一个字段。

### 唯一的生产者:apply 主循环

`server/etcdserver/server.go:248` 定义字段、`:567` 初始化。`applyAll()` 每应用完一批日志,就把进度推进上去(`server.go:978`):

```go
func (s *EtcdServer) applyAll(ep *etcdProgress, apply *toApply) {
	s.applySnapshot(ep, apply)
	s.applyEntries(ep, apply)
	// ...
	s.applyWait.Trigger(ep.appliedi) // 进度推进到 appliedi，放行所有 <= 它的等待者
}
```

apply 是**全局单点、串行**的循环,`appliedi` 天然**单调递增**——恰好满足 `WaitTime` 对「deadline 单调」的前提。

### 核心消费者:线性一致读的「第二段等待」

上一篇讲的线性一致读分两步,`WaitTime` 负责第二步:

1. **确认阶段**(用 `pkg/notify` + ReadState):向 leader 要 ReadIndex,拿到可靠的 `confirmedIndex`——「读这一刻,集群至少 commit 到了这里」。
2. **追平阶段**(用 `WaitTime`):本地 apply 可能还没到 `confirmedIndex`。`server/etcdserver/read/read.go:133`:

```go
appliedIndex := r.server.AppliedIndex()
if appliedIndex < confirmedIndex {
	select {
	case <-r.server.ApplyWait(confirmedIndex): // 等 apply 追上 confirmedIndex
	case <-r.server.Stopping():
		return
	}
}
// apply 已 >= confirmedIndex，现在读到的一定是线性一致的
nr.notify(nil)
```

多个并发读会等在不同的 `confirmedIndex` 上;apply 主循环一次 `Trigger(appliedi)` 就把所有「目标已被追平」的读一次性放行——这正是**按阈值一对多放行**的价值。这里 §3.2 的 `lastTriggerDeadline` 也在真实系统里救场:`AppliedIndex() < confirmedIndex` 的检查和随后的 `ApplyWait` 之间那道窗口,靠它兜底不阻塞。

### 其他消费者

| 消费方 | 位置 | 等什么 |
|--------|------|--------|
| 线性一致读追平 | `read/read.go:135` | `ApplyWait(confirmedIndex)` |
| 鉴权 token 校验 | `server.go:352` | `applyWait.Wait(index)`,等 auth 状态 apply 到位 |
| 等本地追平 commit | `v3_server.go:458` `waitAppliedIndex()` | `ApplyWaitCommit()` |
| Lease HTTP handler | `server.go:646` | `ApplyWaitCommit`,follower 处理前先追平 |

其中 `ApplyWaitCommit()`(`server.go:632`)= `applyWait.Wait(s.getCommittedIndex())`,语义是「等本地 apply 追上当前 committed index」。

## 五、wait / notify / WaitTime 三者对比

| | wait（按 ID） | notify（广播） | WaitTime（按阈值） |
|--|--------------|---------------|-------------------|
| 拓扑 | 一对一 | 一对多 | 一对多 |
| 唤醒条件 | 精确 ID 匹配 | 任意事件发生 | deadline `<=` 触发值 |
| 传数据 | 传 apply 结果 | 纯信号 | 纯信号 |
| 适用 | 写请求拿回**自己那条**结果 | leader 变更等**全局事件** | 等进度**追上某刻度** |

「等 apply 追上第 N 号」天生是一个**阈值语义**:不关心是谁、有几个在等,只要进度过线就一起放行。用 `wait` 得为每个 index 造 ID,用 `notify` 又表达不了「过线才放行」,唯有 `WaitTime` 贴合。三者合起来,正好覆盖 etcd 内部「精确路由 / 全局广播 / 阈值放行」三类等待需求。

## 关键源码位置索引

| 内容 | 位置 |
|------|------|
| WaitTime 实现 | `pkg/wait/wait_time.go`（Wait / Trigger / lastTriggerDeadline / 全局 closec）|
| `applyWait` 字段定义 | `server/etcdserver/server.go:248` |
| 初始化 `NewTimeList()` | `server.go:567` |
| 唯一生产者 `Trigger(appliedi)` | `server.go:978`（`applyAll`）|
| `ApplyWait` / `ApplyWaitCommit` 封装 | `interface.go:22`、`server.go:632` |
| 线性一致读追平消费 | `server/etcdserver/read/read.go:135` |
| `waitAppliedIndex` 消费 | `server/etcdserver/v3_server.go:458` |
