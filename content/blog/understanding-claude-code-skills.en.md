+++
title = "Understanding Claude Code Skills: From SOP to Open Standard"
date = 2026-09-29
description = "A Skill is a semantically triggered capability package. This post systematically walks through Claude Code Skills — trigger mechanics, task-type skills, progressive disclosure, composition with SubAgents, the five-layer capability model, and the road to an open standard."
[taxonomies]
tags = ["claude-code", "agent", "llm"]
[extra]
toc = true
+++

A Skill is a **semantically triggered capability package**: it bundles domain knowledge, execution steps, output conventions, and constraints, and **progressively loads** into the main agent's cognitive space when needed. The fundamental shift it brings: humans no longer orchestrate concrete execution paths — instead they **define the boundary and meaning of a capability**, and the model reads the `description` to understand the semantics, deciding at runtime whether and when to load it.

In one line: **a Skill is a semantically triggered, structured SOP (Standard Operating Procedure) injection mechanism.** Let's unpack it across six layers.

<!-- more -->

## 1. What a Skill is: structured doc vs Skill

First, a boundary that's easy to blur.

A **structured document** is a static file: it carries no trigger conditions, defines no execution flow, prescribes no output structure, and never validates quality automatically. A **Skill**, by contrast, is a standard operating procedure with a semantic entry point:

- Its `description` tells the model **under what circumstances** to load the capability;
- Its body defines execution steps, turning abstract principles into an executable flow;
- Templates constrain the output format for standardized results;
- `allowed-tools` limits which tools it can call, preventing privilege overreach;
- hooks run validation logic automatically once it finishes.

### Trigger mechanics and priority

A Skill triggers two ways: **explicit user invocation** (typing `/skill-name`) and **semantic auto-loading** (Claude reads the `description` and loads on a match). When several Skills match, Claude weighs each `description` against the request, picks the most relevant one, and — when unsure — asks or falls back to a generic flow.

Scope follows a priority order (**Enterprise > Personal > Project**), while Plugins are namespaced as `plugin-name:skill-name`:

| Level | Path | Scope |
| --- | --- | --- |
| Enterprise | Admin-configured | All users in the org |
| Personal | `~/.claude/skills/<name>/SKILL.md` | All your projects |
| Project | `.claude/skills/<name>/SKILL.md` | Current project (committed to git) |
| Plugin | `<plugin>/skills/<name>/SKILL.md` | Projects with the plugin enabled |

### A good description = what + how + when

`description` is the most important field — it directly decides when a Skill triggers. The formula:

```text
description = [what it does] + [how it does it] + [when to use it]
```

For a code-review Skill: `Review code for quality, security, and best practices. Checks for bugs, performance issues, and style violations. Use when the user asks for code review, wants feedback on their code, or mentions reviewing changes.`

## 2. Task-type Skills: deliberate, explicit action

Skills come in two kinds. **Reference-type** Skills are the org's behavioral norms ("what the right way to do things is here" — API standards, code style); Claude decides on its own whether to use them. **Task-type** Skills define a single deliberate action (deploy, release, commit) with boundaries and risk, usually requiring explicit invocation.

A task-type Skill is essentially a Skill with `disable-model-invocation: true`:

```markdown
---
name: deploy
description: Deploy the application to production
disable-model-invocation: true
---
```

### Dynamic injection: arguments and commands

Task-type Skills can inject content at runtime so Claude starts with context already in hand:

- **Argument injection**: `$ARGUMENTS` captures everything; `$1`/`$2` capture positional args.
- **Command injection**: `` !`command` `` runs first, and its output is substituted into the prompt.

```markdown
## Current Context (Auto-detected)

Current branch:
!`git branch --show-current`

Files changed:
!`git diff --stat origin/main 2>/dev/null || git diff --stat HEAD~3`
```

The engineering payoff is direct: Claude boots with key context pre-injected, so the tool calls in its first response drop from 3–5 down to 0–1 — cheaper tokens, faster response, more consistent results.

### The seven-step design checklist

To design a task-type Skill, answer each item:

```text
1. What's the action?          → name it (commit, deploy, review)
2. Who can trigger it?         → disable-model-invocation: true
3. What permissions?           → allowed-tools, scoped to the command level
4. What context at startup?    → !`command` pre-injection
5. What safety net at runtime? → hooks
6. Is the output large?        → if so, context: fork
7. Which model?                → model (haiku for simple, sonnet for complex)
```

The companion principles are **single responsibility** (`/commit`, `/push`, `/review` — not `/git-all-in-one`) and **least privilege** (`Bash(git commit:*)` — not `Bash(*)`).

## 3. Progressive disclosure: organizing lots of knowledge

As a Skill's knowledge grows, a **three-layer architecture** keeps complexity in check — this is progressive disclosure in practice:

```text
.claude/skills/financial-analyzing/
├── SKILL.md          # Layer 1 TOC: entry + routing (always loaded, < 500 lines)
├── reference/        # Layer 3 appendix: knowledge base (formulas, standards, benchmarks)
├── templates/        # Layer 3 appendix: output templates
├── scripts/          # Layer 3 appendix: executable scripts
└── data/             # Layer 3 appendix: static data
```

- **Layer 1, the TOC**: `description` plus a Quick Reference routing table, so Claude locates fast instead of scanning line by line;
- **Layer 2, the chapters**: the SKILL.md body, loaded only once activated;
- **Layer 3, the appendix**: read only when the body references it.

### Contract references, not weak references

The key is how you reference. Don't just write a path — write a **contract** that tells Claude exactly when to load and what it'll get:

```markdown
# ❌ Weak reference (Claude doesn't know when to load)
See `reference/revenue.md` for more details.

# ✅ Contract reference (trigger + path + expected content)
## Revenue Analysis
When the user asks about revenue growth, ARPU, or revenue composition:
→ Load `reference/revenue.md` for calculation formulas and industry benchmarks
```

Here scripts act as the **bridge from "knowledge" to "action"**: deterministic logic like formulas gets packaged into `calculate_ratios.py`, so Claude needn't understand every line — just run it and get an exact result. The splitting heuristic: inline the core semantics, outsource the deterministic logic, separate structure, defer data, and split off examples.

## 4. Skills vs SubAgents: who does the work

This is the whole framework's **meta-distinction**: **Skills answer "how to do it" (knowledge injection); SubAgents answer "who does it" (task delegation).** The test is plain — does this need "another person" to carry it, or just "one more manual" to guide it?

The two nest in both directions:

**① A SubAgent preloads a Skill** (SubAgent is the boss, Skill is the handbook): turn a generic agent into a domain expert.

```markdown
# .claude/agents/api-doc-generator.md
---
name: api-doc-generator
tools: [Read, Grep, Glob, Write, Bash]
skills:
  - api-generating          # ← preload the Skill as domain knowledge
---
You are an API documentation specialist.
```

**② A Skill dispatches a SubAgent via `context: fork`** (Skill is the boss, SubAgent is the executor): good for a self-contained task that needs no interaction with the main conversation and just returns a report.

```markdown
---
name: deep-research
description: Research a topic thoroughly in the codebase
context: fork           # ← run in an isolated subagent
agent: Explore          # ← subagent type
---
Research $ARGUMENTS thoroughly...
```

> Note: `context: fork` only fits **task-type Skills with explicit instructions.** Reference-type content (like API conventions) has no concrete task; a subagent handed the conventions won't know what to do and returns empty.

### Pipelines: chaining multiple stages

Going further, multiple "SubAgent + Skill" units form a pipeline where each stage's output is the next stage's input:

```text
Stage 1 route-scanner  →  Stage 2 doc-writer  →  Stage 3 quality-checker
skills: route-scanning    skills: doc-writing     skills: quality-checking
model: haiku              model: sonnet           model: haiku
```

Three keys: **define clear inter-stage interfaces** (each with an explicit output format), **each Skill focuses on one thing** (single responsibility, testable and reusable), and **keep orchestration logic in CLAUDE.md** (change the flow by editing orchestration, not the Skills/Agents).

## 5. Architectural placement: the five-layer capability model

Zoom out, and Claude Code's capabilities fall into five layers, with Skills sitting at the pivotal second layer:

| Layer | Name | Analogy | Responsibility |
| --- | --- | --- | --- |
| Base | LLM | CPU + Runtime | Reasoning and control loop |
| 1 | Tool Layer | syscall | Atomic callable capabilities |
| 2 | **Knowledge Layer (Skills)** | operating procedures | In what context, by what steps, calling which tools |
| 3 | Agent Layer (SubAgents/Teams) | execution orchestration | Task decomposition and separation of duties |
| 4 | Automation Layer (Hooks) | middleware | Inject validation and constraints at key points |
| 5 | Distribution Layer (Plugins) | packaging & distribution | Reuse across projects and orgs |

> If Skills stayed resident like CLAUDE.md, they'd degrade into part of CLAUDE.md and lose the "precise knowledge delivery" advantage. **Progressive disclosure isn't a token-saving trick — it's an architectural necessity at the knowledge layer.**

From this coordinate system, a Skill grows through three levels: **Level 1**, one Skill solves one standardized task (a single manual); **Level 2**, add a knowledge base, templates, scripts, and permissions to handle complex edge cases in a domain (a seasoned expert); **Level 3**, multiple Skills and SubAgents collaborate as a pipeline, backed by Hooks automation and Plugins distribution (a mature enterprise).

Correspondingly, there are **four design patterns**: template-driven (fixes unstable output), script-augmented (fixes unstable results), knowledge-layering (controls complexity), and tool-isolation (controls privilege risk). Pick via a decision tree:

```text
Does your Skill need to…
├─ Standardize output format?    → add templates/ (template-driven)
├─ Do deterministic calc/match?  → add scripts/ (script-augmented)
├─ Hold > 500 lines of knowledge?→ split into reference/ (knowledge-layering)
└─ Enforce a security boundary?  → configure allowed-tools (tool-isolation)
```

## 6. Why Skills broke out

Skills broke past Claude Code toward becoming an industry standard on three traits:

1. **Declarative** — the carrier is plain Markdown (YAML frontmatter + body), not imperative code.
2. **Self-contained** — a Skill is just a folder, with no registry or specific runtime dependency; `git clone` is the install.
3. **Knowledge-centric** — the value isn't in the format, the tools, or the runtime, but in the actionable domain wisdom of "how to do something."

By contrast, a SubAgent encapsulates **behavior** (create an isolated context, execute tool calls, return results), which is tightly bound to a specific runtime and thus can't travel. **The root cause: Skills encapsulate knowledge, whose value doesn't depend on who reads it; SubAgents encapsulate behavior, whose execution must depend on a specific runtime.**

On December 9, 2025, Anthropic, OpenAI, and Block jointly founded the **Agentic AI Foundation (AAIF)** under the Linux Foundation — seen as the "W3C of the AI era." It reflects three trends: **declarative beats imperative** (a knowledge package outlives code), **standardization beats platform lock-in** (compete at the tool layer, cooperate at the standard layer), and **knowledge reuse beats model competition** (once knowledge is standardized and extracted, model differences return to the essence of reasoning ability).

## Conclusion

From trigger mechanics to task-type Skills, from progressive disclosure to pipeline collaboration, to the five-layer model and the open standard — the through-line of this Skills framework is always one sentence:

> **Humans define the boundary and meaning of a capability; the model decides when and by what steps to call the tools.**

And the meta-distinction running through it all — **knowledge vs behavior** — explains both the respective roles of Skills and SubAgents, and ultimately why Skills could become a cross-platform open standard.
