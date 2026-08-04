// Package main 演示 etcd pkg/contention 的 TimeoutDetector——
// 「把监控抽象成几十行可复用小工具」的教科书范例:一个通用的**卡顿/超时探测器**。
//
// ============================================================================
// 一、TimeoutDetector 要解决什么
// ============================================================================
//
// 很多系统里有「本应周期性发生的事」:raft leader 每隔 heartbeat 就该给每个
// follower 发一次心跳;某后台任务每隔 N 秒该 tick 一次……我们想知道:
// **这些事是不是被拖慢了?** 如果 leader 因为慢磁盘 / CPU 过载没能按时发心跳,
// 就该告警(可能引发 leader 选举抖动)。
//
// TimeoutDetector 就是回答这个问题的通用工具。它的关键设计:
//   - **完全不知道「心跳」是什么**,只处理抽象的「带 id 的周期事件」。任何
//     「本应固定间隔发生的事」都能用它探测——高度通用。
//   - 内部就一个 map[id]→上次事件时间戳,被观测时才算,零后台 goroutine、零定时器。
//
// ============================================================================
// 二、三个核心成员 + 唯一的核心方法 Observe
// ============================================================================
//
//	maxDuration time.Duration        // 预期的最大间隔(阈值)
//	records     map[uint64]time.Time // id → 该 id 上次事件的时间戳
//	mu          sync.Mutex           // 保护 records(共享状态,需并发安全)
//
// Observe(id) 是全部精华:
//  1. 取出该 id 的「上次时间」pt;若从没见过(首次)→ 无可比较,返回 ok=true。
//  2. exceed = now - pt - maxDuration   // 实际间隔 超出阈值 多少
//     exceed > 0 → 迟到了,返回 ok=false + 迟到量。
//  3. 无论如何都把 records[id] 更新为 now(为下次比较做准备)。
//     返回 (是否正常, 超出多少)——不只给 bool,还给 exceed 便于日志量化。
//
// Reset() 清空所有记录:当「周期性前提」不再成立时(如 raft 里 leader 角色变更),
//
//	历史时间戳失去意义,清空避免误报。
//
// ============================================================================
// 三、在 etcd 里怎么用(server/etcdserver/raft.go)
// ============================================================================
//
//	构造:td = NewTimeoutDetector(2 * heartbeat)   // 宽容一倍:2 个心跳间隔内该发出
//	观测:每次发心跳消息时 ok, exceed := td.Observe(m.GetTo())  // 以「发给谁」为 id
//	      if !ok { log.Warn("leader failed to send out heartbeat on time; "+
//	                        "took too long, leader is overloaded likely from slow disk") }
//	重置:leadership 变化时 td.Reset()             // 角色变了,重新计时
//
//	要点:
//	- 按 id(follower)分桶,能精确指出「是发给哪个节点的心跳慢了」。
//	- 阈值语义(2*heartbeat)由调用方决定,工具本身不写死。
//
// ============================================================================
// 四、使用要点 / 坑
// ============================================================================
//
//   - 首次 Observe 一个 id 永远返回 ok=true(没有「上一次」可比),不是 bug。
//   - 它测的是「相邻两次同 id 事件的间隔」,不是「某动作的耗时」——要测耗时得在
//     动作前后各 Observe 一次(或换别的工具)。它天生适合「周期性事件迟到」检测。
//   - records 只增不减:见过的 id 会一直留在 map 里。事件 id 空间大且长期运行时,
//     需要靠 Reset() 或外部约束控制内存(etcd 里 id 是集群成员数,很小,无忧)。
//   - 并发安全由内部 mu 保证,可多 goroutine 直接 Observe。
//
// ============================================================================
//
// 说明:为便于独立运行,本文件内置了一个与 etcd pkg/contention.TimeoutDetector
// 等价的最小实现(见文件末尾),逻辑与 go.etcd.io/etcd/pkg/v3/contention 的
// contention.go 完全一致。
package main

import (
	"fmt"
	"sync"
	"time"
)

func main() {
	demo1()
	demo2()
}

// ---------------------------------------------------------------------------
// demo1:基本语义 —— 首次放行、准点 OK、迟到报警、Reset 清零
// ---------------------------------------------------------------------------
func demo1() {
	fmt.Println("################ demo1: TimeoutDetector 基本语义 ################")

	// 阈值 = 50ms:相邻两次同 id 事件间隔超过 50ms 就算迟到。
	td := NewTimeoutDetector(50 * time.Millisecond)

	const peer = uint64(0x1) // 事件 id(可理解为「发给 follower 1 的心跳」)

	// 第 1 次:没有历史 → 一律放行
	ok, exceed := td.Observe(peer)
	fmt.Printf("1st observe:            ok=%v exceed=%v  (首次无可比较,放行)\n", ok, exceed)

	// 准点:20ms 后再观测,< 50ms 阈值 → 正常
	time.Sleep(20 * time.Millisecond)
	ok, exceed = td.Observe(peer)
	fmt.Printf("after 20ms (on time):   ok=%v exceed=%v  (未超阈值)\n", ok, exceed)

	// 迟到:80ms 后再观测,> 50ms 阈值 → 报警,并给出超出量
	time.Sleep(80 * time.Millisecond)
	ok, exceed = td.Observe(peer)
	fmt.Printf("after 80ms (late):      ok=%v exceed=%v  ← 迟到,触发告警\n", ok, exceed)
	if !ok {
		fmt.Printf("   >> WARN: event %#x took too long, exceeded by %v\n", peer, exceed)
	}

	// Reset 后历史清空:紧接着的观测又被当「首次」放行
	td.Reset()
	ok, exceed = td.Observe(peer)
	fmt.Printf("after Reset (1st again): ok=%v exceed=%v  (Reset 清空历史,重新计时)\n\n", ok, exceed)
}

// ---------------------------------------------------------------------------
// demo2:复刻 raft 心跳场景 —— 按 follower 分桶,只有慢的那个被点名
// ---------------------------------------------------------------------------
func demo2() {
	fmt.Println("################ demo2: 模拟 raft 心跳,多 follower 分桶探测 ################")

	heartbeat := 30 * time.Millisecond
	// 与 etcd 一致:期望在 2 个心跳间隔内发出 → 阈值 = 2*heartbeat
	td := NewTimeoutDetector(2 * heartbeat)

	followers := []uint64{0xA, 0xB, 0xC}

	// 先给每个 follower 各发一次心跳,建立基线(首次都放行)
	for _, f := range followers {
		td.Observe(f)
	}
	fmt.Printf("baseline: 已为 follower %#x 建立首次心跳时间\n", followers)

	// 模拟下一轮:A、C 准点(1 个心跳间隔后),B 卡住了(拖到 3 个心跳间隔)
	fmt.Println("下一轮心跳:A/C 准点,B 因慢盘卡住 ...")
	time.Sleep(1 * heartbeat)
	report := func(f uint64) {
		ok, exceed := td.Observe(f)
		status := "on time ✓"
		if !ok {
			status = fmt.Sprintf("LATE ✗ (exceeded %v) → leader overloaded likely from slow disk", exceed.Round(time.Millisecond))
		}
		fmt.Printf("  heartbeat to follower %#x: %s\n", f, status)
	}
	report(0xA) // 1*heartbeat 后,< 2*heartbeat → OK
	report(0xC) // 同上 → OK

	// B 又多等了 2 个心跳才发出(距上次共 3*heartbeat)→ 超过 2*heartbeat 阈值
	time.Sleep(2 * heartbeat)
	report(0xB) // ← 只有 B 被点名

	fmt.Println("\n结论:分桶计时让告警精确到「是发给哪个 follower 的心跳慢了」。")
}

// ===========================================================================
// TimeoutDetector: 与 etcd pkg/contention.TimeoutDetector 等价的最小实现
// (逐字段、逐方法对照 contention.go)
// ===========================================================================

// TimeoutDetector 通过观测「两次本应固定间隔发生的事件」之间的实际耗时,
// 探测协程饥饿 / 卡顿。若实际间隔超过预期,报告结果。
type TimeoutDetector struct {
	mu          sync.Mutex           // 保护以下所有字段
	maxDuration time.Duration        // 预期的最大间隔(阈值)
	records     map[uint64]time.Time // id → 该 id 上次事件时间
}

// NewTimeoutDetector 创建一个阈值为 maxDuration 的探测器。
func NewTimeoutDetector(maxDuration time.Duration) *TimeoutDetector {
	return &TimeoutDetector{
		maxDuration: maxDuration,
		records:     make(map[uint64]time.Time),
	}
}

// Reset 清空全部历史记录。当「周期性前提」不再成立时调用(如 raft leader 角色变更)。
func (td *TimeoutDetector) Reset() {
	td.mu.Lock()
	defer td.mu.Unlock()
	td.records = make(map[uint64]time.Time)
}

// Observe 观测一个 id 的事件,计算与上次同 id 事件的间隔。
// 返回:该间隔是否未超阈值(ok),以及超出阈值的量(exceed,未超时为 0 或负,仅 >0 有意义)。
func (td *TimeoutDetector) Observe(id uint64) (bool, time.Duration) {
	td.mu.Lock()
	defer td.mu.Unlock()

	ok := true
	now := time.Now()
	exceed := time.Duration(0)

	if pt, found := td.records[id]; found {
		exceed = now.Sub(pt) - td.maxDuration // 实际间隔 - 阈值
		if exceed > 0 {
			ok = false
		}
	}
	td.records[id] = now // 更新「上次时间」,为下次比较做准备
	return ok, exceed
}
