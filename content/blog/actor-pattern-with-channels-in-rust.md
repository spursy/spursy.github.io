+++
title = "用 Channel 实现 Actor:Rust 里不用锁的并发状态"
date = 2026-10-27
description = "多个任务要改同一份状态，第一反应往往是 Arc<Mutex<_>>。但还有另一条路：把状态交给一个任务独占，外界只能发消息——这就是 Actor 模式。本文用 longbridge-ai-acp 里的 actor_counter 例子讲清 Actor 是什么、解决什么问题，再看真实源码 DesktopSession 为什么非用它不可。"
[taxonomies]
tags = ["rust", "actor", "channel", "tokio", "concurrency"]
+++

> 源码取自 [`longbridge-ai-acp`](https://github.com/longbridge/longbridge-ai-acp):教学例子 `examples/actor_counter.rs`,真实用法 `src/desktop.rs`。本文聚焦一个问题——**多任务共享可变状态,除了加锁还能怎么办**。

## 一、问题:多个任务改同一个计数器

最小的并发问题:开两个任务,各自给一个计数器加 100 次,最后要得到 200。直觉写法是把状态共享出去、用同步原语保护它。`examples/shared_counter.rs` 给了三种标准做法:

```rust
// 1) 原子整数:无锁,但只适合单个整数
let count = Arc::new(AtomicI64::new(0));
c1.fetch_add(1, Ordering::Relaxed);

// 2) Mutex:通用,锁住再改
let mut guard = c1.lock().await;
*guard += 1;

// 3) RwLock:多读单写
let mut guard = c1.write().await;
*guard += 1;
```

三者都能得到 200,共同点是:**状态被多个任务共享(`Arc`),靠「加锁」来保证并发安全**。这是「共享内存 + 锁」的思路。它有效,但随着状态变复杂,你要自己盯着:锁的粒度、加锁顺序(死锁)、持锁时能不能 `.await`、哪些字段要一起改……

Actor 模式换了一个角度:**既然共享才需要加锁,那就干脆不共享。**

## 二、Actor 是什么:状态只归一个任务

看 `examples/actor_counter.rs`。它用三样东西搭出一个 Actor,全程没有 `Arc`、没有锁:

**1) 消息枚举——Actor 的「接口」**

```rust
enum CounterMsg {
    Increment,
    /// 带一个一次性回信通道,调用方可以 await 到返回值
    Get(oneshot::Sender<i64>),
}
```

**2) Actor 本体——一个独占状态的任务**

```rust
async fn counter_actor(mut mailbox: mpsc::Receiver<CounterMsg>) {
    let mut count: i64 = 0;                 // count 只属于这个任务
    while let Some(msg) = mailbox.recv().await {
        match msg {
            CounterMsg::Increment => count += 1,
            CounterMsg::Get(reply) => { let _ = reply.send(count); }
        }
    }
}
```

`count` 是这个 `async fn` 的局部变量,**除了它自己没有任何人能碰到**。既然不共享,就不存在数据竞争,也就不需要锁。

**3) 句柄(Handle)——只暴露「发消息」,不暴露状态**

```rust
#[derive(Clone)]
struct CounterHandle {
    sender: mpsc::Sender<CounterMsg>,
}

impl CounterHandle {
    fn new() -> Self {
        let (tx, rx) = mpsc::channel(32);
        tokio::spawn(counter_actor(rx));   // 启动 actor 任务
        Self { sender: tx }
    }

    async fn increment(&self) {
        let _ = self.sender.send(CounterMsg::Increment).await;
    }

    async fn get(&self) -> i64 {
        let (tx, rx) = oneshot::channel();          // 临时回信通道
        let _ = self.sender.send(CounterMsg::Get(tx)).await;
        rx.await.expect("actor dropped the reply channel")
    }
}
```

调用方只拿到 `CounterHandle`,它可以 `clone` 给任意多个任务,但它们能做的只有「往 mailbox 里塞消息」。于是:

```rust
let counter = CounterHandle::new();
let c1 = counter.clone();
let c2 = counter.clone();
tokio::spawn(async move { for _ in 0..100 { c1.increment().await; } });
tokio::spawn(async move { for _ in 0..100 { c2.increment().await; } });
// ...
println!("final count = {}", counter.get().await); // 200
```

两个任务并发地发 `Increment`,但所有修改都在 actor 任务里**串行**执行——mailbox 天然把并发请求排成一队。没有锁,也不可能有数据竞争。

这里有个关键技巧:**用 `oneshot` 把单向消息变成「请求/响应」**。`Increment` 是「发了就不管」(fire-and-forget),而 `Get` 需要拿到返回值——做法是把一个 `oneshot::Sender` 放进消息里一起发过去,actor 处理完往这个通道回信,调用方 `await` 它。这就是在消息传递之上模拟出一次同步调用。

## 三、两种思路对比

| | 共享内存 + 锁 | Actor(消息传递) |
| --- | --- | --- |
| 状态在哪 | `Arc<Mutex<T>>`,多任务共享 | 一个任务独占,别人碰不到 |
| 并发安全靠什么 | 运行时加锁互斥 | 根本不共享,天然无竞争 |
| 并发请求如何排序 | 抢锁,顺序不确定 | mailbox 先进先出,顺序确定 |
| 容易踩的坑 | 死锁、锁粒度、持锁 `.await` | 消息设计、mailbox 背压 |
| 取返回值 | 直接读 | 带 `oneshot` 回信 |

天下没有免费的午餐。`examples/counter_bench.rs` 用同样的负载(8 任务 × 10 万次)跑了一个粗略对比,结论也在注释里写明了:**消息传递每次操作都有额外开销(建消息、过 channel、调度 actor),裸 `AtomicI64` 没有**。所以:

- 单个整数、只要快——用原子或锁。
- 状态复杂、有多个字段要一起维护一致、还想要清晰的生命周期和串行语义——Actor 的工程收益远大于那点每操作开销。

**Actor 不是用来替代锁的性能方案,而是一种「让复杂有状态的并发变简单」的结构。**

## 四、Actor 解决了什么问题

把例子里的收益抽象出来,就是 Actor 的卖点:

1. **消灭数据竞争**:状态不共享,编译器和你都不用再为锁操心。
2. **串行化**:所有改动在一个任务里按消息顺序发生,状态机天然一致;「同一时刻只处理一件事」免费得到。
3. **封装**:外界只看到 `Handle` 上几个方法,看不到内部状态长什么样,想改实现不影响调用方。
4. **生命周期清晰**:actor 就是一个 task,它的启动/停止/取消都能用「关 mailbox / abort task」干净地表达。
5. **易于传递**:`Handle` 只是个 `Sender`,`Clone + Send + 'static`,随便塞进任何地方。

第 5 点,正是 `longbridge-ai-acp` 非用 Actor 不可的原因。

## 五、ai-acp 为什么用这个模式:`DesktopSession`

`longbridge-ai-acp` 是一个 ACP(Agent Client Protocol)运行时,其中 `DesktopSession` 要做的事是:让一个桌面 UI(GPUI / Tauri 的 model)持有一个**长期存活**的 AI 会话,不断发 prompt、收流式事件。打开 `src/desktop.rs`,你会看到和计数器一模一样的三件套:

**消息枚举(会话能接受的命令):**

```rust
enum SessionCommand {
    Prompt {
        blocks: Vec<ContentBlock>,
        accepted: oneshot::Sender<Result<(), SessionControlError>>, // 回信:收下了 or Busy
    },
    Cancel,
    Shutdown,
}
```

**句柄 + 事件端:**

```rust
pub struct DesktopSession {
    commands: mpsc::UnboundedSender<SessionCommand>,       // 发命令(actor 的 mailbox)
    events: mpsc::UnboundedReceiver<DesktopSessionEvent>,  // 收事件
    task: Option<tokio::task::JoinHandle<()>>,             // actor 任务本体
    // ...
}
```

**Actor 本体——`drive_session` 循环独占真正的 SDK 会话:**

```rust
async fn drive_session(
    mut session: ActiveSession<'_, Agent>,                 // ← 被独占的状态
    mut commands: mpsc::UnboundedReceiver<SessionCommand>,
    events: mpsc::UnboundedSender<DesktopSessionEvent>,
) -> Result<(), agent_client_protocol::Error> {
    while let Some(command) = commands.recv().await {
        match command {
            SessionCommand::Prompt { blocks, accepted } => { /* 发起一轮对话 */ }
            SessionCommand::Cancel => {}
            SessionCommand::Shutdown => break,
        }
    }
    Ok(())
}
```

为什么这里**必须**用 Actor,而不能简单地 `Arc<Mutex<ActiveSession>>` 共享出去?几个硬约束:

**① 被保护的状态根本没法共享。** `ActiveSession<'_, Agent>` 带生命周期:它借用着底层 ACP 连接;对于外部 agent(Codex / Claude 子进程),它还间接持有那个子进程。它既不是 `'static` 也不方便跨任务共享,你没法把它塞进一个要被 UI model 长期持有、到处调用的 `Arc<Mutex<_>>`。**Actor 把它关在一个 task 里独占,外界永远只拿到一个 `Sender`**——而 `Sender` 是 `Clone + Send + 'static`,正好是 GPUI/Tauri model 想要的句柄。

**② 一问一答,用 `oneshot` 精确回信。** `prompt()` 需要知道「命令被收下了吗」,于是 `Prompt` 里带了 `accepted` 回信通道,和计数器的 `Get` 一模一样:

```rust
async fn send_prompt_command(commands: ..., blocks: ...) -> Result<(), SessionControlError> {
    let (accepted_tx, accepted_rx) = oneshot::channel();
    commands.send(SessionCommand::Prompt { blocks, accepted: accepted_tx })
        .map_err(|_| SessionControlError::Closed)?;
    accepted_rx.await.unwrap_or(Err(SessionControlError::Closed))
}
```

**③ 串行化带来「一次只能有一轮对话」的免费保证。** 一轮没结束就再发 prompt,actor 直接用回信通道回一个 `Busy`,不需要任何锁:

```rust
Some(SessionCommand::Prompt { accepted, .. }) => {
    let _ = accepted.send(Err(SessionControlError::Busy));
}
```

**④ 顺序性。** 流式更新和「这轮结束」的信号走在不同的通道上,actor 用一个带 `biased` 的 `select!` 保证:先把排队的内容块收完,再宣布本轮结束,否则残留的 chunk 会漏进下一轮——这种「谁先谁后」的裁决,放在单个 actor 循环里是最自然的:

```rust
tokio::select! {
    biased;
    message = session.read_update() => { /* 转发 Update 事件 */ }
    outcome = &mut stop_rx        => { /* 收到终止原因,发 TurnFinished */ }
    command = commands.recv()     => { /* 处理 Cancel / Shutdown / 重叠的 Prompt */ }
}
```

**⑤ 生命周期即 task 生命周期。** `Cancel`、`Shutdown` 都只是消息;而 `Drop` 直接 `task.abort()`——对外部 agent,这会让官方 SDK 顺带终止子进程组。启停彻底、确定:

```rust
impl Drop for DesktopSession {
    fn drop(&mut self) {
        if let Some(task) = self.task.take() { task.abort(); }
    }
}
```

**⑥ 命令端与事件端可以拆开。** `split()` 把会话拆成一个可 `clone` 的命令句柄 `DesktopSessionHandle` 和一个独占的事件端 `DesktopSessionEvents`——UI 状态层发命令,后台任务专心收流式事件,两边都不用碰 SDK 的生命周期。这正是「`Handle` 易于传递」那条收益的直接兑现。

## 六、从计数器到 DesktopSession

两段代码是同一个骨架:

| 角色 | `actor_counter.rs` | `desktop.rs` |
| --- | --- | --- |
| 消息枚举 | `CounterMsg` | `SessionCommand` |
| 独占的状态 | `count: i64` | `ActiveSession<'_, Agent>` |
| Actor 循环 | `counter_actor` | `drive_session` / `drive_turn` |
| mailbox | `mpsc::channel(32)` | `mpsc::unbounded_channel()` |
| 请求/响应 | `Get(oneshot)` | `Prompt { accepted }`、`stop_rx` |
| 句柄 | `CounterHandle` | `DesktopSession` / `DesktopSessionHandle` |

计数器里 Actor 是「可以不用锁」的优雅选择;到了 `DesktopSession`,它几乎是**唯一**合理的结构——因为被保护的状态(带生命周期、裹着子进程的 SDK 会话)根本没法安全共享,而 UI 又需要一个轻巧、可克隆、`'static` 的句柄。

## 小结

一句话记住 Actor:**不共享状态,只传递消息。** 把状态交给一个任务独占,外界通过 mailbox 发命令、用 `oneshot` 取回信——数据竞争从源头消失,串行语义、封装、清晰的生命周期都顺带拿到。

它不是用来跑赢锁的性能招式(消息传递确实有每操作开销),而是**让复杂、有状态、还带着不可共享资源(连接、子进程、句柄)的并发变得可维护**的结构。`longbridge-ai-acp` 的 `DesktopSession` 就是这个道理最实在的注脚。

---

*工程地址:[github.com/longbridge/longbridge-ai-acp](https://github.com/longbridge/longbridge-ai-acp)。例子:`examples/actor_counter.rs`、`examples/shared_counter.rs`、`examples/counter_bench.rs`;真实用法:`src/desktop.rs`。*
