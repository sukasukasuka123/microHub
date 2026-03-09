package tool

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sync"

	"google.golang.org/grpc"

	pb "github.com/sukasukasuka123/microHub/proto/gen/proto"
)

// ════════════════════════════════════════════════════════════
//  ToolHandler 接口
// ════════════════════════════════════════════════════════════

// ToolHandler 由业务方实现。
//
// Execute 返回 <-chan *pb.ToolResponse：
//   - 业务方在独立 goroutine 中生产响应帧，写入 chan
//   - 关闭 chan 表示本次任务的所有帧已发完
//   - 出错时可向 chan 写入 status="error" 的帧后关闭，
//     也可直接 return nil, err（框架自动包装为错误帧）
//
// 示例：
//
//	func (h *HelloHandler) Execute(req *pb.ToolRequest) (<-chan *pb.ToolResponse, error) {
//	    ch := make(chan *pb.ToolResponse, 8)
//	    go func() {
//	        defer close(ch)
//	        for i := 1; i <= 3; i++ {
//	            ch <- pb_api.MustOKResp("hello", req.TaskId, MyData{Idx: i})
//	        }
//	    }()
//	    return ch, nil
//	}
type ToolHandler interface {
	ServiceName() string
	Execute(req *pb.ToolRequest) (<-chan *pb.ToolResponse, error)
}

// ════════════════════════════════════════════════════════════
//  ActiveSender — Tool 主动推送能力（可选）
// ════════════════════════════════════════════════════════════

// ActiveSender 允许 Tool 在任意时刻向 Hub 主动推送消息。
// task_id 为空时，Hub 识别为主动推送帧（非任务响应）。
type ActiveSender interface {
	Send(resp *pb.ToolResponse) error
}

// ActiveHandler 可选接口：
// 若 ToolHandler 同时实现此接口，BaseTool 在每条流建立时注入 ActiveSender。
//
// 示例：
//
//	type MyHandler struct{ sender tool.ActiveSender }
//	func (h *MyHandler) OnRegister(s tool.ActiveSender) { h.sender = s }
//	// 任意时刻：h.sender.Send(&pb.ToolResponse{...})
type ActiveHandler interface {
	OnRegister(sender ActiveSender)
}

// ════════════════════════════════════════════════════════════
//  streamSession — 单条流的三级流水线
//
//  Recv goroutine
//    → taskCh
//      → Execute goroutine (per task, launched by executeLoop)
//          → respCh
//            → Send goroutine
// ════════════════════════════════════════════════════════════

type task struct {
	req *pb.ToolRequest
}

// streamSession 管理单条双向流的完整生命周期。
// 每条流对应一个 streamSession 实例。
type streamSession struct {
	stream  pb.HubService_DispatchStreamServer
	handler ToolHandler
	toolID  string

	// taskCh：Recv goroutine → executeLoop
	// 缓冲足够大，确保 Recv 不因 executeLoop 繁忙而阻塞
	taskCh chan task

	// respCh：executeOne goroutine → Send goroutine
	// 缓冲足够大，确保 executeOne 不因 Send 繁忙而阻塞
	respCh chan *pb.ToolResponse

	// senderClosed 在 respCh 关闭前被关闭，
	// 供 activeSender 用来感知 session 即将结束，避免向已关闭 chan 写入。
	senderClosed chan struct{}

	// taskWg 追踪活跃的 executeOne goroutine 数量
	// executeLoop 退出时等待所有 executeOne 完成，再关闭 respCh
	taskWg sync.WaitGroup

	// wg 追踪 executeLoop 和 sendLoop 两个后台 goroutine
	wg sync.WaitGroup

	ctx    context.Context
	cancel context.CancelFunc
}

func newStreamSession(
	stream pb.HubService_DispatchStreamServer,
	handler ToolHandler,
) *streamSession {
	ctx, cancel := context.WithCancel(stream.Context())
	return &streamSession{
		stream:       stream,
		handler:      handler,
		toolID:       handler.ServiceName(),
		taskCh:       make(chan task, 128),
		respCh:       make(chan *pb.ToolResponse, 256),
		senderClosed: make(chan struct{}),
		ctx:          ctx,
		cancel:       cancel,
	}
}

// run 启动流水线，阻塞直到流关闭。
// 退出时保证 executeLoop 和 sendLoop 均已退出。
func (s *streamSession) run() error {
	s.wg.Add(2)
	go s.executeLoop()
	go s.sendLoop()

	// recvLoop 在调用方 goroutine 中运行（即 DispatchStream 的 goroutine）
	err := s.recvLoop()

	// Recv 结束 → 关闭 taskCh，通知 executeLoop 没有更多任务
	close(s.taskCh)

	// 等待 executeLoop 和 sendLoop 退出
	s.wg.Wait()
	s.cancel()
	return err
}

// ── Recv goroutine ───────────────────────────────────────

// recvLoop 持续接收来自 Hub 的请求，写入 taskCh。
// 不执行任何业务逻辑，单次循环耗时极短。
func (s *streamSession) recvLoop() error {
	for {
		req, err := s.stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			s.cancel()
			return err
		}

		select {
		case s.taskCh <- task{req: req}:
		case <-s.ctx.Done():
			return s.ctx.Err()
		}
	}
}

// ── Execute goroutine ────────────────────────────────────

// executeLoop 从 taskCh 消费任务，为每个任务启动独立 goroutine。
// 本身不阻塞（除非 ctx 取消），确保 recvLoop 的提交速度不受 Execute 耗时影响。
func (s *streamSession) executeLoop() {
	defer func() {
		// 等待所有 executeOne goroutine 写完 respCh 后，
		// 先关闭 senderClosed（通知 activeSender 不再接受新消息），
		// 再关闭 respCh（通知 sendLoop 退出）。
		// 顺序严格保证：activeSender.Send() 检测到 senderClosed 后不再写入，
		// 此后关闭 respCh 才是安全的。
		s.taskWg.Wait()
		close(s.senderClosed)
		close(s.respCh)
		s.wg.Done()
	}()

	for t := range s.taskCh {
		req := t.req
		s.taskWg.Add(1)
		go func() {
			defer s.taskWg.Done()
			s.executeOne(req)
		}()
	}
}

// executeOne 执行单个任务，将所有响应帧写入 respCh。
// 在独立 goroutine 中运行，与其他任务完全并发。
func (s *streamSession) executeOne(req *pb.ToolRequest) {
	respChan, err := s.handler.Execute(req)
	if err != nil {
		// 同步错误（参数校验等）：直接写错误帧
		select {
		case s.respCh <- buildErrResp(s.toolID, req.TaskId, "EXECUTE_FAILED", err.Error()):
		case <-s.ctx.Done():
		}
		return
	}

	// 消费业务方的响应 chan，转发到 sendLoop
	for resp := range respChan {
		if resp == nil {
			continue
		}
		s.fillRespMeta(resp, req.TaskId)
		select {
		case s.respCh <- resp:
		case <-s.ctx.Done():
			return
		}
	}
}

// fillRespMeta 补全响应帧的 tool_name / task_id / status 字段。
func (s *streamSession) fillRespMeta(resp *pb.ToolResponse, taskID string) {
	if resp.ToolName == "" {
		resp.ToolName = s.toolID
	}
	if resp.TaskId == "" {
		resp.TaskId = taskID
	}
	if resp.Status == "" {
		resp.Status = "ok"
	}
}

// ── Send goroutine ───────────────────────────────────────

// sendLoop 从 respCh 消费响应帧，逐一发送到 Hub。
// 不执行任何业务逻辑，单次循环耗时极短。
func (s *streamSession) sendLoop() {
	defer s.wg.Done()

	for resp := range s.respCh {
		if err := s.stream.Send(resp); err != nil {
			log.Printf("[%s] sendLoop err task_id=%s: %v", s.toolID, resp.TaskId, err)
			s.cancel()
			return
		}
	}
}

// ════════════════════════════════════════════════════════════
//  activeSender 实现
// ════════════════════════════════════════════════════════════

type activeSender struct {
	respCh chan<- *pb.ToolResponse
	toolID string
	// closed 在 session 关闭 respCh 之前被关闭，
	// 确保 Send() 不会向已关闭的 chan 写入（panic）。
	closed <-chan struct{}
}

func (a *activeSender) Send(resp *pb.ToolResponse) error {
	if resp.ToolName == "" {
		resp.ToolName = a.toolID
	}

	// 先检查 closed，再尝试写入。
	// 不能只靠 select：Go 在两个 case 同时就绪时随机选择，
	// 若 respCh 已关闭而 closed 也已关闭，select 可能选到
	// "case respCh <- resp" 并 panic，而不是走 closed 分支。
	select {
	case <-a.closed:
		return fmt.Errorf("activeSender.Send: session closed")
	default:
	}

	// respCh 未关闭，执行实际写入。
	// 用 recover 兜底极窄竞态窗口：closed 检查通过后、写入前，
	// 若 executeLoop 恰好关闭了 respCh，panic 会被捕获为 error。
	return a.safeSend(resp)
}

func (a *activeSender) safeSend(resp *pb.ToolResponse) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("activeSender.Send: session closed (recovered)")
		}
	}()
	select {
	case a.respCh <- resp:
		return nil
	case <-a.closed:
		return fmt.Errorf("activeSender.Send: session closed")
	}
}

// ════════════════════════════════════════════════════════════
//  BaseTool
// ════════════════════════════════════════════════════════════

// BaseTool 封装 gRPC 服务端，向业务方暴露简洁的 ToolHandler 接口。
type BaseTool struct {
	pb.UnimplementedHubServiceServer
	handler ToolHandler
}

// New 构造 BaseTool。
func New(handler ToolHandler) *BaseTool {
	return &BaseTool{handler: handler}
}

// DispatchStream 实现 gRPC 双向流接口。
// 内部启动三级流水线（recv/execute/send），本函数在流关闭前阻塞。
func (b *BaseTool) DispatchStream(stream pb.HubService_DispatchStreamServer) error {
	session := newStreamSession(stream, b.handler)

	// 注入 ActiveSender（Handler 实现 ActiveHandler 时生效）
	if ah, ok := b.handler.(ActiveHandler); ok {
		sender := &activeSender{
			respCh: session.respCh,
			toolID: b.handler.ServiceName(),
			closed: session.senderClosed,
		}
		ah.OnRegister(sender)
	}

	return session.run()
}

// DispatchSimple 兼容单次调用（Stream=false 场景）。
// 阻塞直到 Execute 返回的 chan 关闭，取第一帧返回。
func (b *BaseTool) DispatchSimple(_ context.Context, req *pb.ToolRequest) (*pb.ToolResponse, error) {
	toolID := b.handler.ServiceName()

	respChan, err := b.handler.Execute(req)
	if err != nil {
		return buildErrResp(toolID, req.TaskId, "EXECUTE_FAILED", err.Error()), nil
	}

	var resps []*pb.ToolResponse
	for resp := range respChan {
		if resp == nil {
			continue
		}
		if resp.ToolName == "" {
			resp.ToolName = toolID
		}
		if resp.TaskId == "" {
			resp.TaskId = req.TaskId
		}
		if resp.Status == "" {
			resp.Status = "ok"
		}
		resps = append(resps, resp)
	}

	if len(resps) == 0 {
		return buildErrResp(toolID, req.TaskId, "EMPTY_RESPONSE", "Execute returned no response"), nil
	}
	if len(resps) > 1 {
		log.Printf("[%s] DispatchSimple: Execute 返回 %d 帧，仅取第一帧；多帧请用 DispatchStream",
			toolID, len(resps))
	}
	return resps[0], nil
}

// Serve 启动 gRPC 服务（阻塞直到退出）。
func (b *BaseTool) Serve(addr string) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("[%s] listen: %w", b.handler.ServiceName(), err)
	}
	srv := grpc.NewServer()
	pb.RegisterHubServiceServer(srv, b)
	log.Printf("[%s] gRPC 已启动 %s", b.handler.ServiceName(), addr)
	return srv.Serve(lis)
}

// ── 辅助 ─────────────────────────────────────────────────

func buildErrResp(toolName, taskID, code, msg string) *pb.ToolResponse {
	return &pb.ToolResponse{
		ToolName: toolName,
		TaskId:   taskID,
		Status:   "error",
		Result:   []byte("{}"),
		Errors:   []*pb.ErrorDetail{{Code: code, Message: msg}},
	}
}
