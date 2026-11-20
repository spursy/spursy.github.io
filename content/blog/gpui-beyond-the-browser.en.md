+++
title = "Drawing UI Without a Browser: From Electron/Tauri's Trade-offs to GPUI and gpui-kit"
date = 2026-11-19
description = "Desktop developers want the web's productivity without Electron/Tauri's performance and architecture compromises. This post dissects how Electron and Tauri work and where they stall, walks the browser's Style→Layout→Paint→Composite pipeline step by step, then shows how GPUI replaces that whole pipeline with a single frame pipeline — covering what 60/120fps means, why the GPU is so fast, and how gpui-kit talks to it. Finally: GPUI's own dilemma, and how gpui-kit's layered design fills it in."
[taxonomies]
tags = ["rust", "gpui", "ui", "gpui-kit", "performance"]
[extra]
toc = true
+++

> "Are we GUI yet?" started as a Rust-community question. Desktop developers keep circling the same tension: **they want modern UI productivity, but don't want to pay for it with the performance and architecture compromises of web stacks like Electron and Tauri.** This post works that tension all the way through — from the browser's rendering pipeline, to GPUI's frame pipeline, to how gpui-kit finally makes this road walkable.

## 1. Electron and Tauri: how they work, and where they stall

Both solve the same problem: **let developers write cross-platform desktop UI in HTML/CSS/JS.** They just take different routes.

```
 Electron (2013)                      Tauri (2019)
 ┌─────────────────────┐             ┌─────────────────────┐
 │  Bundles all of      │             │  Uses the system     │
 │  Chromium: Blink+V8  │             │  WebView: WKWebView / │
 │  + Node.js backend   │             │  WebView2 + Rust      │
 └─────────────────────┘             └─────────────────────┘
   Heavy · consistent · owned          Tiny · light · inconsistent
```

Two often-confused terms first:

- **Chromium** is Google's open-source browser engine (Blink to render + V8 to run JS), **and it can be bundled whole into your app**.
- **WebView** is "a web-rendering control embedded in a native app." The **system WebView** is provided by the OS: WKWebView (WebKit) on macOS, WebView2 (Chromium) on Windows, WebKitGTK on Linux.

| | Electron (bundles Chromium) | Tauri (system WebView) |
|---|---|---|
| Engine source | Ships a full copy | Uses what the OS has |
| Size | Large (100MB+) | Small (a few MB) |
| Cross-platform consistency | High (same engine everywhere) | Low (different engine per OS) |
| Who controls the version | The developer | The OS |

**Why did Electron bundle Chromium back then?** In 2013 there was no decent system WebView (WebView2 didn't arrive until ~2020). To get a modern, consistent engine on all three platforms, the only reliable option was to carry Chromium along. **Why did Tauri pick the system WebView?** By 2019 every major OS had a modern WebView, so Tauri used it to trade for size, memory, and security — at the cost of cross-platform inconsistency.

Either way, they share one root premise: **the UI layer is a browser.** Every limitation flows from there — and to see it clearly, we first need to see how a browser turns HTML/CSS into pixels.

## 2. The browser rendering pipeline: Style → Layout → Paint → Composite

Every frame, the browser turns "the DOM + CSS description" into "pixels on screen" through a one-way pipeline:

```
  JS mutates DOM / styles
        │
   ① Style      Resolve each element's final look (color/size/border…)
        │
   ② Layout     Compute where/how big each element is (= reflow)
        │
   ③ Paint      Turn elements into draw commands / bitmaps (= repaint)
        │
   ④ Composite  Assemble the layers on the GPU into the final frame
        │
      Screen
```

What each step is for:

### ① Style (style recalculation)

With the DOM and all CSS rules in hand, the browser computes **each element's "final effective style"** (computed style). Because CSS has the cascade, inheritance, selector specificity, and `!important`, one property may be hit by many rules and must resolve to a single value.

- **Problem it solves**: collapse "a pile of scattered, possibly conflicting CSS rules" into "one definite style per element."
- **Cost**: selector matching isn't cheap when there are many elements and complex selectors.

### ② Layout / Reflow

From the styles, compute **each element's geometry — position (x/y) and size (width/height)**.

- **Problem it solves**: tons of CSS sizes are relative (`width:50%`, `flex:1`, `auto`, how tall text grows after wrapping) and must be resolved from "relative description" into "absolute coordinates" on the spot.
- **Why it's the most expensive**: it **cascades**. Changing one element's width can reflow its children, siblings, even the whole page. Reading `offsetHeight` or `getBoundingClientRect()` also forces a **synchronous reflow** — a classic jank source.

### ③ Paint / Repaint

Now that "where and how big" is known, decide **what color each pixel gets** — drawing text, backgrounds, borders, shadows, gradients into draw commands, usually across several layers.

- **Problem it solves**: fill the "geometric skeleton" into "visible content."
- **Cost**: cheaper than Layout, but shadows, blur, and complex gradients make it heavier. Changing only color/background — things that **don't affect geometry** — triggers Paint only and skips Layout.

### ④ Composite

Assemble the painted **layers on the GPU into the final frame**, applying `transform`, `opacity`, and z-ordering.

- **Problem it solves**: put volatile elements on their own layer so that when they move, the browser **only shifts that layer at composite time — no re-Layout, no re-Paint** — which is very cheap; the GPU does compositing, translation, and alpha blending blazingly fast.
- This is exactly why front-end animation prefers `transform/opacity` — the cheapest path.

Concretely, these three are: **`transform`** (move / scale / rotate an element, but **without changing the slot it occupies in layout** — neighbors don't reflow around it), **`opacity`** (overall transparency, 0 invisible to 1 opaque — fades rely on it), and **z-ordering** (`z-index` decides who sits on top when things overlap — it's how modals and dropdowns float above the content). What they share: they **only place and blend layers that are already painted — touching neither geometry (Layout) nor pixels (Paint)** — and "shift a layer, alpha-blend, stack in order" is precisely what the GPU is best at. Hence a front-end rule of thumb: **move with `transform` instead of changing `left/top`, and fade with `opacity` instead of toggling `display`** — the former runs composite-only and stays smooth, while the latter triggers a Layout reflow and tends to stutter.

Keep the "cost gradient" in mind:

| What you changed | Which steps run | Cost |
|---|---|---|
| Position/size/add-remove nodes | Style → **Layout** → Paint → Composite | 🔴 Most expensive |
| Color/background/shadow | Style → ~~Layout~~ → Paint → Composite | 🟡 Medium |
| `transform` / `opacity` | **Composite** only | 🟢 Cheapest |

**This is where Electron/Tauri's performance ceiling comes from.** This pipeline is a general mechanism designed to render *any* document, so every frame does "resolve styles + compute geometry + paint in layers." Once content gets complex (a 10k-row table, a live candlestick chart), Layout+Paint easily eat a frame's budget; add JS's single thread and GC pauses, and it stutters. There's a second cut at the architecture level: **the front end (JS) and the back end (Node/Rust) are two worlds separated by an IPC serialization boundary** — slow under heavy data, and it splits your code in half.

The root problem isn't "which framework is bad" — it's that **both use a browser as the UI layer.** So… what if we didn't use a browser?

## 3. What GPUI is, and how it works

**GPUI is a GPU-accelerated UI framework written in Rust by the Zed editor team.** Its fundamental difference is one sentence: **there's no browser inside** — no DOM, HTML, CSS, JS, no WebView. It draws the UI directly on the GPU.

Key traits:

1. **Draws straight to the GPU**: text, shapes, images, gradients are all drawn by GPUI itself via the GPU (Metal on macOS, Direct3D on Windows, a Vulkan-class API on Linux).
2. **One language top to bottom**: UI and business logic are both Rust — **no front/back language switch, no IPC serialization boundary** — the UI reads and writes the same in-memory data directly.
3. **Hybrid rendering model**: immediate-mode flavored — the element tree is rebuilt each frame from state — paired with an entity system for state, and taffy (a Flexbox engine) for layout.
4. **Rust underneath**: no GC, real concurrency.

### The frame pipeline: how one GPUI frame flows

> Phase names vary slightly by GPUI version (early `layout`, later split into `request_layout` / `prepaint` / `paint`); below is the stable idea.

```
  State changes → cx.notify() marks a View dirty (no change = no draw)
        │
   ① render()       The dirty View rebuilds a throwaway element tree
        │            (styles are hard-set in Rust method chains; no CSS cascade to resolve)
   ② request_layout Feed flexbox styles to taffy
        │
   ③ taffy solves    Compute every element's x/y/width/height in one pass
        │
   ④ prepaint       Geometry known: register hitboxes / clipping / deferred overlays
        │
   ⑤ paint          Each element emits GPU primitives into a flat Scene
        │            (quads / glyphs / images / paths)
   ⑥ GPU raster      The Scene is batched and submitted; the GPU rasterizes
        │            millions of pixels for this frame at once
      Screen (re-runs only when marked dirty again)
```

### Why the pipeline starts from "state changes"

The first time you see this diagram, it's easy to get stuck at the start: **why does it begin with "state changes" rather than with "draw"?** The answer lives in a core formula shared by every modern UI framework (React, SwiftUI, GPUI):

> **UI = f(state)** — what's on screen is a *function* of the current state. State is the input, the picture is the output.

The implication: **you never manipulate the picture directly, you only change the *state*, and the picture is just a reflection of it.** When a price changes, the old (imperative) way hunts down the text control and calls `label.setText("229")`; the declarative way only changes the data, `stock.price = 229`, and the framework recomputes the picture itself. So **a frame's only cause is "state changed"** — with no data change, this frame should be identical to the last, and needn't be redrawn at all. The whole pipeline is really answering one question: **the state changed; what does the new picture look like?**

So what *is* "state," and how does it change? State is the data your app holds, living in GPUI's entities. In a market-data example, a watchlist's state might be `stocks = [{AAPL, 228}, {TSLA, 410}]`, `selected = "AAPL"`. It never changes by itself — some external event always changes it, and there are just three sources:

| Source | Example | What state changes |
|---|---|---|
| User input | click a stock / press ↓ / type | `selected = "TSLA"` |
| External data | a quote pushed from the server | `stocks[0].price = 229` |
| Time / timer | an animation tick, a per-second refresh | some progress value +1 |

And "how it changes" is one ordinary line of Rust:

```rust
self.stocks[0].price = 229;  // ① change the data: state changes here
cx.notify();                 // ② mark dirty: tell the framework "this changed, redraw it"
```

Note the second line, `cx.notify()`, **doesn't draw — it sets a "dirty" flag.** Why not draw immediately? Because immediate drawing is wasteful — if three quotes arrive within one frame, "draw on every change" wastes two draws. GPUI's approach: **on a state change, only mark dirty, don't redraw now; at the next render moment, look at which Views are dirty and redraw only those, skipping the rest entirely (not even calling `render` — just reusing last frame).** That's the "no change = no draw" in the diagram, and the heart of GPUI's efficiency: **change drives rendering, instead of blindly redrawing everything every frame.**

Once that starting point clicks, the six steps are just the pipeline that turns "the dirty part" back into pixels. Keyword by keyword:

- **① render() → element tree**: the dirty View runs `render()`, producing an **element tree** that describes "what this part looks like" with nested elements. It's **built fresh each frame and thrown away** (an immediate-mode flavor), not retained-and-diffed like the DOM — because it's Rust, rebuilding is cheap. Styles are hard-set right in a **Rust method chain** like `.bg().padding()`; there's no CSS cascade to resolve.
- **② request_layout + ③ taffy**: feed each element's **flexbox** style (row vs column, how wide, spacing) to the layout library **taffy**, which solves every element's x/y/width/height in one pass. This is the browser's Layout, but a clean, dedicated solver.
- **④ prepaint**: with geometry known, do the three things that *require* knowing exact positions — register **hitboxes** (hit regions, so a click knows which element it landed on), **clipping** (confine content to a container's bounds; rounded corners and scrolling rely on it), and **deferred overlays** (dropdowns / modals registered to the top layer, laid over the content after it's drawn).
- **⑤ paint → primitives + Scene**: translate each element into GPU-ready **primitives** — **quads** (rounded rects with border/shadow), **glyphs**, images, paths — all flattened into a flat list called the **Scene**, essentially "the blueprint for this frame." Up to here it's all CPU, in Rust, with not a single pixel produced yet.
- **⑥ GPU rasterization**: hand the Scene to the GPU; same-type primitives are **batched** and submitted at once, and the GPU **rasterizes** this frame's millions of pixels in parallel across thousands of cores (see the next section). Then it's on screen, and the pipeline halts until state changes again and some View is marked dirty.

In one line: **user clicks / a quote arrives → change one piece of data (state changes) → `cx.notify()` marks it dirty → at render time, only the dirty Views rebuild their element trees → taffy positions them → prepaint registers interaction → paint emits primitives into the Scene → the GPU batches and rasterizes to pixels → on screen → halt until the next change.** The core is one sentence: **state is the cause, the picture is the effect.**

### How it *replaces* the DOM/CSS pipeline

Put the two pipelines side by side and the subtraction becomes clear:

| Browser pipeline | GPUI frame pipeline | Key difference |
|---|---|---|
| **Style** (cascade resolution) | **Essentially none** | Styles are hard-set in Rust method chains; no CSS selectors/cascade to resolve — **the whole step is cut** |
| **Layout / reflow** | `request_layout` + **taffy** | Also flexbox, but dedicated, no document baggage, **no forced-synchronous-reflow footguns** |
| (no direct analog) | **prepaint** | After geometry: register hitboxes / clipping / overlays |
| **Paint / repaint** | **paint** (emit primitives into Scene) | Produces GPU primitives directly, not layered bitmaps |
| **Composite** (persistent layer tree) | **Scene → GPU batched raster** | No persistent layer tree; each frame re-submits the flat primitive list, fast enough thanks to Rust+GPU |
| DOM (retained + mutated + React diff) | **Element tree rebuilt and thrown away each frame** | No DOM, no diff; rebuilding is cheap because it's Rust |
| JS runtime + GC | Rust, no GC | **No frame randomly interrupted by garbage collection** |

**The essential difference**: to render *any document*, the browser carries a heavy general pipeline; GPUI compresses it to "render rebuilds the element tree → taffy lays out → emit primitives → GPU rasterizes," **dropping the cascade and the persistent compositor layers**, and uses Rust (no GC) + GPU batching to hold the per-frame budget. So every Electron/Tauri trade-off is answered:

| Web-stack trade-off | GPUI's approach | Result |
|---|---|---|
| DOM/CSS render cost, dropped frames | GPU direct draw, no reflow/repaint machinery | Easy 60/120fps |
| JS single thread + GC jank | Rust, no GC, real concurrency | No GC pauses |
| Chromium size, slow startup | No browser engine at all | Small, fast start, low memory |
| Two worlds + IPC boundary | UI and logic both Rust, shared data | No serialization boundary |
| WebView cross-platform drift | Draws the UI itself | Pixel-identical on every OS |

## 4. 60/120fps, why the GPU is fast, and how gpui-kit calls it

### What 60/120fps means

**FPS = frames drawn per second.** A UI doesn't move continuously; it flips through still frames fast. The number tracks the screen's refresh rate:

- **60fps** → each frame has only ~**16.7ms** (1000÷60) to "lay out + paint this frame";
- **120fps** → each frame has only ~**8.3ms** (high-refresh / ProMotion displays).

What matters isn't "high average FPS" but that **no single frame overruns**: a frame that misses its budget drops (jank) — the stutter you feel while scrolling. So "supports 60/120fps" is a promise that **however complex the UI, each frame still fits in 8.3/16.7ms.**

### Why the GPU can be that fast

Drawing a screen is fundamentally "**turning vector shapes into individual pixels**," a step called **rasterization**: walk the pixels a shape covers and decide each one's color. A screen has millions of pixels, and "what color does this pixel get" is basically **the same independent computation** for each — exactly the GPU's home turf.

First, really see what rasterization does. A screen is a **grid of pixels** — millions of little squares that each show just one color; but your code describes shapes in **vector** language: "draw a blue rounded rect from (10,10) to (110,60)," "put a letter A here" — with no notion of "pixel" in it. Rasterization translates between the two, asking two questions **per pixel**: **① is this pixel covered by the shape?** (coverage — e.g. at a rounded corner, whether those pixels fall inside the arc) and **② if so, what color?** (a flat fill goes straight in; a gradient/alpha/texture gets a blended color). Edges also need **anti-aliasing**: a boundary rarely lands exactly on the grid lines, so edge pixels get a "half-blue, half-white" transition color, which is what makes diagonals and corners look smooth instead of a jagged staircase. The classic example is **drawing a letter**: in the font file, "A" is an outline of Bézier curves, and rasterization "fills" that outline into the pixel grid — pixels inside go dark, outside stay light, on the edge go gray. Every character on your screen is really a little patch of pixels produced this way.

Why is this a natural fit for the GPU? Because **each pixel's color is independent** — pixel A's color doesn't depend on pixel B's result. That kind of **massive, uniform, independent** computation is exactly what the GPU is good at:

| | CPU | GPU |
|---|---|---|
| Cores | a few to dozens of strong cores | hundreds to thousands of weak cores |
| Good at | complex logic, serial work | **one operation, applied to massive data at once** |
| Analogy | a few PhDs solving hard problems | thousands of kids doing arithmetic in parallel |

Move "fill millions of pixels" from a few CPU cores to thousands of GPU cores running in parallel, and it's both fast and able to hold a high frame rate. That's the whole meaning of "GPU acceleration."

### How gpui-kit calls the GPU

GPUI / gpui-kit never touch the hardware directly — there's a chain:

```
 gpui-kit's components
      │ build element tree → taffy layout → produce a Scene (flat primitive list)
      │ all of the above runs on the CPU, in Rust
      ▼
 Graphics API: Metal (macOS) / Direct3D (Windows) / Vulkan (Linux)
      │ via
      ▼
 GPU driver → GPU hardware → rasterize to pixels → screen
```

Concretely: the first five steps (`render` builds the tree, `taffy` lays out, producing the `Scene`) run mostly on the **CPU**; the GPU enters at step six — the thousands of **quads (rounded rects with border/shadow), glyphs, and images** in the Scene are **batched by type** and submitted through Metal/Direct3D/Vulkan, where the GPU's shaders rasterize the whole frame in parallel. Text and SVGs are **pre-rendered into a GPU texture atlas and cached**, so repeats aren't recomputed. That's the hardware backing for gpui-kit holding 120fps on your machine (the integrated GPU on a Mac's M-series chip, or a discrete GPU on a PC).

## 5. GPUI's dilemma: dropping the browser means rebuilding everything

No free lunch. **The price of abandoning the browser is that everything the browser gave you for free over decades must be rebuilt from scratch.** Before you can draw a page in GPUI, you first have to lay an infrastructure pyramid:

```
   ┌──────────────────────────────────┐
   │ Component library (where UI work starts) │  ← the most-missing layer
   ├──────────────────────────────────┤
   │ State/reactivity · animation · assets    │
   ├──────────────────────────────────┤
   │ Accessibility · clipboard · WebView · cross-platform │
   ├──────────────────────────────────┤
   │ Text input (IME · selection · undo · mapping) │
   ├──────────────────────────────────┤
   │ Overlays & layering (Popover · deferred)  │
   ├──────────────────────────────────┤
   │ Interaction (events · focus · keyboard · scroll · clip) │
   ├──────────────────────────────────┤
   │ Text (shaping · CJK breaking · fallback · raster) │
   ├──────────────────────────────────┤
   │ Window · GPU backend · primitives · layout · HiDPI │  ← without it, not a single pixel
   └──────────────────────────────────┘
```

Bottom to top, every tier is work the browser quietly did for you and self-drawing must lay itself:

- **① Window · GPU backend · primitives · layout · HiDPI** — the bedrock. You open the window yourself, wire up Metal/Direct3D/Vulkan, define **primitives** like quads and glyphs, bring in a layout engine (taffy), and handle **HiDPI** scaling — without this tier, not a single pixel appears.
- **② Text (shaping · CJK breaking · fallback · raster)** — "draw a line of text" is far harder than it sounds. **Shaping**: arrange characters into glyphs per the font's rules, handling ligatures and composition; **CJK line breaking**: Chinese has no spaces, so you must decide yourself where a line may wrap; **font fallback**: when the current font lacks a character (an emoji, a rare glyph), swap in another font automatically; finally **rasterize** the glyphs into pixels. This pile is the most underestimated pit in desktop development.
- **③ Interaction (events · focus · keyboard · scroll · clip)** — which element the mouse hit (hit-testing), how Tab moves focus between controls, keyboard-shortcut dispatch, scroll containers, and **clipping** content that overflows its bounds. Every "obviously it just works" behavior has to be built from scratch.
- **④ Overlays & layering (Popover · deferred)** — dropdowns, tooltips, and dialogs must float on the **topmost** layer and be able to spill past their parent's bounds. That takes **deferred rendering**: draw the content first, record the overlay, lay it on top last (exactly what prepaint registers in the frame pipeline).
- **⑤ Text input (IME · selection · undo · mapping)** — beyond IME, there's drag-**selection**, the `Ctrl+Z` **undo stack**, and **mapping** keystrokes to edit commands. A genuinely usable input box sits on a whole text-editing substrate.
- **⑥ Accessibility · clipboard · WebView · cross-platform** — accessibility (detailed below), reading/writing the system **clipboard**, embedding a **WebView** when needed (say, to render a rich-text email), and wiring every tier above across **macOS / Windows / Linux** while keeping pixels identical.
- **⑦ State/reactivity · animation · assets** — the **reactive** machinery behind `cx.notify()` dirty-marking, smooth **animation**, and loading/caching **assets** like images and fonts.
- **⑧ Component library** — the tip of the pyramid, and the **real starting point for drawing a page**. With all seven tiers below in place, you still only hold "a canvas that can draw pixels, take input, and react" — a whole layer of **ready-made components** short of "drop in a Button and go." And that layer is exactly the one that's missing.

The lower the layer, the harder the barrier. Two thorns hurt most, and each is worth unpacking.

#### Thorn 1: IME — typing Chinese

**An IME (Input Method Editor) is the system software that "translates a run of keystrokes into the final characters."** Latin scripts barely need it — press `a`, get `a`. But Chinese, Japanese, and Korean have huge character sets and can't be one-key-one-character, so they rely on it entirely. What you think of as one step ("type → character appears") actually hides a whole interaction:

```
   You press: n i h a o
        │
   ┌────▼──────────────────────────┐
   │ System IME (Pinyin / Sogou …)   │
   │  ① show "nihao" as an underlined │  ← pre-edit (composition string)
   │     pinyin run, floating at caret │
   │  ② pop a candidate window:        │  ← candidate window
   │     1.你好 2.拟好 …               │
   └────┬──────────────────────────┘
        │ press space / a digit to pick 你好
        ▼
   finally commit 你好 into the input
```

For a framework, several things here **must be handled by the framework itself** — all of which the browser's `<input>` already does for you: draw the "not-yet-committed pinyin" at the correct caret position and update it live as you type; feed the **caret's screen coordinates** to the OS so the candidate window pops right under the cursor; handle backspace / caret movement mid-composition; and commit into the text only once you finalize. Without this layer, the result is **you simply can't type Chinese, or the candidate window drifts off and the pinyin run doesn't show** — exactly why many early self-draw frameworks "worked in English but broke on Chinese." To serve CJK users, GPUI has to wire up macOS's `NSTextInputClient`, Windows' TSF, and Linux's IBus/Fcitx one by one — genuinely hard labor.

#### Thorn 2: Accessibility

**Accessibility is the whole set of capabilities that "let people who can't see, can't hear, or can't use a mouse operate your UI."** The crux: users with disabilities don't "see" your UI with their eyes — a **screen reader** (VoiceOver on macOS, NVDA on Windows) "reads" it aloud. And a screen reader can't read pixels; it needs a **parallel semantic description** — an **accessibility tree**:

```
   A blind user Tabs onto a button
        │
   The screen reader asks the OS: what's here?
        │
   ┌────▼───────────────────────┐
   │ Accessibility tree (semantic   │
   │  copy of the UI)               │
   │  Button "Buy" · clickable ·    │
   │  currently focused             │
   └────┬───────────────────────┘
        │ the reader speaks:
        ▼
   "Buy, button" → the user hears it, knows Space places the order
```

In the **browser**, you write `<button>Buy</button>` and it **automatically** builds this semantic tree and hands it to the OS; with **GPUI's self-drawing**, the screen has only the pixels it painted, and **the OS has no idea that glowing region is a "button."** So the framework must maintain a semantic node for each control by hand (this is a button, its text is "Buy", it's focusable), assemble the accessibility tree, and translate it — via **AccessKit** from the pyramid — into each platform's a11y protocol. Without it, **a screen reader sweeps across blank nothing and blind users are locked out**; and under laws like the US Section 508 and the EU EAA, accessibility is a **hard gate for entering enterprise / government markets**, not a nice-to-have.

What the two thorns share is plain:

| Capability | Browser (Electron/Tauri) | GPUI self-draw |
|---|---|---|
| **IME / CJK input** | Built into `<input>`, all-platform IME protocols wired | Wire up macOS / Windows / Linux system IMEs yourself |
| **Accessibility** | HTML tags auto-build the accessibility tree | Maintain a semantic node per control yourself (via AccessKit) |

Add the ecosystem gap: few people know GPUI, and there's no npm-scale library of ready components. The result — **GPUI is beautiful, but basically "only Zed can afford it."** Who lays this pyramid into a foundation ordinary teams can build on?

## 6. gpui-kit: the layer that fills the dilemma

**gpui-kit is the answer.** Driven by the real needs of Longbridge's desktop client, it tracks GPUI's `main` branch: **a real product finds problems → improvements are validated in real code → the general results are given back to open source** — a closed loop.

### How it's designed: behavior and style, in layers

gpui-kit's core philosophy is one line — **"Behavior belongs to the foundation; presentation belongs to the application"** — mirroring the web's Radix (headless) + shadcn (styled):

| Layer | Name | Role |
|---|---|---|
| Entry crate | **gpui-kit** | The single dependency; re-exports everything below and pins a matching GPUI version |
| Unstyled behavior core | **gpui-base** | Behavior/state/infrastructure, unstyled — for teams building **their own design system** |
| Styled components | **gpui-component** | Ready-to-use, themed components — for building applications |
| Script runtime | **gpui-shell** | A Rust-hosted JS runtime with capabilities granted one at a time — for plugins/scripted apps |

So a team can **reuse mature interaction logic and skin it their own way**, instead of rebuilding "how the cursor blinks, how CJK gets typed."

#### An aside: what "conventions" do Radix / shadcn / Ant stand for?

We keep invoking Radix and shadcn, so here's what each of these front-end libraries actually represents as a convention — gpui-kit's layering simply carries them into Rust:

- **Radix UI = the headless (unstyled) convention.** It gives you only a component's **behavior core**: open/close logic, keyboard operation, focus management, accessibility semantics — with **no visual styling at all**; how it looks is entirely yours. It also established the **compound-components** style: a Select isn't one black box but a set of freely composable parts — `Select.Root / Select.Trigger / Select.Content / Select.Item`. **gpui-base mirrors this layer.**
- **shadcn/ui = the "the code is yours" convention.** It's deliberately unusual — **not a dependency you `npm install`**, but components whose source you **copy into your own project** and own from then on. It layers Tailwind styling on top of Radix (behavior), so you get mature behavior for free *and* can edit the styling freely, untethered from an upstream version.
- **Ant Design = the "batteries-included" convention.** The other extreme: **finished components carrying a whole in-house design language** — `import` and go, consistent and feature-complete, at the cost of being **heavier and harder to deeply customize**. **gpui-component mirrors this end** — a themed, ready-to-run set handed straight to application builders.

They also share two general conventions that gpui-kit adopts too: **① variants** — one Button covers every style through two axes, `variant` (primary/ghost/danger) and `size` (sm/md/lg), instead of a dozen separate buttons; and **② theme tokens** — colors/spacing/radii distilled into a set of semantic tokens so reskinning or light/dark switching takes effect wholesale. In a line: **Radix owns behavior, shadcn owns code ownership, Ant owns out-of-the-box completeness**, and gpui-kit, with `gpui-base` (headless) + `gpui-component` (styled), claims both the "fully customizable" and the "ready to use" ends.

### What it actually ships

gpui-kit provides **80 components and primitives** across Basic / Form / Layout / Advanced / Primitives. Here they are, **drawn one by one** (ASCII sketches; the real styling renders with your theme; the label is the API name):

**Basic (31)**

```text
Accordion       ▾ Panel A   ▸ Panel B   ▸ Panel C
Alert           [ ⚠  Disk almost full                         ✕ ]
Attachment      📎 report.pdf · 1.2MB                            ✕
Avatar          ( JD )   ( 🙂 )   ( +3 )
Badge           Inbox ⁹⁹⁺         ● new
Bubble          ╭──────────────╮  chat bubble
Button          [ Primary ]   [ Ghost ]   « Danger »
Checkbox        [x] checked    [ ] empty    [–] indeterminate
Collapsible     ▸ click to expand   /   ▾ expanded content
DropdownButton  [ Save ▾ ]
Icon            ★   ♥   ⚙   ✉   🔍
Image           [🖼  image ]
Kbd             [⌘]+[K]
Label           Username:
Marker          📍 map marker   /   ● highlight dot
Message         💬 a message
MessageScroller ↑ more messages… (auto stick-to-bottom)
Pagination      ‹  1  [2]  3  4  …  9  ›
Progress        ▓▓▓▓▓▓▓░░░  70%
Questionnaire   Survey:  ◉ A    ○ B    ○ C
Radio           (◉) selected    ( ) empty
Rating          ★★★★☆  4.0
Skeleton        ▒▒▒▒▒   ▒▒▒▒▒▒▒▒    (loading skeleton)
Slider          ●────────○────────
Spinner         ◐ ◓ ◑ ◒   loading…
Stepper         (1)━━(2)━━(3)        current: step 2
Switch          (●   ) ON     (   ●) OFF
Tag             ‹ rust ✕ ›   ‹ ui ✕ ›
TextView        a read-only block of text……
Toggle          [ B ] [*I*] [ U ]    pressed = I
Tooltip         [?] ╭ tooltip text ╮
```

**Form (12)**

```text
Input           ┌ placeholder…             ┐
Textarea        ┌ multi-line input…        ┐
                └ ……                       ┘
Editor          1│ let x = 1;     ← syntax-highlighted code editor
Select          ┌ Choose…                ▾ ┐
Combobox        ┌ search & pick…       🔍 ▾ ┐
NumberInput     ┌ 42 ┐ [▲][▼]
DatePicker      ┌ 2026-11-20    📅 ┐
TimeField       ┌ 14:30    🕑 ┐
OtpInput        [1][2][3] – [4][5][6]
Speech          🎙 voice input…    ●recording
ColorPicker     ■ #12a594   ▾
Form            Label: ┌───────┐     ✓ validated
```

**Layout (13)**

```text
DescriptionList  Name: GPUI    Lang: Rust    License: Apache-2.0
GroupBox         ┌ Group title ─────┐  │  …content…  │  └───────────┘
Root             〈app root · mount point〉
Theme            ☀ / 🌙   semantic theme tokens (color·spacing·radius)
Dialog           ┌ Dialog ───────✕┐  │ content…  [Cancel][OK] │  └────────────┘
Notification     🔔 Order filled                                ✕
Popover          [button] ╭ popover content ╮
Resizable        Pane A  ┆⇆┆  Pane B        (draggable splitter)
Scrollable       content ▐▂▂▂▐ ▲▼         (with scrollbar)
Sheet            ▁▁▁▁▁ bottom sheet ▁▁▁▁▁
Sidebar          │ ▸ Home  │  main
                 │ ▸ Setup │  content
StatusBar        ▌ Ready · UTF-8 · Ln 12 · 120fps ▐
Toolbar          [↶][↷] │ [B][I][U] │ [🔍]
```

**Advanced (14)**

```text
Diff            + an added line   /   - a removed line   /     unchanged
Calendar        Mo Tu We Th Fr Sa Su
                       1  2  3  4  5
                 6  7 [8] 9 10 11 12
Carousel        ‹  ◻ ◻ ◼ ◻  ›       (carousel)
Command         ┌ 🔍 type a command…       ⌘K ┐
Chart           line ╱╲╱ · bars ▁▃▅▇ · pie ◕
List            • item one   • item two   • item three
Menu            ┌ New / Open / ──── / Quit ┐
Settings        Settings ▸ General   ▸ Appearance   ▸ Keymap
DataTable       ┌ Sym   │ Px  ┐
                │ AAPL  │ 229 │   (sortable / selectable / virtualized)
                │ TSLA  │ 412 │
                └───────┴─────┘
Dock            ▚ dockable / floating panel layout ▞
GridLayout      ┼─┼─┼
                ┼─┼─┼     grid layout
Tabs            [Overview]   Detail    Settings
Tree            ▾ src / ▾ ui / • ws.rs
VirtualList     ≡ 10k-row list (renders only visible items)
```

**Primitives & utilities (11)**

```text
AlertDialog     ┌ Delete this? ─────┐  │ [Cancel]  [Delete] │  └────────────────┘
Clipboard       📋 copy / paste
Empty           📭  no data yet
Focus Trap      ⟦ focus locked inside this region ⟧
HoverCard       @user  ▸  ╭ hover card ╮
Input Group     [ https:// ][ domain         ][ Search ]
Plot            ∴·∵  scatter / freeform plotting
Root View       〈window root view〉
Shimmer         ▒▓▒▓▒  shimmer loading animation
Table           ┼ basic table (no sort / virtualization) ┼
TitleBar        ● ● ●    Window title                 — ▢ ✕
```

Plus a full set of supporting capabilities — exactly the mid-upper layers of the pyramid:

- **Code-editing core**: `Rope` (text buffer) + `Highlighter` (Tree-sitter highlighting + LSP diagnostics/completion/hover), claimed performant at 200K lines;
- **Rich content**: native Markdown / HTML rendering + built-in charts;
- **Accessibility**: via AccessKit;
- **Theming**: semantic themes with multiple sizes;
- **Cross-platform**: macOS / Windows / Linux at 120fps, plus WASM support and optional JS extensions.

Map these back to the pyramid in section 5 — **layer 7 (the component library) is filled in**, and the loop closes.

## Closing

The throughline is clean:

- **Electron/Tauri** trade "fast to build, full ecosystem" for "a performance ceiling and a split architecture," rooted in using a browser as the UI layer;
- **GPUI** goes the other way — drops the browser, draws straight to the GPU, trading "rebuild all the infrastructure" for "native performance + one unified language + pixel-identical cross-platform";
- **gpui-kit** then amortizes the "rebuild everything" cost layer by layer, turning high-performance cross-platform desktop development from "only Zed can afford it" into "more people can just sit down and draw a page."

"Are we GUI yet?" — at least now, there's a serious third road worth walking.
