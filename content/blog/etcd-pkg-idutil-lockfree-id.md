+++
title = "etcd pkg/idutil：无锁、无协调的分布式唯一 ID"
date = 2026-08-03
description = "从一个可独立运行的 demo 出发，讲清 etcd idutil 如何用『位布局 + 一次原子自增 + 受控溢出』生成无锁、跨节点不撞、重启不撞的唯一 ID，以及它在写请求路径上如何和 pkg/wait 串成闭环。"
[taxonomies]
tags = ["etcd"]
[extra]
toc = true
+++

前几篇讲了 `pkg/wait` 家族——按 ID 精确唤醒的 `Wait`、按阈值批量放行的 `WaitTime`，还有广播的 `notify`。它们都有一个共同的前提：**每个写请求得先有一个唯一 ID**，才能在 `wait.Register(id)` 上登记、等 apply 完成后被精确唤醒。

这个 ID 从哪来?就是本篇的主角 `pkg/idutil`。它只有 40 行,却把「无锁、无中心协调、跨节点不撞、重启不撞、单调、极快」这一串看似矛盾的需求,用**一个 uint64 的位布局 + 一次原子自增**全部满足。这篇从可独立运行的 demo 出发讲清它的设计,再看它在写请求路径上怎么用。

<!-- more -->

## 一、idutil 要解决什么

etcd 每个写请求都需要一个唯一 ID。这个 ID 的约束很苛刻：

- **同节点内**:单调、不重复、生成极快——它在写热路径上,**不能用 mutex 抢一个全局计数器**。
- **跨节点之间**:天然不撞——多个 member 各自发号,**不做任何协调**(没有中心发号器、没有 RPC)。
- **重启之后**:不和重启前已发出的 ID 相同——否则老请求残留在 `wait` 上的登记,会被新 ID 误唤醒。

一句话:要一个**本地生成、无锁、无协调,却能全局唯一**的号。idutil 的答卷是把「唯一性」拆成三个正交维度,分别塞进一个 64 位整数的不同比特区间。

## 二、64 位布局

```
| prefix   | suffix                |
| 2 bytes  | 5 bytes    | 1 byte   |
| memberID | timestamp  | cnt      |
 高 2 字节 = memberID（节点隔离）
 中 5 字节 = 启动时刻的毫秒时间戳（重启隔离）
 低 1 字节 = 计数器（同节点内自增）
```

三段各司其职:

1. **memberID 前缀（高 2B）** → 解决**跨节点不撞**:不同节点前缀不同,值域天然不相交。
2. **毫秒时间戳（中 5B）** → 解决**重启不撞**:重启后时间前进,起点不同。5 字节毫秒 ≈ 2⁴⁰ ms ≈ **35 年**窗口,足够覆盖一个进程的生命周期。
3. **计数器（低 1B）** → 解决**同节点内递增**:每次 `Next()` 就 +1。

## 三、最小可运行实现

> 完整可运行源码：[golang/etcd/pkg-idutil/main.go](https://github.com/spursy/spursy.github.io/blob/main/golang/etcd/pkg-idutil/main.go)，克隆仓库后 `cd golang && go run ./etcd/pkg-idutil` 即可执行。

下面是去掉 etcd 依赖、可以直接 `go run` 的等价实现，逻辑与 `go.etcd.io/etcd/pkg/v3/idutil` 的 `id.go` 完全一致。核心只有 `NewGenerator` 和 `Next` 两个方法。

```go
const (
	tsLen     = 5 * 8          // 时间戳占 5 字节 = 40 位
	cntLen    = 8              // 计数器占 1 字节 = 8 位
	suffixLen = tsLen + cntLen // suffix 共 6 字节 = 48 位
)

type Generator struct {
	prefix uint64 // 高 2 字节：memberID << 48，启动后不变
	suffix uint64 // 低 6 字节：timestamp+cnt，Next() 对它 atomic 自增
}

func NewGenerator(memberID uint16, now time.Time) *Generator {
	prefix := uint64(memberID) << suffixLen
	unixMilli := uint64(now.UnixNano()) / uint64(time.Millisecond/time.Nanosecond)
	// 取毫秒时间戳的低 40 位，左移 8 位给计数器腾出低字节。
	suffix := lowbit(unixMilli, tsLen) << cntLen
	return &Generator{prefix: prefix, suffix: suffix}
}

// Next 生成下一个唯一 ID：对整个 suffix 原子 +1（计数器满会进位到时间戳位），
// 再用 prefix 或上低 48 位。无锁。
func (g *Generator) Next() uint64 {
	suffix := atomic.AddUint64(&g.suffix, 1)
	id := g.prefix | lowbit(suffix, suffixLen)
	return id
}

// lowbit 取 x 的低 n 位。
func lowbit(x uint64, n uint) uint64 {
	return x & (math.MaxUint64 >> (64 - n))
}
```

`NewGenerator(0x12, ...0x3456ms)` 之后,第一次 `Next()` 得到 `0x12000000345601`——拆开正好是 `memberID=0x12 | timestamp=0x3456 | cnt=0x01`。

## 四、几个关键设计

### 1. 用位布局把复合唯一性拆成正交维度

`prefix := uint64(memberID) << suffixLen` 把 16 位的 memberID 顶到最高 2 字节;`suffix := lowbit(unixMilli, tsLen) << cntLen` 把时间戳低 40 位左移 8 位、给计数器腾出最低字节。三段互不重叠地拼进同一个 uint64,每段独立保证一个维度的唯一性。比起设计一个复杂算法,「**用移位和掩码把 [节点][时间][计数] 精确拼进指定比特区间**」清晰得多。

### 2. 一次 atomic.AddUint64 替代锁

`Next()` 的推进是 `atomic.AddUint64(&g.suffix, 1)`——单条 CPU 指令级别的原子自增。`prefix` 启动后是常量、从不改动,无需保护;只有 `suffix` 需要同步,而它被压缩成一次原子加,**没有 mutex 的锁竞争与上下文切换**。这是它敢放在写请求热路径上的关键。把「读—改—写」压成一次原子操作,是无锁编程的经典手法。

### 3. 计数器「故意」溢出进位到时间戳位（最精妙处）

计数器只有 1 字节(0~255),一眼看去每毫秒只能发 256 个 ID,早该不够用。但 `Next()` 的自增是对**整个 6 字节 suffix** 做 `AddUint64`,不是只加低 8 位。于是计数器满 256 后,进位会「爬」到时间戳字节里:

```
... suffix = 1000:255   （时间戳=1000, cnt=255）
Next()  ->  1001:000    ← 计数器满，进位顶到时间戳段
```

设计者**故意**允许这件事,等价于把计数器空间从 2⁸ 借用到整个 suffix 的 2⁴⁸。

**唯一性为什么不破?** 因为进位吃掉的时间戳,是「未来的毫秒值」——只要真实时间还没走到那里,这些值就没人用过。而 etcd 吞吐 ≪ 256 req/ms(250k req/s ≈ 0.25 req/ms,差了三个数量级),计数器的进位速度远追不上真实时间的推进,借来的未来空间(约 35 年)根本用不完。同时 `lowbit(suffix, suffixLen)` 只保留低 48 位,**进位再猛也越不过 48 位边界去污染高 2 字节的 memberID**,跨节点隔离不受影响。

一句话:**用「位布局 + 受控溢出」把无锁自增的窗口从 2⁸ 撑到 2⁴⁸,以「吞吐远低于时间推进速率」作为唯一性的前提。** 比 Snowflake 更简洁——省掉了时钟回拨检测和序列号回绕阻塞的全部复杂度。

### 4. lowbit：一行位掩码

`lowbit(x, n)` = `x & (math.MaxUint64 >> (64-n))`。先把 64 个 1 右移 `64-n` 位,得到「低 n 位为 1」的掩码,再 `&` 掉 x 的高位。两处用到:构造时截断时间戳到 40 位、`Next()` 里把 suffix 夹在 48 位内。后者正是防止进位越界污染前缀的那道闸。

### 使用要点 / 坑

- **唯一性是「概率 + 前提」保证,不是数学上的绝对保证**:前提是吞吐 ≪ 256 req/ms 且进程寿命 < 35 年。现实中达不到临界点。
- **依赖本地时钟重启后不大幅回退**:若机器时间被大幅回拨,重启后可能落回旧时间段。实践中重启间隔 > 1ms、加上 memberID 隔离,基本无碍。
- **生成的是 uint64**:某些场景(如 LeaseGrant 要正 int64)会再 `& ((1<<63)-1)` 掐掉符号位。

## 五、在 etcd 中的真实用法

一句话概括:凡是需要「**本地生成、跨节点不撞、无锁、极快**」的唯一号,etcd 就用 `idutil.Generator`。它是 `EtcdServer` 里名为 `reqIDGen` 的那唯一一个字段。

### 初始化:两个入参对应两条唯一性保证

`server/etcdserver/server.go:266` 定义字段、`:327` 初始化:

```go
reqIDGen: idutil.NewGenerator(uint16(b.cluster.nodeID), time.Now()),
```

`nodeID` → 前缀 → 跨节点不撞;`time.Now()` → 时间戳 → 重启不撞。

### 核心场景:写请求的「发号 → 登记 → 唤醒」闭环

`processInternalRaftRequestOnce`（`v3_server.go:1058`）把 idutil 和 `pkg/wait` 缝在一起:

```go
r.Header = &pb.RequestHeader{
    ID: s.reqIDGen.Next(),   // 1) idutil 发一个唯一号
}
// ...
id := r.ID
if id == 0 {
    id = r.Header.ID
}
ch := s.w.Register(id)       // 2) 用这个号在 wait 上登记，拿到等待 channel

err = s.r.Propose(cctx, data) // 3) 把请求丢进 raft
// ...
select {
case x := <-ch:              // 4) apply 完成后 wait.Trigger(id) 精确唤醒本请求
    return x.(*apply2.Result), nil
case <-cctx.Done():
    s.w.Trigger(id, nil)     //    超时：自己 Trigger 一下做 GC，避免泄漏
    return nil, ...
}
```

idutil 的价值在这里凸显:`wait` 按 ID 一对一,要求 **key 全局唯一且生成极快**。

- 若两个并发写请求拿到同一个 ID,apply 结果会串台(唤醒错请求)——idutil 的原子自增保证同节点内不重号。
- 这条路径在写热路径上,每个请求都要发号,**不能用锁**——一次 `atomic.AddUint64` 搞定。
- 跨节点即便各自 apply,日志里的请求 ID 也不撞(前缀不同),便于全局追踪。

### 其他消费者

| 消费方 | 位置 | 用途 |
|--------|------|------|
| 内部 raft 请求发号 | `v3_server.go:1067` | 每个写请求 `r.Header.ID = reqIDGen.Next()` |
| LeaseGrant 分配 lease id | `v3_server.go:438` | 用户没给 id 时自动生成,`& ((1<<63)-1)` 取正 |
| ConfChange id | `server.go:1745` | 配置变更条目的 id |

## 六、为什么不用别的方案

| 方案 | 问题 |
|------|------|
| 全局 `mutex + count++` | 写热路径上锁竞争;且重启后从 0 重来会和旧 ID 撞 |
| UUID | 16 字节太大,要塞进 raft header 和日志;不单调,不利观测 |
| Snowflake | 思路相近但更重:要处理时钟回拨、序列号回绕阻塞;idutil 用「受控溢出」把这些复杂度直接省掉 |
| 中心发号器 | 引入 RPC 和单点,违背「无协调」初衷 |

idutil 用**一个 uint64 的位布局 + 一次原子自增**,同时满足「无锁、无协调、跨节点不撞、重启不撞、单调、可观测」,这是它在 etcd 写路径上不可替代的原因。它和前几篇的 `wait` / `WaitTime` / `notify` 合起来,构成了 etcd 请求处理的一条完整链路:**idutil 发号 → wait 登记 → raft 提交 → apply 推进(WaitTime)→ Trigger 精确唤醒**。

## 关键源码位置索引

| 内容 | 位置 |
|------|------|
| Generator 实现（NewGenerator / Next / lowbit / 位布局注释） | `pkg/idutil/id.go` |
| `reqIDGen` 字段定义 | `server/etcdserver/server.go:266` |
| 初始化 `NewGenerator(nodeID, time.Now())` | `server.go:327` |
| `NextRequestID` 封装 | `server/etcdserver/interface.go:34` |
| 写请求发号 + 交给 wait | `v3_server.go:1067`、`:1106` |
| LeaseGrant 自动分配 id | `v3_server.go:438` |
| ConfChange id | `server.go:1745` |
