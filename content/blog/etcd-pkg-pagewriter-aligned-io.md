+++
title = "etcd pkg/ioutil.PageWriter：页对齐缓冲写与 WAL 的崩溃安全"
date = 2026-08-03
description = "从磁盘扇区讲起，说清什么是页对齐、为什么 WAL 非要对齐不可，再从一个可独立运行的 demo 出发拆解 PageWriter 的『对齐 + 批量 + 大块透写』三板斧，以及它如何为 torn write 的识别提供可预期边界。"
[taxonomies]
tags = ["etcd"]
[extra]
toc = true
+++

前几篇讲的都是 etcd 的并发原语(`wait` / `WaitTime` / `notify`)和无锁 ID(`idutil`)。这篇转向 IO：`pkg/ioutil` 里只有百来行的 `PageWriter`,却是 etcd **WAL(预写日志)写路径上的性能与正确性关键件**。

要讲清它,得先从磁盘硬件的「扇区」讲起,搞明白什么是「页对齐」、为什么 WAL 非对齐不可——这是 PageWriter 存在的全部理由。然后从一个可独立运行的 demo 拆解它的实现,最后看它如何和 WAL 的崩溃恢复配合。

<!-- more -->

## 一、从磁盘扇区说起

### 扇区(sector)：磁盘的最小读写单位

磁盘(机械盘或 SSD)**不能按单个字节读写**,物理最小操作单位是「扇区」:

- 传统机械盘:扇区 = **512 字节**
- 现代盘(4Kn / SSD):扇区 = **4096 字节**

这带来一个关键后果——**read-modify-write(读-改-写)**。如果你只想改扇区里的几个字节,磁盘必须:

```
读出整个扇区 → 改掉其中一部分 → 整个扇区写回
```

一次逻辑写变成「一读一写」,既慢又写放大。同理,读一份跨两个扇区的数据,磁盘会把**两个扇区都整块读上来**,再由软件截取你要的部分(读放大)。

### 页(page)与页对齐

「页」在这里**不是操作系统内存页**,而是代码选定的一个**对齐块** `pageBytes`,是若干扇区的整数倍。etcd WAL 里:

```go
const minSectorSize = 512               // 保守的最小扇区假设
const walPageBytes  = 8 * minSectorSize // = 4096 = 4KB
```

**页对齐**的意思是:每次落盘的**起点和长度都是 `pageBytes` 的整数倍**,即都卡在页边界上。

```
字节:    0     4096    8192   12288
        |──页0──|──页1──|──页2──|   （页=4KB）
对齐写:  |████████|████████|      整页整页地写   ✓
不对齐:      |██████|██|          跨在页中间啃头啃尾 ✗
```

判断对齐的数学表达:`offset % pageBytes == 0 且 length % pageBytes == 0`。

为什么取 512 做基准、再 ×8 成 4KB?因为 512 是最保守假设——对齐到 512 的倍数在 512B 盘上对齐,而 4096 又是 512 的整数倍,所以 **512B 盘和 4K 盘两种硬件都对齐**;放大到 4KB 则是在「对齐粒度」和「落盘频率」之间取平衡。

## 二、为什么 WAL 非对齐不可

有两个动机,一个关乎性能,一个关乎正确性——后者才是 WAL 的命门。`walPageBytes` 的注释点破了后者:

```go
// It should be a multiple of the minimum sector size so that WAL can safely
// distinguish between torn writes and ordinary data corruption.
```

### 动机 1（性能）：规避 read-modify-write

不满整页的写会触发 read-modify-write。页对齐后每次整页覆盖,磁盘直接写、不用先读。WAL 是每条日志都要落盘的高频路径,收益显著。

### 动机 2（正确性）：区分「撕裂写」与「数据损坏」

**撕裂写(torn write)**:断电时,一次跨多扇区的写可能只写完前半、后半没写。这是掉电的正常现象。

问题是——崩溃恢复读到一条残缺记录,怎么判断它是 **(a) 断电 torn write**(正常,截断丢弃即可)还是 **(b) 真数据损坏**(异常,必须报错停机)?**页对齐让这个判断成为可能**:所有写都按扇区/页对齐,合法的残缺只会出现在**文件尾部、可预期的对齐边界上**。

⚠️ **一个常见误解要澄清**:页对齐**并不能阻止 torn write 物理发生**——一条跨两页的记录,断电照样可能被撕裂。对齐的目的从来不是「阻止」,而是让撕裂发生后**能被可靠地识别、并和真损坏区分开**。torn write 是磁盘物理特性,软件挡不住;软件能做的是事后正确判断。这一点后面第五节细说。

## 三、PageWriter 的角色

上层 WAL encoder 按「一条记录」写,长度五花八门、天然不对齐。PageWriter 夹在 encoder 和文件之间,把这些不规整的写**攒起来、补齐、凑成整页再落盘**——它实现 `io.Writer` 接口,对上层透明。

## 四、最小可运行实现

> 完整可运行源码：[golang/etcd/pkg-ioutil-pagewriter/main.go](https://github.com/spursy/spursy.github.io/blob/main/golang/etcd/pkg-ioutil-pagewriter/main.go)，克隆仓库后 `cd golang && go run ./etcd/pkg-ioutil-pagewriter` 即可执行。

核心是三个字段和一个 `Write` 方法。先看字段:

```go
type PageWriter struct {
	w                 io.Writer
	pageOffset        int    // 缓冲基址相对页边界的偏移
	pageBytes         int    // 每页字节数
	bufferedBytes     int    // 缓冲中待写字节数
	buf               []byte // 写缓冲（容量 = 水位线 + 一页 slack）
	bufWatermarkBytes int    // 触发 flush 的水位线（< len(buf)）
}

func NewPageWriter(w io.Writer, pageBytes, pageOffset int) *PageWriter {
	return &PageWriter{
		w:                 w,
		pageOffset:        pageOffset,
		pageBytes:         pageBytes,
		buf:               make([]byte, defaultBufferBytes+pageBytes), // 多留一页 slack
		bufWatermarkBytes: defaultBufferBytes,
	}
}
```

`Write` 是心脏,分四个阶段:

```go
func (pw *PageWriter) Write(p []byte) (n int, err error) {
	// fast path：不越过水位线，纯拷贝进 buffer（批量合并小写）
	if len(p)+pw.bufferedBytes <= pw.bufWatermarkBytes {
		copy(pw.buf[pw.bufferedBytes:], p)
		pw.bufferedBytes += len(p)
		return len(p), nil
	}
	// 1) 补齐 slack 页，让 buffer 对齐到页边界
	slack := pw.pageBytes - ((pw.pageOffset + pw.bufferedBytes) % pw.pageBytes)
	if slack != pw.pageBytes {
		partial := slack > len(p)
		if partial {
			slack = len(p) // 数据不够填满 slack
		}
		copy(pw.buf[pw.bufferedBytes:], p[:slack])
		pw.bufferedBytes += slack
		n = slack
		p = p[slack:]
		if partial {
			return n, nil // 不强制一次不对齐的 flush
		}
	}
	// 2) buffer 已页对齐，flush 出去
	if err = pw.Flush(); err != nil {
		return n, err
	}
	// 3) 大块透写：整页的部分直接写底层，不过 buffer、不拷贝（zero-copy）
	if len(p) > pw.pageBytes {
		pages := len(p) / pw.pageBytes
		c, werr := pw.w.Write(p[:pages*pw.pageBytes])
		n += c
		if werr != nil {
			return n, werr
		}
		p = p[pages*pw.pageBytes:]
	}
	// 剩下不足一页的尾巴，递归写回 buffer 攒着
	c, werr := pw.Write(p)
	n += c
	return n, werr
}
```

flush 负责真正落盘 + 对齐记账:

```go
func (pw *PageWriter) flush() (int, error) {
	if pw.bufferedBytes == 0 {
		return 0, nil // 空缓冲短路，保证幂等
	}
	n, err := pw.w.Write(pw.buf[:pw.bufferedBytes])
	// 对齐记账：更新下一批缓冲基址相对页边界的偏移
	pw.pageOffset = (pw.pageOffset + pw.bufferedBytes) % pw.pageBytes
	pw.bufferedBytes = 0
	return n, err
}
```

## 五、几个关键设计

### 1. 三板斧：对齐 + 批量 + 大块透写

| 手法 | 代码位置 | 作用 |
|------|----------|------|
| **批量** | fast path `len(p)+bufferedBytes <= watermark` | 大量小写攒在内存,零 syscall |
| **对齐 (slack)** | `slack := pageBytes - (...)` | 用数据前段把 buffer 补齐到页边界再 flush |
| **大块透写 (zero-copy)** | `if len(p) > pageBytes` 直接 `pw.w.Write` | 整页部分不经 buffer、不拷贝,直落底层 |

一次 `Write` 500B（`pageBytes=128`、已 buffered 200）会依次:补 slack 56B → buffer 满 256B 对齐 flush → 透写 384B(3页) → 剩 60B 尾巴递归回 buffer。落盘全部页对齐。

### 2. slack 空间：多留一页的巧妙

`buf` 容量故意是 `defaultBufferBytes + pageBytes`,**比水位线多整整一页**。这一页叫 slack 空间。

为什么?fast path 保证走完后 `bufferedBytes` 最多逼近水位线;而补 slack 时还要往 buffer 里**再塞最多 `pageBytes-1` 字节**。若 buffer 容量等于水位线,这一步就越界 panic。多留一页,恰好接住这个溢出——差一个字节严丝合缝装下。于是补齐分支可以**无条件 copy、零边界检查、零额外分配**,代码极其干净。**用一页固定内存,换来热路径的简洁与零分配。**

### 3. pageOffset：让对齐跨越多次写与文件接续

`slack = pageBytes - ((pageOffset + bufferedBytes) % pageBytes)` 里的 `pageOffset` 是「buffer 基址相对页边界的偏移」。它在 `flush` 后更新为 `(pageOffset + bufferedBytes) % pageBytes`。

它的价值:让对齐记账能**跨越多次 Write/Flush、甚至接续已有文件**。WAL 常是「接着已有文件继续写」,所以初始 `pageOffset` 要取文件当前 Seek 位置(见下节)。没有它,PageWriter 只能假设「永远从页边界开始」,一旦上次停在页中间就全盘算错。

### 4. partial slack：宁可不落盘，也不做不对齐落盘

若这次数据不够填满 slack 缺口(`partial`),buffer 仍不对齐,此刻**直接返回、不 flush**,把数据留着等下次补。这是「宁可不落盘,也不破坏页对齐承诺」——`TestPageWriterPartialSlack` 专门验证此行为。

## 六、在 etcd WAL 中的真实用法

全 etcd 里 PageWriter 只有一个使用者——`server/storage/wal/encoder.go`:

```go
const walPageBytes = 8 * minSectorSize   // :36  对齐单位 = 4KB

func newEncoder(w io.Writer, prevCrc uint32, pageOffset int) *encoder {
	return &encoder{
		bw: ioutil.NewPageWriter(w, walPageBytes, pageOffset), // :49
		// ...
	}
}
```

### 初始 pageOffset：从文件当前位置起算

```go
// encoder.go:57  newFileEncoder
offset, _ := f.Seek(0, io.SeekCurrent)   // 文件当前写到哪
return newEncoder(f, prevCrc, int(offset)) // 用它做 pageOffset
```

传错就会算错 slack、破坏对齐——`TestPageWriterOffset` 验证:flush 64 字节后 `pageOffset` 变成 64,下个 encoder 接着用。

### 互补的另一层：8 字节帧对齐

WAL 在 PageWriter 的「页对齐」之外,还在**记录帧**层面做了 8 字节对齐(`encodeFrameSize`):

```go
// force 8 byte alignment so length never gets a torn write
padBytes = (8 - (dataBytes % 8)) % 8
if padBytes != 0 {
	lenField |= uint64(0x80|padBytes) << 56  // 高位编码 padding 长度
}
```

这层保证的是「**长度字段自己不会被撕裂**」。为什么重要?恢复时若连「这条记录多长」都读错,decoder 会算错下一条记录的起点,整个扫描全乱。两层对齐不同粒度、互补。

## 七、回到那个误解：对齐如何服务 torn write 的「识别」

前面说过,页对齐挡不住 torn write 物理发生。那 WAL 怎么在恢复时识别它?靠的**不是对齐本身,而是两道防线**(见 `decoder.go`):

**防线 1 — CRC 校验**:每条记录带 CRC,恢复时重算比对(`decoder.go:138`)。跨扇区记录被撕裂,后半是旧数据,CRC 必然不过。CRC 负责「发现这条记录有问题」。

**防线 2 — isTornEntry（区分断电 vs 损坏）**:CRC 不过后,`isTornEntry`(`decoder.go:170`)把记录**按扇区(512B)切块**,只要有任意一个扇区块**全是 0**,就判定为 torn write:

```go
// 按扇区边界切分 data，再逐块检查
chunkLen := int(minSectorSize - (fileOff % minSectorSize))
// ...
// if any data for a sector chunk is all 0, it's a torn write
if isZero { return true }
```

为什么全 0 扇区 = 断电?因为 WAL 文件**预分配填 0**。掉电时前面扇区已写真实数据、后面某扇区还没来得及写 → 保持全 0。这种「前有数据、中间突然全 0」正是断电指纹;而真损坏通常是位翻转(随机脏值,不会整扇区全 0)。

**对齐在这里的作用**:正因为记录边界、长度字段落在可预期的对齐位置,`isTornEntry` 才能正确地按扇区切分、找到那个全 0 扇区。识别出后,decoder 返回 `io.ErrUnexpectedEOF`,上层把 WAL **截断到最后一条完整记录**(`lastValidOff`),丢弃残缺、正常恢复。

所以完整链路是:**页对齐/帧对齐提供可预期边界 → CRC 发现残缺 → isTornEntry 按扇区全 0 区分断电与损坏 → 截断到 lastValidOff**。对齐是「事前提供边界」,不是「事前阻止撕裂」。

## 八、总结

`PageWriter` 用「对齐 + 批量 + 大块透写」三板斧,把上层不规整的记录写整流成页对齐的落盘:

- **批量**:水位线缓冲合并小写,减少 syscall。
- **对齐**:slack 空间(多留一页)保证 flush 长度页对齐,既免 read-modify-write,又为崩溃恢复提供可预期边界。
- **透写**:大块整页部分 zero-copy 直落底层。

而它服务的终极目标,是 WAL 的**崩溃安全**——让 torn write 在恢复时可被识别、和真损坏区分。理解「扇区 → 页对齐 → 为什么对齐 → PageWriter 如何维持对齐 → 对齐如何服务 torn write 识别」这条线,就抓住了 etcd WAL 写路径的钥匙。

## 关键源码位置索引

| 内容 | 位置 |
|------|------|
| PageWriter 实现（Write 四段式 / slack / flush 记账） | `pkg/ioutil/pagewriter.go` |
| 对齐单位 `walPageBytes` + 注释 | `server/storage/wal/encoder.go:36`、`:33` |
| `NewPageWriter(w, walPageBytes, pageOffset)` | `encoder.go:49` |
| 从文件 Seek 位置取初始 pageOffset | `encoder.go:57` `newFileEncoder` |
| 记录帧 8 字节对齐 | `encoder.go:100` `encodeFrameSize` |
| `minSectorSize = 512` | `server/storage/wal/decoder.go:34` |
| torn write 识别 `isTornEntry`（按扇区全 0） | `decoder.go:170` |
| CRC 校验 | `decoder.go:138` |
| offset / partial slack 测试 | `pkg/ioutil/pagewriter_test.go:71`、`:45` |
