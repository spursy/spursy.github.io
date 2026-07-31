// Package main 演示 etcd pkg/wait 里的 WaitTime（timeList）——
// 「按逻辑时间（单调递增的 deadline）批量放行」的等待原语。
//
// ============================================================================
// 一、WaitTime 是什么
// ============================================================================
//
// pkg/wait 有两套东西：
//   - Wait（wait.go）：按「ID」一对一，Trigger(id, result) 精确唤醒一个等待者，带数据。
//   - WaitTime（wait_time.go，本文件）：按「逻辑时间」一对多，Trigger(deadline)
//     一次唤醒所有「等待目标 <= deadline」的等待者，只传信号、不带数据。
//
// 典型场景：有一个单调递增的进度计数（etcd 里就是 applied index）。
//   - 消费者：「等 apply 追上第 N 号」→ Wait(N)，阻塞在返回的 channel 上；
//   - 生产者：「已经 apply 到第 M 号了」→ Trigger(M)，一次放行所有 N <= M 的等待者。
//
// ============================================================================
// 二、核心机制（两点最精妙）
// ============================================================================
//
//  1. 用「关闭 channel」做放行：每个还没到的 deadline 对应一个 chan struct{}，
//     Trigger 时把所有 <= deadline 的 channel close 掉 —— 等在上面的 goroutine
//     同时返回。这和 notify 一样，都是「close 即广播」。
//
//  2. lastTriggerDeadline 消除竞态（关键）：进度是单调递增的，一旦 Trigger(M)
//     过了，之后任何 Wait(N<=M) 都不该再阻塞。timeList 记下 lastTriggerDeadline，
//     若 Wait 的 deadline 已经被触发过，直接返回一个「预先 close 好的全局 closec」，
//     立即放行。这样杜绝了「我 Wait 的那一刻，Trigger 刚好已经发生」导致的永久阻塞。
//
// 一句话：一个单调递增的进度线，谁想等某个刻度就登记一个 channel；进度推进到 X 时，
//         一次关闭 <= X 的所有 channel 放行全部；已经越过的刻度用预关闭 channel 秒放行。
//
// ============================================================================
// 三、和 wait / notify 的区别
// ============================================================================
//
//   - wait（按 ID）    ：一对一、带数据、精确路由。写请求拿回自己那条 apply 结果。
//   - notify（广播）   ：一对多、纯信号、无序。leader 变更等全局事件叫醒所有人。
//   - WaitTime（按阈值）：一对多、纯信号、按单调阈值。「等进度追上某个刻度」。
//
// ============================================================================
// 四、使用要点 / 坑
// ============================================================================
//
//   - deadline 必须来自一个单调递增的量（如 applied index），否则 lastTriggerDeadline
//     的「已越过即秒放行」语义会失真。
//   - 只传信号、不带数据：放行后要读的进度值得另外自己拿。
//   - Trigger 是「<= 全放行」，不是「精确等于」：晚触发的大 deadline 会顺带放行之前所有小的。
//
// ============================================================================
//
// 说明：为便于独立运行，本文件内置了一个与 etcd pkg/wait.WaitTime 等价的最小实现
// （见文件末尾 timeList），逻辑与 go.etcd.io/etcd/pkg/v3/wait 的 wait_time.go 一致。
package main

import (
	"fmt"
	"sync"
	"time"
)

func main() {
	// ------------------------------------------------------------------
	// 演示 1：一次 Trigger 放行所有 deadline <= 它的等待者
	// 模拟 applied index：3 个消费者分别等 apply 追上 5 / 8 / 12 号。
	// ------------------------------------------------------------------
	fmt.Println("=== demo1: Trigger(N) wakes all waiters with deadline <= N ===")
	tl := NewTimeList()

	var wg sync.WaitGroup
	for _, d := range []uint64{5, 8, 12} {
		wg.Add(1)
		go func(deadline uint64) {
			defer wg.Done()
			<-tl.Wait(deadline) // 阻塞，直到进度追上 deadline
			fmt.Printf("waiter(deadline=%d) released\n", deadline)
		}(d)
	}

	time.Sleep(100 * time.Millisecond) // 等 3 个消费者都登记好

	tl.Trigger(8) // 进度推进到 8：放行 5、8 两个；12 还得继续等
	fmt.Println("-> Trigger(8): waiters 5 and 8 should be released")
	time.Sleep(100 * time.Millisecond)

	tl.Trigger(20) // 进度推进到 20：放行剩下的 12
	fmt.Println("-> Trigger(20): waiter 12 released")
	wg.Wait()

	// ------------------------------------------------------------------
	// 演示 2：等待一个「已经越过」的 deadline —— 立即放行（走全局 closec）
	// ------------------------------------------------------------------
	fmt.Println("\n=== demo2: waiting on an already-passed deadline returns immediately ===")
	// 此时 lastTriggerDeadline = 20
	start := time.Now()
	<-tl.Wait(10) // 10 <= 20，已越过，秒回，不阻塞
	fmt.Printf("Wait(10) returned immediately after %v (lastTrigger=20)\n", time.Since(start).Round(time.Millisecond))
}

// ============================================================================
// timeList: 与 etcd pkg/wait.WaitTime 等价的最小实现（去掉 etcd 依赖，便于独立运行）
// ============================================================================

// closec 是一个「一出生就被关闭」的全局 channel，
// 用于给「等待一个已经越过的 deadline」的调用者做零成本的立即放行。
var closec chan struct{}

func init() { closec = make(chan struct{}); close(closec) }

// timeList 按逻辑时间（单调递增的 deadline）管理一批等待 channel。
type timeList struct {
	l                   sync.Mutex
	lastTriggerDeadline uint64                   // 已触发到的最大 deadline
	m                   map[uint64]chan struct{} // deadline -> 等待 channel
}

func NewTimeList() *timeList {
	return &timeList{m: make(map[uint64]chan struct{})}
}

// Wait 返回一个在给定 deadline 上等待的 channel；
// 当 Trigger 被调用且其 deadline >= 本 deadline 时，该 channel 被关闭（放行）。
func (tl *timeList) Wait(deadline uint64) <-chan struct{} {
	tl.l.Lock()
	defer tl.l.Unlock()
	// 已经越过：直接返回预关闭的全局 channel，立即放行（消除竞态）。
	if tl.lastTriggerDeadline >= deadline {
		return closec
	}
	ch := tl.m[deadline]
	if ch == nil {
		ch = make(chan struct{})
		tl.m[deadline] = ch
	}
	return ch
}

// Trigger 把进度推进到 deadline，并放行所有目标 <= deadline 的等待者。
func (tl *timeList) Trigger(deadline uint64) {
	tl.l.Lock()
	defer tl.l.Unlock()
	tl.lastTriggerDeadline = deadline
	for t, ch := range tl.m {
		if t <= deadline {
			delete(tl.m, t)
			close(ch) // 关闭即放行：等在上面的 goroutine 同时返回
		}
	}
}
