+++
title = "etcd pkg/schedule.FIFO：把并发提交收敛成串行执行的单 goroutine 调度器"
date = 2026-08-04
description = "从一个可独立运行的 demo 出发，拆解 etcd pkg/schedule 的 FIFO 串行调度器：它如何用『单消费者 goroutine + resume 边沿唤醒 + sync.Cond 完成等待 + 优雅停机』把来自多方的并发提交，收敛成一条严格有序、不阻塞提交方的执行流，以及它如何支撑 raft apply 与 MVCC compaction。"
[taxonomies]
tags = ["etcd"]
[extra]
toc = true
+++

前几篇拆的是 etcd 的并发原语(`wait` / `WaitTime` / `notify`)、无锁 ID(`idutil`)、页对齐 IO(`PageWriter`)和卡顿探测器(`contention`)。这篇看一个"调度"模块:`pkg/schedule` 里的 **FIFO 串行调度器**。

它解决的问题很具体:**一批任务必须严格按提交顺序、一个接一个串行执行,但又不想让提交方被执行耗时阻塞。** raft 日志的 apply、MVCC 的 compaction,都是这种「既要串行正确、又不能卡住上游」的场景。这篇从它的实现讲起,看它怎么用几个 channel + 一把锁 + 一个 `sync.Cond` 干净地满足这组看似矛盾的需求。

<!-- more -->

## 一、它承诺什么

一句话:**把来自多方的并发提交,收敛成单 goroutine 的串行执行。**

- **严格 FIFO 串行**:所有 `Schedule` 进来的 Job,严格按提交先后、一个接一个执行,永不并发。于是 **Job 内部完全不用担心并发安全**。
- **提交方不阻塞**:`Schedule` 只是把 Job 塞进队列就立即返回,执行由后台 goroutine 异步完成。提交"接收下一批"和"执行当前批"在时间上解耦。
- **可优雅停机**:`Stop()` 会把队列里剩余的任务清理跑完,再确认后台 goroutine 真正退出。

## 二、核心数据结构

```go
type fifo struct {
	mu sync.Mutex

	resume    chan struct{} // 「空闲→忙碌」的边沿唤醒信号(缓冲为 1)
	scheduled int           // 已开始调度(取出执行)的任务数
	finished  int           // 已完成任务数
	pendings  []Job         // 待办队列(FIFO 核心)

	ctx    context.Context    // 传给每个 Job 的 Do(ctx);停机时被 cancel
	cancel context.CancelFunc // 触发停机;兼作"是否已停"的哨兵

	finishCond *sync.Cond    // 完成条件的复合等待(WaitFinish)
	donec      chan struct{} // run() 退出时 close,让 Stop 同步等待
}
```

构造时启动**唯一**的后台 goroutine——串行的根源就在这:

```go
func NewFIFOScheduler() Scheduler {
	f := &fifo{
		resume: make(chan struct{}, 1),
		donec:  make(chan struct{}, 1),
	}
	f.finishCond = sync.NewCond(&f.mu)          // 复用同一把锁
	f.ctx, f.cancel = context.WithCancel(context.Background())
	go f.run()                                  // 唯一消费者
	return f
}
```

> 完整可运行源码：[golang/etcd/pkg-schedule/main.go](https://github.com/spursy/spursy.github.io/blob/main/golang/etcd/pkg-schedule/main.go)，克隆仓库后 `cd golang && go run ./etcd/pkg-schedule` 即可执行。

## 三、四个核心机制

### 1. 单消费者 goroutine → 串行的根源

只有一个 `run()` goroutine 会执行 Job,任务之间天然串行:

```go
func (f *fifo) run() {
	defer func() {
		close(f.donec)
		close(f.resume)
	}()

	for {
		var todo Job
		f.mu.Lock()
		if len(f.pendings) != 0 {
			f.scheduled++
			todo = f.pendings[0] // 取队头 → FIFO
		}
		f.mu.Unlock()

		if todo == nil {
			select {
			case <-f.resume:     // 队列空 → 睡眠等唤醒
			case <-f.ctx.Done(): // 停机 → 清理剩余 pending 后退出
				// ... 见机制 4
			}
		} else {
			f.executeJob(todo, false)
		}
	}
}
```

从 `pendings[0]` 取队头、`Schedule` 往尾部 `append`——**先进先出**就靠这一头一尾。因为只有这一个 goroutine 在跑 Job,「apply 顺序 == 提交顺序」是免费保证的。

### 2. resume channel:边沿唤醒,不忙等也不阻塞生产者

队列空时,消费者不能空转烧 CPU,得睡;有活来了要被叫醒。这个"睡眠-唤醒"由 `resume` 承担,而且用了一个精妙的**边沿触发**技巧:

```go
func (f *fifo) Schedule(j Job) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.cancel == nil {
		panic("schedule: schedule to stopped scheduler")
	}

	// 只在「队列从空变非空」时才发唤醒信号
	if len(f.pendings) == 0 {
		select {
		case f.resume <- struct{}{}:
		default: // 已有信号在缓冲里,不重复发,且不阻塞生产者
		}
	}
	f.pendings = append(f.pendings, j)
}
```

三个细节合起来才完整:

- **只在「空→非空」的边沿发信号**:队列本来就有活时,`run` 正忙着跑,根本不需要叫醒,发了也是浪费。
- **缓冲为 1**:最多存一个"待唤醒"信号,足够表达"有活了"这一个事实。
- **`select` + `default` 非阻塞发送**:缓冲已满(已有信号)就走 `default` 跳过——**生产者永远不会被 `Schedule` 阻塞**。

这就是典型的"边沿触发唤醒"模式:用一个容量 1 的 channel 表达"从空闲变忙碌"这一次状态跃迁。

### 3. sync.Cond:等待"完成数达标 且 队列已空"的复合条件

`WaitFinish(n)` 要等的不是单个事件,而是**两个变量的组合谓词**:"至少 n 个完成,且没有 pending 剩余"。这种复合条件等待,正是 `sync.Cond` 的主场:

```go
func (f *fifo) WaitFinish(n int) {
	f.finishCond.L.Lock()
	for f.finished < n || len(f.pendings) != 0 {  // for 循环复查复合条件
		f.finishCond.Wait()
	}
	f.finishCond.L.Unlock()
}
```

每完成一个 Job 就 `Broadcast` 唤醒所有等待者重新判断(见下一节的 `executeJob`)。用 `Cond` 而非 channel,是因为等待的是"计数 + 队列长度"两个受锁保护变量的组合——`Cond`(锁 + `for` 复查)表达这种谓词最自然。注意 `sync.NewCond(&f.mu)` **复用了同一把 `mu`**,保证"检查条件"和"改变条件"在同一把锁下同步。

### 4. 优雅停机:cancel 发信号,donec 确认退出

`Stop` 分两步——先发停止信号,再同步等待后台真正退出:

```go
func (f *fifo) Stop() {
	f.mu.Lock()
	f.cancel()      // 触发 ctx.Done()
	f.cancel = nil  // 置 nil:之后再 Schedule 会 panic(防止向已停调度器提交)
	f.mu.Unlock()
	<-f.donec       // 阻塞等 run() 真正退出
}
```

`run()` 收到 `ctx.Done()` 后,会把队列里没跑完的任务**带着 cancelled 的 ctx 清理跑一遍**,再退出:

```go
case <-f.ctx.Done():
	f.mu.Lock()
	pendings := f.pendings
	f.pendings = nil
	f.mu.Unlock()
	for _, todo := range pendings {
		f.executeJob(todo, true) // 清理剩余 pending
	}
	return                       // run 退出 → defer close(donec)
```

`<-f.donec` 是关键的**同步点**:`f.cancel()` 只是异步发信号,`run` 不会瞬间停;`run` 退出时 `defer close(f.donec)`,而"从已关闭 channel 接收会立即返回"这一特性,让 `Stop()` 阻塞到 `run` 真正结束才返回。于是 **`Stop()` 返回时,后台 goroutine 一定已彻底退出、剩余任务已清理完毕**——把"发信号"升级成了"确认停下来了"。

## 四、两个健壮性细节

**① 执行完才出队。** 出队动作 `pendings = pendings[1:]` 放在 `executeJob` 的 `defer` 里,而不是取出时:

```go
func (f *fifo) executeJob(todo Job, updatedFinishedStats bool) {
	defer func() {
		if !updatedFinishedStats {
			f.finishCond.L.Lock()
			f.finished++
			f.pendings = f.pendings[1:] // ← 执行完才出队
			f.finishCond.Broadcast()
			f.finishCond.L.Unlock()
		}
		if err := recover(); err != nil {
			fmt.Printf("execute job %q failed: %v\n", todo.Name(), err)
		}
	}()
	todo.Do(f.ctx)
}
```

为什么?这样"**正在执行的任务仍算 pending**",`WaitFinish` 里 `len(pendings)!=0` 的判断才准确——不会把还在跑的任务误算成已完成而提前放行。

**② recover 兜底。** 单个 Job `panic` 被 `recover` 接住,只记录不外抛,**不会打挂整个调度 goroutine**。否则一个坏任务就能让后续所有任务永远排不上队。

## 五、跑个 demo 看看

配套 demo 的两个场景直观展示了「串行」和「优雅停机」:

```
=== demo1: concurrent submit, FIFO serial execution ===
executed job-0
executed job-1
executed job-2
executed job-3
executed job-4
execution order = [0 1 2 3 4] (== submit order, serial)
pending=0 scheduled=5 finished=5

=== demo2: Stop drains pending jobs ===
before stop: pending=3
slow job done
queued-1 executed (drained on stop)
queued-2 executed (drained on stop)
scheduler stopped
```

demo1 里 5 个任务**故意让耗时递减**(先提交的睡得更久),但执行顺序仍是严格的 `[0 1 2 3 4]`——证明是串行按提交序,而非按完成快慢。demo2 里 `Stop()` 把堆积的 3 个 pending 任务全部清理跑完才返回。

## 六、在 etcd 里的两大用途

全 etcd 里,这个 FIFO 调度器主要服务两个对象。

### 场景 1:apply raft 日志到状态机(最重要)

`server/etcdserver/server.go` 的 `run()` 主循环:

```go
sched := schedule.NewFIFOScheduler(lg)          // :764
for {
	select {
	case ap := <-s.r.apply():                   // 从 raft 拿到一批待 apply 的日志
		f := schedule.NewJob("server_applyAll", func(context.Context) {
			s.applyAll(&ep, &ap)
		})
		sched.Schedule(f)                       // :844 入队即返回,去收下一批
	}
}
```

意图完全对上前面四个机制:apply 要写 bbolt、走 applier 责任链,**很慢**;主循环只管 `Schedule` 入队、马上回去接收 raft 的下一批,不被 apply 卡住。真正的 apply 由单 goroutine 一个接一个执行,天然保证 **apply 顺序 == raft 日志顺序**——而顺序错了就是状态机不一致的致命 bug。

### 场景 2:MVCC compaction

`server/storage/mvcc/kvstore.go` 里,store 持有一个 `fifoSched`,把每次压缩包成 Job 入队:

```go
j := schedule.NewJob("kvstore_compact", func(ctx context.Context) {
	if ctx.Err() != nil {          // 调度器已停 → 走 barrier 收尾
		s.compactBarrier(ctx, ch)
		return
	}
	hash, err := s.scheduleCompaction(rev, prevCompactRev) // 删旧版本
	// ...
	close(ch)                      // 通知调用方压缩完成
})
s.fifoSched.Schedule(j)
```

- **压缩必须串行**:并发压缩同一份 MVCC 数据会互相干扰,排队保证一次只压一个。
- **不阻塞提交方**:`compact()` 入队后立即返回一个 `ch`,调用方 `<-ch` 异步等结果。
- **compactBarrier 的巧思**:当调度器正在关闭(`ctx.Err()!=nil`),压缩不能直接跑,于是**重新 `Schedule` 一个 barrier 塞进同一条 FIFO 队列**,借「串行 + 停止时清空」的语义保证收尾 channel 一定被处理,不泄漏。

## 七、为什么不直接 `go func()`

| | 直接 `go func()` | pkg/schedule FIFO |
|--|-----------------|-------------------|
| 顺序 | 无序、并发 | 严格 FIFO 串行 |
| 提交方 | 不阻塞但失控 | 不阻塞且可控(入队即返回) |
| 停止 | 难以统一 drain | `Stop()` 统一清理剩余任务 |
| 崩溃隔离 | 一个 panic 可能没人管 | `recover` 兜底,不打挂调度 goroutine |

apply 和 compaction 都要求「**又要串行正确、又不能阻塞上游**」,这正是 FIFO 调度器存在的理由:把并发提交收敛成一条有序、可控、可优雅停机的执行流。

## 八、总结

`pkg/schedule` 的 FIFO 调度器用四个机制满足了一组看似矛盾的需求:

- **单消费者 goroutine** → 串行正确(顺序 == 提交序,Job 无需关心并发);
- **resume 边沿唤醒** → 提交方不阻塞、消费者不忙等;
- **sync.Cond** → 干净地等待"完成数达标 + 队列清空"的复合条件;
- **cancel + donec** → 优雅停机,发信号并同步确认后台退出。

再加上"执行完才出队"和"recover 兜底"两个细节,它成了一个"必须有序、串行、可优雅停机"后台任务的模板级实现。理解它,也就理解了 etcd 是怎么把 raft apply 这条命脉,做到既严格有序又不拖慢主循环的。

## 关键源码位置索引

| 内容 | 位置 |
|------|------|
| 调度器实现（run / Schedule / resume / Stop） | `pkg/schedule/schedule.go` |
| apply 调度器创建 | `server/etcdserver/server.go:764` |
| apply Job 入队 | `server/etcdserver/server.go:844` |
| apply 调度器停止 | `server/etcdserver/server.go:822` `sched.Stop()` |
| MVCC 调度器字段 | `server/storage/mvcc/kvstore.go:76` `fifoSched` |
| compaction Job | `server/storage/mvcc/kvstore.go:236` |
| compactBarrier 重入队 | `server/storage/mvcc/kvstore.go:147, 201` |
