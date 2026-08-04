+++
title = "etcd pkg/contention.TimeoutDetector：把「监控」抽象成几十行可复用小工具"
date = 2026-08-04
description = "从 raft 那句经典告警『leader is overloaded likely from slow disk』出发，拆解 etcd 里只有几十行的通用卡顿探测器 TimeoutDetector：它如何用一个 map 回答『本该周期性发生的事，是不是被拖慢了』，以及它在心跳循环中如何按 follower 分桶精确定位慢节点。"
[taxonomies]
tags = ["etcd"]
[extra]
toc = true
+++

前几篇拆的是 etcd 的并发原语(`wait` / `WaitTime` / `notify`)、无锁 ID(`idutil`)和页对齐 IO(`PageWriter`)。这篇挑一个更小的模块:`pkg/contention` 里只有 **70 行**的 `TimeoutDetector`。它不解决任何业务逻辑,而是一个纯粹的**可观测性小工具**——用来回答一个看似简单却很通用的问题:**本该周期性发生的事,是不是被拖慢了?**

如果你在生产环境跑过 etcd,大概率见过这条告警:

```
leader failed to send out heartbeat on time; took too long,
leader is overloaded likely from slow disk
```

这条日志的背后,就是 `TimeoutDetector`。这篇从它讲起。

<!-- more -->

## 一、它要解决什么问题

很多系统里有「**本应固定间隔发生的事**」:

- raft leader 每隔一个 `heartbeat` 间隔,就该给每个 follower 发一次心跳;
- 某个后台任务每隔 N 秒该 tick 一次;
- 某个上报循环每分钟该上报一次指标……

我们想知道:**这些事有没有迟到?** 尤其是 raft 心跳——如果 leader 因为**慢磁盘 fsync、CPU 饥饿、GC 停顿**被拖住,心跳循环没能准点跑,follower 收不到心跳就会发起选举,导致 leader 抖动、集群可用性下降。「心跳迟到」是这类故障的**早期征兆**,值得被探测并告警。

`TimeoutDetector` 就是回答这个问题的通用工具。它的设计有两个关键取舍:

1. **它完全不知道「心跳」是什么**。它只处理抽象的「带 id 的周期事件」——任何「本应固定间隔发生的事」都能用它探测。这种「不耦合具体业务」的抽象,是它能成为可复用小工具的原因。
2. **零后台开销**。没有 goroutine、没有定时器,只在被观测的那一刻做一次减法。状态就是一个 map。

## 二、全部实现:一个 map + 一次减法

整个类型只有三个字段:

```go
type TimeoutDetector struct {
	mu          sync.Mutex           // 保护以下所有字段
	maxDuration time.Duration        // 预期的最大间隔(阈值)
	records     map[uint64]time.Time // id → 该 id 上次事件的时间
}
```

精华全在 `Observe` 一个方法里:

```go
func (td *TimeoutDetector) Observe(id uint64) (bool, time.Duration) {
	td.mu.Lock()
	defer td.mu.Unlock()

	ok := true
	now := time.Now()
	exceed := time.Duration(0)

	if pt, found := td.records[id]; found {
		exceed = now.Sub(pt) - td.maxDuration // 实际间隔 - 阈值
		if exceed > 0 {
			ok = false
		}
	}
	td.records[id] = now // 更新「上次时间」,为下次比较做准备
	return ok, exceed
}
```

逐步拆解:

1. 取出该 `id` 的「上次时间」`pt`。**若从没见过这个 id(首次)**,`found == false`,跳过比较,直接返回 `ok=true`——没有「上一次」就无从判断迟到,一律放行。
2. 若见过,算 `now - pt`(相邻两次同 id 事件的真实间隔),再减去阈值 `maxDuration`。**`exceed > 0` 就说明迟到了**,返回 `ok=false` + 迟到量。
3. 无论如何,都把 `records[id]` 更新为 `now`,为下一次比较建立新基线。

还有一个 `Reset`,清空所有历史:

```go
func (td *TimeoutDetector) Reset() {
	td.mu.Lock()
	defer td.mu.Unlock()
	td.records = make(map[uint64]time.Time)
}
```

什么时候需要清空?当「周期性前提」不再成立的时候——下一节会看到,raft 里 leader 角色一变,历史时间戳就失去意义,必须 `Reset` 避免误报。

> 完整可运行源码：[golang/etcd/pkg-contention/main.go](https://github.com/spursy/spursy.github.io/blob/main/golang/etcd/pkg-contention/main.go)，克隆仓库后 `cd golang && go run ./etcd/pkg-contention` 即可执行。

## 三、几个容易踩的点

**① 首次 Observe 永远返回 `ok=true`。** 这不是 bug,是设计——没有「上一次」就没法比较。所以用它监控时,第一个周期永远不会告警,从第二个周期起才开始判断。

**② 它测的是「相邻两次同 id 事件的间隔」,不是「某个动作的耗时」。** 如果你想测「一次 fsync 花了多久」,这个工具不合适(那得在动作前后各记一次时间)。它天生适合「**周期性事件是否迟到**」这一类场景。

**③ `records` 只增不减。** 见过的 id 会一直留在 map 里。如果 id 空间很大且程序长期运行,map 会持续增长——需要靠 `Reset()` 或外部约束控制内存。在 etcd 里,id 是集群成员数(通常个位数),所以完全无忧。

**④ 并发安全由内部 `mu` 保证**,可以多 goroutine 直接 `Observe`。

## 四、在 etcd 里的真实用法

全 etcd 里 `TimeoutDetector` 只有一个使用者——`server/etcdserver/raft.go` 的 raft 心跳循环。

### 构造:阈值 = 2 个心跳间隔

```go
// server/etcdserver/raft.go:143
// set up contention detectors for raft heartbeat message.
// expect to send a heartbeat within 2 heartbeat intervals.
td: contention.NewTimeoutDetector(2 * cfg.heartbeat),
```

注意阈值取的是 `2*heartbeat` 而非 `1*heartbeat`——**「宽容一倍」**,给正常的调度抖动留一点余量,避免一有波动就告警。这个阈值语义由**调用方**决定,工具本身不写死任何数字,这也是它通用的体现。

### 观测:以「心跳发给谁」作为事件 id

```go
// server/etcdserver/raft.go:385
if m.GetType() == raftpb.MsgHeartbeat {
	ok, exceed := r.td.Observe(m.GetTo())   // 以 follower 的 id 作为事件 id
	if !ok {
		r.lg.Warn(
			"leader failed to send out heartbeat on time; took too long, "+
			"leader is overloaded likely from slow disk",
			zap.String("to", fmt.Sprintf("%x", m.GetTo())),
			zap.Duration("heartbeat-interval", r.heartbeat),
		)
	}
}
```

关键在于**用 `m.GetTo()`(心跳的目标 follower)作为 id**,于是每个 follower 各有一条独立的时间线。这样告警能**精确到「是发给哪个节点的心跳慢了」**,而不是笼统地说「心跳慢了」。这就是「按 id 分桶」的价值。

### 重置:leadership 变化时清空

```go
// server/etcdserver/raft.go:206
rh.updateLeadership(newLeader)
r.td.Reset()   // 角色/leader 变了,历史时间戳失去意义,重新开始
```

当本节点不再是 leader、或刚成为 leader 时,之前记录的「上次发心跳时间」已经没有参考价值(可能是很久以前、也可能压根没发过),`Reset()` 清空历史、重新计时,避免误报。

## 五、跑个 demo 看看

配套 demo 用等价实现还原了上述行为。第二个 demo 直接模拟 raft 心跳场景:给三个 follower(A/B/C)建立基线后,让 A、C 准点、B 因「慢盘」卡住,观察谁被点名:

```
################ demo2: 模拟 raft 心跳,多 follower 分桶探测 ################
baseline: 已为 follower [0xa 0xb 0xc] 建立首次心跳时间
下一轮心跳:A/C 准点,B 因慢盘卡住 ...
  heartbeat to follower 0xa: on time ✓
  heartbeat to follower 0xc: on time ✓
  heartbeat to follower 0xb: LATE ✗ (exceeded 32ms) → leader overloaded likely from slow disk

结论:分桶计时让告警精确到「是发给哪个 follower 的心跳慢了」。
```

只有 B 被点名,A/C 正常——这正是「按 id 分桶」在真实告警里的效果。

## 六、设计巧思小结

| 手法 | 说明 |
|------|------|
| **监控抽象成可复用小工具** | 完全不知道「心跳」是什么,只处理抽象的「带 id 的周期事件」——任何固定间隔的事都能用 |
| **按 id 分桶** | 每个 follower 独立计时,精确定位是哪个节点的心跳慢了 |
| **相对判定 + 返回超出量** | 不只给 bool,还返回 `exceed`(迟到多少),便于日志量化 |
| **零后台开销** | 无 goroutine、无定时器,只在被 `Observe` 时算一次;状态就一个 map |
| **阈值由调用方决定** | `2*heartbeat` 的「宽容一倍」策略放在 raft 侧,工具不写死 |

70 行代码,却把一个通用的监控需求封装得干净利落。它提醒我们:**「监控」本身也可以被抽象成一个不依赖具体业务的小工具**——当你下次也想探测「某件周期性的事有没有迟到」时,不妨想想这个 map + 一次减法的模式。

## 关键源码位置索引

| 内容 | 位置 |
|------|------|
| `TimeoutDetector` 实现（Observe / Reset） | `pkg/contention/contention.go` |
| 包说明 | `pkg/contention/doc.go` |
| `td *contention.TimeoutDetector` 字段 | `server/etcdserver/raft.go:100` |
| 构造 `NewTimeoutDetector(2*heartbeat)` | `raft.go:143` |
| `Observe(m.GetTo())` + 告警日志 | `raft.go:385` |
| leadership 变化时 `td.Reset()` | `raft.go:206` |
