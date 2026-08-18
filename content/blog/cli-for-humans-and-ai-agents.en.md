+++
title = "Designing CLI Output for Humans and AI Agents Alike"
date = 2026-08-18
description = "Using cli-terminal as an example, how one neutral data structure and one render function can feed both the human reading a table and the agents and scripts consuming JSON — plus a few CLI ergonomics touches like clap's two-level help."
[taxonomies]
tags = ["rust", "cli"]
+++

We used to design CLI output for one reader: the human sitting at the terminal. Aligned tables, colored highlights, friendly prompts came first. But there's now a second, increasingly important kind of "reader" — **scripts and AI agents**. They don't care about layout; they want structured data that `jq` can parse and a program can consume reliably.

[cli-terminal](https://github.com/spursy/cli-terminal), a small tool demonstrating OAuth device-flow login, uses a very lightweight design in its output layer to serve both audiences. This post unpacks it, along with a few CLI ergonomics choices along the way.

<!-- more -->

## 1. The core idea: commands produce data, rendering lives in one place

The easiest trap is letting each command `println!` its own layout. Then "add an output format" means editing every command, and the human-format and machine-format logic scatter everywhere and fight each other.

cli-terminal splits the two concerns:

- **A command only produces a neutral data structure** — here, a plain `serde_json::Value`;
- **One** render function decides how to print it based on `--format`.

So the `auth status` handler looks like this, entirely unaware of whether the final output is a table or JSON:

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

**Adding a new format = adding one match arm, touching no command** — that's the biggest payoff of this design.

## 2. Two audiences, two formats

The format itself is a clap `ValueEnum` with just two variants:

```rust
#[derive(ValueEnum, Clone, Copy, Default, Debug)]
pub enum OutputFormat {
    /// Human-readable aligned table (default).
    #[default]
    #[value(name = "table", alias = "pretty")]
    Table,
    /// Machine-readable JSON — for scripts, jq, and AI agents.
    Json,
}
```

Two details worth noting:

- **The default is `table`** (`#[default]`). A human typing at a terminal is the most common case, so the default should serve the human; an agent adds `--format json` explicitly to state its intent.
- **`alias = "pretty"`**: `--format pretty` and `--format table` are equivalent. This kind of small alias spares the user from having to remember "is it table or pretty?" — at essentially zero cost.

The render function branches on format:

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

So the same command shows a human:

```
Field      Value
---------  -----
logged_in  true
client_id  cli-terminal
token      valid
```

while `--format json` gives an agent structured data ready for `jq '.token'`:

```json
{
  "logged_in": true,
  "client_id": "cli-terminal",
  "token": "valid"
}
```

## 3. Dependency-free table alignment

The table pulls in no table crate — it's just a two-column aligned print. First scan all keys (including the header) for the widest one, then use that as the left-aligned padding width:

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

For a demo tool with a handful of fields, pulling in a dependency just for "alignment" isn't worth it. **Dependencies have a cost** — compile time, supply-chain surface, version churn — so layout you can solve in a dozen lines of std shouldn't drag in a crate. This isn't "reinventing the wheel"; it's keeping complexity proportional to the actual size of the problem.

Scalar rendering also smooths over the human/machine difference: strings are **unquoted** (a human reading a table doesn't want to see `"valid"`), and `null` shows as `-`:

```rust
fn scalar_to_string(v: &serde_json::Value) -> String {
    match v {
        serde_json::Value::String(s) => s.clone(),
        serde_json::Value::Null => "-".to_string(),
        other => other.to_string(),
    }
}
```

## 4. A fallback that never silently drops data

Table mode only makes sense when the **top level is an object** (it can flatten into two `Field | Value` columns). But what if a command produces an array, or a nested structure?

Notice the `_ =>` in the `Table` branch above: **any non-object shape falls back to pretty JSON**, rather than forcing a table or printing nothing.

```rust
_ => println!("{}", serde_json::to_string_pretty(value).unwrap_or_default()),
```

This fallback matters: it guarantees **no data is silently dropped**. The worst thing an output layer can do is "show nothing for some inputs" — that makes the caller think the result was empty. Better to degrade to less-pretty JSON than to withhold the data.

## 5. `--format` is a global flag

`--format` hangs off the top-level `Cli` and is `global = true`:

```rust
#[arg(long, global = true, default_value = "table")]
format: OutputFormat,
```

`global = true` makes it apply to **every subcommand** — both `cli-terminal auth status --format json` and `cli-terminal whoami --format json` accept it. The user needn't remember "which command supports json"; the mental model is uniform: **any command can switch format**. `--verbose` uses the same trick.

## 6. Aside: clap's two-level help

A CLI's "documentation" is its `--help`, and cli-terminal draws a distinction here too. Each command has two help levels — `-h` for a brief summary, `--help` for the detailed version with examples:

```rust
#[command(
    after_help = "Each command has two help levels:\n  \
      cli-terminal <command> -h       brief summary (options only)\n  \
      cli-terminal <command> --help   full detail: description and examples"
)]
```

This leans on a clap convention: the **first line** of a doc comment is the summary (shown by `-h`), and the **paragraph after a blank line** is the long help (shown by `--help`). So the comments in the command definition are layered like this:

```rust
/// Show the identity associated with the stored token
///
/// Calls the protected `GET /userinfo` endpoint with the stored bearer token.
/// Requires a valid, non-expired token; a 401 prompts re-authentication.
/// Example: cli-terminal whoami
/// Example: cli-terminal whoami --format json
Whoami,
```

Someone in a hurry scans options with `-h`; a first-time user reads the description and examples with `--help`. **One comment, two levels of detail** — once again, "one source, multiple presentations."

## 7. Wrap-up

The points in this post are facets of the same principle:

| Design point | The general lesson |
|--------------|--------------------|
| Commands produce neutral data, rendering in one place | Separate "producing data" from "presenting it"; a new format is just one arm |
| `table` default / `json` explicit + `pretty` alias | Default serves humans, machines state intent explicitly; cheap aliases lower recall burden |
| Dependency-free table alignment | Keep complexity proportional to the problem; don't pull a crate for trivia |
| Non-object falls back to pretty JSON | An output layer must never silently drop data |
| `--format` as a global flag | A uniform mental model: any command can switch format |
| clap two-level help | One doc comment yields both brief and detailed help |

As AI agents increasingly invoke command-line tools directly, "machine-readable output" has gone from a nicety to a baseline skill for a CLI. And getting there needs no heavy machinery — one neutral data structure and one rendering choke point is enough. Full source at [github.com/spursy/cli-terminal](https://github.com/spursy/cli-terminal).

## Key source locations

| What | Where |
|------|-------|
| Output format enum / render dispatch / table alignment | [`src/output.rs`](https://github.com/spursy/cli-terminal/blob/main/src/output.rs) |
| Global `--format` flag / two-level help / command definitions | [`src/cli/command.rs`](https://github.com/spursy/cli-terminal/blob/main/src/cli/command.rs) |
