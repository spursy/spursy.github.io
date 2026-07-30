+++
title = "etcd pkg/notify：用 channel 实现一对多广播"
date = 2026-07-30
description = "从一个可独立运行的 demo 出发，讲清 etcd pkg/notify 如何用 close(channel) 做可重复的一对多广播，以及它在线性一致读里的真实用途。"
[taxonomies]
tags = ["etcd"]
+++

上一篇讲的 `pkg/wait` 是**一对一**：一个 ID 精确对应一个等待者，用来把「异步共识」包装成「同步调用」。但 etcd 内部还有另一类需求——**一个全局事件发生，要同时唤醒一批不确定数量的等待者，且不关心它们是谁**。这时用的是另一个更小的包 `pkg/notify`。

它只有几十行，核心就一句话：**关闭一个 channel 天然就是「广播」**。这篇文章从一个可独立运行的最小 demo 出发，讲清它的设计，再对照 etcd 源码看它在线性一致读里怎么用。

<!-- more -->

## 一、pkg/notify 是什么

`wait` 是一对一（一个 id 对一个等待者）；`notify` 是**一对多广播**：一个事件发生，同时唤醒所有正在等的 goroutine，且不关心它们是谁、有几个。

核心机制利用了 Go 的一个特性——**`close(ch)` 会让所有 `<-ch` 立即返回**：

- `Receive()` 返回当前 channel，消费者 `<-ch` 阻塞等在上面；
- `Notify()` **先创建一个新 channel 替换旧的，再 `close` 旧 channel** —— 所有等在旧 channel 上的 goroutine 被同时唤醒；下一轮等待者拿到的是新 channel，因此可以重复广播。

一句话：**关闭 channel 就是广播，用完即换新的以实现可重复广播。**

## 二、最小可运行实现

> 完整可运行源码：[golang/etcd/pkg-notify/main.go](https://github.com/spursy/spursy.github.io/blob/main/golang/etcd/pkg-notify/main.go)，克隆仓库后 `cd golang && go run ./etcd/pkg-notify` 即可执行。

下面是一个去掉 etcd 依赖、可以直接 `go run` 的等价实现，逻辑与 `go.etcd.io/etcd/pkg/v3/notify` 一致。核心就是 `Receive` / `Notify` 两个方法。

```go
// Notifier 是一个线程安全的结构，用于向多个消费者广播某个事件的发生。
type Notifier struct {
	mu      sync.RWMutex
	channel chan struct{}
}

func NewNotifier() *Notifier {
	return &Notifier{channel: make(chan struct{})}
}

// Receive 返回一个可用于等待通知的 channel。
// 消费者通过「channel 被关闭」这一事件得到通知。
func (n *Notifier) Receive() <-chan struct{} {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.channel
}

// Notify 关闭当前传给消费者的 channel（广播唤醒所有等待者），
// 并创建一个新 channel 供下一次通知使用。
func (n *Notifier) Notify() {
	newChannel := make(chan struct{})
	n.mu.Lock()
	channelToClose := n.channel
	n.channel = newChannel
	n.mu.Unlock()
	close(channelToClose) // 关闭旧 channel：所有 <-ch 同时返回
}
```

用它做「一次广播唤醒所有消费者」：

```go
n := NewNotifier()

var wg sync.WaitGroup
for i := 0; i < 3; i++ {
	wg.Add(1)
	go func(id int) {
		defer wg.Done()
		<-n.Receive() // 阻塞，直到被广播唤醒
		fmt.Printf("consumer %d awakened\n", id)
	}(i)
}

time.Sleep(100 * time.Millisecond) // 等消费者都进入等待
n.Notify()                         // 一次调用，3 个消费者同时醒来
wg.Wait()
```

只调用一次 `Notify()`，3 个消费者同时醒来——**无需知道有几个消费者**，这就是「一对多广播」的价值。

多轮周期性广播时，消费者每轮都要重新 `Receive()`：

```go
go func() {
	for i := 0; i < 3; i++ {
		<-n2.Receive() // 每轮都要重新 Receive 拿新 channel
		fmt.Printf("tick %d\n", i+1)
	}
}()

for i := 0; i < 3; i++ {
	time.Sleep(200 * time.Millisecond)
	n2.Notify() // 每 200ms 广播一次
}
```

## 三、几个关键设计

### 1. 一对多广播

一次 `Notify()` 同时唤醒所有等待者，无需知道有几个消费者。比手动给每个消费者发消息、或用 `sync.Cond` 逐个 `Broadcast` 都更简单——因为 `close(channel)` 本身就是「一次操作、全体收到」。

### 2. 可重复通知——先换新再关闭旧

`Notify()` 每次都**先建一个新 channel 替换 `n.channel`，再 `close` 旧的**。顺序很关键：新等待者在旧 channel 被关闭前就已经拿到了新 channel，因此这一轮的关闭只影响「已经等在旧 channel 上的人」，不会误伤下一轮。这样就能一轮又一轮地广播（周期性放行、状态反复变更）。

### 3. 零内存信号

用 `chan struct{}`，只传「事件发生了」这个信号本身，不占内存。`context.Done()` 底层就是同款思路。

### 4. 线程安全

内部用 `RWMutex` 保护 channel 的读取与替换：`Receive()` 用读锁（可并发），`Notify()` 用写锁（替换指针）。`Receive` / `Notify` 都可被多个 goroutine 并发调用。

### 使用要点 / 坑

- **消费者每轮必须重新 `Receive()`**：`Notify` 换了新 channel，还拿着旧 channel 的会错过下一次。
- **不携带数据**：只传信号，要传数据得另配一个受锁保护的共享变量。
- **边沿触发而非电平触发**：`Notify` 时若当下没人在 `Receive`，这次广播就「错过」了。适合「周期性放行」而非「必达消息」。

## 四、在 etcd 中的真实用法

一句话概括：凡是「一个**全局状态事件**发生，需要一次性唤醒一批**不确定数量**的等待者，且不必区分它们是谁」的场景，etcd 就用 `pkg/notify` 做**一对多广播**。核心服务对象是**线性一致读**。

`server/etcdserver/server.go` 里创建了 3 个 `notify.Notifier`：

| Notifier | 何时 `Notify()`（广播） | 谁在 `Receive()`（等待） | 作用 |
|----------|------------------------|--------------------------|------|
| `leaderChanged`(:236) | 本节点当选新 leader 时(:789) | 线性一致读循环 | leader 变了 → **丢弃旧读请求** |
| `firstCommitInTerm`(:282) | 新任期首次提交时(:1949) | ReadIndex 逻辑(:2207) | 可以**重发 ReadIndex** 了 |
| `clusterVersionChanged`(:283) | 集群版本变化时 | 版本监控循环(:2231) | 集群版本变了 |

### 场景 1：leaderChanged —— 服务线性一致读

线性一致读用 ReadIndex 机制：读之前先向 leader 确认「当前 commit 到哪」，等本地 apply 追上再读；多个并发读**一起等同一次确认**。

问题在于：若等待期间 **leader 切换**，基于旧 leader 的确认全部作废，必须**立刻唤醒这批旧读请求并让它们失败重试**，否则可能读到不一致数据。

`server/etcdserver/read/read.go:98` 的消费方：

```go
leaderChangedNotifier := r.server.LeaderChanged() // = s.leaderChanged.Receive()
select {
case <-leaderChangedNotifier: // 被广播唤醒
	continue                  // 丢弃本轮、重来
// ...
}
```

`server.go:789` 当选新 leader 时广播：

```go
if newLeader {
	s.leaderChanged.Notify() // 一次广播，唤醒所有等待的读
}
```

一次 `Notify()` 同时叫醒所有阻塞的读逻辑，无需知道有几个——这正是 demo 里第 1 点在真实系统里的价值。

### 场景 2：firstCommitInTerm —— 触发 ReadIndex 重发

新 leader 在提交本任期第一条日志前，ReadIndex 返回值不可靠。读逻辑要等到「本任期首次提交」事件后才重发 ReadIndex。

`read.go:194`：

```go
case <-firstCommitInTermNotifier:
	firstCommitInTermNotifier = r.server.FirstCommitInTermNotify() // 重新 Receive 拿新 channel
	// ... 重发 ReadIndex
```

`server.go:1949` 首次提交时 `s.firstCommitInTerm.Notify()` 广播。这里完美示范了 demo 第 2 点的**可重复通知**：消费者被唤醒后立刻重新 `Receive()`，为下一任期做准备。

### 一个重要对照：read 包里自定义的带 error notifier

`server/etcdserver/read/util.go` 定义了一个**自己的** notifier：

```go
type notifier struct {
	c   chan struct{}
	err error // ← 比 pkg/notify 多一个 err 字段
}
func (nc *notifier) notify(err error) { nc.err = err; close(nc.c) }
```

它用于「读完成结果」通知，因为需要**顺带传一个 err**；`pkg/notify` 则用于 leaderChanged / firstCommitInTerm 这类**纯信号、不带数据**的广播。两者思路完全一致（都靠 `close(channel)` 广播），区别只是要不要携带数据。

## 五、为什么用 notify 而不是 wait

| | 写请求（wait） | 这些内部事件（notify） |
|--|--------------|------------------------|
| 拓扑 | 一对一，按 ID 精确唤醒 | 一对多，广播全部 |
| 传数据 | 传 apply 结果 | 只传信号 |
| 原因 | 每个写要拿回**自己那条**结果 | leader 变 / 新任期是全局事件，所有读都得知道，无需区分是谁 |

边界很清晰：需要精确路由到某一个、并回传数据，用 `wait`；只是把纯信号一次广播给所有人，用 `notify`。

## 关键源码位置索引

| 内容 | 位置 |
|------|------|
| notify 实现 | `pkg/notify/notify.go`（Receive / Notify）|
| 3 个 Notifier 定义 | `server/etcdserver/server.go:236, 282, 283` |
| `leaderChanged.Notify()` | `server.go:789` |
| `firstCommitInTerm.Notify()` | `server.go:1949` |
| 线性一致读循环消费 | `server/etcdserver/read/read.go:98` |
| ReadIndex 重发消费 | `server/etcdserver/read/read.go:194` |
| 带 error 的自定义 notifier | `server/etcdserver/read/util.go` |
