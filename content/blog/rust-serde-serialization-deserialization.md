+++
title = "Rust 序列化实战：serde 与 serde_json 到底怎么配合"
date = 2026-08-27
description = "从一份真实的 CLI 代码出发，讲清 Rust 里 serde 与 serde_json 的关系：序列化/反序列化的方向、Serializer/Deserializer 抽象、Value 动态类型，以及如何写一个宽容的自定义反序列化器。"
[taxonomies]
tags = ["rust", "serde"]
[extra]
toc = true
+++

在 Rust 里做 API 通信、配置解析、数据存储，几乎绕不开 `serde` 和 `serde_json`。但这两个名字常常一起出现，很多人分不清它们各自负责什么，也说不清「序列化」和「反序列化」到底是两个什么方向相反的动作。

这篇文章从一份真实的 CLI 代码出发（一个和后端 JSON 服务通信的 Rust 命令行工具），把这条链路上的核心概念一次讲清楚：`serde` 与 `serde_json` 的分工、序列化/反序列化的方向、`Serializer`/`Deserializer` 抽象接口、`Value` 动态类型，以及如何写一个「宽容型」的自定义序列化和反序列化器来兼容不规范的服务端数据。

<!-- more -->

## 一、序列化 vs 反序列化：两个方向相反的动作

先把最基础的方向理清楚，后面所有概念都建立在它之上。

- **序列化（Serialize）**：把 Rust 内存里的**结构体/值** → 转成可传输/存储的**字节流或文本**（如 JSON）。方向：`内存 → 外部`。
- **反序列化（Deserialize）**：把外部的**字节流/文本** → 还原成 Rust 的**结构体/值**。方向：`外部 → 内存`。

两者互为逆操作：

```
   Rust struct ──serialize──▶  "{\"promptToken\": 123}"   （发送 / 写文件）
   Rust struct ◀─deserialize── "{\"promptToken\": 123}"   （接收 / 读文件）
```

为什么必须分成两个？因为方向相反、输入输出类型完全不同，无法用一个函数表达。看两个自定义函数的签名差异就一目了然：

```rust
// 反序列化：拿到一个「读取器」，吐出目标类型
fn de_i64_flexible<'de, D: Deserializer<'de>>(d: D) -> Result<i64, D::Error>

// 序列化：拿到「值 + 写入器」，把值写进去
fn ser_i64_as_string<S: Serializer>(v: &i64, s: S) -> Result<S::Ok, S::Error>
```

对应到 serde 的两个 trait：

| | `Serialize` | `Deserialize` |
|---|---|---|
| 方法 | `fn serialize(&self, ...)` 读自己 | `fn deserialize(...) -> Self` 造出自己 |
| 数据流向 | 内存 → 外部 | 外部 → 内存 |
| 谁需要 | 你要**发送/保存**这个类型时 | 你要**接收/加载**这个类型时 |

一个类比：把结构体想成一件**组装好的家具**。序列化 = 拆开打包成扁平的板材（方便运输），反序列化 = 按图纸把板材重新组装成家具。打包和组装是两套不同的动作，所以需要两个 trait。

Rust 为什么要**显式声明**这两个 trait，而不是像 Python/Go 那样运行时自动完成？因为 **Rust 没有运行时反射**。为了零成本抽象，Rust 不携带运行时类型信息，所以 serde 用 `#[derive(Serialize, Deserialize)]` 在**编译期**生成对应代码。这也意味着两个方向可以独立存在——只往外发的请求参数只需要 `Serialize`，只解析的响应只需要 `Deserialize`。

## 二、serde vs serde_json：引擎与插件

理清方向后，再看这两个 crate 的分工。一句话：**`serde` 是「框架 / 引擎」，`serde_json` 是「某个具体格式的插件」。**

| | **serde** | **serde_json** |
|---|---|---|
| 角色 | 序列化**框架核心** | JSON **格式实现** |
| 提供什么 | `Serialize` / `Deserialize` trait、`Serializer` / `Deserializer` 抽象接口、`#[derive]` 宏 | JSON 读写逻辑、`Value`、`json!` 宏、`to_string`/`from_str` 等函数 |
| 绑定格式 | **不绑定任何格式** | **只管 JSON** |

`serde` 定义「怎么序列化」的抽象规则，但**不产出任何具体格式的数据**；`serde_json` 才真正把数据变成 JSON、或从 JSON 还原回来。同一套 serde 核心之上还有 `serde_yaml`（YAML）、`toml`（TOML）、`bincode`（二进制）等实现：

```
                ┌─ serde_json   → JSON
   serde ───────┼─ serde_yaml   → YAML
  (定标准/引擎)   ├─ toml         → TOML
                └─ bincode      → 二进制
```

这样拆分的最大好处是**解耦格式和逻辑**：你的结构体只需加**一次** serde 的派生宏，就能同时支持多种格式，换格式只换实现库，结构体一个字都不用改：

```rust
#[derive(Serialize, Deserialize)]
struct DetailGroup { /* ... */ }

serde_json::to_string(&g)?;   // 变 JSON
serde_yaml::to_string(&g)?;   // 变 YAML   ← 结构体没改
toml::to_string(&g)?;         // 变 TOML
```

## 三、Serializer 与 Deserializer：抽象的读写器

`Serialize`/`Deserialize`（无 r 结尾）描述的是**数据方**——「我这个类型能被转」。而 `Serializer`/`Deserializer`（有 r 结尾）描述的是**格式方**——「我负责转成/自某种格式」，由 serde_json 这类库来实现。

| | **`Serializer`** | **`Deserializer`** |
|---|---|---|
| 方向 | Rust 值 → 格式（输出） | 格式 → Rust 值（输入） |
| 角色 | 写入器 / 编码器 | 读取器 / 解码器 |
| 典型方法 | `serialize_i64`、`serialize_str`、`serialize_map`… | `deserialize_i64`、`deserialize_str`… |

一个类比：`Serializer` 像**打印机**（你把文档发给它，它按自己的方式打印成 JSON/YAML）；`Deserializer` 像**扫描仪 / OCR**（你给它一张纸，它识别成结构化数据）。

### S::Ok 是什么

自定义序列化函数的返回类型里有个 `S::Ok`，它是 `Serializer` trait 的一个**关联类型**：

```rust
pub trait Serializer {
    type Ok;      // 序列化成功时产出的类型
    type Error;   // 序列化失败时的错误类型
}
```

`S::Ok` 具体是什么由实现决定：serde_json 写入缓冲区的序列化器，`Ok` 是 `()`；而 `to_value` 用的序列化器，`Ok` 是 `serde_json::Value`。所以自定义函数**不能写死**返回类型，必须用 `S::Ok` 原样透传，才能适配任意格式：

```rust
fn ser_i64_as_string<S: Serializer>(v: &i64, s: S) -> Result<S::Ok, S::Error> {
    s.serialize_str(&v.to_string())   // 本身就返回 Result<S::Ok, S::Error>
}
```

用关联类型而非泛型参数，是因为对一个**确定的序列化器**来说产出类型是**唯一固定**的——JSON 写入器永远产出 `()`，不会一会儿这个一会儿那个。

## 四、serde_json::Value：JSON 的万能容器

有时候你**不知道或不关心**完整结构，只想接住任意一段 JSON。这时用 `serde_json::Value`——它是一个枚举，能递归表示任意 JSON：

```rust
pub enum Value {
    Null,                       // null
    Bool(bool),                 // true / false
    Number(Number),             // 数字
    String(String),             // 字符串
    Array(Vec<Value>),          // 数组：元素还是 Value
    Object(Map<String, Value>), // 对象：值还是 Value
}
```

它类似 Python 的 `dict`/`list` 混合体、Go 的 `interface{}`。区别是 Rust 用 enum 把所有 JSON 形态**穷举成有限几种**，`match` 时编译器能检查是否漏了分支，更安全。

常用取值方法都返回 `Option`，类型不匹配给 `None`，不会 panic：

```rust
let period = g.map_data
    .get("period")            // Option<&Value>
    .and_then(|v| v.as_str()) // Value → Option<&str>
    .unwrap_or("-");
```

选型建议：结构固定、字段已知时用 `#[derive(Deserialize)]` 结构体（类型安全、有编译检查）；结构动态/未知/只透传时才用 `Value`（灵活，但失去编译期保证，且访问要一路判空）。

## 五、实战：写一个「宽容型」序列化和反序列化器

现在把上面的概念串起来看一个真实问题。有些后端不规范，同一个数量字段有时返回数字、有时返回字符串，甚至空串或 null：

```json
"promptToken": 151285168      // 数字
"promptToken": "151285168"    // 字符串
"promptToken": ""             // 空串
"promptToken": null           // null
```

serde 默认要求 `i64` 字段必须是 JSON 数字，遇到字符串/null 会直接报错。解决办法是写一个自定义反序列化器，用 `#[serde(deserialize_with = "...")]` 挂到字段上：

```rust
use serde::{Deserialize, Deserializer};

/// 反序列化一个 i64：服务端可能发数字，也可能发带引号的字符串。
/// 空串和 null 映射为 0。
fn de_i64_flexible<'de, D>(deserializer: D) -> Result<i64, D::Error>
where
    D: Deserializer<'de>,
{
    match serde_json::Value::deserialize(deserializer)? {
        serde_json::Value::Null => Ok(0),
        serde_json::Value::Number(n) => n
            .as_i64()
            .ok_or_else(|| serde::de::Error::custom(format!("invalid i64 number: {n}"))),
        serde_json::Value::String(s) => {
            let s = s.trim();
            if s.is_empty() {
                Ok(0)
            } else {
                s.parse::<i64>().map_err(serde::de::Error::custom)
            }
        }
        other => Err(serde::de::Error::custom(format!(
            "expected i64 number or string, got {other}"
        ))),
    }
}
```

拆解几个关键点：

- **签名**：`fn(D) -> Result<i64, D::Error> where D: Deserializer<'de>` 是 serde 规定的自定义反序列化器固定形式。`where D: Deserializer<'de>` 这个 **trait 约束**是准入门槛——它既限制调用方只能传合格的「读取器」，又赋予函数体使用该 trait 全部能力的权利（比如函数里能写 `D::Error` 这个关联类型）。
- **策略**：先把输入解析成通用的 `serde_json::Value`（不预设类型），再手动 `match` 分情况——这正是「先接住任意 JSON，再自己判断」的典型用法。
- **`serde::de::Error::custom(...)`**：用来构造符合 serde 要求的错误对象。

挂到结构体字段上，还能配合 `default`（缺字段时用默认值）和 `alias`（兼容 camelCase 字段名）：

```rust
#[derive(Debug, Default, Clone, Deserialize, Serialize)]
pub struct DetailGroup {
    #[serde(default, deserialize_with = "de_i64_flexible")]
    pub prompt_token: i64,

    // 兼容服务端的 camelCase 字段名
    #[serde(default, alias = "webSearchCount", deserialize_with = "de_i64_flexible")]
    pub web_search_count: i64,

    // 结构不固定的元数据，直接存成 Value 透传
    #[serde(default)]
    pub map_data: serde_json::Value,
}
```

对应地还有一个 `de_f64_flexible`，套路完全一样，用于费用这类浮点字段。这是一种**防御性反序列化**：让 CLI 不会因为服务端把数字包成字符串就崩溃。

> 注意：如果字段是 `Option<i64>`，`deserialize_with` 指向的函数返回类型也要改成 `Option<i64>`，不能直接套用上面这个。

### 反过来：写一个自定义序列化器

反序列化解决的是「读进来」的兼容，有时我们还需要控制「写出去」的形态。比如某些下游系统要求**大整数必须以字符串形式传输**（避免 JavaScript `Number` 丢精度），这时就要写一个自定义序列化器，用 `#[serde(serialize_with = "...")]` 挂到字段上：

```rust
use serde::Serializer;

/// 序列化一个 i64 为带引号的字符串（避免下游 JS 丢失大整数精度）。
fn ser_i64_as_string<S>(v: &i64, serializer: S) -> Result<S::Ok, S::Error>
where
    S: Serializer,
{
    serializer.serialize_str(&v.to_string())
}
```

和反序列化器对照着看，签名的差异恰好体现了两个方向的本质区别：

| | 反序列化器 `de_i64_flexible` | 序列化器 `ser_i64_as_string` |
|---|---|---|
| 输入 | 一个 `Deserializer`（读取器） | `&i64` 值 + 一个 `Serializer`（写入器） |
| 输出 | `Result<i64, D::Error>` | `Result<S::Ok, S::Error>` |
| 做的事 | 从读取器**取**数据，还原成 `i64` | 把值**写**进写入器 |

几个关键点：

- **返回类型用 `S::Ok`**：前面讲过，它是 `Serializer` 的关联类型，具体是 `()` 还是 `Value` 由调用方决定，不能写死，`serialize_str` 本身就返回 `Result<S::Ok, S::Error>`，直接透传即可。
- **`serialize_str`** 把值以字符串形式写出；如果要保持数字形态则用 `serialize_i64`。

挂到字段上，`deserialize_with` 和 `serialize_with` 可以**同时存在**——入站宽容、出站规范，两个方向各管各的：

```rust
#[serde(
    default,
    deserialize_with = "de_i64_flexible",  // 读：数字/字符串/null 都能接
    serialize_with = "ser_i64_as_string"   // 写：统一输出成字符串
)]
pub prompt_token: i64,
```

这正好呼应了第一节的结论：**序列化和反序列化是方向相反的两个动作**，规则可以完全不对称，所以 serde 用两个独立的钩子（`serialize_with` / `deserialize_with`）分别控制。

## 六、四个转换入口：别把方向和载体搞混

serde_json 里有**两个维度**容易混：

1. **方向**：`from_*`（反序列化，读入） vs `to_*`（序列化，输出）。
2. **载体**：`*_str`/`*_string`（文本 JSON） vs `*_value`（内存里的 `Value` 对象）。

组合出四个常用函数：

| 函数 | 方向 | 载体 | 作用 |
|------|------|------|------|
| `from_str` | 反 | 文本 | JSON 字符串 → 结构体 |
| `from_value` | 反 | Value | `Value` → 结构体 |
| `to_string` / `to_string_pretty` | 正 | 文本 | 结构体 → JSON 字符串 |
| `to_value` | 正 | Value | 结构体 → `Value` |

所以严格互逆的是 `from_value` ↔ `to_value`、`from_str` ↔ `to_string`。而像 `from_value` 和 `to_string_pretty` 这种，**方向相反、载体也不同**，并不是一对逆操作。

一段真实代码里的对照（收到响应再打印）：

```rust
// 收到响应：文本 → Value → 结构体
let value = client::request_json_body(...).await?;   // 得到 Value
let report: DetailReport =
    serde_json::from_value(value).map_err(Into::into)?; // Value → 结构体

// 打印请求体：Value → 美化文本给人看
let text = serde_json::to_string_pretty(&body).unwrap_or_default();
```

这里 `.map_err(Into::into)` 值得一提：`from_value` 产出的错误是 `serde_json::Error`，但函数签名要求返回 `anyhow::Error`，两者类型不同不能直接返回。`Into::into` 依赖 `anyhow::Error` 实现了 `From<serde_json::Error>`，自动完成错误类型转换。它等价于 `|e| e.into()`，也和 `?` 运算符的自动转换是同一个机制。

而 `.unwrap_or_default()` 则是「尽力而为」：序列化失败时不 panic，退化成空字符串——适合日志/调试场景，但会静默吞掉错误，关键数据还是应该用 `?` 把错误抛上去。

## 七、小结

把这条链路上的概念收敛成一张表：

| 概念 | 一句话 |
|------|--------|
| 序列化 / 反序列化 | 方向相反：内存→外部 / 外部→内存，无法合成一个函数 |
| serde / serde_json | 引擎 / 插件：serde 定抽象，serde_json 实现 JSON |
| Serialize / Deserialize | 数据方 trait：「我能被转」 |
| Serializer / Deserializer | 格式方接口：「我负责转成/自某格式」，由 serde_json 实现 |
| S::Ok | 序列化器的关联类型，成功产出类型，由实现决定 |
| serde_json::Value | JSON 万能容器 enum，接住任意/动态 JSON |
| deserialize_with | 给字段挂自定义解析器，兼容不规范数据 |
| from_/to_ × str/value | 方向 × 载体两个维度，别搞混 |

一句话总结：**serde 是格式无关的序列化框架（定义 `Serialize`/`Deserialize` 和 `Serializer`/`Deserializer` 抽象），serde_json 是它的 JSON 具体实现（提供 `Value`、四个转换入口和 `json!` 宏）；序列化和反序列化是方向相反的两个动作，必须分别声明；遇到不规范的服务端数据，用 `deserialize_with` + `Value` 写一个宽容型解析器即可优雅兼容。**
