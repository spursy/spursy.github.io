// Package main 演示 etcd pkg/notify 的「一次性广播通知」（one-shot broadcast）。
//
// ============================================================================
// 一、pkg/notify 是什么
// ============================================================================
//
// wait 是一对一（一个 id 对一个等待者）。notify 是一对多广播：
// 一个事件发生，同时唤醒所有正在等的 goroutine，且不关心它们是谁、有几个。
//
// 核心机制：利用 Go「close(ch) 会让所有 <-ch 立即返回」这一特性。
//   - Receive() 返回当前 channel，消费者 <-ch 等在上面；
//   - Notify()  先创建新 channel 替换旧的，再 close 旧 channel
//     -> 所有等在旧 channel 上的 goroutine 被同时唤醒；
//        下一轮等待者拿到的是新 channel（因此可重复广播）。
//
// 一句话：关闭一个 channel 天然就是「广播」，用完即换新的实现可重复广播。
//
// ============================================================================
// 二、pkg/notify 的好处
// ============================================================================
//
//  1. 一对多广播：一次 Notify() 同时唤醒所有等待者，无需知道有几个消费者。
//     比手动给每个消费者发消息、或用 sync.Cond 都更简单。
//
//  2. 可重复通知：Notify() 每次换一个新 channel，因此可以一轮又一轮地广播
//     （如周期性放行、状态反复变更）。
//
//  3. 零内存信号：用 chan struct{}，只传「事件发生了」这个信号本身，不占内存。
//     context.Done() 底层就是同款思路。
//
//  4. 线程安全：内部用 RWMutex 保护 channel 的读取与替换，Receive/Notify
//     可被多个 goroutine 并发调用。
//
// etcd 里的用途：线性一致读中，多个并发读请求都调 LinearizableReadNotify()
// 等在同一个 notifier 上；一旦 ReadIndex 确认完成，一次 Notify() 放行所有等待的读。
//
// ============================================================================
// 三、使用要点 / 坑
// ============================================================================
//
//   - 消费者每轮必须重新调 Receive()：Notify 换了新 channel，
//     还拿着旧 channel 的会错过下一次。
//   - 不携带数据：只传信号，要传数据得另配共享变量（etcd 配一个受锁保护的状态）。
//   - 边沿触发而非电平触发：Notify 时若没人在 Receive，这次广播就「错过」了。
//     适合「周期性放行」而非「必达消息」。
//
// ============================================================================
//
// 说明：为便于独立运行，本文件内置了一个与 etcd pkg/notify 等价的最小实现
// （见文件末尾 Notifier），逻辑与 go.etcd.io/etcd/pkg/v3/notify 一致。
package main

import (
	"fmt"
	"sync"
	"time"
)

func main() {
	// ------------------------------------------------------------------
	// 演示 1：一次广播唤醒多个消费者
	// ------------------------------------------------------------------
	fmt.Println("=== demo1: one Notify wakes all consumers ===")
	n := NewNotifier()

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-n.Receive() // 阻塞，直到被广播唤醒
			fmt.Printf("consumer %d awakened\n", id)
		}(i)
	}

	time.Sleep(100 * time.Millisecond) // 等消费者都进入等待
	n.Notify()                         // 一次调用，3 个消费者同时醒来
	wg.Wait()

	// ------------------------------------------------------------------
	// 演示 2：多轮周期性广播（每轮消费者重新 Receive）
	// ------------------------------------------------------------------
	fmt.Println("\n=== demo2: repeated broadcast (tick 3 times) ===")
	n2 := NewNotifier()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 3; i++ {
			<-n2.Receive() // 每轮都要重新 Receive 拿新 channel
			fmt.Printf("tick %d\n", i+1)
		}
		close(done)
	}()

	for i := 0; i < 3; i++ {
		time.Sleep(200 * time.Millisecond)
		n2.Notify() // 每 200ms 广播一次
	}
	<-done
	fmt.Println("done")
}

// ============================================================================
// Notifier: 与 etcd pkg/notify 等价的最小实现（去掉 etcd 依赖，便于独立运行）
// ============================================================================

// Notifier 是一个线程安全的结构，用于向多个消费者广播某个事件的发生。
type Notifier struct {
	mu      sync.RWMutex
	channel chan struct{}
}

// NewNotifier 返回一个新的 Notifier。
func NewNotifier() *Notifier {
	return &Notifier{
		channel: make(chan struct{}),
	}
}

// Receive 返回一个可用于等待通知的 channel。
// 消费者通过「channel 被关闭」这一事件得到通知。
func (n *Notifier) Receive() <-chan struct{} {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.channel
}

// Notify 关闭当前传给消费者的 channel（广播唤醒所有等待者），
// 并创建一个新 channel 供下一次通知使用。
func (n *Notifier) Notify() {
	newChannel := make(chan struct{})
	n.mu.Lock()
	channelToClose := n.channel
	n.channel = newChannel
	n.mu.Unlock()
	close(channelToClose)
}
