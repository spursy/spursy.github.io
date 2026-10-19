+++
title = "Three Kinds of Moat: The Valuation Logic of Speedtest, Hugging Face, and Arena"
date = 2026-10-19
description = "One buy-side research framework applied to three companies — Ookla (Speedtest/Downdetector), Hugging Face, and Arena (LMArena): what they do, how they make money, who acquired them, and why their valuations hold up."
[taxonomies]
tags = ["Equity Research", "Buy-side Research"]
[extra]
toc = true
+++

Running the same framework across three companies in a row is exactly what reveals how different their moats are. This post uses a four-part structure — *what they do → how they make money → acquisition status → why the valuation holds up* — to break down Ookla (Speedtest / Downdetector), Hugging Face, and Arena (LMArena) one by one.

> A few financial abbreviations used throughout, with full names:
> **ARR (Annual Recurring Revenue)**, **PS (Price-to-Sales ratio) = valuation ÷ annual revenue**, **PE (Price-to-Earnings ratio) = valuation ÷ profit**, **TAM (Total Addressable Market)**, **VC (Venture Capital)**.

<!-- more -->

## 1. Ookla — Speedtest and Downdetector

First, a common misconception: **Speedtest and Downdetector are not two separate companies — they are two products under the same parent, Ookla.**

### What it does

- **Speedtest**: a free consumer network speed-test tool. Its real value is that hundreds of millions of users run tests voluntarily, continuously crowdsourcing real network-performance data worldwide — the platform processes about **250 million tests per month**.
- **Downdetector**: a crowdsourced service-outage detector. Users report that a service is down; the system sets a dynamic threshold on report volume and flags an outage on an abnormal spike, also pulling in social-media complaints as a supplementary signal. It covers **25,000+ services across 64 countries**.

### Business model

Essentially "free consumer product collects data + data sold to enterprises":

- **Speedtest**: the core is the enterprise data product **Speedtest Intelligence**, selling test data to telecom carriers (AT&T, Verizon), cloud providers, and regulators for benchmarking; plus licensed custom software and consumer ads.
- **Downdetector**: alerts, dashboards, and the API are all Enterprise-paid, priced client by client.
- **Scale**: in 2025 the Connectivity division generated about **$231M** in revenue with roughly **$76.1M** net income (30%+ net margin).

### Acquisition status

Acquired twice: by **Ziff Davis in 2014**; and by **Accenture in 2026** for **$1.2B**, buying all of Ookla (Speedtest, Downdetector, Ekahau, RootMetrics) from Ziff Davis — signed in March, closed in June.

### Why the valuation holds up

$1.2B against ~$231M revenue is about **5x PS (Price-to-Sales)**. The logic:

1. **A one-of-a-kind data moat** — over a decade of real network data from hundreds of millions of users, hard to replicate quickly with money.
2. **A "free funnel + sell data" flywheel**, with near-zero marginal cost of acquisition.
3. **High margin, strong cash flow** (30%+ net margin) — an asset-light data business, not a cash-burning story.
4. **Sticky, must-have customers** who treat it as an industry benchmark source.

> **Sources:**
> - [Accenture to acquire Ziff Davis Connectivity division including Ookla, Speedtest for $1.2 billion — Storyboard18](https://www.storyboard18.com/brand-marketing/accenture-to-acquire-ziff-davis-connectivity-division-including-ookla-speedtest-for-1-2-billion-91213.htm)
> - [Accenture buys Speedtest and Downdetector in $1.2B deal — The Next Web](https://thenextweb.com/news/accenture-buys-speedtest-and-downdetector-in-1-2b-deal)
> - [Accenture completed the acquisition of Ookla from Ziff Davis — MarketScreener](https://www.marketscreener.com/news/accenture-inc-completed-the-acquisition-of-ookla-llc-from-ziff-davis-inc-nasdaqgs-zd--ce7f5cddd880f322)
> - [Speedtest.net — Wikipedia](https://en.wikipedia.org/wiki/Speedtest.net)
> - [What Is Downdetector? How Crowdsourced Outage Detection Works](https://isthatdown.com/resources/what-is-downdetector)

## 2. Hugging Face

### What it does

The world's largest open-source AI model and dataset platform/community, often called "the GitHub of AI." The core is the **Hub**: hosting **3M+ open-source models** and vast datasets, used by **18M+ developers**; it also maintains de-facto-standard open-source libraries like `transformers` and `datasets`. Since 2025 it entered robotics hardware via the acquisition of Pollen Robotics.

### Business model

A classic "open core + free funnel + enterprise monetization," similar to GitHub. **ARR (Annual Recurring Revenue) is about $150M**, across three lines:

1. **Enterprise Hub (~45% of revenue)**: starting at **$50/user/month**, offering private repos, SSO, audit logs, compliance, BYO cloud.
2. **Inference Endpoints**: managed inference, priced by compute.
3. **Pro subscriptions**: a premium tier for individual developers.

In one line: free for developers, selling "speed, privacy, compliance, compute" to enterprises.

### Acquisition status

**Both directions**: as acquirer it bought the French robotics firm **Pollen Robotics**; as the acquired — on **Sept 2, 2026** it signed a definitive agreement with **NVIDIA**, announced **Sept 3**, for about **$12.93B** (~$11.9B to shareholders + up to ~$1.0B employee retention equity), expected to close in **H1 2027**, pending regulatory approval. NVIDIA pledged the platform stays open and does not require its compute.

### Why the valuation holds up

At ~$150M ARR, that's about **86x PS (Price-to-Sales)** — far above Ookla. The premium comes from strategic positioning, not current profit:

1. **Developer network effects = the deepest moat**, the highest concentration of AI practitioners on earth.
2. **The chokepoint position as "neutral infrastructure" for the AI ecosystem** — precisely why NVIDIA will pay 86x PS.
3. **Synergy for NVIDIA**: owning the model-distribution entrance matters far more strategically than the $150M of revenue.
4. **Rapid growth**: ARR from $35M at end-2023 to ~$150M in 2026, near-doubling.
5. **New TAM (Total Addressable Market) upside**: robotics and more extend the story's boundary.

> **Sources:**
> - [NVIDIA to Acquire Hugging Face — NVIDIA Blog](https://blogs.nvidia.com/blog/nvidia-to-acquire-hugging-face/)
> - [Nvidia confirms it will buy Hugging Face for $12.9 billion — TechCrunch](https://techcrunch.com/2026/09/03/nvidia-confirms-it-will-buy-hugging-face-for-12-9-billion/)
> - [NVIDIA Form 8-K (FY2026) — SEC](https://www.sec.gov/Archives/edgar/data/0001045810/000104581026000078/nvda-20260902.htm)
> - [How Hugging Face Makes Money: ~$150M Revenue, Nvidia Deal — ValueAdd VC](https://valueaddvc.com/blog/how-does-hugging-face-make-money-hub-hosting-enterprise-and-the-4-5b-valuation-breakdown)
> - [Hugging Face revenue, valuation & funding — Sacra](https://sacra.com/c/hugging-face/)

## 3. Arena — LMArena

An **AI model evaluation company** that grew out of, and rebranded from, the open-source project Chatbot Arena / LMArena.

### What it does

A "third-party judge" that scores and ranks large models. Originating from UC Berkeley's Chatbot Arena: users enter the same prompt, two anonymous models answer side by side, users vote for the better one, and the mass of votes aggregates into a public leaderboard covering text, code, image, video, and more. Ranked by real human blind-test preference, it is treated across the industry as a neutral, trustworthy yardstick of model capability.

### Business model

Again "free community funnel + enterprise monetization," but the thing sold is evaluation services:

- **AI Evaluations**: commercialized in Sept 2026 — enterprises and model labs pay Arena to run evaluations through its community, with clients including OpenAI, Google, and xAI.
- **Blistering growth**: **ARR (Annual Recurring Revenue)** went from ~$30M in Jan 2026 to ~**$100M** by June.

### Acquisition status

Currently **not acquired** — an independent, fast-growing startup; and no outbound acquisitions yet. The funding trajectory is steep:

- May 2026: seed round **$100M**, valuation **$600M**
- Jan 2026: Series A **$150M**, valuation **$1.7B**
- Oct 8, 2026: Series B **$200M**, valuation **$3.1B** (led by Lightspeed and Khosla)

Total raised ~**$450M**, with valuation up ~5x in about a year.

### Why the valuation holds up

At ~$100M ARR and $3.1B valuation, that's about **30x PS (Price-to-Sales)** — between Ookla and Hugging Face:

1. **A scarce "neutral judge" position**, akin to "the credit-rating agency / Michelin guide of AI" — the judging authority itself is hard to replace.
2. **Two-sided network effects**: more voters make the leaderboard more authoritative, which makes labs more compelled to participate.
3. **A data moat**: the accumulated human-preference data is a scarce training/alignment corpus.
4. **Stunning growth**: revenue up several-fold in months, valuation 5x in a year.
5. **A bet on continued AI-market expansion**: as long as models keep iterating, evaluation is a lasting necessity.

> **Sources:**
> - [AI evaluation startup LMArena raises $150M at $1.7B valuation — SiliconANGLE](https://siliconangle.com/2026/01/06/ai-evaluation-startup-lmarena-raises-150m-1-7b-valuation/)
> - [LMArena raised $200M Series B at $3.1B valuation — arr.club](https://www.arr.club/signal/lmarena-raised-200m-series-b-at-3-1b-valuation)
> - [Arena Raises $200M in Series B Funding at $3.1B Valuation — FinSMEs](https://www.finsmes.com/2026/10/arena-raises-200m-in-series-b-funding-at-3-1b-valuation.html)
> - [Report: LMArena Business Breakdown & Founding Story — Contrary Research](https://research.contrary.com/company/lmarena)
> - [LMArena — Wikipedia](https://en.wikipedia.org/wiki/LMArena)

## Side-by-side comparison

| | Ookla (Speedtest/Downdetector) | Hugging Face | Arena (LMArena) |
|---|---|---|---|
| Essence | Network-intelligence data company | AI open-source distribution platform | AI model evaluation / judge |
| Monetization | Sells network-performance data to enterprises | Enterprise Hub + inference hosting | Paid evaluation services |
| Moat | A decade of data accumulation | Developer network effects | Neutral judging authority + preference data |
| ~PS (Price-to-Sales) | 5x (cash-flow driven) | 86x (ecosystem chokepoint) | 30x (growth + judging authority) |
| Status | Acquired by Accenture (mature) | Acquired by NVIDIA (mature platform) | Independent, high-growth (early) |

The three are not selling the same thing at all: **Ookla sells the cash flow of a data moat, Hugging Face sells the future option on a distribution entrance, and Arena sells the authority of "who gets to decide."** One framework, three moats — which is exactly the question that buy-side research has to answer behind any high or low PS multiple.
