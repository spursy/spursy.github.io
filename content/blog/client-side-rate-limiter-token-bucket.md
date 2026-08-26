+++
title = "客户端限流：用 Semaphore 实现一个惰性补充的令牌桶"
date = 2026-08-26
description = "从一份生产代码出发，讲清令牌桶限流的设计：为什么把令牌建模成信号量、为什么惰性补充、以及在其上叠加 429 退避重试。"
[taxonomies]
tags = ["rust", "rate-limiter"]
[extra]
toc = true
+++

限流通常让人第一反应是「服务端的事」——网关、Nginx、Redis 计数器。但在写 API SDK / CLI 客户端时，**客户端主动限流**同样重要：在把请求打出去之前先自己卡一道，既保护服务端不被打爆，又能在空闲后允许短时突发。

这篇文章从一份真实的客户端限流代码出发，讲清**令牌桶（Token Bucket）**的设计：为什么把令牌建模成 `Semaphore` 的许可、为什么用「惰性补充」而不是后台定时器、以及如何在其上再叠加一层 429 退避重试。

<!-- more -->

## 一、令牌桶是什么

用一个比喻最好理解：想象一个**桶**，水龙头**匀速滴令牌**进去：

- 桶有固定容量 `max_tokens`（装满就溢出）；
- 令牌以固定速率 `tokens_per_second` 补充；
- 每来一个请求，就要从桶里**拿走一个令牌**才能通过；
- 桶空了，请求就**等待**，直到有新令牌。

两个参数决定了它的全部行为：

| 参数 | 控制什么 |
|------|---------|
| **速率** `tokens_per_second` | 长期平均处理速度（稳态限流） |
| **容量** `max_tokens` | 瞬时突发能力（攒了多少就能一口气发多少） |

和**漏桶（Leaky Bucket）**的关键区别在于：漏桶「出水」恒定、不允许突发；令牌桶攒着的令牌可以一次用掉、**允许突发**——空闲一阵子后能瞬间冲一批请求，更灵活，也更贴合真实客户端的流量形态。

## 二、把令牌建模成信号量的许可

> 本文分析的完整源码：[spursy/cli-terminal · src/api/rate_limiter.rs](https://github.com/spursy/cli-terminal/blob/master/src/api/rate_limiter.rs)。下文代码为便于讲解略有精简。

核心思路：用 `tokio::sync::Semaphore` 的**许可（permit）数**表示「桶里当前可用的令牌数」。

```rust
use std::sync::OnceLock;
use std::time::Duration;
use tokio::sync::Semaphore;
use tokio::time::{sleep, Instant};

pub struct RateLimiter {
    /// 信号量：可用 permit 数 == 当前桶里可用令牌数。
    semaphore: Semaphore,
    /// 补充速率：每秒加多少令牌。
    tokens_per_second: u32,
    /// 桶容量（突发上限）。
    max_tokens: u32,
    /// 上次补充令牌的时间戳。
    last_refill: tokio::sync::Mutex<Instant>,
}

impl RateLimiter {
    pub fn new(tokens_per_second: u32, max_tokens: u32) -> Self {
        Self {
            semaphore: Semaphore::new(max_tokens as usize), // 初始桶是满的
            tokens_per_second,
            max_tokens,
            last_refill: tokio::sync::Mutex::new(Instant::now()),
        }
    }
}
```

初始化时 `Semaphore::new(max_tokens)`，桶是满的——一上来就允许突发到上限。

## 三、惰性补充：没有后台定时器

最容易想到的补充方式是起一个后台任务，每秒往桶里加令牌。但那样要多养一个 timer 任务，还得处理它的生命周期。这份实现用了更轻量的做法——**惰性补充（lazy refill）：不主动加令牌，而是每次请求来时，按「距上次补充经过了多久」算出该补多少。**

```rust
impl RateLimiter {
    /// 按经过的时间比例补充令牌，上限为 max_tokens。
    async fn refill_tokens(&self) {
        let mut last_refill = self.last_refill.lock().await;
        let now = Instant::now();
        let elapsed = now.duration_since(*last_refill);

        // 经过的时间 × 速率 = 应补的令牌数。
        let tokens_to_add = (elapsed.as_secs_f64() * f64::from(self.tokens_per_second)) as u32;

        if tokens_to_add > 0 {
            let current = self.semaphore.available_permits() as u32;
            let room = self.max_tokens.saturating_sub(current); // 距桶满还差多少
            let tokens_to_add = tokens_to_add.min(room);        // 不能超过容量

            if tokens_to_add > 0 {
                self.semaphore.add_permits(tokens_to_add as usize);
                // ★ 只有真正加了令牌才推进时钟。
                *last_refill = now;
            }
        }
    }
}
```

这里有一个**极易写错的细节**（也是最精妙处）：

> **只有真正加了令牌，才推进 `last_refill` 时钟。**

假设速率是 10 令牌/秒（每 100ms 一个），但请求来得很密、每 30ms 一次。若每次进来就无条件把 `last_refill = now`，那 `tokens_to_add = 0.03 × 10 = 0`（取整后为 0），却把时钟推进了——**那 30ms 的时间被白白丢弃**，令牌永远补不满。

正确做法是：**不足一个令牌的零头时间不推进时钟**，让它继续累积，直到攒够一个整令牌再一次性补上并推进。这样长期速率才严格等于 `tokens_per_second`。

## 四、获取令牌：try_acquire + forget

```rust
impl RateLimiter {
    /// 获取一个令牌，没有就 sleep 到下一个令牌到期再重试。
    pub async fn acquire(&self) {
        loop {
            self.refill_tokens().await; // 先按时间补桶

            // 非阻塞地尝试拿一个令牌。
            if let Ok(permit) = self.semaphore.try_acquire() {
                // ★ forget 阻止 permit 在 drop 时自动归还，
                //   令牌只能由 refill_tokens 补回 —— 这才是真正的「消费」。
                permit.forget();
                return;
            }

            // 桶空：算出下一个令牌何时到期，sleep 相应时长后重试。
            let last_refill = *self.last_refill.lock().await;
            let elapsed = Instant::now().duration_since(last_refill);
            let time_per_token = Duration::from_secs_f64(1.0 / f64::from(self.tokens_per_second));
            let wait_time = time_per_token
                .saturating_sub(elapsed)
                .max(Duration::from_millis(1)); // 至少 1ms，避免忙等
            sleep(wait_time).await;
        }
    }
}
```

这里的关键是 **`permit.forget()`**。

`Semaphore` 的许可默认遵循 RAII：`try_acquire()` 拿到的 `permit` 一旦离开作用域被 drop，许可会**自动归还**给信号量。如果放任它归还，那「拿令牌」这个动作就白做了——桶里的令牌数根本没减少，等于没限流。

`forget()` 显式「遗忘」这个许可，让它**不再归还**。于是令牌数只减不增，唯一能让它回升的通道就是 `refill_tokens` 里的 `add_permits`。**这正是把「信号量」正确改造成「令牌桶」的点睛之笔。**

拿不到令牌时不是自旋，而是精确算出「距下一个令牌到期还要多久」，`sleep` 那么长再重试；`max(1ms)` 兜底防止忙等空转。

## 五、在其上叠加 429 退避重试

客户端限流控住了**自己**的发送速率，但服务端可能有它自己的、更严格的限制，会返回 `429 Too Many Requests`。所以在令牌桶之上再包一层「服务端限流兜底」：

```rust
impl RateLimiter {
    pub async fn execute<F, T, E>(&self, _request_name: &str, mut f: F) -> Result<T, E>
    where
        F: FnMut() -> std::pin::Pin<Box<dyn std::future::Future<Output = Result<T, E>> + Send>>,
        E: std::fmt::Display,
    {
        const MAX_RETRIES: u32 = 3;
        let mut retry_count = 0;
        let mut backoff = Duration::from_secs(1);

        loop {
            self.acquire().await; // ① 先过自己的令牌桶

            match f().await {
                Ok(result) => return Ok(result),
                Err(e) => {
                    let msg = format!("{e}");
                    let is_rate_limit = msg.contains("429")
                        || msg.contains("rate limit")
                        || msg.contains("too many requests");

                    // ② 命中服务端限流 → 指数退避重试。
                    if is_rate_limit && retry_count < MAX_RETRIES {
                        retry_count += 1;
                        sleep(backoff).await;
                        backoff *= 2; // 1s → 2s → 4s
                        continue;
                    }
                    return Err(e); // 其它错误立即返回
                }
            }
        }
    }
}
```

几点设计：

- **双重防护**：`acquire()` 尊重自己的限流，429 退避尊重服务端的限流，两者叠加。
- **闭包必须是 `FnMut`**：因为要**重试**，同一个请求 `f` 可能被调用多次，所以不能是只能调一次的 `FnOnce`。
- **返回 `Pin<Box<dyn Future + Send>>`**：让 `execute` 成为通用包装器，能接住任意异步请求；`Send` 是为了能在 Tokio 多线程运行时里跨线程调度。
- **靠错误文本判断限流**：`format!("{e}")` 里匹配关键字，因此约束 `E: Display`。这是一种偏「鸭子类型」的务实做法，代价是耦合了错误信息的措辞。

调用方通常这样用：

```rust
let quote = limiter
    .execute("get_quote", || {
        Box::pin(async move { client.get_quote("AAPL").await })
    })
    .await?;
```

## 六、进程级单例：OnceLock 懒加载

限流只有在**全局共享同一个桶**时才有意义——如果每次调用都 `new` 一个新桶，等于没限流。用 `OnceLock` 实现「进程级、懒加载、线程安全」的单例：

```rust
static RATE_LIMITER: OnceLock<RateLimiter> = OnceLock::new();

pub fn global_rate_limiter() -> &'static RateLimiter {
    // 第一次调用才真正构造；并发首调也只会创建一个实例。
    RATE_LIMITER.get_or_init(|| RateLimiter::new(10, 20))
}
```

为什么是 `OnceLock` 而不是直接 `static`？因为 `static` 的初始值必须是**编译期常量**，而 `RateLimiter::new(10, 20)` 要在运行时才能算出来。`OnceLock` 声明时留空，第一次调用 `global_rate_limiter()` 时才 `get_or_init`，且保证多线程并发首调只初始化一次。传给它的是**闭包**而非值——这样只有真正需要时才执行构造（惰性求值）。

这里的默认 `10 req/s、突发 20` 是一个保守配置：长期每秒最多 10 个请求，但空闲后攒满可一次冲 20 个。

## 七、几个关键设计小结

| 设计点 | 做法 | 为什么 |
|--------|------|--------|
| 令牌 = 信号量许可 | `Semaphore` + `permit.forget()` | 复用成熟并发原语；`forget` 让「拿走」真正生效 |
| 惰性补充 | 请求驱动、按 elapsed 补 | 省掉后台 timer，实现轻量 |
| 只在补令牌时推进时钟 | 零头时间不丢弃 | 保证长期速率严格等于设定值 |
| 等待而非拒绝 | `sleep` 到下一个令牌 | 客户端场景更希望「慢一点」而非「失败」 |
| 429 退避 | 指数退避重试 3 次 | 尊重服务端限流，双重防护 |
| 全局单例 | `OnceLock` 懒加载 | 共享同一个桶，限流才有效 |

一句话总结：**用信号量的许可数表示令牌，`forget()` 表示消费、`add_permits` 惰性补充，靠「只在补令牌时推进时钟」保证速率精确；再在上层叠加 429 指数退避，用 `OnceLock` 收敛成进程级单例。** 一个不到 140 行、无后台任务的客户端令牌桶就成了。
