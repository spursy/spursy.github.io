+++
title = "etcd pkg/ioutil.PageWriter: Page-Aligned Buffered Writes and WAL Crash Safety"
date = 2026-08-03
description = "Starting from disk sectors, this post explains what page alignment is and why the WAL must align, then dissects PageWriter's batch + align + passthrough tricks from a self-contained runnable demo, and how it provides predictable boundaries for torn-write detection."
[taxonomies]
tags = ["etcd"]
[extra]
toc = true
+++

The earlier posts covered etcd's concurrency primitives (`wait` / `WaitTime` / `notify`) and its lock-free ID (`idutil`). This one turns to IO: the ~100-line `PageWriter` in `pkg/ioutil` is a **performance and correctness cornerstone on etcd's WAL (write-ahead log) write path**.

To explain it we must start from the disk's hardware "sector," understand what "page alignment" is and why the WAL cannot avoid it — that is the entire reason PageWriter exists. Then we dissect its implementation from a self-contained runnable demo, and finally see how it works with WAL crash recovery.

<!-- more -->

## 1. Starting from the disk sector

### Sector: the disk's minimum read/write unit

A disk (spinning or SSD) **cannot read or write single bytes**; its minimum physical operation unit is the "sector":

- Traditional spinning disk: sector = **512 bytes**
- Modern disk (4Kn / SSD): sector = **4096 bytes**

This has a key consequence — **read-modify-write**. If you only want to change a few bytes inside a sector, the disk must:

```
read the whole sector → modify part of it → write the whole sector back
```

One logical write becomes a "read plus a write" — slow and write-amplifying. Likewise, reading data that spans two sectors makes the disk read **both sectors in full**, then software slices out what you want (read amplification).

### Page and page alignment

"Page" here is **not the OS memory page** but an **alignment block** `pageBytes` chosen by the code, a multiple of the sector size. In etcd's WAL:

```go
const minSectorSize = 512               // conservative minimum-sector assumption
const walPageBytes  = 8 * minSectorSize // = 4096 = 4KB
```

**Page alignment** means: every disk write's **start and length are integer multiples of `pageBytes`**, i.e. landing on page boundaries.

```
byte: 0      4096     8192     12288
      |──pg0──|──pg1──|──pg2──|   (page = 4KB)
aligned:|████████|████████|       whole pages at a time   ✓
unaligned:  |██████|██|           straddling page middles ✗
```

Mathematically: `offset % pageBytes == 0 && length % pageBytes == 0`.

Why base it on 512 then ×8 to 4KB? Because 512 is the most conservative assumption — aligning to a multiple of 512 aligns on 512B disks, and 4096 is itself a multiple of 512, so **both 512B and 4K disks are aligned**; enlarging to 4KB balances "alignment granularity" against "flush frequency."

## 2. Why the WAL cannot avoid alignment

Two motivations, one about performance and one about correctness — the latter is the WAL's lifeline. The comment on `walPageBytes` reveals it:

```go
// It should be a multiple of the minimum sector size so that WAL can safely
// distinguish between torn writes and ordinary data corruption.
```

### Motivation 1 (performance): avoid read-modify-write

A sub-page write triggers read-modify-write. Once page-aligned, each write covers whole pages, so the disk writes directly without reading first. The WAL is a high-frequency path where every log entry hits disk, so the benefit is significant.

### Motivation 2 (correctness): distinguish "torn write" from "data corruption"

**Torn write**: on power loss, a write spanning multiple sectors may have its first half done and its second half not. This is a normal power-loss phenomenon.

The problem — crash recovery reads a partial record; how does it decide whether it is **(a) a power-loss torn write** (normal, just truncate and discard) or **(b) real data corruption** (abnormal, must error out and halt)? **Page alignment makes this decision possible**: with all writes sector/page aligned, a legitimate remnant can only appear at the **file tail, on predictable aligned boundaries**.

⚠️ **A common misconception to clear up**: page alignment **does not prevent torn writes from physically happening** — a record spanning two pages can still be torn on power loss. The goal of alignment is never to "prevent," but to let a torn write, once it happens, be **reliably identified and distinguished from real corruption**. Torn writes are a physical disk property that software cannot block; what software can do is judge correctly after the fact. More in section 7.

## 3. PageWriter's role

The upper-layer WAL encoder writes "one record" at a time, with varying, naturally unaligned lengths. PageWriter sits between the encoder and the file, **buffering these irregular writes, padding them, and coalescing them into whole pages before hitting disk** — it implements `io.Writer`, transparent to the caller.

## 4. A minimal runnable implementation

> Full runnable source: [golang/etcd/pkg-ioutil-pagewriter/main.go](https://github.com/spursy/spursy.github.io/blob/main/golang/etcd/pkg-ioutil-pagewriter/main.go). Clone the repo, then `cd golang && go run ./etcd/pkg-ioutil-pagewriter`.

The core is three fields and one `Write` method. First the fields:

```go
type PageWriter struct {
	w                 io.Writer
	pageOffset        int    // offset of the buffer base relative to a page boundary
	pageBytes         int    // bytes per page
	bufferedBytes     int    // bytes pending in the buffer
	buf               []byte // write buffer (capacity = watermark + one page of slack)
	bufWatermarkBytes int    // watermark that triggers flush (< len(buf))
}

func NewPageWriter(w io.Writer, pageBytes, pageOffset int) *PageWriter {
	return &PageWriter{
		w:                 w,
		pageOffset:        pageOffset,
		pageBytes:         pageBytes,
		buf:               make([]byte, defaultBufferBytes+pageBytes), // one extra page of slack
		bufWatermarkBytes: defaultBufferBytes,
	}
}
```

`Write` is the heart, in four stages:

```go
func (pw *PageWriter) Write(p []byte) (n int, err error) {
	// fast path: not crossing the watermark, pure copy into buffer (batch small writes)
	if len(p)+pw.bufferedBytes <= pw.bufWatermarkBytes {
		copy(pw.buf[pw.bufferedBytes:], p)
		pw.bufferedBytes += len(p)
		return len(p), nil
	}
	// 1) fill the slack page so the buffer is page-aligned
	slack := pw.pageBytes - ((pw.pageOffset + pw.bufferedBytes) % pw.pageBytes)
	if slack != pw.pageBytes {
		partial := slack > len(p)
		if partial {
			slack = len(p) // not enough data to complete the slack
		}
		copy(pw.buf[pw.bufferedBytes:], p[:slack])
		pw.bufferedBytes += slack
		n = slack
		p = p[slack:]
		if partial {
			return n, nil // do not force an unaligned flush
		}
	}
	// 2) buffer is page-aligned, flush it
	if err = pw.Flush(); err != nil {
		return n, err
	}
	// 3) passthrough: write the whole-page part directly to the backend, no buffer, no copy (zero-copy)
	if len(p) > pw.pageBytes {
		pages := len(p) / pw.pageBytes
		c, werr := pw.w.Write(p[:pages*pw.pageBytes])
		n += c
		if werr != nil {
			return n, werr
		}
		p = p[pages*pw.pageBytes:]
	}
	// remaining sub-page tail: recurse back into the buffer
	c, werr := pw.Write(p)
	n += c
	return n, werr
}
```

`flush` does the real disk write plus alignment bookkeeping:

```go
func (pw *PageWriter) flush() (int, error) {
	if pw.bufferedBytes == 0 {
		return 0, nil // empty-buffer short circuit, idempotent
	}
	n, err := pw.w.Write(pw.buf[:pw.bufferedBytes])
	// alignment bookkeeping: update the next buffer base's offset relative to a page boundary
	pw.pageOffset = (pw.pageOffset + pw.bufferedBytes) % pw.pageBytes
	pw.bufferedBytes = 0
	return n, err
}
```

## 5. Key design points

### 1. Three tricks: align + batch + passthrough

| Trick | Location | Purpose |
|-------|----------|---------|
| **batch** | fast path `len(p)+bufferedBytes <= watermark` | buffer many small writes in memory, zero syscalls |
| **align (slack)** | `slack := pageBytes - (...)` | use the front of the data to pad the buffer to a page boundary before flush |
| **passthrough (zero-copy)** | `if len(p) > pageBytes` direct `pw.w.Write` | whole-page part bypasses the buffer, no copy |

A single 500B `Write` (`pageBytes=128`, already 200 buffered) does: fill slack 56B → buffer full 256B aligned flush → passthrough 384B (3 pages) → remaining 60B tail recurses into buffer. Every disk write is page-aligned.

### 2. Slack space: the cleverness of one extra page

`buf`'s capacity is deliberately `defaultBufferBytes + pageBytes`, **one whole page more than the watermark**. That page is the slack space.

Why? The fast path guarantees `bufferedBytes` at most approaches the watermark afterward; but filling slack then copies **up to `pageBytes-1` more bytes** into the buffer. If capacity equaled the watermark, that step would overflow and panic. One extra page exactly catches this overflow — fitting with one byte to spare. So the padding branch can `copy` **unconditionally, with zero bounds checks and zero extra allocation**, keeping the code extremely clean. **One page of fixed memory buys hot-path simplicity and zero allocation.**

### 3. pageOffset: alignment across multiple writes and file continuation

In `slack = pageBytes - ((pageOffset + bufferedBytes) % pageBytes)`, `pageOffset` is "the buffer base's offset relative to a page boundary." It is updated in `flush` to `(pageOffset + bufferedBytes) % pageBytes`.

Its value: alignment bookkeeping can **span multiple Write/Flush calls, and even continue an existing file**. The WAL often "continues writing an existing file," so the initial `pageOffset` must be the file's current Seek position (next section). Without it, PageWriter could only assume "always starting from a page boundary," and would miscalculate the moment the previous flush stopped mid-page.

### 4. Partial slack: rather not flush than flush unaligned

If this write cannot fill the slack gap (`partial`), the buffer is still unaligned, so it **returns immediately without flushing**, keeping the data for next time. This is "rather not flush than break the page-alignment guarantee" — verified by `TestPageWriterPartialSlack`.

## 6. How etcd's WAL actually uses it

Across all of etcd, PageWriter has exactly one user — `server/storage/wal/encoder.go`:

```go
const walPageBytes = 8 * minSectorSize   // :36  alignment unit = 4KB

func newEncoder(w io.Writer, prevCrc uint32, pageOffset int) *encoder {
	return &encoder{
		bw: ioutil.NewPageWriter(w, walPageBytes, pageOffset), // :49
		// ...
	}
}
```

### Initial pageOffset: reckoned from the file's current position

```go
// encoder.go:57  newFileEncoder
offset, _ := f.Seek(0, io.SeekCurrent)   // where the file is currently written to
return newEncoder(f, prevCrc, int(offset)) // use it as pageOffset
```

Passing it wrong miscalculates slack and breaks alignment — `TestPageWriterOffset` verifies: after flushing 64 bytes, `pageOffset` becomes 64, which the next encoder continues from.

### A complementary layer: 8-byte frame alignment

Beyond PageWriter's page alignment, the WAL also does 8-byte alignment at the **record frame** level (`encodeFrameSize`):

```go
// force 8 byte alignment so length never gets a torn write
padBytes = (8 - (dataBytes % 8)) % 8
if padBytes != 0 {
	lenField |= uint64(0x80|padBytes) << 56  // encode padding length in the high bits
}
```

This layer guarantees the **length field itself is never torn**. Why does it matter? During recovery, if even "how long is this record" is misread, the decoder miscomputes the next record's start and the whole scan goes off the rails. The two alignment layers are complementary, at different granularities.

## 7. Back to the misconception: how alignment serves torn-write "detection"

As noted, page alignment cannot prevent a torn write from physically happening. So how does the WAL identify it during recovery? **Not by alignment itself, but by two lines of defense** (see `decoder.go`):

**Defense 1 — CRC check**: each record carries a CRC, recomputed and compared at recovery (`decoder.go:138`). A torn cross-sector record has a stale second half, so the CRC necessarily fails. CRC "discovers that a record has a problem."

**Defense 2 — isTornEntry (distinguish power loss vs corruption)**: after a CRC failure, `isTornEntry` (`decoder.go:170`) splits the record into **sector-sized (512B) chunks**, and if any sector chunk is **all zeros**, it declares a torn write:

```go
// split data on sector boundaries, then check each chunk
chunkLen := int(minSectorSize - (fileOff % minSectorSize))
// ...
// if any data for a sector chunk is all 0, it's a torn write
if isZero { return true }
```

Why does an all-zero sector mean power loss? Because the WAL file is **preallocated and zero-filled**. On power loss the earlier sectors already hold real data while some later sector wasn't written yet → it stays all zeros. This "data followed by a suddenly all-zero sector" is the power-loss fingerprint; real corruption is usually bit-flips (random garbage, not a whole zero sector).

**Alignment's role here**: precisely because record boundaries and the length field land on predictable aligned positions, `isTornEntry` can correctly split by sector and find the all-zero sector. Once detected, the decoder returns `io.ErrUnexpectedEOF`, and the upper layer **truncates the WAL to the last complete record** (`lastValidOff`), discarding the remnant and recovering normally.

So the full chain is: **page/frame alignment provides predictable boundaries → CRC discovers the remnant → isTornEntry distinguishes power loss from corruption by all-zero sectors → truncate to lastValidOff**. Alignment "provides boundaries beforehand," it does not "prevent tearing beforehand."

## 8. Summary

`PageWriter` uses three tricks — align + batch + passthrough — to funnel the upper layer's irregular record writes into page-aligned disk writes:

- **batch**: watermark buffering coalesces small writes, fewer syscalls.
- **align**: slack space (one extra page) guarantees flush lengths are page-aligned, avoiding read-modify-write and providing predictable boundaries for crash recovery.
- **passthrough**: large whole-page parts go zero-copy straight to the backend.

Its ultimate goal is the WAL's **crash safety** — letting torn writes be identified at recovery and distinguished from real corruption. Grasping the line "sector → page alignment → why align → how PageWriter maintains alignment → how alignment serves torn-write detection" gives you the key to etcd's WAL write path.

## Source location index

| Item | Location |
|------|----------|
| PageWriter impl (four-stage Write / slack / flush bookkeeping) | `pkg/ioutil/pagewriter.go` |
| alignment unit `walPageBytes` + comment | `server/storage/wal/encoder.go:36`, `:33` |
| `NewPageWriter(w, walPageBytes, pageOffset)` | `encoder.go:49` |
| initial pageOffset from file Seek position | `encoder.go:57` `newFileEncoder` |
| record-frame 8-byte alignment | `encoder.go:100` `encodeFrameSize` |
| `minSectorSize = 512` | `server/storage/wal/decoder.go:34` |
| torn-write detection `isTornEntry` (all-zero sector) | `decoder.go:170` |
| CRC check | `decoder.go:138` |
| offset / partial slack tests | `pkg/ioutil/pagewriter_test.go:71`, `:45` |
