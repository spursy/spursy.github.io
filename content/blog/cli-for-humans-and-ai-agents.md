+++
title = "同时服务人和 AI agent 的 CLI 输出设计"
date = 2026-08-18
description = "以 cli-terminal 为例，聊聊一个命令行工具怎么用一份中立数据、一个渲染函数，同时喂饱「读表格的人」和「吃 JSON 的 agent 与脚本」，以及 clap 两级帮助等几处 CLI 人机工程学的小心思。"
[taxonomies]
tags = ["rust", "cli"]
+++

过去我们设计 CLI 的输出，读者只有一个：坐在终端前的人。所以对齐的表格、彩色高亮、友好的提示语是第一优先级。但现在多了一类越来越重要的「读者」——**脚本和 AI agent**。它们不看排版，只要能被 `jq` 解析、能被程序稳定消费的结构化数据。

[cli-terminal](https://github.com/spursy/cli-terminal) 这个演示 OAuth 设备码登录的小工具，在输出层用一个很轻的设计同时照顾了这两类受众。这篇就拆一下它，以及顺带几处 CLI 人机工程学的小选择。

<!-- more -->

## 一、核心思路：命令只产出数据，渲染集中在一处

最容易踩的坑，是让每个命令自己 `println!` 排版。这样一来「加一种输出格式」就得改遍所有命令，而且人类格式和机器格式的逻辑会散落各处、互相打架。

cli-terminal 的做法是把两件事拆开：

- **命令只负责产出一个中立的数据结构**——这里直接用 `serde_json::Value`；
- **一个** render 函数根据 `--format` 决定怎么把它打出来。

于是 `auth status` 的处理函数长这样，它完全不关心最终是表格还是 JSON：

```rust
pub fn status(format: &OutputFormat) -> Result<()> {
    let value = match token::load()? {
        None => serde_json::json!({ "logged_in": false }),
        Some(t) => serde_json::json!({
            "logged_in": true,
            "client_id": t.client_id,
            "token": if t.is_expired() { "expired" } else { "valid" },
        }),
    };
    crate::output::print_json_value(&value, format);
    Ok(())
}
```

**加一种新格式 = 加一个 match 分支，不碰任何命令**——这是这套设计最大的收益。

## 二、两类受众，两种格式

格式本身就是一个 clap 的 `ValueEnum`，只有两个取值：

```rust
#[derive(ValueEnum, Clone, Copy, Default, Debug)]
pub enum OutputFormat {
    /// 人类可读的对齐表格（默认）
    #[default]
    #[value(name = "table", alias = "pretty")]
    Table,
    /// 机器可读 JSON —— 给脚本、jq 和 AI agent
    Json,
}
```

两个细节值得注意：

- **默认是 `table`**（`#[default]`）。人在终端敲命令是最常见的场景，默认就该照顾人；agent 显式加 `--format json` 表达意图。
- **`alias = "pretty"`**：`--format pretty` 和 `--format table` 等价。这种小别名能省掉用户「到底叫 table 还是 pretty」的记忆负担，成本几乎为零。

render 函数按格式分流：

```rust
pub fn print_json_value(value: &serde_json::Value, format: &OutputFormat) {
    match format {
        OutputFormat::Json => {
            println!("{}", serde_json::to_string_pretty(value).unwrap_or_default());
        }
        OutputFormat::Table => match value {
            serde_json::Value::Object(map) => {
                let rows: Vec<(String, String)> = map
                    .iter()
                    .map(|(k, v)| (k.clone(), scalar_to_string(v)))
                    .collect();
                print_two_column(&["Field", "Value"], &rows);
            }
            _ => println!("{}", serde_json::to_string_pretty(value).unwrap_or_default()),
        },
    }
}
```

于是同一条命令，人看到的是：

```
Field      Value
---------  -----
logged_in  true
client_id  cli-terminal
token      valid
```

而 `--format json` 给 agent 的是可以直接 `jq '.token'` 的结构化数据：

```json
{
  "logged_in": true,
  "client_id": "cli-terminal",
  "token": "valid"
}
```

## 三、零依赖的表格对齐

表格这块没有引入任何 table crate，就是一段两列对齐的打印。先扫一遍所有 key（连表头一起）算出最宽的那个，再用它做左对齐的填充宽度：

```rust
fn print_two_column(headers: &[&str; 2], rows: &[(String, String)]) {
    let width = rows
        .iter()
        .map(|(k, _)| k.len())
        .chain(std::iter::once(headers[0].len()))
        .max()
        .unwrap_or(0);

    println!("{:<width$}  {}", headers[0], headers[1], width = width);
    println!("{}  {}", "-".repeat(width), "-".repeat(headers[1].len()));
    for (k, v) in rows {
        println!("{k:<width$}  {v}", width = width);
    }
}
```

对一个只有几个字段的演示工具，为「对齐」引一个依赖并不划算。**依赖是有成本的**——编译时间、供应链面、版本升级——能用十几行标准库解决的排版，就别引库。这不是「造轮子」，而是把复杂度控制在问题的实际规模上。

标量渲染也顺手处理了人机差异：字符串**去掉引号**（人读表格不想看到 `"valid"`），`null` 显示成 `-`：

```rust
fn scalar_to_string(v: &serde_json::Value) -> String {
    match v {
        serde_json::Value::String(s) => s.clone(),
        serde_json::Value::Null => "-".to_string(),
        other => other.to_string(),
    }
}
```

## 四、不静默丢数据的回退

表格模式只对**顶层是 object** 的数据有意义（能摊成 `Field | Value` 两列）。那如果某条命令产出的是数组、或一个嵌套结构呢？

注意上面 `Table` 分支里的 `_ =>`：**任何非 object 的形状，都回退成 pretty JSON**，而不是硬套表格或者干脆不打。

```rust
_ => println!("{}", serde_json::to_string_pretty(value).unwrap_or_default()),
```

这条「回退」很关键：它保证**没有数据会被静默丢弃**。一个输出层最忌讳的就是「某种输入下什么都不显示」，那会让调用者以为是空结果。宁可退化成不那么好看的 JSON，也要把数据完整交出去。

## 五、`--format` 是全局 flag

`--format` 挂在顶层 `Cli` 上，并且是 `global = true`：

```rust
#[arg(long, global = true, default_value = "table")]
format: OutputFormat,
```

`global = true` 让它对**所有子命令**都生效——`cli-terminal auth status --format json` 和 `cli-terminal whoami --format json` 都认。用户不用记「哪个命令支持 json、哪个不支持」，心智模型是统一的：**任何命令都能切格式**。`--verbose` 也用了同样的手法。

## 六、顺带一提：clap 的两级帮助

CLI 的「文档」就是它的 `--help`，cli-terminal 在这里也做了区分。它给每个命令都写了两级帮助——短横线 `-h` 给简要、`--help` 给带示例的详细版：

```rust
#[command(
    after_help = "Each command has two help levels:\n  \
      cli-terminal <command> -h       brief summary (options only)\n  \
      cli-terminal <command> --help   full detail: description and examples"
)]
```

这靠 clap 的一个约定实现：doc 注释的**第一行是 summary**（`-h` 显示），**空行之后的段落是 long help**（`--help` 显示）。所以命令定义里的注释是这样分层的：

```rust
/// Show the identity associated with the stored token
///
/// Calls the protected `GET /userinfo` endpoint with the stored bearer token.
/// Requires a valid, non-expired token; a 401 prompts re-authentication.
/// Example: cli-terminal whoami
/// Example: cli-terminal whoami --format json
Whoami,
```

赶时间的人 `-h` 一眼扫过选项；第一次用的人 `--help` 看描述和示例。**同一份注释，喂出两种详略**——又一次「一份来源、多种呈现」。

## 七、小结

这篇的几个点，本质上是同一条原则的不同侧面：

| 设计点 | 通用经验 |
|--------|---------|
| 命令产出中立数据，渲染集中一处 | 「产生数据」和「呈现数据」分层；加格式只加一个分支 |
| table 默认 / json 显式 + `pretty` 别名 | 默认照顾人，机器显式表达意图；廉价别名降低记忆负担 |
| 零依赖表格对齐 | 把复杂度控制在问题的实际规模，别为小事引库 |
| 非 object 回退 pretty JSON | 输出层绝不静默丢数据 |
| `--format` 全局 flag | 统一心智模型：任何命令都能切格式 |
| clap 两级帮助 | 一份 doc 注释喂出简要 / 详细两种帮助 |

在 AI agent 越来越多地直接调用命令行工具的今天，「机器可读输出」已经从锦上添花变成一个 CLI 的基本素养。而做到它其实不需要多重的机制——一份中立数据、一个渲染分流点，就够了。完整代码见 [github.com/spursy/cli-terminal](https://github.com/spursy/cli-terminal)。

## 关键源码位置索引

| 内容 | 位置 |
|------|------|
| 输出格式枚举 / 渲染分流 / 表格对齐 | [`src/output.rs`](https://github.com/spursy/cli-terminal/blob/main/src/output.rs) |
| 全局 `--format` flag / 两级帮助 / 命令定义 | [`src/cli/command.rs`](https://github.com/spursy/cli-terminal/blob/main/src/cli/command.rs) |
