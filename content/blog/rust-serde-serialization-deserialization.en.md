+++
title = "Rust Serialization in Practice: How serde and serde_json Work Together"
date = 2026-08-27
description = "Starting from real CLI code, this post clarifies the relationship between serde and serde_json in Rust: the direction of serialization/deserialization, the Serializer/Deserializer abstractions, the Value dynamic type, and how to write a lenient custom deserializer."
[taxonomies]
tags = ["rust", "serde"]
[extra]
toc = true
+++

If you do API communication, config parsing, or data storage in Rust, you can hardly avoid `serde` and `serde_json`. But the two names show up together so often that many people can't tell what each is responsible for, nor articulate what exactly "serialization" and "deserialization" are as two opposite-direction actions.

This post starts from a piece of real CLI code (a Rust command-line tool talking to a backend JSON service) and clarifies the core concepts along that path all at once: the division of labor between `serde` and `serde_json`, the direction of serialization/deserialization, the `Serializer`/`Deserializer` abstract interfaces, the `Value` dynamic type, and how to write a "lenient" custom deserializer to tolerate ill-behaved server data.

<!-- more -->

## 1. Serialize vs Deserialize: Two Opposite Directions

Let's nail the direction first — every concept below builds on it.

- **Serialize**: turn a Rust in-memory **struct/value** → into a transmittable/storable **byte stream or text** (like JSON). Direction: `memory → outside`.
- **Deserialize**: turn an external **byte stream/text** → back into a Rust **struct/value**. Direction: `outside → memory`.

The two are inverse operations:

```
   Rust struct ──serialize──▶  "{\"promptToken\": 123}"   (send / write file)
   Rust struct ◀─deserialize── "{\"promptToken\": 123}"   (receive / read file)
```

Why must they be two separate things? Because the directions are opposite, and the input/output types differ entirely — you can't express both with one function. The signatures of two custom functions make it obvious:

```rust
// Deserialize: take a "reader", produce the target type
fn de_i64_flexible<'de, D: Deserializer<'de>>(d: D) -> Result<i64, D::Error>

// Serialize: take "value + writer", write the value into it
fn ser_i64_as_string<S: Serializer>(v: &i64, s: S) -> Result<S::Ok, S::Error>
```

Mapped to serde's two traits:

| | `Serialize` | `Deserialize` |
|---|---|---|
| Method | `fn serialize(&self, ...)` reads itself | `fn deserialize(...) -> Self` builds itself |
| Data flow | memory → outside | outside → memory |
| Who needs it | when you **send/save** a type | when you **receive/load** a type |

An analogy: think of a struct as an **assembled piece of furniture**. Serialize = disassemble and pack it into flat panels (easy to ship); deserialize = reassemble the panels into furniture per the instructions. Packing and assembling are two different actions, hence two traits.

Why does Rust require you to **explicitly declare** these two traits, instead of doing it automatically at runtime like Python/Go? Because **Rust has no runtime reflection**. For zero-cost abstraction, Rust carries no runtime type info, so serde generates the corresponding code at **compile time** via `#[derive(Serialize, Deserialize)]`. This also means the two directions can exist independently — outbound request params that are only sent need `Serialize` only; a response that's only parsed needs `Deserialize` only.

## 2. serde vs serde_json: Engine and Plugin

With the direction clear, look at the division of labor between the two crates. In one line: **`serde` is the "framework / engine", `serde_json` is a "plugin for one specific format".**

| | **serde** | **serde_json** |
|---|---|---|
| Role | serialization **framework core** | JSON **format implementation** |
| Provides | `Serialize` / `Deserialize` traits, `Serializer` / `Deserializer` abstract interfaces, `#[derive]` macros | JSON read/write logic, `Value`, the `json!` macro, `to_string`/`from_str`, etc. |
| Bound to a format | **not bound to any format** | **JSON only** |

`serde` defines the abstract rules of "how to serialize", but **produces no data in any concrete format**; `serde_json` is what actually turns data into JSON or restores it from JSON. On top of the same serde core there are also `serde_yaml` (YAML), `toml` (TOML), `bincode` (binary), and more:

```
                ┌─ serde_json   → JSON
   serde ───────┼─ serde_yaml   → YAML
  (defines std) ├─ toml         → TOML
                └─ bincode      → binary
```

The biggest benefit of this split is **decoupling format from logic**: your struct only needs serde's derive macro **once** to support many formats at the same time; switching format only switches the implementation crate, without changing the struct at all:

```rust
#[derive(Serialize, Deserialize)]
struct DetailGroup { /* ... */ }

serde_json::to_string(&g)?;   // to JSON
serde_yaml::to_string(&g)?;   // to YAML   ← struct unchanged
toml::to_string(&g)?;         // to TOML
```

## 3. Serializer and Deserializer: Abstract Reader/Writer

`Serialize`/`Deserialize` (no trailing r) describe the **data side** — "my type can be converted". `Serializer`/`Deserializer` (with trailing r) describe the **format side** — "I'm responsible for converting to/from some format", implemented by crates like serde_json.

| | **`Serializer`** | **`Deserializer`** |
|---|---|---|
| Direction | Rust value → format (output) | format → Rust value (input) |
| Role | writer / encoder | reader / decoder |
| Typical methods | `serialize_i64`, `serialize_str`, `serialize_map`… | `deserialize_i64`, `deserialize_str`… |

An analogy: `Serializer` is like a **printer** (you hand it a document and it prints it out as JSON/YAML in its own way); `Deserializer` is like a **scanner / OCR** (you give it a sheet of paper and it recognizes it into structured data).

### What is S::Ok

The return type of a custom serialize function contains `S::Ok`, which is an **associated type** of the `Serializer` trait:

```rust
pub trait Serializer {
    type Ok;      // the type produced on successful serialization
    type Error;   // the error type on failure
}
```

What `S::Ok` actually is depends on the implementation: serde_json's serializer that writes into a buffer has `Ok = ()`; the serializer used by `to_value` has `Ok = serde_json::Value`. So a custom function **must not hardcode** the return type — it must pass `S::Ok` through as-is to work with any format:

```rust
fn ser_i64_as_string<S: Serializer>(v: &i64, s: S) -> Result<S::Ok, S::Error> {
    s.serialize_str(&v.to_string())   // already returns Result<S::Ok, S::Error>
}
```

An associated type is used rather than a generic parameter because for a **fixed serializer**, the produced type is **unique and fixed** — a JSON writer always produces `()`, never one thing then another.

## 4. serde_json::Value: JSON's Universal Container

Sometimes you **don't know or don't care** about the full structure and just want to catch an arbitrary chunk of JSON. That's when `serde_json::Value` fits — an enum that recursively represents any JSON:

```rust
pub enum Value {
    Null,                       // null
    Bool(bool),                 // true / false
    Number(Number),             // number
    String(String),             // string
    Array(Vec<Value>),          // array: elements are still Value
    Object(Map<String, Value>), // object: values are still Value
}
```

It resembles Python's `dict`/`list` combination or Go's `interface{}`. The difference is Rust uses an enum to **enumerate all JSON shapes into a finite set**, so when you `match` it the compiler can check you didn't miss a branch — safer.

Common accessors all return `Option`, yielding `None` on type mismatch, never panicking:

```rust
let period = g.map_data
    .get("period")            // Option<&Value>
    .and_then(|v| v.as_str()) // Value → Option<&str>
    .unwrap_or("-");
```

Selection advice: when the structure is fixed and fields are known, use a `#[derive(Deserialize)]` struct (type-safe, compile-checked); only use `Value` when the structure is dynamic/unknown/pass-through (flexible, but loses compile-time guarantees and requires null checks all the way).

## 5. In Practice: Writing a "Lenient" Serializer and Deserializer

Now let's tie the concepts together on a real problem. Some backends are ill-behaved: the same numeric field is sometimes returned as a number, sometimes as a string, sometimes even an empty string or null:

```json
"promptToken": 151285168      // number
"promptToken": "151285168"    // string
"promptToken": ""             // empty string
"promptToken": null           // null
```

serde by default requires an `i64` field to be a JSON number, and errors out on a string/null. The fix is a custom deserializer attached to the field via `#[serde(deserialize_with = "...")]`:

```rust
use serde::{Deserialize, Deserializer};

/// Deserialize an i64: the service may send a number or a quoted string.
/// Empty strings and null map to 0.
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

A few key points:

- **Signature**: `fn(D) -> Result<i64, D::Error> where D: Deserializer<'de>` is serde's fixed form for a custom deserializer. The **trait bound** `where D: Deserializer<'de>` is the admission gate — it both restricts callers to pass a valid "reader" and grants the function body the full capabilities of that trait (e.g. it can write the associated type `D::Error`).
- **Strategy**: first parse the input into a generic `serde_json::Value` (no assumed type), then `match` case by case — the classic "catch any JSON first, then decide yourself" pattern.
- **`serde::de::Error::custom(...)`**: constructs an error object that satisfies serde's requirements.

Attached to a struct field, it also combines with `default` (use the default when the field is missing) and `alias` (tolerate camelCase field names):

```rust
#[derive(Debug, Default, Clone, Deserialize, Serialize)]
pub struct DetailGroup {
    #[serde(default, deserialize_with = "de_i64_flexible")]
    pub prompt_token: i64,

    // tolerate the service's camelCase field name
    #[serde(default, alias = "webSearchCount", deserialize_with = "de_i64_flexible")]
    pub web_search_count: i64,

    // metadata with no fixed shape — store as Value and pass through
    #[serde(default)]
    pub map_data: serde_json::Value,
}
```

There's a matching `de_f64_flexible` following exactly the same pattern, for float fields like fees. This is **defensive deserialization**: keeping the CLI from crashing just because the service wraps a number in a string.

> Note: if the field is `Option<i64>`, the function that `deserialize_with` points to must also return `Option<i64>` — you can't reuse the one above directly.

### The Other Way: Writing a Custom Serializer

Deserialization handles compatibility on the "read in" side; sometimes we also need to control the shape of what we "write out". For example, some downstream systems require **large integers to be transmitted as strings** (to avoid precision loss in JavaScript `Number`). That calls for a custom serializer, attached to the field via `#[serde(serialize_with = "...")]`:

```rust
use serde::Serializer;

/// Serialize an i64 as a quoted string (avoid downstream JS losing precision on big ints).
fn ser_i64_as_string<S>(v: &i64, serializer: S) -> Result<S::Ok, S::Error>
where
    S: Serializer,
{
    serializer.serialize_str(&v.to_string())
}
```

Reading it side by side with the deserializer, the signature difference captures exactly the essence of the two opposite directions:

| | Deserializer `de_i64_flexible` | Serializer `ser_i64_as_string` |
|---|---|---|
| Input | a `Deserializer` (reader) | an `&i64` value + a `Serializer` (writer) |
| Output | `Result<i64, D::Error>` | `Result<S::Ok, S::Error>` |
| What it does | **takes** data from the reader, restores an `i64` | **writes** the value into the writer |

A few key points:

- **Return type uses `S::Ok`**: as covered earlier, it's `Serializer`'s associated type — whether it's `()` or `Value` is decided by the caller, so it can't be hardcoded. `serialize_str` itself returns `Result<S::Ok, S::Error>`, so just pass it through.
- **`serialize_str`** writes the value as a string; use `serialize_i64` if you want to keep it as a number.

On a field, `deserialize_with` and `serialize_with` can **coexist** — lenient inbound, normalized outbound, each direction minding its own business:

```rust
#[serde(
    default,
    deserialize_with = "de_i64_flexible",  // read: accepts number/string/null
    serialize_with = "ser_i64_as_string"   // write: always emits a string
)]
pub prompt_token: i64,
```

This echoes the conclusion of section 1: **serialization and deserialization are two opposite-direction actions**, whose rules can be entirely asymmetric — which is why serde uses two independent hooks (`serialize_with` / `deserialize_with`) to control them separately.

## 6. Four Conversion Entry Points: Don't Mix Up Direction and Carrier

serde_json has **two dimensions** that are easy to confuse:

1. **Direction**: `from_*` (deserialize, read in) vs `to_*` (serialize, write out).
2. **Carrier**: `*_str`/`*_string` (JSON text) vs `*_value` (an in-memory `Value` object).

They combine into four common functions:

| Function | Direction | Carrier | Purpose |
|------|------|------|------|
| `from_str` | in | text | JSON string → struct |
| `from_value` | in | Value | `Value` → struct |
| `to_string` / `to_string_pretty` | out | text | struct → JSON string |
| `to_value` | out | Value | struct → `Value` |

So the strict inverses are `from_value` ↔ `to_value` and `from_str` ↔ `to_string`. Something like `from_value` and `to_string_pretty`, with **opposite direction and different carrier**, is *not* an inverse pair.

A contrast in real code (receive a response, then print):

```rust
// receive response: text → Value → struct
let value = client::request_json_body(...).await?;   // get a Value
let report: DetailReport =
    serde_json::from_value(value).map_err(Into::into)?; // Value → struct

// print request body: Value → pretty text for humans
let text = serde_json::to_string_pretty(&body).unwrap_or_default();
```

`.map_err(Into::into)` is worth a note: `from_value` yields a `serde_json::Error`, but the function signature requires returning `anyhow::Error` — different types, can't return directly. `Into::into` relies on `anyhow::Error` implementing `From<serde_json::Error>` to convert the error type automatically. It's equivalent to `|e| e.into()`, and it's the same mechanism as the `?` operator's automatic conversion.

`.unwrap_or_default()`, on the other hand, is "best-effort": on serialization failure it doesn't panic but degrades to an empty string — fine for logging/debugging, but it silently swallows the error, so critical data should still propagate the error with `?`.

## 7. Summary

Condensing the concepts along this path into one table:

| Concept | One line |
|------|--------|
| Serialize / Deserialize | Opposite directions: memory→outside / outside→memory; can't be one function |
| serde / serde_json | Engine / plugin: serde defines the abstraction, serde_json implements JSON |
| Serialize / Deserialize (traits) | Data-side traits: "my type can be converted" |
| Serializer / Deserializer | Format-side interfaces: "I convert to/from a format", implemented by serde_json |
| S::Ok | The serializer's associated type, the produced type, decided by the implementation |
| serde_json::Value | A universal-container enum for JSON, catches any/dynamic JSON |
| deserialize_with | Attach a custom parser to a field to tolerate ill-behaved data |
| from_/to_ × str/value | Two dimensions — direction × carrier; don't mix them up |

In one line: **serde is a format-agnostic serialization framework (defining the `Serialize`/`Deserialize` and `Serializer`/`Deserializer` abstractions), and serde_json is its JSON implementation (providing `Value`, the four conversion entry points, and the `json!` macro); serialization and deserialization are two opposite-direction actions that must be declared separately; when facing ill-behaved server data, write a lenient parser with `deserialize_with` + `Value` to tolerate it gracefully.**
