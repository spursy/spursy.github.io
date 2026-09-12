+++
title = "同步框架里优雅地做异步：ECS 的 async→sync 桥"
date = 2026-09-12
description = "Bevy ECS 系统是同步函数、绝对不能 await，而行情推送和 REST 请求都是异步的。本文用 tui-bevy-ecs 里的 RT / STOCKS / UPDATE_TX 三个桥接单例，讲清「同步世界」如何优雅地驱动「异步世界」并把结果安全地送回来——一个可迁移到任何同步框架 + 异步 IO 场景的模式。"
[taxonomies]
tags = ["rust", "bevy", "ecs", "tokio", "async"]
+++

> 源码：[github.com/spursy/tui-bevy-ecs](https://github.com/spursy/tui-bevy-ecs)。本文聚焦其中最硬核的一块：同步 ECS 与异步 SDK 的桥接。

## 一、矛盾：ECS 系统不能 `await`

Bevy ECS 的系统是**普通同步函数**。调度器按帧同步地跑它们，签名里不能有 `async`，函数体里也不能 `.await`：

```rust
pub fn render_watchlist(/* ... */) {
    // 这里是同步的，不能 await 任何东西
}
```

但真实世界的数据来源全是异步的：

- 行情通过 WebSocket **推送**过来。
- 刷新时要 `ctx.quote(..).await` 打 **REST** 接口。

**两个世界不能直接互相调用**：同步系统里不能 `await` 异步请求，异步任务里也不能随便碰 ECS 的 World（它不是 `Send`/`Sync` 友好的共享结构，且并发改 World 会破坏调度器的借用假设）。

这就是所有「同步框架 + 异步 IO」都会撞上的核心矛盾。tui-bevy-ecs 用**三个全局单例**搭了一座桥。

## 二、三件套：桥的三根柱子

| 单例 | 类型 | 角色 |
| --- | --- | --- |
| `RT` | `OnceLock<tokio::runtime::Handle>` | 让同步系统 `RT.get().spawn(..)` 派发异步后台任务 |
| `STOCKS` | `LazyLock<StockStore>` | 全局行情缓存：后台任务写、渲染系统读 |
| `UPDATE_TX` | `OnceLock<UnboundedSender<CommandQueue>>` | 后台任务把批量操作 / 重绘信号送回主循环 |

一句话：**`RT` 派发异步任务，`STOCKS` 共享数据，`UPDATE_TX` 把结果送回 ECS。**

```
渲染系统(同步) ──RT.spawn──▶ tokio 异步任务 ──写──▶ STOCKS 缓存
                                    │
                                    └──UPDATE_TX──▶ 主循环 ──应用──▶ ECS world(标脏重绘)
```

三根柱子在程序启动时装配好：

```rust
// 主循环启动处
let (update_tx, mut update_rx) = mpsc::unbounded_channel();
UPDATE_TX.set(update_tx).ok();
RT.set(tokio::runtime::Handle::current()).ok();   // 把当前 tokio runtime 句柄存进 RT
```

## 三、核心实现：`refresh_stock_debounced`

这个函数把「同步系统 → 异步任务 → 写缓存 → 通知 ECS」完整走了一遍：

```rust
pub fn refresh_stock_debounced(counter: Counter) {   // 注意：没有 async，是同步函数
    let Some(rt) = RT.get() else { return };         // ① 拿到异步世界的入口
    rt.spawn(async move {                            // ② 派发异步任务（桥接的关键一跃）
        // ③ 防抖：快速翻页时划过的股票被冲掉
        tokio::time::sleep(Duration::from_millis(150)).await;

        // ④ 模拟 REST 请求，⑤ 写入全局缓存
        let (last_done, change_rate) = fake_rest_quote(&counter).await;
        STOCKS.modify(counter.clone(), |s| s.update_from_push_quote(last_done, change_rate));

        // ⑥ 通知主循环「有更新了，请重绘」
        if let Some(tx) = UPDATE_TX.get() {
            let queue = bevy_ecs::system::CommandQueue::default();
            let _ = tx.send(queue);
        }
    });
}
```

逐段看它妙在哪：

**① 同步世界拿异步世界的入口**

```rust
let Some(rt) = RT.get() else { return };
```

`let ... else` 拿到 runtime 句柄就绑定，没初始化就直接 `return`。这是同步侧接触异步侧的第一步。

**② 派发异步任务——桥接的关键一跃**

```rust
rt.spawn(async move { ... });
```

`rt.spawn` 把 `async move { ... }` 丢到 tokio 后台**独立执行，并立即返回**。所以调用它的 ECS 系统**不会被阻塞**，渲染循环继续流畅。`move` 把 `counter` 所有权搬进异步块。

**这一跳是整座桥的核心**：同步函数没有 `await`，却成功启动了一段异步工作——代价是「fire-and-forget」，它不等结果，结果通过另外两根柱子回流。

**③ 防抖（debounce）**

```rust
tokio::time::sleep(Duration::from_millis(150)).await;
```

`.await` 只发生在后台任务里，不碰主线程。作用：用户快速上下翻股票时会瞬间划过多只，等 150ms 再真正请求，让「一闪而过」的股票请求被冲掉，只有停下来那只才真去拉数据。**把防抖放进异步任务，是同步侧做不到的——同步函数没法「等一会儿再干」而不阻塞。**

**④⑤ 拉数据 + 写共享缓存**

```rust
let (last_done, change_rate) = fake_rest_quote(&counter).await;
STOCKS.modify(counter.clone(), |s| s.update_from_push_quote(last_done, change_rate));
```

数据一旦写进 `STOCKS`，渲染系统下次读就能看到新值。**这是「结果回流」的第一条路：纯数据走共享缓存。** `STOCKS` 内部是 `Mutex<HashMap>`（真实项目用 `DashMap`），保证异步写、同步读的并发安全。

**⑥ 通知 ECS：把「该重绘」的信号送回**

```rust
if let Some(tx) = UPDATE_TX.get() {
    let queue = bevy_ecs::system::CommandQueue::default();
    let _ = tx.send(queue);
}
```

`CommandQueue` 是 Bevy 的批量操作队列，能装「往 World 增删资源/实体」这类结构性改动。这里只改了缓存、不需要动 World 结构，所以发一个**空队列**，纯粹当「有更新了，请重绘一帧」的信号。

## 四、结果如何安全地回到 ECS World

主循环在 `tokio::select!` 里有一条分支专门收这个信号：

```rust
Some(mut cmd) = update_rx.recv() => {
    cmd.apply(&mut app.world);                 // 把 CommandQueue 应用到 world
    render_state.mark_dirty(DirtyFlags::ALL);  // 标脏 → 下个 tick 触发重绘
}
```

**这是全设计最关键的安全保证**：异步任务从不直接碰 World，它只把「想做的改动」打包成 `CommandQueue`，通过 mpsc 送回**主循环**；由主循环在**单线程、无并发**的上下文里 `cmd.apply(&mut world)`。这样：

- World 永远只被主循环这一个线程改，**没有数据竞争**。
- 改动是**批量、原子**地应用，不会改到一半被打断。

所以「结果回流」有两条路，各司其职：

| 回流的东西 | 走哪条路 | 谁来应用 |
| --- | --- | --- |
| **纯数据**（新股价） | 写 `STOCKS` 共享缓存 | 无需应用，渲染系统直接读 |
| **结构变更**（增删资源/实体）+ 重绘信号 | `UPDATE_TX` 送 `CommandQueue` | 主循环 `cmd.apply(&mut world)` |

## 五、完整数据流

```
[同步 ECS 系统] enter_stock / Refresh 动作
      │ 调用
      ▼
refresh_stock_debounced(counter)   ← 同步函数
      │ ① RT.get() 拿异步入口
      │ ② rt.spawn 派发异步任务 ─────────────────┐
      └─（立即返回，系统不阻塞）                  │
                                                ▼
                             [tokio 后台异步任务] async move {
                                ③ sleep 150ms 防抖
                                ④ fake_rest_quote().await 拉数据
                                ⑤ STOCKS.modify() 写缓存   ← 数据回流路①
                                ⑥ UPDATE_TX.send() 通知主循环 ← 信号回流路②
                             }
                                                │
                                                ▼
                       [主循环] update_rx.recv()
                                ⑦ cmd.apply(world) + 标脏  ← 单线程安全应用
                                ⑧ 下个 render tick → 渲染系统读 STOCKS → 重绘
```

## 六、这个模式可以迁移到哪

这套「三件套」不是 Bevy 独有，它是**任何同步框架 + 异步 IO**都能借鉴的通用模式：

- **`RT`（runtime 句柄）**：同步代码派发异步任务的入口。任何持有 tokio `Handle` 的地方都能 `spawn`。
- **`STOCKS`（共享缓存）**：异步写、同步读的并发安全容器（`Mutex`/`RwLock`/`DashMap`）。适合「数据最终一致即可」的场景。
- **`UPDATE_TX`（回传通道）**：异步任务把「需要在主上下文里做的事」打包送回，由主线程串行应用。适合「必须在特定线程/上下文里改的状态」（GUI 主线程、ECS World、数据库事务…）。

核心思想一句话：**同步侧只负责「派发」和「读缓存」，绝不 `await`；异步侧只负责「干活」和「回传」，绝不直接碰受保护的状态；两者之间用共享缓存（数据）和 channel（信号/结构变更）解耦。**

## 小结

`refresh_stock_debounced` 就是那座桥的具体实现：它用 `RT` 把耗时异步工作甩到后台（不卡渲染）、在后台把结果写进 `STOCKS` 共享缓存、再用 `UPDATE_TX` 回敲主循环触发安全的 World 应用与重绘——**三个桥接单例在一个函数里全部登场，完成了「异步 SDK 世界 → 同步 ECS 渲染世界」的完整闭环**。

下一篇聊性能：这个 demo 如何用**脏标记 + ratatui 双缓冲 diff** 两级节流，把 CPU 和终端 IO 都省到极致。

---

*完整源码：[github.com/spursy/tui-bevy-ecs](https://github.com/spursy/tui-bevy-ecs)*
