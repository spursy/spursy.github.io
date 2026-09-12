+++
title = "一帧是怎么省出来的：脏标记 + ratatui 双缓冲 diff 的两级节流"
date = 2026-09-13
description = "终端 App 也要抠性能。tui-bevy-ecs 用两级节流把 CPU 和终端 IO 省到极致：外层用脏标记（DirtyFlags 位集）决定「这帧要不要跑整个 ECS update」，内层用 ratatui 的双缓冲 diff 决定「画哪几个单元格」。本文拆解这套「拉取式渲染 + 双重省」的设计。"
[taxonomies]
tags = ["rust", "ratatui", "tui", "performance"]
+++

> 源码：[github.com/spursy/tui-bevy-ecs](https://github.com/spursy/tui-bevy-ecs)。本文聚焦渲染性能：如何用两级节流避免无谓的计算和终端写入。

## 一、终端 App 为什么也要抠性能

你可能觉得终端程序又不渲染 3D，能费多少 CPU？但一个「实时行情」TUI 有两个隐藏成本：

1. **每帧重算的成本**：如果每 33ms（30 FPS）都无脑跑一遍「读数据 → 排版 → 组装 widget」，即便屏幕根本没变，也白烧 CPU。
2. **终端写入的成本**：终端 IO 不便宜。把整屏字符全部重发一遍，会产生大量 ANSI 转义序列写进 stdout，在 SSH / 慢终端下尤其肉疼，还会闪烁。

tui-bevy-ecs 用**两级节流**分别干掉这两个成本：

| 层级 | 解决什么 | 手段 | 决定 |
| --- | --- | --- | --- |
| **外层** | 每帧重算的成本 | 脏标记（`DirtyFlags` 位集） | 这帧**要不要**跑整个 `app.update()` |
| **内层** | 终端写入的成本 | ratatui 双缓冲 diff | 要画的话，**画哪几个单元格** |

一句话：**外层决定「画不画」，内层决定「画哪里」。**

## 二、先理解前提：拉取式（pull）渲染

关键设计前提是——**数据变了不会「推」给终端**。而是「该画时，渲染系统主动去读数据快照再画」。这让「数据更新」和「屏幕重绘」解耦：数据可以频繁变，但只有在「该画的那一帧」才去读它的最新值。

正因为解耦，才有空间做外层节流：既然重绘是主动拉取的，那就可以在「拉取前」先问一句「有必要拉吗？」——没必要就整帧跳过。

## 三、外层：脏标记决定「这帧要不要画」

### 位集：用一个 u32 记录「哪些区域脏了」

```rust
bitflags! {
    pub struct DirtyFlags: u32 {
        const NONE         = 0;
        const WATCHLIST    = 0b0000_0001;
        const STOCK_DETAIL = 0b0000_0010;
        const PORTFOLIO    = 0b0000_0100;
        const INDEXES      = 0b0000_1000;
        const POPUP_HELP   = 0b0001_0000;
        const STATUS_BAR   = 0b1000_0000_0000;
        const ALL          = 0xFFFF_FFFF;
    }
}
```

每一位代表一个 UI 区域。用位集的好处：**标脏、检测、合并都是单条位运算，几乎零成本**，还能一次表达「多个区域同时脏」。

### 精准标脏：行情推送只脏「价格区域」

最关键的一个方法：一次行情推送**不该**让整屏都脏，它只影响显示实时价格的区域：

```rust
pub fn mark_quote_update(mut self) -> Self {
    // 只标价格相关区域，而不是 ALL
    self.insert(Self::WATCHLIST | Self::STOCK_DETAIL | Self::INDEXES | Self::STATUS_BAR);
    self
}
```

> 源码注释一针见血：*"A quote push only touches the regions that show live prices — not the whole screen. This is the key to a high skip rate."*（行情推送只碰显示实时价的区域，不碰整屏——这是高跳过率的关键。）

对比一下不同事件的标脏范围：

| 事件 | 标脏范围 | 代码 |
| --- | --- | --- |
| 行情推送 | 仅价格区域 | `mark_quote_update()` |
| 上下移动光标 | `WATCHLIST \| STOCK_DETAIL` | `input.rs` |
| 切屏 | `ALL`（整屏必须重画） | `mark_all_dirty()` |
| 后台刷新回来 | `ALL` | 主循环 |

### 主循环：脏才跑，不脏就跳

外层节流的落点在主循环的渲染分支：

```rust
_ = render_tick.tick() => {            // 每 33ms
    if render_state.needs_render() {   // 有脏标记？
        app.update();                  // 是 → 跑整个 ECS 一帧
        render_state.clear();          // 画完清脏
    } else {
        render_state.skip();           // 否 → 整帧跳过，连 app.update() 都不调
    }
}
```

**注意「跳过」跳掉的是整个 `app.update()`**——所有系统的读数据、排版、组装 widget 全省了。对一个大部分时间「没人操作、也没新推送」的 TUI，这个跳过率可以非常高。

### 顺带量化「省了多少」

`RenderState` 还内建了一个效率读数，把「跳过率」直接算出来：

```rust
pub fn efficiency(&self) -> f64 {
    let total = self.render_count + self.skip_count;
    if total == 0 { 0.0 }
    else { (self.skip_count as f64 / total as f64) * 100.0 }
}
// stats() → "renders: 12, skips: 88, efficiency: 88.0%"
```

`efficiency` 就是**被跳过的帧占比**——这正是脏标记带来的收益，可以直接打在状态栏上自证。

## 四、内层：ratatui 双缓冲 diff 决定「画哪几个格子」

假设外层判定「这帧要画」，进了 `app.update()`，渲染系统在 `terminal.draw(|frame| ...)` 闭包里组装 widget。**但组装好的 widget 并不是直接写终端的**——它先写进一块内存 `Buffer`（一个二维数组，每格存「字符 + 前景色 + 背景色 + 修饰」）：

```rust
frame.render_widget(list, area);   // 只写进内存 Buffer，此刻还没碰真实终端
```

真正落终端时，**ratatui 会拿「这一帧的新 Buffer」和「上一帧的旧 Buffer」逐格比对（diff）**，只挑出**变化了的单元格**，交给 `CrosstermBackend` 转成 ANSI 序列写 stdout：

```
新 Buffer ─┐
           ├─ 逐格 diff ─▶ 只有变化的格子 ─▶ CrosstermBackend ─▶ ANSI ─▶ stdout
旧 Buffer ─┘
```

所以哪怕外层放行了一整帧，内层也只会把**真正变化的那几个字符**发给终端。比如行情推送只改了一个价格数字，最终写终端的可能就那么几个单元格，而不是整屏。

> 这套逻辑在 ratatui crate 内部（本工程只调 `terminal.draw`，看不到实现）。原理其实很简单：`Terminal` 持有**两块 Buffer**（当前帧 / 上一帧），`draw` 结束时做两件事——

内层其实是**双重节流**，分别省在两个维度：

```rust
// ① 空间维度：Buffer::diff 逐格比对，只收变化的格子
pub fn diff(&self, other) -> Vec<(x, y, &Cell)> {
    for (cur, prev) in other.zip(self) {
        if cur != prev { updates.push((x, y, cur)); }  // 变了才收
    }
}
// ② 输出维度：后端只为差异格子发 ANSI，且颜色/光标做增量
for (x, y, cell) in updates {
    if !紧邻上一格 { MoveTo(x, y) }          // 不连续才发光标移动
    if cell.fg != 上一格.fg { SetFg(..) }    // 颜色变了才发色码
    Print(cell.symbol);
}
// 最后交换两块 Buffer：这一帧变成「上一帧」，备用块清空
```

- **① diff（空间省）**：一屏几千格，只有一个价格变，`updates` 里就只有那几项。
- **② 后端增量（转义序列省）**：连续同色的一串格子不会重复发颜色码，光标也只在不连续时才移动。

两步叠加，把「昂贵的终端写入」压到最小。

## 五、两级配合：一次行情推送的完整旅程

```
行情推送到达（每 200ms 一次）
  │
  ├─【外层】mark_quote_update() → 只标价格区域脏（不是 ALL）
  │
  └─ 下一个 render tick（33ms）
        │ needs_render()? → 有脏 → app.update()
        │                   无脏 → skip()（整帧省掉）
        ▼
     渲染系统 terminal.draw：读 STOCKS 组装 widget → 写内存 Buffer
        │
        └─【内层】新旧 Buffer diff → 只有那个价格数字变了
                    → 只把几个单元格的 ANSI 写进 stdout
```

两级各挡一层浪费：

- **外层**挡掉「屏幕没变还重算」——没脏就连 `app.update()` 都不跑。
- **内层**挡掉「重算了还全屏重写」——只写变化的格子。

即便外层偶尔误放（比如标了 `ALL` 但实际只有一格变），**内层 diff 还能兜底**，保证终端写入永远只发真正的差异。这是一种「双保险」：任一层失手，另一层还能省。

## 六、可迁移的设计心法

这套两级节流不止用于 TUI，任何「按帧/按 tick 刷新 + 有明确变更来源」的渲染系统都适用：

1. **拉取式而非推送式**：让「数据更新」和「重绘」解耦，才有空间在重绘前判断「要不要」。
2. **用位集做细粒度脏标记**：不同事件标不同区域，让「无关操作」尽量不触发重绘；标脏/检测都是零成本位运算。
3. **重绘前先问「有必要吗」**：外层跳过整个重算，是最大的一笔省。
4. **底层再做 diff 兜底**：即使上层放行，也只把真正的差异落到昂贵的输出（终端 / 网络 / DOM）。
5. **把跳过率量化出来**：`efficiency()` 这种读数能让优化「可见」，也便于回归时发现「怎么跳过率突然掉了」。

## 小结

tui-bevy-ecs 的渲染是一条**拉取式 + 两级节流**的流水线：外层用 `DirtyFlags` 位集决定「这帧要不要跑整个 `app.update()`」（行情推送只标价格区域，是高跳过率的关键），内层用 ratatui 双缓冲 diff 决定「画哪几个单元格」。**Bevy 管调度与数据注入，ratatui + crossterm 管把字符高效打到终端**——两级各省一层，任一层失手另一层兜底。

这是本系列的收尾。三篇分别讲了 **ECS 做 TUI 的架构新意**、**async→sync 桥的异步难点**、以及**两级节流的性能优化**——覆盖了这个小 demo 里最值得沉淀的三块设计。

---

*完整源码：[github.com/spursy/tui-bevy-ecs](https://github.com/spursy/tui-bevy-ecs)*
