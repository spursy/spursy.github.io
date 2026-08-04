// Package main 演示 etcd pkg/schedule 的「FIFO 串行调度器」。
//
// ============================================================================
// 一、pkg/schedule 是什么
// ============================================================================
//
// 一个单 goroutine、串行、按提交顺序（FIFO）执行任务的调度器。核心承诺：
// 所有提交的 Job 严格按提交先后顺序、一个接一个地串行执行，永远不会并发。
//
// 注意：名字里的「批量」指内部用一个 slice 把待办任务攒成一批队列逐个消费，
// 而不是并发批处理。它的价值是「把来自多方的并发提交，收敛成串行执行」。
//
// ============================================================================
// 二、设计原理（三个核心机制）
// ============================================================================
//
//  1. 单消费者 goroutine 保证串行：
//     只有一个后台 run() goroutine 会执行 Job，任务之间天然串行，
//     Job 内部无需担心并发安全。
//
//  2. resume channel 做「空闲<->忙碌」唤醒（避免忙等 & 不阻塞生产者）：
//     - 队列空时消费者 <-resume 睡眠，不空转浪费 CPU；
//     - 生产者只在「队列从空变非空」时才发信号（边沿触发），避免重复通知；
//     - resume 缓冲为 1 + select/default 非阻塞发送，生产者绝不被阻塞。
//
//  3. sync.Cond 做完成等待（WaitFinish）：
//     等待的是复合数值条件「完成数 >= n 且队列已空」，
//     Cond 的「等待-广播-重新判断谓词」模型正好契合。
//
// ============================================================================
// 三、几个健壮性设计
// ============================================================================
//
//   - 执行完才出队（pendings[1:] 放在 defer 里）：保证正在执行的任务仍算 pending，
//     WaitFinish 的 len(pendings)!=0 判断才准确，不会把在执行的任务漏算成已完成。
//   - recover 兜底：单个 Job panic 不会打挂整个调度 goroutine。
//   - 优雅停止：Stop 触发 ctx.Done，run() 清理剩余 pending 任务后 close(donec)，
//     Stop 阻塞等 run 真正退出；cancel 置 nil 后再 Schedule 会 panic（防止向已停调度器提交）。
//
// ============================================================================
// 四、在 etcd 里的用途
// ============================================================================
//
// 主要用于 MVCC 历史版本压缩（compaction）：把多次压缩请求排队串行执行，
// 避免并发压缩相互干扰，同时不阻塞提交方。
//
// ============================================================================
//
// 说明：为便于独立运行，本文件内置了一个与 etcd pkg/schedule 等价的最小实现
// （去掉 zap/verify 依赖），逻辑与 go.etcd.io/etcd/pkg/v3/schedule 一致。
package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

func main() {
	s := NewFIFOScheduler()

	// ------------------------------------------------------------------
	// 演示 1：并发提交，但严格按提交顺序串行执行
	// ------------------------------------------------------------------
	fmt.Println("=== demo1: concurrent submit, FIFO serial execution ===")
	var order []int
	var mu sync.Mutex

	for i := 0; i < 5; i++ {
		n := i
		s.Schedule(NewJob(fmt.Sprintf("job-%d", n), func(ctx context.Context) {
			// 即使每个任务耗时不同，执行也不会重叠（串行）
			time.Sleep(time.Duration(50-n*5) * time.Millisecond)
			mu.Lock()
			order = append(order, n)
			mu.Unlock()
			fmt.Printf("executed job-%d\n", n)
		}))
	}

	// 等待至少 5 个任务完成且队列清空
	s.WaitFinish(5)
	fmt.Printf("execution order = %v (== submit order, serial)\n", order)
	fmt.Printf("pending=%d scheduled=%d finished=%d\n\n",
		s.Pending(), s.Scheduled(), s.Finished())

	// ------------------------------------------------------------------
	// 演示 2：优雅停止会清理掉未执行的 pending 任务
	// ------------------------------------------------------------------
	fmt.Println("=== demo2: Stop drains pending jobs ===")
	s2 := NewFIFOScheduler()
	done := make(chan struct{})
	// 第一个任务故意阻塞一会儿，让后面的任务堆积在队列里
	s2.Schedule(NewJob("slow", func(ctx context.Context) {
		<-done // 阻塞直到我们放行
		fmt.Println("slow job done")
	}))
	s2.Schedule(NewJob("queued-1", func(ctx context.Context) {
		fmt.Println("queued-1 executed (drained on stop)")
	}))
	s2.Schedule(NewJob("queued-2", func(ctx context.Context) {
		fmt.Println("queued-2 executed (drained on stop)")
	}))

	time.Sleep(20 * time.Millisecond)
	fmt.Printf("before stop: pending=%d\n", s2.Pending())
	close(done) // 放行 slow job
	s2.Stop()   // 停止：会执行完当前并清理剩余 pending
	fmt.Println("scheduler stopped")
}

// ============================================================================
// 以下为与 etcd pkg/schedule 等价的最小实现（去掉外部依赖，便于独立运行）
// ============================================================================

// Job 是被调度执行的任务。
type Job interface {
	Name() string
	Do(context.Context)
}

type job struct {
	name string
	do   func(context.Context)
}

func (j job) Name() string           { return j.name }
func (j job) Do(ctx context.Context) { j.do(ctx) }

// NewJob 构造一个 Job。
func NewJob(name string, do func(ctx context.Context)) Job {
	return job{name: name, do: do}
}

// Scheduler 可以调度任务。
type Scheduler interface {
	Schedule(j Job)   // 提交任务（FIFO 顺序执行）
	Pending() int     // 待办任务数（含正在执行的）
	Scheduled() int   // 已开始调度的任务数
	Finished() int    // 已完成任务数
	WaitFinish(n int) // 等待至少 n 个完成且队列清空
	Stop()            // 停止调度器并清理剩余任务
}

type fifo struct {
	mu sync.Mutex

	resume    chan struct{}
	scheduled int
	finished  int
	pendings  []Job

	ctx    context.Context
	cancel context.CancelFunc

	finishCond *sync.Cond
	donec      chan struct{}
}

// NewFIFOScheduler 返回一个按 FIFO 顺序串行执行任务的调度器。
func NewFIFOScheduler() Scheduler {
	f := &fifo{
		resume: make(chan struct{}, 1),
		donec:  make(chan struct{}, 1),
	}
	f.finishCond = sync.NewCond(&f.mu)
	f.ctx, f.cancel = context.WithCancel(context.Background())
	go f.run()
	return f
}

// Schedule 提交一个将按 FIFO 顺序串行执行的任务。
func (f *fifo) Schedule(j Job) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.cancel == nil {
		panic("schedule: schedule to stopped scheduler")
	}

	// 仅在「队列从空变非空」时才发唤醒信号（边沿触发）。
	if len(f.pendings) == 0 {
		select {
		case f.resume <- struct{}{}:
		default: // 已有信号在缓冲里，不重复发，且不阻塞生产者
		}
	}
	f.pendings = append(f.pendings, j)
}

func (f *fifo) Pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pendings)
}

func (f *fifo) Scheduled() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scheduled
}

func (f *fifo) Finished() int {
	f.finishCond.L.Lock()
	defer f.finishCond.L.Unlock()
	return f.finished
}

// WaitFinish 等待至少 n 个任务完成，且所有 pending 任务都已完成。
func (f *fifo) WaitFinish(n int) {
	f.finishCond.L.Lock()
	for f.finished < n || len(f.pendings) != 0 {
		f.finishCond.Wait()
	}
	f.finishCond.L.Unlock()
}

// Stop 停止调度器并取消所有待办任务。
func (f *fifo) Stop() {
	f.mu.Lock()
	f.cancel()
	f.cancel = nil // 之后再 Schedule 会 panic
	f.mu.Unlock()
	<-f.donec // 等 run() 真正退出
}

func (f *fifo) run() {
	defer func() {
		close(f.donec)
		close(f.resume)
	}()

	for {
		var todo Job
		f.mu.Lock()
		if len(f.pendings) != 0 {
			f.scheduled++
			todo = f.pendings[0] // 取队头
		}
		f.mu.Unlock()

		if todo == nil {
			select {
			case <-f.resume: // 队列空 → 睡眠等唤醒
			case <-f.ctx.Done(): // 停止 → 清理剩余 pending 后退出
				f.mu.Lock()
				pendings := f.pendings
				f.pendings = nil
				f.mu.Unlock()
				for _, todo := range pendings {
					f.executeJob(todo, true)
				}
				return
			}
		} else {
			f.executeJob(todo, false)
		}
	}
}

func (f *fifo) executeJob(todo Job, updatedFinishedStats bool) {
	defer func() {
		if !updatedFinishedStats {
			f.finishCond.L.Lock()
			f.finished++
			f.pendings = f.pendings[1:] // 执行完才出队
			f.finishCond.Broadcast()
			f.finishCond.L.Unlock()
		}
		if err := recover(); err != nil {
			// 单个 Job panic 兜底：不打挂调度 goroutine
			fmt.Printf("execute job %q failed: %v\n", todo.Name(), err)
		}
	}()

	todo.Do(f.ctx)
}
