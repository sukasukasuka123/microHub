package hub

import (
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"

	pb "github.com/sukasukasuka123/microHub/proto/gen/proto"
)

// ════════════════════════════════════════════════════════════
//  pendingTask — 追踪单次派发任务的响应
// ════════════════════════════════════════════════════════════

// pendingTask 代表一次 Hub→Tool 派发，等待 Tool 的所有响应帧。
//
// 生命周期：
//  1. Hub 调用 SingleStream.Send() 时创建并注册到 pending map
//  2. recvLoop 收到非 "partial" 帧时调用 finish()，关闭 done
//  3. Hub 侧 Dispatch 通过 select{resChan / done / ctx} 消费响应
type pendingTask struct {
	// resChan 接收 Tool 返回的每一帧响应（buffered，容量足够大避免 recvLoop 阻塞）
	resChan chan *pb.ToolResponse
	// done 在任务完成（最后一帧到达或流断开）时被关闭
	done chan struct{}
	once sync.Once
}

func newPendingTask(bufSize int) *pendingTask {
	return &pendingTask{
		resChan: make(chan *pb.ToolResponse, bufSize),
		done:    make(chan struct{}),
	}
}

// finish 标记任务结束（幂等，可被多方调用）。
func (t *pendingTask) finish() {
	t.once.Do(func() { close(t.done) })
}

// ════════════════════════════════════════════════════════════
//  SingleStream — 单条双向流
// ════════════════════════════════════════════════════════════

// defaultRespBufSize 是每个 pendingTask.resChan 的缓冲大小。
// 设为 8 而非 64：绝大多数任务帧数 ≤ 8，过大的缓冲浪费内存且对 GC 有压力。
// 若业务方单次返回超过 8 帧，recvLoop 会短暂阻塞等待消费方排空，属于预期行为。
const defaultRespBufSize = 8

// SingleStream 封装一条 Hub→Tool 的 gRPC 双向流。
//
// 并发模型：
//   - 由 streamPool 持有，多个 goroutine 可并发调用 Send()
//   - recvLoop 独占 Recv()，在构造时自动启动
//   - 流关闭后 closed 置为 true，streamPool 负责清理和补充
type SingleStream struct {
	stream pb.HubService_DispatchStreamClient

	// pending：task_id → *pendingTask
	// 使用 sync.Map 避免 recvLoop 与 Send() 之间的锁竞争
	pending sync.Map

	// closed 流是否已关闭（recvLoop 退出后置 true）
	closed atomic.Bool

	// addr 仅用于日志
	addr string

	// onClose 流关闭时的回调（由 streamPool 注入，用于自我清理）
	onClose func(s *SingleStream)
}

// NewSingleStream 构造并立即启动 recvLoop。
// onClose 在流断开时被调用（可传 nil）。
func NewSingleStream(
	stream pb.HubService_DispatchStreamClient,
	addr string,
	onClose func(s *SingleStream),
) *SingleStream {
	s := &SingleStream{
		stream:  stream,
		addr:    addr,
		onClose: onClose,
	}
	go s.recvLoop()
	return s
}

// Send 向 Tool 发送一个任务请求，返回用于等待响应的 pendingTask。
//
// 调用方（Hub Dispatch）持有 task 后，通过以下方式等待结果：
//
//	select {
//	case resp := <-task.resChan:  // 单帧响应
//	case <-task.done:             // 任务完成
//	case <-ctx.Done():            // 超时/取消
//	}
func (s *SingleStream) Send(req *pb.ToolRequest) (*pendingTask, error) {
	// 先注册 pending，再检查 closed，再发送。
	// 顺序保证：即使 recvLoop 在 Send() 期间退出，handleClose 也能
	// 扫到这条 pending 并调用 finish()，不会造成调用方永久阻塞。
	task := newPendingTask(defaultRespBufSize)
	s.pending.Store(req.TaskId, task)

	if s.closed.Load() {
		s.pending.Delete(req.TaskId)
		return nil, fmt.Errorf("stream closed addr=%s", s.addr)
	}

	if err := s.stream.Send(req); err != nil {
		s.pending.Delete(req.TaskId)
		return nil, fmt.Errorf("stream.Send addr=%s: %w", s.addr, err)
	}
	return task, nil
}

// IsClosed 返回流是否已关闭。
func (s *SingleStream) IsClosed() bool {
	return s.closed.Load()
}

// Close 主动关闭流（Hub 侧发 EOF）。
func (s *SingleStream) Close() {
	_ = s.stream.CloseSend()
}

// ── recvLoop ─────────────────────────────────────────────

// recvLoop 持续接收 Tool 的响应，按 task_id 路由到对应 pendingTask。
// 本函数独占 stream.Recv()，在独立 goroutine 中运行，直到流关闭。
func (s *SingleStream) recvLoop() {
	defer s.handleClose()

	for {
		resp, err := s.stream.Recv()
		if err != nil {
			if err != io.EOF {
				log.Printf("[SingleStream] addr=%s recvLoop err: %v", s.addr, err)
			}
			return
		}
		s.dispatch(resp)
	}
}

// dispatch 将响应路由到对应的 pendingTask。
// 由 recvLoop 独占调用，单 goroutine，无需额外同步。
func (s *SingleStream) dispatch(resp *pb.ToolResponse) {
	// task_id 为空：Tool 主动推送，无需匹配
	if resp.TaskId == "" {
		log.Printf("[SingleStream] addr=%s 收到主动推送 tool_name=%s", s.addr, resp.ToolName)
		return
	}

	v, ok := s.pending.Load(resp.TaskId)
	if !ok {
		log.Printf("[SingleStream] addr=%s 未知 task_id=%s，忽略", s.addr, resp.TaskId)
		return
	}
	task := v.(*pendingTask)

	isLast := resp.Status != "partial"

	// 非 "partial" 帧：先从 pending 摘除，再写入 resChan，最后 finish()。
	// 顺序保证：调用方在 done 关闭后排空 resChan 时，数据已经在 chan 里了。
	if isLast {
		s.pending.Delete(resp.TaskId)
	}

	// 写入 resChan：此处阻塞而非丢弃。
	// resChan 缓冲（defaultRespBufSize）足够容纳单次任务的所有帧；
	// 若业务方返回超大量帧导致 chan 满，宁可让 recvLoop 在此阻塞，
	// 也不能静默丢帧后误报任务完成。
	task.resChan <- resp

	if isLast {
		task.finish()
	}
}

// handleClose 流断开时清理所有 pending 任务，通知等待方。
func (s *SingleStream) handleClose() {
	s.closed.Store(true)

	// 通知所有还在等待的任务：流已断开
	s.pending.Range(func(k, v any) bool {
		task := v.(*pendingTask)
		task.finish() // 关闭 done，让 Dispatch 侧 select 解除阻塞
		s.pending.Delete(k)
		return true
	})

	if s.onClose != nil {
		s.onClose(s)
	}
	log.Printf("[SingleStream] addr=%s 流已关闭", s.addr)
}
