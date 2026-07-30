// Package main 演示 etcd pkg/wait 的「提案-等待模型」（Propose + Wait）。
//
// ============================================================================
// 一、pkg/wait 是什么
// ============================================================================
//
// 你发起一个异步操作（提交给 Raft、丢进队列、发个异步 RPC），但调用方希望
// 同步拿到结果。中间隔着「谁来处理、什么时候处理完」的不确定性。
// wait 用一个 ID -> channel 的映射把这两端接起来：
//
//   - 发起方 Register(id) 拿到一个 channel，然后阻塞等在上面；
//   - 处理完成方 Trigger(id, result) 按同一个 id 把结果塞进 channel，唤醒发起方。
//
// 一句话：把「异步的处理过程」包装成「同步的 API 调用」。
//
// ============================================================================
// 二、pkg/wait 的好处
// ============================================================================
//
//  1. 优雅的并发抽象：把异步共识/异步处理包装成同步 API，调用方代码简单直观。
//     etcd 里 v3_server.go 的 Put() 内部是「提交 Raft 提案 -> 等 apply」，
//     但对外表现为一次普通的同步调用。
//
//  2. 精确的一对一唤醒：以请求 ID 为 key，Trigger 只唤醒对应的那个等待者，
//     天然「一对一、恰好一次」。close(ch) 后 map 删除，重复 Trigger 是无操作。
//
//  3. 生产者与消费者时间解耦（缓冲为 1 的核心价值）：
//     Register 创建的是 make(chan any, 1)。Trigger 里 `ch <- x` 后立刻 close。
//     缓冲为 1 保证即使发起方还没来得及 <-ch，Trigger 也不会阻塞、能直接返回。
//     这样一个慢的 / 超时的 / 已离开的发起方，永远无法阻塞负责唤醒的关键路径
//     （etcd 里就是 apply 主循环）。避免「一个客户端的慢拖垮整个状态机」。
//
//  4. 分片锁降低竞争：内部用 64 个 (RWMutex + map) 分片，id % 64 定位分片，
//     高并发注册 / 唤醒时锁竞争小。
//
//  5. close 兜底不泄漏：Trigger 写入后 close(ch)。即使发起方因超时提前离开，
//     值静静躺在缓冲区，channel 无引用后被 GC 回收，不阻塞、不泄漏。
//
// ============================================================================
// 三、使用要点 / 坑
// ============================================================================
//
//   - id 必须唯一：重复 Register 同一 id 会 panic("dup id")。
//   - 必须配超时：处理方若永不 Trigger，等待方会永久阻塞、channel 泄漏在 map 里。
//     etcd 靠 context 超时 + 提案失败时主动 Trigger 兜底。
//   - 只能唤醒一次：Trigger 里 close(ch) 后再 Trigger 同 id 是无操作（map 已删）。
//
// ============================================================================
//
// 说明：为便于独立运行，本文件内置了一个与 etcd pkg/wait 等价的最小实现
// （见文件末尾 minimalWait），逻辑与 go.etcd.io/etcd/pkg/v3/wait 一致。
package main

import (
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// Result 是异步操作的返回结果。
type Result struct {
	Value string
	Err   error
}

// Server 模拟一个「内部异步、对外同步」的服务。
type Server struct {
	w      *minimalWait // 提案-等待器
	reqID  uint64       // 单调递增的请求 ID
	taskCh chan task    // 模拟异步处理队列（etcd 里是 Raft 提案通道）
}

type task struct {
	id   uint64
	data string
}

func NewServer() *Server {
	s := &Server{
		w:      newWait(),
		taskCh: make(chan task, 100),
	}
	go s.worker() // 启动后台处理者
	return s
}

// Do 是一个同步 API：内部是异步的，但调用方感觉是同步的。
func (s *Server) Do(data string) (Result, error) {
	id := atomic.AddUint64(&s.reqID, 1)

	ch := s.w.Register(id)               // ① 注册，拿到等待 channel
	s.taskCh <- task{id: id, data: data} // ② 把任务丢给异步处理者

	select {
	case x := <-ch: // ③ 阻塞等待被唤醒
		return x.(Result), nil
	case <-time.After(3 * time.Second): // 超时兜底，避免永久泄漏
		return Result{}, fmt.Errorf("request %d timeout", id)
	}
}

// worker 是后台处理者：处理完后按 id 唤醒对应的等待者。
func (s *Server) worker() {
	for t := range s.taskCh {
		// 模拟处理耗时
		result := Result{Value: "processed: " + t.data}
		s.w.Trigger(t.id, result) // ④ 用同一个 id 回传结果
	}
}

func main() {
	s := NewServer()

	// 单次调用演示
	r, err := s.Do("hello")
	fmt.Println(r.Value, err) // processed: hello <nil>

	// 并发调用演示：每个请求都能精确拿到自己的结果
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			data := fmt.Sprintf("req-%d", n)
			res, err := s.Do(data)
			if err != nil {
				fmt.Printf("%s error: %v\n", data, err)
				return
			}
			fmt.Printf("%s -> %s\n", data, res.Value)
		}(i)
	}
	wg.Wait()
}

// ============================================================================
// minimalWait: 与 etcd pkg/wait 等价的最小实现（去掉 etcd 依赖，便于独立运行）
// ============================================================================

const defaultListElementLength = 64

type listElement struct {
	l sync.RWMutex
	m map[uint64]chan any
}

// minimalWait 提供按 ID 等待 / 触发事件的能力。
type minimalWait struct {
	e []listElement
}

func newWait() *minimalWait {
	res := &minimalWait{
		e: make([]listElement, defaultListElementLength),
	}
	for i := 0; i < len(res.e); i++ {
		res.e[i].m = make(map[uint64]chan any)
	}
	return res
}

// Register 返回一个在给定 id 上等待的 channel。
// 注意：缓冲为 1 —— 这是「生产者与消费者时间解耦」的关键（见文件头好处 #3）。
func (w *minimalWait) Register(id uint64) <-chan any {
	idx := id % defaultListElementLength
	newCh := make(chan any, 1)
	w.e[idx].l.Lock()
	defer w.e[idx].l.Unlock()
	if _, ok := w.e[idx].m[id]; !ok {
		w.e[idx].m[id] = newCh
	} else {
		log.Panicf("dup id %x", id)
	}
	return newCh
}

// Trigger 用给定 id 唤醒等待的 channel，并传入结果 x。
func (w *minimalWait) Trigger(id uint64, x any) {
	idx := id % defaultListElementLength
	w.e[idx].l.Lock()
	ch := w.e[idx].m[id]
	delete(w.e[idx].m, id)
	w.e[idx].l.Unlock()
	if ch != nil {
		ch <- x // 缓冲为 1：即使还没人接收也不阻塞，立即返回
		close(ch)
	}
}

// IsRegistered 报告给定 id 是否仍在等待。
func (w *minimalWait) IsRegistered(id uint64) bool {
	idx := id % defaultListElementLength
	w.e[idx].l.RLock()
	defer w.e[idx].l.RUnlock()
	_, ok := w.e[idx].m[id]
	return ok
}
