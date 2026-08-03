// Package main 演示 etcd pkg/idutil 的 Generator——
// 「无锁、无中心协调」的分布式唯一 ID 生成器。
//
// ============================================================================
// 一、idutil 解决什么问题
// ============================================================================
//
// etcd 每个写请求都要一个唯一 ID，用来在 wait.Register(id) 上登记、等 apply
// 完成后 Trigger(id) 精确唤醒自己（见 v3_server.go:1067 -> 1106）。这个 ID 必须：
//   - 同一节点内：单调、不重复、生成极快（写请求路径上，不能加锁抢全局计数器）。
//   - 跨节点之间：天然不撞（多个 member 各自生成，不做任何协调）。
//   - 重启之后：不和重启前已发出的 ID 相同（否则老请求的 wait 会被新 ID 误唤醒）。
//
// idutil 用「一个 uint64 的位布局 + 一次 atomic 自增」全部搞定，没有锁、没有 RPC、
// 没有中心发号器。
//
// ============================================================================
// 二、64 位布局（核心）
// ============================================================================
//
//   | prefix   | suffix                |
//   | 2 bytes  | 5 bytes    | 1 byte   |
//   | memberID | timestamp  | cnt      |
//   高 2 字节 = memberID（节点隔离）
//   中 5 字节 = 启动时刻的毫秒时间戳（重启隔离）
//   低 1 字节 = 计数器（同节点内自增）
//
// 三段各司其职，把「唯一性」拆成三个正交维度：
//   1. memberID 前缀    -> 解决「跨节点不撞」：不同节点前缀不同，值域天然不相交。
//   2. 毫秒时间戳（5B）  -> 解决「重启不撞」：重启后时间前进，起点不同。
//      5 字节毫秒 ≈ 2^40 ms ≈ 35 年窗口，足够覆盖一个进程的生命周期。
//   3. 计数器（1B）      -> 解决「同节点内递增」：每次 Next() 就 +1。
//
// ============================================================================
// 三、最妙的一笔：计数器「故意」溢出进位到时间戳位
// ============================================================================
//
// 计数器只有 1 字节（0~255），一眼看去每毫秒只能发 256 个 ID，早该不够用。
// 但 Next() 的自增是对「整个 6 字节 suffix」做 atomic.AddUint64，而不是只加低 8 位：
//
//     suffix := atomic.AddUint64(&g.suffix, 1)   // 对 timestamp+cnt 一起进位
//     id := g.prefix | lowbit(suffix, suffixLen) // 只保留低 48 位，高位 memberID 不被污染
//
// 于是计数器满 256 后，进位会「爬」到时间戳字节里。设计者故意允许这件事：
//   - 等价于把「计数器空间」从 2^8 借用到整个 suffix 的 2^48。
//   - 唯一性为什么不破？因为进位吃掉的时间戳，是「未来的毫秒值」——只要真实时间
//     还没走到那里，这些值就没人用过；而 etcd 吞吐 << 256 req/ms（250k req/s ≈
//     0.25 req/ms），计数器几乎永远追不上真实时间的推进，进位借来的空间用不完。
//   - prefix（memberID）在或运算里始终独立，进位再猛也进不到高 2 字节，跨节点隔离不受影响。
//
// 一句话：用「位布局 + 受控溢出」把无锁自增的窗口从 2^8 撑到 2^48，
//         以吞吐远低于时间推进速率作为唯一性的保证前提。比 Snowflake 更简洁。
//
// ============================================================================
// 四、为什么能无锁
// ============================================================================
//
//   - prefix 启动后就是常量，Next() 从不改它 -> 无需保护。
//   - suffix 的推进用 atomic.AddUint64，单条 CPU 指令级别的原子自增，多 goroutine
//     并发调用 Next() 也不会重号，且没有 mutex 的上下文切换开销（见 BenchmarkNext）。
//   - 「读—改—写」被压缩成一次原子加，这是它能放在写请求热路径上的关键。
//
// ============================================================================
// 五、使用要点 / 坑
// ============================================================================
//
//   - 唯一性是「概率 + 前提」保证，不是数学上的绝对保证：前提是「吞吐 << 256 req/ms」
//     且「进程寿命 < 35 年」。极端超高吞吐下计数器进位追上真实时间，才可能和未来
//     某毫秒的 ID 撞——现实中达不到。
//   - memberID 只有 2 字节（65536 个），集群规模远小于此，够用。
//   - 依赖本地时钟「重启后不回退太多」：若机器时间被大幅回拨，重启后可能落回旧时间段。
//     实践中 etcd 进程重启间隔 > 1ms，加上 memberID 隔离，基本无碍。
//   - 生成的是 uint64；某些场景（如 LeaseGrant 要正 int64）会再 & ((1<<63)-1) 掐掉符号位。
//
// ============================================================================
//
// 说明：为便于独立运行，本文件内置了一个与 etcd pkg/idutil.Generator 等价的最小实现
// （见文件末尾），逻辑与 go.etcd.io/etcd/pkg/v3/idutil 的 id.go 完全一致。
package main

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	// ------------------------------------------------------------------
	// 演示 1：位布局——拆解一个生成出来的 ID
	// memberID=0x12, 时间戳偏移 0x3456 ms，第一个 Next() 应得 0x12000000345601
	// ------------------------------------------------------------------
	fmt.Println("=== demo1: 64-bit layout breakdown ===")
	g := NewGenerator(0x12, time.Unix(0, 0).Add(0x3456*time.Millisecond))
	id := g.Next()
	fmt.Printf("id = 0x%014x\n", id)
	fmt.Printf("  prefix(memberID) = 0x%x\n", id>>suffixLen)          // 0x12
	fmt.Printf("  timestamp(5B)    = 0x%x\n", (id>>cntLen)&0xffffffffff) // 0x3456
	fmt.Printf("  counter(1B)      = 0x%x\n", id&0xff)                // 0x01

	// ------------------------------------------------------------------
	// 演示 2：同节点内 Next() 单调 +1
	// ------------------------------------------------------------------
	fmt.Println("\n=== demo2: Next() is monotonic +1 within a node ===")
	for i := 0; i < 3; i++ {
		fmt.Printf("  next = 0x%014x\n", g.Next())
	}

	// ------------------------------------------------------------------
	// 演示 3：跨节点 / 重启 都不撞
	// ------------------------------------------------------------------
	fmt.Println("\n=== demo3: uniqueness across nodes & restarts ===")
	a := NewGenerator(0, time.Time{}).Next()
	b := NewGenerator(1, time.Time{}).Next() // 不同 memberID
	c := NewGenerator(0, time.Now()).Next()  // 同 member，不同启动时刻（模拟重启）
	fmt.Printf("  node0@t0     = 0x%014x\n", a)
	fmt.Printf("  node1@t0     = 0x%014x  (different node -> different prefix)\n", b)
	fmt.Printf("  node0@now    = 0x%014x  (restart -> different timestamp)\n", c)

	// ------------------------------------------------------------------
	// 演示 4：计数器溢出「故意」进位到时间戳位（不会污染 memberID 前缀）
	// 手动把 suffix 逼到低字节即将进位处，观察进位爬到 timestamp 字节。
	// ------------------------------------------------------------------
	fmt.Println("\n=== demo4: counter overflow carries INTO timestamp, never into memberID ===")
	g2 := NewGenerator(0x12, time.Unix(0, 0).Add(0x3456*time.Millisecond))
	// 连发 256 个，低字节(cnt) 从 0x01 转一圈回来并向 timestamp 进 1。
	var last uint64
	for i := 0; i < 256; i++ {
		last = g2.Next()
	}
	fmt.Printf("  after 256 ids: 0x%014x  (timestamp byte went 0x3456 -> 0x3457, prefix 0x12 intact)\n", last)

	// ------------------------------------------------------------------
	// 演示 5：并发无锁——多 goroutine 同时 Next() 不重号
	// ------------------------------------------------------------------
	fmt.Println("\n=== demo5: lock-free concurrent Next() produces no duplicates ===")
	g3 := NewGenerator(7, time.Now())
	const G, N = 8, 2000
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := make(map[uint64]struct{}, G*N)
	dup := 0
	for i := 0; i < G; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < N; j++ {
				x := g3.Next()
				mu.Lock()
				if _, ok := seen[x]; ok {
					dup++
				}
				seen[x] = struct{}{}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	fmt.Printf("  generated %d ids across %d goroutines, duplicates = %d\n", G*N, G, dup)
}

// ============================================================================
// Generator: 与 etcd pkg/idutil.Generator 等价的最小实现（去掉依赖，便于独立运行）
// ============================================================================

const (
	tsLen     = 5 * 8        // 时间戳占 5 字节 = 40 位
	cntLen    = 8            // 计数器占 1 字节 = 8 位
	suffixLen = tsLen + cntLen // suffix 共 6 字节 = 48 位
)

// Generator 基于「memberID 前缀 + 时间戳 + 计数器」生成唯一 ID。
type Generator struct {
	prefix uint64 // 高 2 字节：memberID << 48，启动后不变
	suffix uint64 // 低 6 字节：timestamp+cnt，Next() 对它 atomic 自增
}

func NewGenerator(memberID uint16, now time.Time) *Generator {
	prefix := uint64(memberID) << suffixLen
	unixMilli := uint64(now.UnixNano()) / uint64(time.Millisecond/time.Nanosecond)
	// 取毫秒时间戳的低 40 位，左移 8 位给计数器腾出低字节。
	suffix := lowbit(unixMilli, tsLen) << cntLen
	return &Generator{prefix: prefix, suffix: suffix}
}

// Next 生成下一个唯一 ID：对整个 suffix 原子 +1（计数器满会进位到时间戳位），
// 再用 prefix 或上低 48 位。无锁。
func (g *Generator) Next() uint64 {
	suffix := atomic.AddUint64(&g.suffix, 1)
	id := g.prefix | lowbit(suffix, suffixLen)
	return id
}

// lowbit 取 x 的低 n 位。
func lowbit(x uint64, n uint) uint64 {
	return x & (math.MaxUint64 >> (64 - n))
}
