+++
title = "工具规模爆炸怎么办：Cloudflare Code Mode 与 Longbridge /omni 的两条路"
date = 2026-09-23
description = "当 MCP 工具数量涨到几百上千个，全量塞进上下文的做法会崩。对比 Cloudflare Code Mode 与 Longbridge /omni 两种收敛方案，从定位、易用性、优化效果到代码实现。"
[taxonomies]
tags = ["mcp", "llm", "agent"]
[extra]
toc = true
+++

标准 MCP 会把每个工具的完整定义(name + description + 完整 JSON Schema 的 inputSchema/outputSchema)全量注入模型上下文。工具只有几个时无所谓,可一旦接入几百上千个工具,光是工具清单就能吃掉几万 token,而且这笔开销与工具规模**线性增长**。

Cloudflare Code Mode 和 Longbridge 的 `/omni` 给出了两条不同的收敛路线。它们的共同点是:不再把工具直接暴露给模型,而是收敛成极少数「元工具」,让模型通过写代码/写管线的方式间接调用。区别则藏在执行模型和实现细节里。

<!-- more -->

## 一、定位:两者都在解决同一个问题

先对齐问题。标准 MCP 的痛点有两个:

1. **上下文开销随规模线性增长**——N 个工具 = N 份 schema,直接决定了 prompt 的基线体积。
2. **多步编排要靠多轮往返**——模型每调一个工具,就要「模型 → 工具 → 模型」走一个来回。5 步操作 = 5 轮推理,token 和延迟都翻倍。

Code Mode 和 `/omni` 都是奔着这两个痛点去的:用少量元工具替代全量工具清单,用「一次提交、批量执行」替代多轮往返。差别在于**用什么语言描述编排**,以及**在哪里执行**。

## 二、核心机制对比

| 维度 | Cloudflare Code Mode | Longbridge /omni |
|---|---|---|
| 编排语言 | JavaScript(异步箭头函数) | JSON 管线(声明式 steps) |
| 元工具数量 | 2 个(search + execute) | 3 个(search + docs + execute) |
| 执行环境 | V8 isolate 沙箱(Dynamic Worker Loader) | Rust 服务端并发执行 |
| 上下文开销 | 固定 ~1000 token,与规模无关 | -97%(162 工具场景) |
| 控制流 | V8 原生提供(if/for/while/Promise.all/try-catch) | 管线内 `$from` 引用 + 并发调度 |
| 结果裁剪 | 由代码自行处理(模型写 JS 过滤) | jq 表达式(`_jq` / `jq` 参数) |
| 沙箱隔离 | 有(V8 isolate) | 无(服务端可信执行) |

## 三、易用性

### Code Mode:让模型写它最擅长的东西

Code Mode 的核心洞察是:**LLM 写 JavaScript 比写自定义 DSL 更顺手**。训练语料里有海量 JS 代码,模型对 `for`、`await`、`Promise.all`、`try/catch` 这些结构烂熟于心。于是 Code Mode 干脆把工具封装成一个 TypeScript 命名空间,模型看到的是这样的类型声明:

```ts
declare const codemode: {
  /** 查询股票实时报价 */
  quote: (input: QuoteInput) => Promise<QuoteOutput>;
  /** 下单 */
  placeOrder: (input: PlaceOrderInput) => Promise<PlaceOrderOutput>;
};
```

模型只要写普通的 async 函数就能完成任意复杂的编排:

```js
async () => {
  const symbols = ["AAPL", "TSLA", "NVDA"];
  const quotes = await Promise.all(symbols.map(s => codemode.quote({ symbol: s })));
  return quotes.filter(q => q.changePercent > 0).map(q => q.symbol);
}
```

好处很直接:控制流、错误处理、并发、数据过滤,全部用模型最熟的语言表达,几乎没有学习成本。

### /omni:声明式 JSON 管线

`/omni` 让模型提交一段 JSON 描述的管线,每一步是一次工具调用,步骤之间通过 `$from` 引用上一步的结果:

```json
{
  "steps": [
    { "id": "q", "tool": "quote", "input": { "symbol": "AAPL" } },
    { "id": "d", "tool": "depth", "input": { "symbol": "AAPL" },
      "jq": ".bids[:5]" }
  ]
}
```

声明式的好处是**结构可校验、执行可控**——服务端能明确知道要调哪些工具、如何并发、如何限流。代价是表达能力受限于管线 schema:复杂的条件分支和循环不如写代码来得自然。

**小结**:纯编排灵活性上 Code Mode 更强(图灵完备的 JS);但 `/omni` 的声明式结构更容易做安全校验和资源约束。

## 四、优化效果

### Code Mode:固定 ~1000 token,与规模无关

这是 Code Mode 最漂亮的一点。它把 N 个工具收敛成 `search` + `execute` 两个元工具:

- `search`:模型根据需求检索相关工具的类型定义(按需拉取,不全量注入)。
- `execute`:提交一段 JS 代码去沙箱执行。

模型上下文里常驻的只有这两个元工具的定义,大约 1000 token。**无论后端接了 100 个还是 2500 个工具,这个基线都不变**——因为工具定义不再进上下文,而是通过 `search` 按需检索。Cloudflare 给出的数字是:2500 个 endpoint 场景下,上下文开销降低 99.9%。

「与规模无关」的本质是:把「工具清单」从**编译期常驻**改成了**运行期按需检索**。

### /omni:-97%,靠 jq 做结果裁剪

`/omni` 在两个环节省 token:

1. **工具发现**:同样用 search 元工具替代全量清单,162 个工具场景下上下文降低约 97%。
2. **结果裁剪**:这是 `/omni` 的特色。工具返回的原始 JSON 往往很大(一个报价接口可能返回几十个字段),`/omni` 允许在调用时附带 jq 表达式,**在服务端就把结果裁剪成模型真正需要的形状**,再返回给模型。

jq 的裁剪能力值得单独说。它提供了:

- `_jq`(单工具调用)/ `jq`(管线步骤)两种参数入口;
- `table` 函数:把对象数组转成列式结构,大幅压缩重复的 key;
- `num` 函数:无损数字转换,保留股票代码、订单号、时间戳这类不能被科学计数法破坏的字段;
- 5 秒执行超时 + 结构化错误(`error_code` + `recoverable`)。

关键在于:**jq 规则是让 LLM 自己写的**。模型知道自己接下来的推理需要哪些字段,就顺手写一个 jq 表达式把无关字段裁掉。这让「结果裁剪」这件事从固定的服务端逻辑变成了模型自适应的行为。

**小结**:Code Mode 的优化重心在**工具定义的注入**(近乎完全消除);`/omni` 除了工具发现,还额外优化了**工具结果的回传**(jq 裁剪)。两者优化的是链路上不同的位置,思路可以互补。

## 五、代码实现

### Code Mode:V8 isolate 是地基

Code Mode 的实现难点全在「安全地执行模型生成的任意 JS」。它依托 Cloudflare 的 Dynamic Worker Loader,把代码丢进一个 V8 isolate 沙箱:

- **控制流不用自己实现**——`if/for/while/Promise.all/try-catch` 全是 V8 引擎原生提供的 JS 语言特性。Code Mode 不需要造一套调度器,直接复用 JS runtime。这是「让模型写 JS」这个选择带来的巨大红利:编排语义 = JS 语义,零额外实现。
- **工具调用通过 RPC 代理**——沙箱里的 `codemode.quote(...)` 实际是一个 proxy,把调用序列化后 RPC 到宿主环境执行真正的 MCP 工具,再把结果序列化回沙箱。
- **隔离保证安全**——模型生成的代码跑在受限 isolate 里,拿不到宿主的文件系统、网络等能力,只能通过被注入的 proxy 访问白名单工具。

代价是需要一套完整的沙箱基础设施(isolate 生命周期、序列化 codec、RPC 代理、abort-and-replay 等)。

### /omni:Rust 服务端调度器

`/omni` 不跑沙箱,因为它执行的不是任意代码,而是**受限的声明式管线**——安全边界天然由 schema 限定。实现重心在:

- **并发调度**:解析 JSON 管线,分析 `$from` 依赖关系,把无依赖的步骤并发执行(Rust async)。
- **jq 引擎**:内嵌 jq 求值器,对每步结果做裁剪,带超时和错误处理。
- **结果编排**:管理步骤间的数据流转,把最终结果回传模型。

因为没有沙箱,`/omni` 的实现更轻;但也因此它只能执行预定义的工具调用管线,无法执行任意逻辑。

## 六、总结:两种收敛哲学

| | Code Mode | /omni |
|---|---|---|
| 一句话 | 让模型**写代码**,在沙箱里执行 | 让模型**写管线**,在服务端执行 |
| 灵活性 | 图灵完备,任意逻辑 | 受限于管线 schema |
| 安全 | 靠 V8 沙箱隔离 | 靠 schema 限定边界 |
| 优化重心 | 工具定义注入(~1000 固定 token) | 工具发现 + 结果裁剪(jq) |
| 实现成本 | 高(需完整沙箱设施) | 中(调度器 + jq 引擎) |

两者都抓住了同一个本质:**工具不该全量进上下文,编排不该走多轮往返**。

- 如果你需要**极致的编排灵活性**、且愿意投入沙箱基础设施,Code Mode 的「让模型写 JS」是天花板方案——控制流零成本复用 JS runtime,上下文开销与工具规模彻底解耦。
- 如果你更看重**执行可控、实现轻量**,且工具调用模式相对规整,`/omni` 的声明式管线 + jq 裁剪是性价比很高的方案,尤其 jq 对**结果回传**的优化是 Code Mode 没有专门处理的一环。

最理想的形态,或许是两者取长:用 Code Mode 的 JS 编排能力承载复杂控制流,同时借鉴 `/omni` 的 jq 思路,让模型在拿回结果时就顺手裁剪——把「工具定义注入」和「工具结果回传」两头的开销一起摁下去。
