+++
title = "把一个大项目的 TUI 层浓缩成 300 行：用最小 demo 镜像真实架构"
date = 2026-09-15
description = "面对一个庞大的生产代码库，怎么快速看懂它的架构？一个高效的办法是「造一个最小可运行的镜像」。本文以 tui-bevy-ecs 为例，讲如何把 longbridge-terminal 的整个 TUI 层浓缩进一个约 300 行、可 cargo run 的 demo——保留骨架、砍掉血肉，作为学习与重构的方法论。"
[taxonomies]
tags = ["rust", "architecture", "refactoring", "learning"]
+++

> 源码：[github.com/spursy/tui-bevy-ecs](https://github.com/spursy/tui-bevy-ecs)。本文不讲某个具体技术点，而讲一种**方法论**：如何用一个最小可运行 demo 镜像大项目的架构。

## 一、问题：大代码库怎么快速看懂

你接手一个几万行的生产项目，想搞懂它的 TUI 层是怎么组织的。直接读源码常常陷进去：

- **信噪比低**：真正的架构骨架被淹没在错误处理、日志、边界情况、业务分支里。
- **牵一发动全身**：想跑通一个最小流程，得先满足一堆依赖（SDK、鉴权、网络、配置）。
- **不敢改**：想验证「如果去掉这层会怎样」，但生产代码不敢乱动。

有一个被低估的办法：**给它造一个最小可运行的镜像（distillation）**——把架构骨架单独拎出来，用一个能 `cargo run` 的小 demo 复现，砍掉所有血肉，只留主干。

`tui-bevy-ecs` 就是这么一个东西：它把 longbridge-terminal 的整个 TUI 层浓缩进约 300 行，每个概念都能在真实项目里找到对应。

## 二、核心原则：保留骨架，砍掉血肉

浓缩不是「随便删代码」，而是有取舍地保留。判断标准就一条：

> **凡是决定「架构长什么样」的，保留；凡是只增加「工程完备性」的，砍掉或用假实现替代。**

具体到 tui-bevy-ecs：

| 保留（架构骨架） | 砍掉 / 假实现（工程血肉） |
| --- | --- |
| ECS 状态机、Resource/Event/System 的分工 | 真实业务字段（只留渲染要用的几个） |
| `tokio::select!` 主循环的四条流 | 真实 WebSocket → 用一个后台任务 `tx.send` 假推送 |
| async→sync 桥（`RT`/`STOCKS`/`UPDATE_TX`） | 真实 REST 请求 → `fake_rest_quote` 返回确定性假数据 |
| 脏标记 + diff 两级节流 | 真实的多区域、弹窗、深度图（只留位定义，不实现） |
| 数据驱动 keymap（一张表驱动输入+提示） | 真实鉴权、配置、错误上报、日志 |

关键技巧是**用「假实现」占住接口位置**，而不是直接删掉。比如：

```rust
/// 站位真实的 ctx.quote(..).await，返回确定性伪数据
async fn fake_rest_quote(counter: &str) -> (f64, f64) {
    let seed = counter.bytes().map(u64::from).sum::<u64>();
    let base = 50.0 + (seed % 400) as f64;
    let change = ((seed % 700) as f64) / 100.0 - 3.5;
    (base, change)
}
```

这样**数据流的形状和真实项目一模一样**（同步系统 → spawn 异步 → await 一个「请求」→ 写缓存），只是把「真的打网络」换成「算个假数」。读者看到的是**架构，而非业务噪声**。

## 三、方法：先画「概念 → 文件」映射表

浓缩之前，先列一张表：真实项目里的每个**架构概念**，将落到 demo 的哪个文件。这张表既是施工蓝图，也是给读者的导览。tui-bevy-ecs 的映射长这样：

| 概念（来自架构文档） | 落在 demo 哪里 |
| --- | --- |
| Bevy ECS 状态机（`AppState`/`NextState`/`OnEnter`/`OnExit`/`run_if`） | `src/app.rs` |
| System 即依赖注入（声明 `Res`/`ResMut`/`EventReader`/`Local`） | `src/systems.rs` |
| Resource vs Event（数据 vs 行为） | `src/systems.rs` |
| Ratatui 渲染（`Terminal` 资源，`Drop` 进出备用屏） | `src/terminal.rs` |
| `tokio::select!` 多路复用主循环 | `src/app.rs` |
| 脏标记渲染 | `src/render.rs` |
| async→sync 桥（`RT`/`STOCKS`/`UPDATE_TX`） | `src/data.rs` |
| 数据驱动 keymap | `src/keymap.rs` |

**有了这张表，「浓缩」就变成了逐行填空**：每个概念找一个最小实现塞进对应文件。表还顺便保证了「不漏关键概念、不混入无关概念」。

## 四、几个让 demo「小而真」的技巧

### 技巧 1：镜像真实的类型形状，哪怕用不满

demo 里有些字段/变体其实没被完全用到，但仍保留，因为它们是真实项目 API 表面的一部分。比如 `WsState::Connecting` 变体、`DirtyFlags` 里没实现的区域位——用一句注释交代：

```rust
// 一些 flags/变体本 demo 没用到，但保留以镜像真实项目的完整区域集
#![allow(dead_code)]
```

这让读者看到的是**完整的形状**，而不是被裁得认不出的残缺版。`#[allow(dead_code)]` 是浓缩类 demo 的常客。

### 技巧 2：用确定性假数据，让行为可复现

真实数据靠网络、不可复现。demo 里所有「外部数据」都用**确定性算法**生成（`fake_rest_quote` 用股票代码的字节和做种子）。好处：任何人 `cargo run` 看到的行为都一样，讲解、截图、写测试都稳定。

### 技巧 3：一个后台任务模拟整个上游

真实项目的行情来自 SDK 的 WebSocket 订阅回调。demo 用一个 tokio 任务每 200ms 往 channel `tx.send` 一条假推送就顶替了——**主循环 `push_stream.next()` 的分支代码和接真 WS 时一模一样**，被替换的只有「谁在 send」。这保证了主循环的架构是真的，上游是假的。

### 技巧 4：保留可运行 + 可测试

浓缩的终点是「**能跑、能测**」，而不是一堆代码片段。tui-bevy-ecs 保留了 `cargo run`（真能在终端里跑起来）和 `cargo test`（keymap、脏标记有单测）。**能跑的 demo 才可信**——它证明你抽出来的骨架是自洽的，没有漏掉让系统转起来的关键环节。

## 五、这套方法能带来什么

**对学习者**：一个能跑的最小镜像，比读万行源码更快建立「架构直觉」。你可以放心地改它——删掉一层看会怎样、加一个系统看调度顺序——这些在生产代码里不敢做的实验，在 demo 里几秒钟就能验证。

**对重构者**：想重构大项目的某一层？先把它浓缩成 demo，在 demo 上试验新架构，验证通了再搬回去。demo 是你的「架构沙盒」。

**对写文档/博客的人**：浓缩后的 demo 每个文件都是一篇博客的天然素材——本系列前几篇（ECS 架构、async→sync 桥、两级节流、一帧的生命周期）全都是从这个 300 行 demo 里长出来的。**先浓缩，再逐块讲解**，是把复杂系统讲清楚的高效路径。

## 六、什么时候不该这么做

浓缩也有成本，不是所有场景都值：

- **架构本身很简单**：没什么骨架可抽，浓缩等于抄一遍。
- **难点在细节而非结构**：如果真正难的是某个算法、某个并发 bug，那该直接读那段代码，而不是做镜像。
- **会长期维护 demo**：镜像和真实项目会随时间漂移。要么明确 demo 是「一次性学习工具」，要么在真实项目里加注释指回 demo（tui-bevy-ecs 的注释就大量写着「Mirrors `src/tui/...`」，反向锚定真实位置）。

## 小结

面对一个庞大的 TUI 层，`tui-bevy-ecs` 的做法是**造一个约 300 行、可 `cargo run` 的最小镜像**：先列「概念→文件」映射表当蓝图，再逐行保留架构骨架、用假实现（`fake_rest_quote`、假推送任务、`#[allow(dead_code)]` 的完整形状）替换工程血肉，最后守住「能跑 + 能测」的底线。**保留骨架、砍掉血肉、确定性可复现**——这套方法既是快速看懂大项目的学习捷径，也是安全试验新架构的重构沙盒，还顺手产出了一整个系列博客的素材。

---

*完整源码：[github.com/spursy/tui-bevy-ecs](https://github.com/spursy/tui-bevy-ecs)*
