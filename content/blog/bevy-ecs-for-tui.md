+++
title = "用 Bevy ECS 写终端 App：不只是游戏引擎"
date = 2026-09-11
description = "ECS 常被当成游戏引擎的专利，但它的本质是「数据与行为分离」。本文用一个可运行的终端股票行情 demo（tui-bevy-ecs），讲清为什么 TUI 也适合 ECS：状态机、资源、事件、系统如何各司其职，以及 System 即依赖注入的魔法。"
[taxonomies]
tags = ["rust", "bevy", "ecs", "tui"]
+++

> 源码：[github.com/spursy/tui-bevy-ecs](https://github.com/spursy/tui-bevy-ecs)。这是一个自包含、可 `cargo run` 的小程序，把 longbridge-terminal 的 TUI 层浓缩进一个 300 行左右的 binary。

## 一、ECS 不是游戏的专利

一提 ECS（Entity-Component-System），大多数人想到的是游戏引擎里成千上万个精灵、粒子。但 ECS 的**内核其实和游戏无关**，它是一种架构模式：

> **把数据（Component/Resource）和行为（System）彻底拆开。** 数据存在一个大仓库（World）里，行为是独立的函数，按需去仓库读写。

对比一下面向对象：

| | 面向对象（OOP） | ECS |
| --- | --- | --- |
| 数据和方法 | 绑在一起（`player.move()`） | 分开（`move_system` 去查所有能动的数据） |
| 扩展方式 | 继承、多态 | 组合数据 + 加系统 |
| 心智模型 | 「对象自带能力」 | 「数据是数据，逻辑是逻辑」 |

一旦接受「数据 vs 行为分离」这个前提，你会发现**很多非游戏程序也适合它**——尤其是有多种「模式/页面」、需要在这些模式间切换、每种模式下跑不同逻辑的程序。终端 TUI 恰好就是这样。

## 二、demo 长什么样

`tui-bevy-ecs` 是个终端里的股票行情面板：

```
cargo run     # 启动 TUI
```

按键：`1` 自选列表 · `2` 持仓 · `3` 订单 · `j`/`k` 或 `↑`/`↓` 移动 · `Enter` 打开详情 · `Esc` 返回 · `r` 刷新 · `q` 退出。

它有多个「屏幕」（Loading / Watchlist / Stock 详情 / Portfolio / Orders），屏幕之间靠状态机切换，每个屏幕跑自己的渲染逻辑——**这正是 ECS 状态机的用武之地**。

## 三、Bevy ECS 的五个构件

经典 ECS 只有 Entity / Component / System 三件。Bevy 在此之上扩展了 **Resource** 和 **State**，让它更适合做完整应用。在本 demo 里，五个构件的分工是：

| 构件 | 本质 | 数据 or 行为 | demo 里的例子 |
| --- | --- | --- | --- |
| **State 状态** | 当前处在哪个屏幕（本质是特殊 Resource） | 数据 | `AppState`（Loading/Watchlist/Stock…） |
| **Event 事件** | 一次性的消息，读走即消费 | 数据（瞬时） | `Key`（Up/Down/Enter 导航意图） |
| **Resource 资源** | 全局唯一、持续存在的数据 | 数据 | `Terminal`、`Selection`、`StockDetail`、`WsState` |
| **System 系统** | 处理逻辑的函数 | **行为** | `render_watchlist`、`enter_stock`… |
| Entity/Component | 挂在实体上的数据 | 数据 | 本 demo 几乎不用（TUI 没有海量实体） |

一句话记忆：**状态管「现在是哪屏」，事件管「刚发生了什么」，资源管「持久的数据」，系统管「干活的逻辑」。前三个是数据，系统是行为。**

> 值得注意：本 demo **几乎不用 Entity/Component**。因为 TUI 应用没有成千上万个带位置/血量的实体要批量处理，它需要的都是「全局唯一的数据」（当前选中行、当前股票、终端句柄），这天然适合 Resource。这是「用 ECS 但只用 Resource 部分」的典型精简用法。

## 四、状态机：多屏路由

Bevy 用状态机来做「当前在哪个屏幕」。注册时一行搞定：

```rust
app.add_state::<AppState>()
```

它会自动插入两个资源：`State<AppState>`（当前值）和 `NextState<AppState>`（待切换目标）。切屏不是立即生效，而是**先排队**：

```rust
// 想切到 Stock 屏：先排队，下一帧才真正切
app.world.insert_resource(NextState(Some(AppState::Stock)));
```

下一次 `app.update()` 时，Bevy 的状态转换调度会读 `NextState`、把 `State<AppState>` 改成新值，并触发对应的 `OnEnter`/`OnExit` 钩子。

**门控系统**是状态机最漂亮的地方。每个屏幕注册自己的渲染系统，用 `run_if(in_state(..))` 限定只在对应状态下跑：

```rust
.add_systems(Update, systems::render_watchlist.run_if(in_state(AppState::Watchlist)))
.add_systems(Update, systems::render_stock.run_if(in_state(AppState::Stock)))
// 进入/离开 Stock 屏的一次性钩子
.add_systems(OnEnter(AppState::Stock), systems::enter_stock)
.add_systems(OnExit(AppState::Stock), systems::exit_stock);
```

机制上，**每帧都会跑 `Update` 调度里的所有系统**，但 `run_if(in_state)` 让只有「当前屏」那个 `render_*` 真正执行——其余被条件跳过。这就像前端路由：URL 变了，只渲染匹配的那个页面组件。

## 五、System 即依赖注入：参数就是声明

这是 ECS 最「啊哈」的一点。看 `render_watchlist` 的签名：

```rust
pub fn render_watchlist(
    mut terminal: ResMut<Terminal>,   // 要一个资源（读写）
    mut events: EventReader<Key>,     // 要读一个事件
    mut selection: ResMut<Selection>, // 要一个资源（读写）
    (state, ws): NavFooter,           // 要两个只读资源（打包）
    mut frames: Local<u64>,           // 系统私有、跨帧保留
) { /* ... */ }
```

**系统从不自己构造依赖，它在参数里「声明」需要什么，Bevy 按类型注入。** 关键规则：

- 参数类型必须实现 `SystemParam`（`Res`/`ResMut`/`EventReader`/`Local`/`Commands`/元组…都实现了）。
- **按类型匹配，不按顺序**——参数顺序随便换，行为不变。
- **只取所需**——`render_watchlist` 用不到 `StockDetail` 就不声明；`render_stock` 才用 `Res<StockDetail>`。
- 元组可以打包（`NavFooter` 是 `(Res<State<AppState>>, Res<WsState>)` 的类型别名），让签名更短。

`Local<T>` 尤其巧妙：它不来自 World，是**系统私有的、跨帧保留的**状态。这里 `frames: Local<u64>` 就是这个系统自己的帧计数器，每帧 `*frames += 1`，无需污染全局。

这套设计的收益：**任何一个系统单看签名，就知道它读写哪些数据**——依赖一目了然，测试时也容易替换。

## 六、把它们串起来：一帧发生了什么

```
用户按 ↓
  → 输入层查 keymap 得到 ActionId::Down
  → 发一个 Event(Key::Down) 进 World  + 标脏
  → 下一个 render tick，app.update() 跑一帧
      → run_if(in_state(Watchlist)) 命中 render_watchlist
          → EventReader<Key> 读到 Down，改 ResMut<Selection>
          → terminal.draw(...) 把列表画到终端
```

数据（Event、Selection）和行为（render_watchlist）自始至终是分开的：输入层只**发事件、改资源**，渲染系统只**读事件、读资源、画**。谁都不直接调用谁——**这就是 ECS 解耦的威力**。

## 七、什么时候该在非游戏项目里用 ECS

ECS 不是银弹。它的甜区是：

- 程序有**多种明确的模式/状态**，且模式间要切换（TUI 多屏、编辑器多模式、状态机驱动的服务）。
- 有**一批全局共享的数据**要被多处读写，你想避免到处传引用。
- 你想让**逻辑（系统）彻底独立、可组合、可条件启用**。

如果你的程序只是线性流程、没什么共享状态，那 ECS 反而是过度设计。但对「多屏 TUI」这种场景，它带来的清晰度非常值。

## 小结

Bevy ECS 的内核是「数据与行为分离」，和游戏无关。本 demo 用它的 **State（多屏路由）+ Resource（全局数据）+ Event（瞬时消息）+ System（依赖注入的逻辑）** 四件套，把一个终端行情面板组织得干净、可扩展——**System 的参数即依赖声明，Bevy 按类型注入**，是整套设计最优雅的一环。

下一篇我会讲这个 demo 里最硬核的部分：**同步的 ECS 系统如何优雅地对接异步 SDK**——`RT` / `STOCKS` / `UPDATE_TX` 三件套搭起的 async→sync 桥。

---

*完整源码：[github.com/spursy/tui-bevy-ecs](https://github.com/spursy/tui-bevy-ecs)*
