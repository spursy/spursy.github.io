// Package main 演示 etcd pkg/ioutil 的 PageWriter——
// 「页对齐的缓冲写」原语：所有落到底层 io.Writer 的写，长度都是 pageBytes 的整数倍。
//
// ============================================================================
// 一、PageWriter 要解决什么
// ============================================================================
//
// etcd 的 WAL（预写日志）对磁盘写有一个硬性要求：**每次真正落盘的写，长度必须
// 按扇区/页对齐（pageBytes 的整数倍）**。为什么？
//   - 规避磁盘的 read-modify-write：写一个不满整页的数据，磁盘要先把整页读上来、
//     改掉一部分、再写回，慢且放大写。按页对齐能让写直接覆盖整页。
//   - 便于区分「撕裂写(torn write)」与「普通数据损坏」：断电时未写完的整页可以被
//     识别，WAL 的崩溃恢复才能正确判断到哪条记录为止是完整的。
//
// 但上层调用者（WAL encoder）是按「一条记录」为单位调用 Write 的，长度五花八门、
// 根本不对齐。PageWriter 就夹在中间：**上层随意写，它负责把交给底层的每一次 Write
// 都凑成 pageBytes 的整数倍**。它实现 io.Writer 接口，对上层透明。
//
// ============================================================================
// 二、三个核心字段与「slack」概念
// ============================================================================
//
//   buf               []byte // 写缓冲，容量 = defaultBufferBytes + pageBytes（多留一页！）
//   bufWatermarkBytes int    // 水位线 = defaultBufferBytes，缓冲到这里就该 flush
//   bufferedBytes     int    // 当前缓冲里攒了多少字节
//   pageOffset        int    // 缓冲基址相对页边界的偏移（关键的对齐记账）
//   pageBytes         int    // 一页多少字节（如 4K）
//
// 最妙的设计是 buf 故意比水位线**多分配了一整页**（+pageBytes）。这一页叫「slack
// 空间」，专门用来放「为了把当前内容补齐到页边界」而多写的那一小段尾巴——它可以
// 越过水位线写进这多出来的一页，从而保证 flush 出去的长度恰好对齐。
//
// pageOffset 是理解全部逻辑的钥匙：它记录「当前缓冲的第一个字节，落在页内的第几位」。
// 因此「当前写到页边界还差多少」= slack = pageBytes - ((pageOffset + bufferedBytes) % pageBytes)。
//
// ============================================================================
// 三、Write 的三段式逻辑（对照源码 Write 方法）
// ============================================================================
//
//  fast path：len(p) + bufferedBytes <= 水位线  → 直接 copy 进 buffer，不落盘。
//             绝大多数小写都走这里，纯内存拷贝、零 syscall。
//
//  一旦这次写会越过水位线，进入「对齐 + 透写」三步：
//
//   1. 补齐 slack 页：算出「离页边界还差 slack 字节」，从 p 里切 slack 字节填进
//      buffer（用的正是那多分配的一页）。若 p 还不够填满 slack（partial），就只填
//      这么多、直接返回——**绝不强制一次不对齐的 flush**。填满后 buffer 内容页对齐了。
//
//   2. Flush：把已对齐的 buffer 整个写给底层 io.Writer（长度必是 pageBytes 整数倍）。
//
//   3. 大块透写(zero-copy)：剩下的 p 若还超过一页，直接把「整页的部分」写给底层，
//      **不经过 buffer、不拷贝**。剩下不足一页的尾巴再递归 Write 回 buffer 攒着。
//
// 一句话：小写攒进 buffer；攒满时先用 slack 页把 buffer 补齐到页边界再 flush；
//         超大写里「整页的部分」直接透写不拷贝，零头留回 buffer。
//         —— 对齐 + 批量 + 大块透写，三板斧。
//
// ============================================================================
// 四、flush 里的对齐记账
// ============================================================================
//
//   pw.pageOffset = (pw.pageOffset + pw.bufferedBytes) % pw.pageBytes
//
// 每次 flush 后，用「刚写出去的字节数」更新 pageOffset——因为下一批缓冲的基址，
// 相对页边界的偏移变了。这行是保证「跨多次 Write/Flush 仍持续对齐」的核心记账。
// 注意：Flush 是「把 buffer 现有内容原样写出」，它本身**不保证对齐**（可能被上层
// 主动调用），对齐只由上面 Write 的三段式逻辑在越过水位线时保证。
//
// ============================================================================
// 五、使用要点 / 坑
// ============================================================================
//
//   - pageBytes 必须 > 0（NewPageWriter 里 verify.Assert 断言，否则 panic）。
//   - 主动 Flush 会写出不对齐的尾巴：Flush 用于「关文件前把 buffer 清空」，此时
//     不再追求对齐。正常写路径的对齐由 Write 内部维持。
//   - pageOffset 初值要传对：WAL 用文件当前 Seek 偏移做 pageOffset（newFileEncoder），
//     否则接着已有文件写时对齐会算错。
//   - 它不是并发安全的：并发由上层（WAL encoder 的 mu）保证。
//
// ============================================================================
//
// 说明：为便于独立运行，本文件内置了一个与 etcd pkg/ioutil.PageWriter 等价的最小实现
// （见文件末尾），逻辑与 go.etcd.io/etcd/pkg/v3/ioutil 的 pagewriter.go 一致。
package main

import (
	"fmt"
	"io"
)

func main() {
	const pageBytes = 128
	// 底层「磁盘」：一个只接受页对齐写的校验 writer，任何不对齐的写都会报错。
	disk := &checkWriter{pageBytes: pageBytes}
	pw := NewPageWriter(disk, pageBytes, 0)

	// ------------------------------------------------------------------
	// 演示 1：一堆小写只进 buffer，不落盘（fast path）
	// ------------------------------------------------------------------
	fmt.Println("=== demo1: small writes are buffered, no disk write yet ===")
	for i := 0; i < 5; i++ {
		_, _ = pw.Write(make([]byte, 30)) // 5*30 = 150 字节，攒在 buffer 里
	}
	fmt.Printf("after 5x30B writes: buffered=%d, disk.writes=%d (still 0)\n", pw.bufferedBytes, disk.writes)

	// ------------------------------------------------------------------
	// 演示 2：一次大写触发「补 slack -> flush -> 大块透写」
	// 当前 buffered=150，缓冲水位线默认很大，这里手动 Flush 演示对齐落盘。
	// ------------------------------------------------------------------
	fmt.Println("\n=== demo2: a big write triggers slack-fill + aligned flush + passthrough ===")
	// 先把水位线调小，制造溢出，观察对齐行为。
	pw2disk := &checkWriter{pageBytes: pageBytes}
	SetBufferBytes(256) // 水位线 256B，buffer 实际 256+128
	pw2 := NewPageWriter(pw2disk, pageBytes, 0)
	_, _ = pw2.Write(make([]byte, 200)) // 先攒 200（<256，进 buffer）
	fmt.Printf("buffered=%d before big write\n", pw2.bufferedBytes)
	// 再来一个 500B 的大写：200+500 越过水位线，触发三段式。
	n, err := pw2.Write(make([]byte, 500))
	fmt.Printf("big write 500B: n=%d err=%v\n", n, err)
	fmt.Printf("after big write: disk.writes=%d disk.bytes=%d (all page-aligned), buffered=%d\n",
		pw2disk.writes, pw2disk.writeBytes, pw2.bufferedBytes)

	// ------------------------------------------------------------------
	// 演示 3：校验所有落盘写都页对齐 + 关文件前 Flush 清空尾巴
	// ------------------------------------------------------------------
	fmt.Println("\n=== demo3: every disk write is page-aligned; final Flush drains the tail ===")
	_ = pw2.Flush() // 关文件语义：把不足一页的尾巴也写出去（此次可不对齐）
	fmt.Printf("after final Flush: disk.writes=%d disk.bytes=%d buffered=%d\n",
		pw2disk.writes, pw2disk.writeBytes, pw2.bufferedBytes)
	fmt.Printf("aligned-violation count = %d (0 means every non-final write was aligned)\n", pw2disk.unaligned-1)
	// 说明：final Flush 那次尾巴可能不对齐，故减 1。
}

// ============================================================================
// checkWriter: 一个只“喜欢”页对齐写的底层 io.Writer，用于验证 PageWriter 的对齐承诺。
// ============================================================================

type checkWriter struct {
	pageBytes  int
	writes     int
	writeBytes int
	unaligned  int // 记录不对齐写的次数
}

func (cw *checkWriter) Write(p []byte) (int, error) {
	if len(p)%cw.pageBytes != 0 {
		cw.unaligned++
	}
	cw.writes++
	cw.writeBytes += len(p)
	return len(p), nil
}

// ============================================================================
// PageWriter: 与 etcd pkg/ioutil.PageWriter 等价的最小实现（去掉 verify 依赖）
// ============================================================================

var defaultBufferBytes = 128 * 1024

// SetBufferBytes 仅为 demo 调整水位线用（etcd 里 defaultBufferBytes 是包级变量）。
func SetBufferBytes(n int) { defaultBufferBytes = n }

// PageWriter 实现 io.Writer：写出去的都是 pageBytes 整数倍，或来自主动 Flush。
type PageWriter struct {
	w                 io.Writer
	pageOffset        int    // 缓冲基址相对页边界的偏移
	pageBytes         int    // 每页字节数
	bufferedBytes     int    // 缓冲中待写字节数
	buf               []byte // 写缓冲（比水位线多一页 slack）
	bufWatermarkBytes int    // 触发 flush 的水位线（< len(buf)，留出 slack）
}

func NewPageWriter(w io.Writer, pageBytes, pageOffset int) *PageWriter {
	if pageBytes <= 0 {
		panic("invalid pageBytes: must be > 0")
	}
	return &PageWriter{
		w:                 w,
		pageOffset:        pageOffset,
		pageBytes:         pageBytes,
		buf:               make([]byte, defaultBufferBytes+pageBytes), // 多留一页 slack
		bufWatermarkBytes: defaultBufferBytes,
	}
}

func (pw *PageWriter) Write(p []byte) (n int, err error) {
	// fast path：不越过水位线，纯拷贝进 buffer。
	if len(p)+pw.bufferedBytes <= pw.bufWatermarkBytes {
		copy(pw.buf[pw.bufferedBytes:], p)
		pw.bufferedBytes += len(p)
		return len(p), nil
	}
	// 1) 补齐 slack 页，让 buffer 对齐到页边界。
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
	// 2) buffer 已页对齐，flush 出去。
	if err = pw.Flush(); err != nil {
		return n, err
	}
	// 3) 大块透写：整页的部分直接写底层，不过 buffer、不拷贝。
	if len(p) > pw.pageBytes {
		pages := len(p) / pw.pageBytes
		c, werr := pw.w.Write(p[:pages*pw.pageBytes])
		n += c
		if werr != nil {
			return n, werr
		}
		p = p[pages*pw.pageBytes:]
	}
	// 剩下不足一页的尾巴，递归写回 buffer 攒着。
	c, werr := pw.Write(p)
	n += c
	return n, werr
}

// Flush 把缓冲数据写出（不保证对齐，供关文件等场景清空）。
func (pw *PageWriter) Flush() error {
	_, err := pw.flush()
	return err
}

func (pw *PageWriter) flush() (int, error) {
	if pw.bufferedBytes == 0 {
		return 0, nil
	}
	n, err := pw.w.Write(pw.buf[:pw.bufferedBytes])
	// 对齐记账：更新下一批缓冲基址相对页边界的偏移。
	pw.pageOffset = (pw.pageOffset + pw.bufferedBytes) % pw.pageBytes
	pw.bufferedBytes = 0
	return n, err
}
