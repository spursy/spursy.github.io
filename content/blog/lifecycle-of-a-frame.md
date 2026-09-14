+++
title = "一帧的生命周期：从按键到终端上的一个像素"
date = 2026-09-14
description = "跟着一次按键和一次行情推送，完整追一帧在 tui-bevy-ecs 里的旅程：输入/推送 → 标脏 → 33ms tick → app.update() → run_if 选中系统 → terminal.draw 组装 widget → 双缓冲 diff → 落终端。把前几篇讲过的 ECS、async→sync 桥、两级节流串成一条时间线。"
[taxonomies]
tags = ["rust", "bevy", "ecs", "ratatui", "tui"]
+++

> 源码：[github.com/spursy/tui-bevy-ecs](https://github.com/spursy/tui-bevy-ecs)。前几篇分别拆了 ECS 架构、async→sync 桥、两级节流；这一篇把它们串成一条时间线——**追一帧从产生到消失的完整生命周期**。

## 一、先看全景：一个循环喂四条流

一切从主循环的 `tokio::select!` 开始（`app.rs:141`）。它同时盯着四条流，谁先就绪就处理谁：

```
                        tokio::select!  (app.rs:141)
   ┌──────────────┬─────────────────┬──────────────────┬─────────────────┐
   │ ① render_tick│ ② update_rx     │ ③ push_stream    │ ④ input_events  │
   │  每 33ms     │  CommandQueue   │  模拟行情推送     │  crossterm 按键  │
   │  脏才 update │  回灌 world     │  写 STOCKS+标脏   │  keymap→动作     │
   └──────┬───────┴────────┬────────┴─────────┬────────┴────────┬────────┘
          │                │                  │                 │
      needs_render?     cmd.apply         STOCKS.modify     handle_global_keys
      → app.update()    +mark_dirty       +mark_quote_update  → 切状态/发事件/动作
```

关键分工：**②③④ 只负责「改数据 + 标脏」，从不直接画屏；只有 ① render_tick 才真正驱动一帧渲染。** 这就是「拉取式渲染」——数据变了不推给终端，而是等下一个 tick 主动去读。

理解了这点，「一帧的生命周期」就分成两半:**上半场**（谁把这帧标脏了）和**下半场**（tick 到了怎么画出来）。

## 二、上半场：一帧是怎么「被需要」的

一帧的诞生，源于某条流把 `RenderState` 标脏。以两个最典型的触发源为例。

### 触发源 A：用户按下 ↓

```
④ input_events.next() 收到按键 (app.rs:174)
  → handle_global_keys() (input.rs:24)
      → keymap.lookup(&event, ctx) 把按键解析成 ActionId::Down (keymap.rs:101)
      → send_event(&mut world, Key::Down)  把导航事件塞进 world (input.rs:49)
      → render_state.mark_dirty(WATCHLIST | STOCK_DETAIL) 精准标脏 (input.rs:50)
```

注意这里**只发了事件、只标了脏**，屏幕此刻纹丝不动。`Key::Down` 事件静静躺在 world 里，等着渲染系统下一帧来消费。

### 触发源 B：一条行情推送到达

```
③ push_stream.next() 收到推送 (app.rs:166)
  → STOCKS.modify(counter, |s| s.update_from_push_quote(..)) 写缓存 (app.rs:167)
  → render_state.mark_dirty(NONE.mark_quote_update()) 只标价格区域 (app.rs:170)
```

同样只是「写缓存 + 标脏」。`mark_quote_update` 是高跳过率的关键——它不标 `ALL`，只标价格相关区域（`render.rs:39`）。

**上半场结束时的状态**：world 里可能多了个事件、`STOCKS` 里可能有了新价，`RenderState.dirty` 位集被点亮了几位。屏幕还是旧的。

## 三、下半场：33ms tick 到了，把这帧画出来

### 第 1 步：脏检查——这帧到底要不要画

```rust
_ = render_tick.tick() => {            // 每 33ms (app.rs:143)
    if render_state.needs_render() {   // 上半场标脏了吗？
        app.update();                  // 是 → 跑整个 ECS 一帧
        render_state.clear();          // 画完清脏
    } else {
        render_state.skip();           // 否 → 整帧跳过，连 app.update() 都不调
    }
}
```

这是**外层节流**：没脏就 `skip()`，整个 `app.update()` 都省了。大部分「没人动、没推送」的 tick 都在这里被挡掉。假设我们上半场标了脏，于是进入 `app.update()`。

### 第 2 步：`app.update()` → 跑调度 → `run_if` 选中系统

`app.update()` 内部 `world.run_schedule(...)` 跑 `Update` 调度里的**所有**系统，但每个 `render_*` 都挂了 `run_if(in_state(..))`（`app.rs:80-84`）。假设当前在 Watchlist 屏：

```
Update 调度里 5 个 render_* 系统全被「考虑」
  → run_if(in_state(Watchlist)) 只放行 render_watchlist
  → 其余 4 个被条件跳过
```

若这帧还伴随状态切换（比如刚按了 Enter），Bevy 会先应用 `NextState`、触发 `OnEnter/OnExit` 钩子，再跑新屏的 render。

### 第 3 步：`render_watchlist` 消费事件 + 读数据 + 组装 widget

选中的系统靠**参数注入**拿到它要的一切（`systems.rs:76`）：

```rust
pub fn render_watchlist(
    mut terminal: ResMut<Terminal>,   // 终端
    mut events: EventReader<Key>,     // 上半场发的 Key 事件
    mut selection: ResMut<Selection>, // 选中行
    (state, ws): NavFooter,
    mut frames: Local<u64>,
) {
    // ① 消费上半场攒下的导航事件，更新选中行
    for key in events.iter() {
        match key { Key::Down => selection.0 = (selection.0 + 1).min(..), .. }
    }
    // ② terminal.draw：读 STOCKS + selection，format! 成带颜色的 Span/Line
    terminal.draw(|frame| {
        // Layout 切区 → 每只股票拼一行 → render_widget 写进内存 Buffer
    });
}
```

这里「上半场发的事件」终于被消费——`Key::Down` 让 `selection.0` 加一。然后 `terminal.draw` 闭包里把 `STOCKS` 的价、`Selection` 的高亮行 `format!` 成 `Span/Line`，`render_widget` **写进内存 Buffer**（`systems.rs:154`）。**此刻仍未碰真实终端。**

### 第 4 步：双缓冲 diff——只挑变化的格子落终端

`terminal.draw` 返回前，ratatui 拿新 Buffer 和上一帧旧 Buffer 逐格比对，只把**变化的单元格**交给 `CrosstermBackend` 转成 ANSI 写 stdout：

```
新 Buffer ─┐
           ├─ 逐格 diff ─▶ 只有变化的格子 ─▶ CrosstermBackend ─▶ ANSI ─▶ stdout ─▶ 终端显示
旧 Buffer ─┘
```

这是**内层节流**（实现在 ratatui crate 内部）。比如触发源 B 只改了一个价格数字，最终写终端的可能就那么几个单元格，而不是整屏。

### 第 5 步：清脏，等待下一帧

```rust
render_state.clear();   // dirty 归零，render_count += 1
```

`RenderState` 记一笔「渲染 +1」，脏位清空。**这一帧的生命周期到此结束**——它从某条流的一次标脏中诞生，在 33ms tick 里被画出，最后归于清脏，等待下一次被「需要」。

## 四、完整时间线：一次行情推送追到底

把上下半场接起来，一次价格更新的完整旅程：

```
t=0ms    ③ push_stream 收到 700.HK 新价
         └─ STOCKS.modify 写缓存 + mark_quote_update() 标价格区域脏
         （屏幕此刻还是旧的）

t=0~33ms （若期间又有推送，只是继续标脏/写缓存，不重复渲染）

t=33ms   ① render_tick 触发
         ├─ needs_render()? → 脏 → app.update()
         │    └─ run_if(in_state(Watchlist)) → render_watchlist
         │         └─ terminal.draw：读 STOCKS 新价 → 写 Buffer
         │              └─ diff：只有那个价格数字变了 → 几个单元格的 ANSI → stdout
         └─ clear()：清脏，render_count += 1
         （终端上那个价格数字刷新了）
```

注意 **t=0 到 t=33ms 之间即使来了 5 条推送，也只在 t=33ms 渲染一次**——这就是「按 tick 拉取」相比「按事件推送」的省：多次数据变更被自然合并进一帧。

## 五、这一帧串起了前三篇的所有设计

| 环节 | 用到的设计 | 对应博客 |
| --- | --- | --- |
| `run_if(in_state)` 选中系统、参数注入 | ECS：状态机 + System 即依赖注入 | 《用 Bevy ECS 写终端 App》 |
| 推送写 `STOCKS`、后台刷新回 `UPDATE_TX` | async→sync 桥三件套 | 《同步框架里优雅地做异步》 |
| `needs_render()` 跳帧 + diff 只画变化 | 脏标记 + 双缓冲 diff 两级节流 | 《一帧是怎么省出来的》 |

**一帧的生命周期，恰好是这三块设计的一次合奏。**

## 小结

在 tui-bevy-ecs 里，一帧的生命是这样的：某条流（按键 / 推送 / 后台回传）**改数据并标脏**（上半场）；下一个 33ms tick 到来，主循环**脏才 `app.update()`**，Bevy 用 `run_if(in_state)` 选中当前屏的 render 系统，系统**消费事件、读数据、`terminal.draw` 组装 widget 写进内存 Buffer**，ratatui **双缓冲 diff 只把变化的单元格落到终端**，最后**清脏等待下一帧**（下半场）。**数据流与渲染流解耦、按 tick 拉取、两级节流**——这条流水线让一个实时行情 TUI 既跟手又省。

---

*完整源码：[github.com/spursy/tui-bevy-ecs](https://github.com/spursy/tui-bevy-ecs)*
