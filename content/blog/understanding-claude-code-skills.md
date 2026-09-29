+++
title = "读懂 Claude Code Skills：从 SOP 到行业标准"
date = 2026-09-29
description = "Skill 是一种可被语义触发的能力包。本文从触发机制、任务型 Skill、渐进式披露、与 SubAgent 的协同，到五层能力模型与行业标准，系统梳理 Claude Code Skills 的设计哲学。"
[taxonomies]
tags = ["claude-code", "agent", "llm"]
[extra]
toc = true
+++

Skill 是一种**可被语义触发的能力包**：它包含领域知识、执行步骤、输出规范与约束条件，并在需要时**渐进式加载**到主 Agent 的认知空间中。它带来的根本转变是——人不再编排具体的执行路径，而是**定义能力的边界与含义**，由模型根据 `description` 理解语义，在运行时决定是否加载、何时加载。

一句话概括：**Skill = 可被语义触发的结构化 SOP（标准操作程序）注入机制**。下面从六个层面逐层展开。

<!-- more -->

## 一、Skill 是什么：结构化文档 vs Skill

先厘清一个容易混淆的边界。

**结构化文档**是静态文件：它不包含触发条件，不定义执行流程，不规定输出结构，也不会自动校验质量。而 **Skill** 是一段具备语义入口的标准操作程序：

- 通过 `description` 告诉模型**在什么情况下**应该加载这项能力；
- 在正文中定义执行步骤，把抽象原则转化为可执行流程；
- 通过模板约束输出格式，确保结果标准化；
- 通过 `allowed-tools` 限制可调用工具的范围，防止越权；
- 通过 hooks 在完成后自动执行验证逻辑。

### 触发机制与优先级

Skill 有两种触发方式：**用户显式触发**（输入 `/skill-name`）与**语义匹配自动加载**（Claude 读取 `description`，匹配后加载）。当多个 Skill 都匹配时，Claude 会评估各自 `description` 与请求的相关性，选最相关的那个；不确定时会询问或走通用流程。

作用范围上存在优先级（**Enterprise > Personal > Project**），Plugin 则以 `plugin-name:skill-name` 命名空间隔离：

| 级别 | 路径 | 适用范围 |
| --- | --- | --- |
| Enterprise | 管理员配置 | 组织内所有用户 |
| Personal | `~/.claude/skills/<name>/SKILL.md` | 个人所有项目 |
| Project | `.claude/skills/<name>/SKILL.md` | 当前项目（提交到 git） |
| Plugin | `<plugin>/skills/<name>/SKILL.md` | 启用该插件的项目 |

### 好的 description = 做什么 + 怎么做 + 什么时候用

`description` 是最重要的字段，它直接决定 Skill 何时被触发。公式是：

```text
description = [做什么] + [怎么做] + [什么时候用]
```

例如代码审查 Skill：`Review code for quality, security, and best practices. Checks for bugs, performance issues, and style violations. Use when the user asks for code review, wants feedback on their code, or mentions reviewing changes.`

## 二、任务型 Skill：令行禁止

Skill 分两类。**参考型**是组织的行为规范层（"在这个世界里什么是正确的做法"，如 API 设计标准、代码风格），Claude 自动选择是否使用；**任务型**定义一次明确的行动（部署、发布、提交），具有边界和风险，通常需要显式触发。

任务型 Skill 的本质，就是加了 `disable-model-invocation: true` 的 Skill：

```markdown
---
name: deploy
description: Deploy the application to production
disable-model-invocation: true
---
```

### 动态注入：参数与命令

任务型 Skill 支持在运行时注入内容，让 Claude 启动即带上下文：

- **参数注入**：`$ARGUMENTS` 接收全部参数，`$1`/`$2` 接收位置参数。
- **命令注入**：`` !`command` `` 会先执行命令，把结果替换进 prompt。

```markdown
## Current Context (Auto-detected)

Current branch:
!`git branch --show-current`

Files changed:
!`git diff --stat origin/main 2>/dev/null || git diff --stat HEAD~3`
```

这样做的工程优势很直接：Claude 启动时上下文已注入关键信息，首次响应的工具调用数从 3–5 次降到 0–1 次，token 更省、响应更快、结果更一致。

### 七步设计清单

设计一个任务型 Skill，可按这份清单逐项回答：

```text
1. 动作是什么？        → 命名（commit、deploy、review）
2. 谁能触发？          → disable-model-invocation: true
3. 需要什么权限？      → allowed-tools 精确到命令级
4. 启动时需要什么上下文？→ !`command` 预注入
5. 执行需要什么安全网？ → hooks
6. 输出量大不大？      → 大则 context: fork
7. 用什么模型？        → model（简单 haiku，复杂 sonnet）
```

配套原则是**单一职责**（`/commit`、`/push`、`/review` 而非 `/git-all-in-one`）与**权限最小化**（`Bash(git commit:*)` 而非 `Bash(*)`）。

## 三、渐进式披露：知识多了怎么组织

当一个 Skill 的知识量增长，就需要**三层架构**来控制复杂度——这正是渐进式披露的落地形态：

```text
.claude/skills/financial-analyzing/
├── SKILL.md          # 层1 目录页：入口 + 路由（总是加载，< 500 行）
├── reference/        # 层3 附录：知识库（公式、规范、基准，按需加载）
├── templates/        # 层3 附录：输出模板
├── scripts/          # 层3 附录：可执行脚本
└── data/             # 层3 附录：静态数据
```

- **层1 目录页**：`description` + 一张 Quick Reference 路由表，让 Claude 快速定位而非逐行扫描；
- **层2 章节**：SKILL.md 正文，激活后才加载；
- **层3 附录**：只有被正文引用时才读取。

### 契约式引用，而非弱引用

关键在于引用方式。别只写一个路径，要写一个**契约**——让 Claude 明确知道何时加载、加载后能得到什么：

```markdown
# ❌ 弱引用（Claude 不知道何时该加载）
See `reference/revenue.md` for more details.

# ✅ 契约式引用（触发条件 + 路径 + 内容预期）
## Revenue Analysis
When the user asks about revenue growth, ARPU, or revenue composition:
→ Load `reference/revenue.md` for calculation formulas and industry benchmarks
```

其中脚本扮演着**"知识 → 行动"的桥梁**：把公式这类确定性逻辑封装成 `calculate_ratios.py`，Claude 不必理解每一行代码，只需执行并拿到精确结果。拆分心法可归纳为：核心语义内联、确定逻辑外包、结构独立、数据延迟、示例分离。

## 四、Skills 与 SubAgent：谁来做

这是全书的**元区分**：**Skills 解决"怎么做"（知识注入），SubAgent 解决"谁来做"（任务委托）**。判断标准很朴素——这件事需要"另一个人"来承担，还是只需要"多一本手册"来指导。

二者可以双向嵌套：

**① SubAgent 预加载 Skill**（SubAgent 是老板，Skill 是工具书）：把通用 Agent 打造成领域专家。

```markdown
# .claude/agents/api-doc-generator.md
---
name: api-doc-generator
tools: [Read, Grep, Glob, Write, Bash]
skills:
  - api-generating          # ← 预加载 Skill 作为领域知识
---
You are an API documentation specialist.
```

**② Skill 通过 `context: fork` 派 SubAgent**（Skill 是老板，SubAgent 是执行者）：适合独立完整、不需与主对话交互的任务，做完把报告送回。

```markdown
---
name: deep-research
description: Research a topic thoroughly in the codebase
context: fork           # ← 在隔离子代理中执行
agent: Explore          # ← 子代理类型
---
Research $ARGUMENTS thoroughly...
```

> 注意：`context: fork` 只适合**有明确指令的任务型 Skill**。参考型内容（如 API 规范）没有具体任务，子代理收到规范却不知该做什么，会直接返回空结果。

### 流水线：多阶段串联

进一步地，多个"SubAgent + Skill"可以组成流水线，每个阶段的输出是下一阶段的输入：

```text
阶段1 route-scanner  →  阶段2 doc-writer  →  阶段3 quality-checker
skills: route-scanning  skills: doc-writing   skills: quality-checking
model: haiku            model: sonnet         model: haiku
```

要点有三：**定义清晰的阶段间接口**（各自明确输出格式）、**每个 Skill 只关注一件事**（单一职责，可测可复用）、**编排逻辑集中在 CLAUDE.md**（调整流程只改编排，不动 Skill/Agent）。

## 五、架构定位：五层能力模型

把视角拉高，Claude Code 的能力可分为五层，Skills 恰处于承上启下的第二层：

| 层 | 名称 | 类比 | 承担 |
| --- | --- | --- | --- |
| 底座 | LLM | CPU + Runtime | 推理与控制循环 |
| 1 | Tool Layer | syscall | 系统可调用的原子能力 |
| 2 | **Knowledge Layer（Skills）** | 操作规约 | 在什么上下文、按什么步骤、调哪些工具 |
| 3 | Agent Layer（SubAgents/Teams） | 执行编排 | 任务拆解与职责分离 |
| 4 | Automation Layer（Hooks） | middleware | 关键节点注入校验与约束 |
| 5 | Distribution Layer（Plugins） | 打包分发 | 跨项目、跨组织复用 |

> 如果 Skills 像 CLAUDE.md 一样常驻，就退化成 CLAUDE.md 的一部分，失去"精准投放知识"的优势。**渐进式披露不是省 token 的技巧，而是知识层架构的必然要求。**

从这个坐标系看，Skill 的成长有三级进化：**第一级**一个 Skill 解决一个标准化任务（一份操作手册）；**第二级**引入知识库、模板、脚本与权限，处理领域内的复杂边界（一位资深专家）；**第三级**多个 Skill 与 SubAgent 协作成流水线，配合 Hooks 自动化与 Plugins 分发（一家成熟企业）。

对应地有**四种设计模式**：模板驱动（解决输出不稳定）、脚本增强（解决结果不稳定）、知识分层（控制复杂度）、工具隔离（控制越权风险）。选择靠一棵决策树：

```text
你的 Skill 需要……
├─ 标准化输出格式？   → 加 templates/（模板驱动）
├─ 确定性计算/匹配？  → 加 scripts/（脚本增强）
├─ 知识量 > 500 行？  → 拆分 reference/（知识分层）
└─ 安全边界控制？     → 配置 allowed-tools（工具隔离）
```

## 六、为什么 Skills 能出圈

Skills 之所以能突破 Claude Code 走向行业标准，靠三个特性：

1. **声明式**——载体是纯 Markdown（YAML frontmatter + 正文），不是命令式代码。
2. **自包含**——一个 Skill 就是一个文件夹，不依赖注册中心或特定 runtime，`git clone` 即安装。
3. **知识本位**——价值不在格式、工具或运行时，而在"怎么做某件事"的可操作领域智慧。

对比之下，SubAgent 封装的是**行为**（创建隔离上下文、执行工具调用、返回结果），强依赖特定运行时，因此无法出圈。**根因是：Skills 封装知识，知识的价值不依赖谁来读；SubAgent 封装行为，行为必须依赖特定运行时。**

2025 年 12 月 9 日，Anthropic、OpenAI、Block 在 Linux Foundation 下联合成立了 **Agentic AI Foundation（AAIF）**，被视作"AI 时代的 W3C"。它折射出三个趋势：**声明式大于命令式**（知识包比代码更具生命力）、**标准化大于平台锁定**（工具层竞争、标准层合作）、**知识复用大于模型竞争**（当知识被标准化抽取，模型差异回归推理能力本质）。

## 小结

从触发机制到任务型，从渐进式披露到流水线协同，再到五层模型与行业标准——这套 Skills 体系的主线始终是同一句话：

> **人定义能力的边界与语义，模型决定何时以何种步骤调用工具。**

而贯穿全程的那个"元区分"——**知识 vs 行为**——既解释了 Skill 与 SubAgent 各自的定位，也最终解释了为什么 Skills 能成为跨平台的开放标准。
