+++
title = "etcd pkg/wait：把异步共识包装成同步调用的「提案-等待模型」"
date = 2026-07-30
[taxonomies]
tags = ["etcd"]
+++

etcd 里所有写请求（Put / Delete / Txn / Compaction）对外都是一次普通的同步调用，但底层却是「把请求投进 Raft，等它在整个集群达成共识并 apply 到状态机」这样一个彻头彻尾的异步过程。中间隔着「谁来处理、什么时候处理完」的巨大不确定性。

etcd 用一个只有百来行的小包 `pkg/wait` 优雅地弥合了这道鸿沟。这篇文章从一个可独立运行的最小 demo 出发，讲清它的设计，再对照 etcd 源码看它在真实系统里怎么用。

<!-- more -->

## 一、pkg/wait 是什么

你发起一个异步操作（提交给 Raft、丢进队列、发个异步 RPC），但调用方希望**同步**拿到结果。`wait` 用一个 `ID -> channel` 的映射把两端接起来：

- 发起方 `Register(id)` 拿到一个 channel，然后阻塞等在上面；
- 处理完成方 `Trigger(id, result)` 按同一个 `id` 把结果塞进 channel，唤醒发起方。

一句话：**把「异步的处理过程」包装成「同步的 API 调用」。**

## 二、最小可运行实现

> 完整可运行源码：[golang/etcd/pkg-wait/main.go](https://github.com/spursy/spursy.github.io/blob/main/golang/etcd/pkg-wait/main.go)，克隆仓库后 `cd golang && go run ./etcd/pkg-wait` 即可执行。

下面是一个去掉 etcd 依赖、可以直接 `go run` 的等价实现。核心就是 `Register` / `Trigger` 两个方法。

```go
const defaultListElementLength = 64

type listElement struct {
	l sync.RWMutex
	m map[uint64]chan any
}

// minimalWait 提供按 ID 等待 / 触发事件的能力。
type minimalWait struct {
	e []listElement // 64 个分片，降低锁竞争
}

func newWait() *minimalWait {
	res := &minimalWait{e: make([]listElement, defaultListElementLength)}
	for i := 0; i < len(res.e); i++ {
		res.e[i].m = make(map[uint64]chan any)
	}
	return res
}

// Register 返回一个在给定 id 上等待的 channel。
// 注意：缓冲为 1 —— 这是「生产者与消费者时间解耦」的关键。
func (w *minimalWait) Register(id uint64) <-chan any {
	idx := id % defaultListElementLength
	newCh := make(chan any, 1)
	w.e[idx].l.Lock()
	defer w.e[idx].l.Unlock()
	if _, ok := w.e[idx].m[id]; !ok {
		w.e[idx].m[id] = newCh
	} else {
		log.Panicf("dup id %x", id)
	}
	return newCh
}

// Trigger 用给定 id 唤醒等待的 channel，并传入结果 x。
func (w *minimalWait) Trigger(id uint64, x any) {
	idx := id % defaultListElementLength
	w.e[idx].l.Lock()
	ch := w.e[idx].m[id]
	delete(w.e[idx].m, id)
	w.e[idx].l.Unlock()
	if ch != nil {
		ch <- x // 缓冲为 1：即使还没人接收也不阻塞，立即返回
		close(ch)
	}
}
```

用它包一个「内部异步、对外同步」的服务：

```go
// Do 是一个同步 API：内部是异步的，但调用方感觉是同步的。
func (s *Server) Do(data string) (Result, error) {
	id := atomic.AddUint64(&s.reqID, 1)

	ch := s.w.Register(id)               // ① 注册，拿到等待 channel
	s.taskCh <- task{id: id, data: data} // ② 把任务丢给异步处理者

	select {
	case x := <-ch: // ③ 阻塞等待被唤醒
		return x.(Result), nil
	case <-time.After(3 * time.Second): // 超时兜底，避免永久泄漏
		return Result{}, fmt.Errorf("request %d timeout", id)
	}
}

// worker 是后台处理者：处理完后按 id 唤醒对应的等待者。
func (s *Server) worker() {
	for t := range s.taskCh {
		result := Result{Value: "processed: " + t.data}
		s.w.Trigger(t.id, result) // ④ 用同一个 id 回传结果
	}
}
```

并发调用时，每个请求都能凭自己唯一的 `id` **精确拿到属于自己的那一条结果**，互不串扰。

## 三、几个关键设计

### 1. 精确的一对一唤醒

以请求 `id` 为 key，`Trigger` 只唤醒对应的那个等待者，天然「一对一、恰好一次」。`close(ch)` 后从 map 删除，重复 `Trigger` 变成无操作。

### 2. 缓冲为 1 —— 生产者与消费者时间解耦（最精妙处）

`Register` 创建的是 `make(chan any, 1)`，`Trigger` 里 `ch <- x` 之后立刻 `close`。

缓冲为 1 保证：**即使发起方还没来得及 `<-ch`，`Trigger` 也不会阻塞、能立即返回。** 这样一个慢的 / 超时的 / 已经离开的发起方，永远无法阻塞负责唤醒的关键路径。在 etcd 里那条关键路径就是 **apply 主循环**——它是全局单点、串行执行的，绝不能被任何一个客户端拖住。缓冲 1 从根上杜绝了「一个慢客户端拖垮整个状态机」。

### 3. 分片锁降低竞争

内部用 64 个 `(RWMutex + map)` 分片，`id % 64` 定位分片，高并发注册 / 唤醒时锁竞争小。

### 4. close 兜底不泄漏

`Trigger` 写入后 `close(ch)`。即使发起方因超时提前离开，值静静躺在缓冲区，channel 无引用后被 GC 回收，不阻塞、不泄漏。

### 使用要点 / 坑

- **id 必须唯一**：重复 `Register` 同一 id 会 panic。
- **必须配超时**：处理方若永不 `Trigger`，等待方会永久阻塞、channel 泄漏在 map 里。etcd 靠 context 超时 + 提案失败时主动 `Trigger` 兜底。
- **只能唤醒一次**：`close(ch)` 后再 `Trigger` 同 id 是无操作。

## 四、在 etcd 中的真实用法

一句话概括：etcd 用 `pkg/wait` 实现**提案-等待模型（Propose-and-Wait）**——凡是「一个写请求要拿回**自己那一条**处理结果」的场景，都用它。

入口在 `server/etcdserver/v3_server.go`，`Put()` / `Txn` / `Compaction` 等对外 API 全部收敛到 `processInternalRaftRequestOnce()`。真实调用流程（对照源码）：

```text
① id := s.reqIDGen.Next()          // 生成全局唯一请求 ID（:1067）
② ch := s.w.Register(id)           // 先注册信箱，拿到缓冲为 1 的 channel（:1106）
③ s.r.Propose(cctx, data)          // 把请求作为提案投入 Raft，立即返回（:1113）
④ select {                         // 阻塞三选一（:1122）
     case x := <-ch:               //   正常拿到 apply 结果
     case <-cctx.Done():           //   超时 / 客户端取消
     case <-s.done:                //   服务停止
   }
```

唤醒方是 apply 主循环：把某条日志 apply 到状态机（走 UberApplier 责任链、写 MVCC）后，调用 `s.w.Trigger(id, result)` 按 ID 精确唤醒对应的发起方 goroutine。

### 源码里体现的关键设计

**先 Register 再 Propose（顺序不能反）。** 若先 Propose，Raft 可能极快 apply 完并 Trigger，此时还没 Register，结果就丢了。先建好信箱再发提案，杜绝竞态。

**等待 channel 缓冲为 1。** apply 是全局单点串行循环，绝不能被任何一个慢的 / 超时的发起方卡住。缓冲 1 让 `Trigger` 里 `ch <- x` 扔下结果立即返回，生产者与消费者时间解耦。这正是 demo 里第 2 点在真实系统里的价值所在。

**超时 + 主动清理（GC wait）。** 提案失败或请求超时时，发起方主动调 `s.w.Trigger(id, nil) // GC wait`。目的不是唤醒自己（自己要 return 了），而是让 `Trigger` 内部 `delete(m, id)` 把信箱从 map 删除，避免永久泄漏。**谁注册谁兜底清理。**

**三路 select 保证任何异常都能退出。** 正常结果 / 超时取消 / 服务停止，goroutine 永不永久阻塞。

## 五、为什么用 wait 而不是广播（notify）

| | 写请求（wait） |
|--|--------------|
| 拓扑 | 一对一：每个请求 ID 对一个等待者 |
| 传结果 | 传，`Trigger(id, result)` 带 apply 结果 |
| 原因 | 每个写要精确拿回**自己那条**的结果，靠唯一 ID 路由 |

如果是「leader 变更、新任期首次提交」这类**全局事件、需要一次唤醒一批不确定数量的等待者**的场景，etcd 用的则是另一个包 `pkg/notify` 做一对多广播。二者的边界很清晰：需要精确路由到某一个、并回传数据，用 `wait`；只是纯信号广播给所有人，用 `notify`。

## 关键源码位置索引

| 内容 | 位置 |
|------|------|
| wait 实现 | `pkg/wait/wait.go`（Register / Trigger / 64 分片锁）|
| 写请求入口 | `server/etcdserver/v3_server.go:295` `Put()` |
| 提案-等待核心 | `server/etcdserver/v3_server.go:1058` `processInternalRaftRequestOnce()` |
| Register 注册信箱 | `v3_server.go:1106` |
| Propose 投提案 | `v3_server.go:1113` |
| 阻塞等待 select | `v3_server.go:1122` |
| GC wait 清理 | `v3_server.go:1116, 1128` |
