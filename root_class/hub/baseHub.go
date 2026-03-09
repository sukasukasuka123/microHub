package hub

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"

	"google.golang.org/protobuf/proto"

	pb "github.com/sukasukasuka123/microHub/proto/gen/proto"
	registry "github.com/sukasukasuka123/microHub/service_registry"
)

// ════════════════════════════════════════════════════════════
//  task_id 生成
// ════════════════════════════════════════════════════════════

var taskSeq atomic.Int64

func newTaskID(hubName string) string {
	return fmt.Sprintf("%s-%d-%d", hubName, time.Now().UnixNano(), taskSeq.Add(1))
}

// ════════════════════════════════════════════════════════════
//  派发相关公有类型
// ════════════════════════════════════════════════════════════

// DispatchTarget 描述一次派发的目标与请求。
// Stream=true  → 走长连接双向流池（推荐）
// Stream=false → 走 DispatchSimple 短连接（兼容简单场景）
type DispatchTarget struct {
	Addr    string
	Request *pb.ToolRequest
	Stream  bool
}

// DispatchResult 单次派发的聚合结果（对外 API 不变）。
type DispatchResult struct {
	Target    DispatchTarget
	Responses []*pb.ToolResponse
	Err       error
}

// AllOK 当且仅当无派发错误且所有响应均为 "ok" 时返回 true。
func (r DispatchResult) AllOK() bool {
	if r.Err != nil {
		return false
	}
	for _, resp := range r.Responses {
		if resp.Status != "ok" {
			return false
		}
	}
	return true
}

// ════════════════════════════════════════════════════════════
//  HubHandler 接口（对外 API 不变）
// ════════════════════════════════════════════════════════════

// HubHandler 由业务方实现，负责路由策略和结果处理。
type HubHandler interface {
	ServiceName() string
	// Execute 根据请求决定派发目标；req==nil 表示定时触发。
	Execute(req *pb.ToolRequest) ([]DispatchTarget, error)
	// OnResults 在所有目标响应聚合完毕后调用（日志/监控/告警）。
	OnResults(results []DispatchResult)
	// Addrs 返回当前所有 Tool 的地址，供连接池热更新使用。
	Addrs() []string
}

// ════════════════════════════════════════════════════════════
//  poolManager — addr → StreamPool 映射，支持热更新
// ════════════════════════════════════════════════════════════

type poolManager struct {
	mu    sync.RWMutex
	pools map[string]*StreamPool
}

func newPoolManager() *poolManager {
	return &poolManager{pools: make(map[string]*StreamPool)}
}

// rebuild 根据最新地址列表热更新流池（关闭多余的，新建缺少的）。
func (m *poolManager) rebuild(addrs []string) {
	cfg := registry.GetGrpcPoolConfig()

	addrSet := make(map[string]struct{}, len(addrs))
	for _, a := range addrs {
		addrSet[a] = struct{}{}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// 关闭已移除的 Tool 的流池
	for addr, sp := range m.pools {
		if _, keep := addrSet[addr]; !keep {
			sp.Close()
			delete(m.pools, addr)
		}
	}

	// 新建尚不存在的流池
	for _, addr := range addrs {
		if _, exists := m.pools[addr]; exists {
			continue
		}
		sp, err := NewStreamPool(addr, cfg)
		if err != nil {
			log.Printf("[poolManager] 新建流池失败 addr=%s: %v", addr, err)
			continue
		}
		m.pools[addr] = sp
	}
}

func (m *poolManager) get(addr string) *StreamPool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.pools[addr]
}

func (m *poolManager) closeAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, sp := range m.pools {
		sp.Close()
	}
	m.pools = make(map[string]*StreamPool)
}

// ════════════════════════════════════════════════════════════
//  BaseHub
// ════════════════════════════════════════════════════════════

// BaseHub 是框架提供的 Hub 基类。
// 业务方通过实现 HubHandler 接口定制路由和结果处理，
// 无需关心流池管理、task_id 生成、并发安全等细节。
type BaseHub struct {
	pb.UnimplementedHubServiceServer
	handler HubHandler
	pm      *poolManager

	ctx    context.Context
	cancel context.CancelFunc
}

// New 创建 BaseHub 并预热连接池。
func New(handler HubHandler) *BaseHub {
	ctx, cancel := context.WithCancel(context.Background())
	b := &BaseHub{
		handler: handler,
		pm:      newPoolManager(),
		ctx:     ctx,
		cancel:  cancel,
	}
	b.pm.rebuild(handler.Addrs())
	return b
}

// ── Serve ────────────────────────────────────────────────

// Serve 包级快捷函数。
func Serve(addr string, handler HubHandler, loopInterval time.Duration) error {
	return New(handler).serve(addr, loopInterval)
}

// ServeAsync 启动 gRPC 监听（阻塞直到退出）。
func (b *BaseHub) ServeAsync(addr string, loopInterval time.Duration) error {
	return b.serve(addr, loopInterval)
}

func (b *BaseHub) serve(addr string, loopInterval time.Duration) error {
	if loopInterval > 0 {
		go b.timerLoop(loopInterval)
	}
	go b.watchRegistry()

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("[%s] listen: %w", b.handler.ServiceName(), err)
	}
	srv := grpc.NewServer()
	pb.RegisterHubServiceServer(srv, b)
	log.Printf("[%s] gRPC 已启动 %s", b.handler.ServiceName(), addr)

	defer func() {
		b.cancel()
		b.pm.closeAll()
	}()
	return srv.Serve(lis)
}

func (b *BaseHub) watchRegistry() {
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-registry.ChangeCh():
			b.pm.rebuild(b.handler.Addrs())
			log.Printf("[%s] 流池已热更新", b.handler.ServiceName())
		}
	}
}

// ── gRPC 服务端实现（Hub 作为 Server 接受外部调用）────────

// DispatchSimple Hub 作为 gRPC Server 接受单次调用（兼容用）。
func (b *BaseHub) DispatchSimple(ctx context.Context, req *pb.ToolRequest) (*pb.ToolResponse, error) {
	targets, err := b.handler.Execute(req)
	if err != nil {
		return errResp(b.handler.ServiceName(), req.TaskId, "EXECUTE_FAILED", err.Error()), nil
	}
	results := b.dispatchAll(ctx, targets)
	b.handler.OnResults(results)
	return b.aggregate(results), nil
}

// DispatchStream Hub 作为 gRPC Server 接受双向流调用（兼容用）。
func (b *BaseHub) DispatchStream(stream pb.HubService_DispatchStreamServer) error {
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		targets, err := b.handler.Execute(req)
		if err != nil {
			_ = stream.Send(errResp(b.handler.ServiceName(), req.TaskId, "EXECUTE_FAILED", err.Error()))
			continue
		}
		results := b.dispatchAll(stream.Context(), targets)
		b.handler.OnResults(results)
		for _, r := range results {
			if r.Err != nil {
				_ = stream.Send(errResp(b.handler.ServiceName(), req.TaskId, "DISPATCH_ERROR", r.Err.Error()))
				continue
			}
			for _, resp := range r.Responses {
				if err := stream.Send(resp); err != nil {
					return err
				}
			}
		}
	}
}

// ── 公开 API ─────────────────────────────────────────────

// Handler 返回底层 HubHandler。
func (b *BaseHub) Handler() HubHandler { return b.handler }

// Dispatch Hub 主动向 Tool 派发任务（核心 API，对外签名不变）。
//
// 流水线：
//
//	Execute(路由) → dispatchAll(并发发送) → 阻塞等待所有 Tool 响应 → OnResults
func (b *BaseHub) Dispatch(ctx context.Context, req *pb.ToolRequest) []DispatchResult {
	targets, err := b.handler.Execute(req)
	if err != nil {
		log.Printf("[%s] Dispatch Execute err: %v", b.handler.ServiceName(), err)
		return nil
	}
	if len(targets) == 0 {
		method := ""
		if req != nil {
			method = req.GetMethod()
		}
		log.Printf("[%s] Dispatch 无目标 service=%s", b.handler.ServiceName(), method)
		return nil
	}
	results := b.dispatchAll(ctx, targets)
	b.handler.OnResults(results)
	return results
}

// DispatchSimpleCall Hub 主动向 Tool 发简单请求（兼容 Stream=false 场景）。
func (b *BaseHub) DispatchSimpleCall(ctx context.Context, req *pb.ToolRequest) (*pb.ToolResponse, error) {
	targets, err := b.handler.Execute(req)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no target for method=%s", req.GetMethod())
	}
	return b.callSimple(ctx, targets[0].Addr, targets[0].Request)
}

// ── 底层派发 ─────────────────────────────────────────────

// dispatchAll 并发向所有目标发送任务，等待全部完成后返回。
// 每个目标在独立 goroutine 中执行，结果写入预分配的 results 切片。
func (b *BaseHub) dispatchAll(ctx context.Context, targets []DispatchTarget) []DispatchResult {
	results := make([]DispatchResult, len(targets))
	var wg sync.WaitGroup

	for i, t := range targets {
		wg.Add(1)
		go func(idx int, target DispatchTarget) {
			defer wg.Done()

			// 每个 goroutine 独立拷贝 req，避免并发写同一个 protobuf 对象。
			// proto.Clone 深拷贝，不含 sync.Mutex 的已初始化状态，安全。
			req := proto.Clone(target.Request).(*pb.ToolRequest)
			if req.HubName == "" {
				req.HubName = b.handler.ServiceName()
			}
			if req.TaskId == "" {
				req.TaskId = newTaskID(b.handler.ServiceName())
			}

			var resps []*pb.ToolResponse
			var err error

			if target.Stream {
				resps, err = b.callStream(ctx, target.Addr, req)
			} else {
				var resp *pb.ToolResponse
				resp, err = b.callSimple(ctx, target.Addr, req)
				if err == nil && resp != nil {
					resps = []*pb.ToolResponse{resp}
				}
			}
			results[idx] = DispatchResult{Target: target, Responses: resps, Err: err}
		}(i, t)
	}

	wg.Wait()
	return results
}

// callStream 通过流池发送任务，阻塞等待所有响应帧返回。
//
// 流水线（Hub 侧）：
//
//	Send(req) → select { resChan / done / ctx } → 收集所有帧 → return
func (b *BaseHub) callStream(ctx context.Context, addr string, req *pb.ToolRequest) ([]*pb.ToolResponse, error) {
	sp := b.pm.get(addr)
	if sp == nil {
		return nil, fmt.Errorf("no stream pool for addr=%s", addr)
	}

	// 从池中取一条流
	res, err := sp.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("StreamPool.Get addr=%s: %w", addr, err)
	}
	stream := res.Conn

	// 发送任务，注册 pendingTask
	task, err := stream.Send(req)
	if err != nil {
		// 流已损坏：不归还，Pool 的 Reset/Ping 会自动丢弃并补充
		return nil, err
	}

	// 归还流到池（长连接，用完即还，下次继续复用）
	sp.Put(res)

	// 阻塞等待所有响应帧
	var responses []*pb.ToolResponse
	for {
		select {
		case resp := <-task.resChan:
			responses = append(responses, resp)
		case <-task.done:
			// done 关闭后，排空 resChan 中可能剩余的帧
			for {
				select {
				case resp := <-task.resChan:
					responses = append(responses, resp)
				default:
					return responses, nil
				}
			}
		case <-ctx.Done():
			return responses, ctx.Err()
		}
	}
}

// callSimple 建立短连接，走 DispatchSimple（兼容 Stream=false）。
// DispatchSimpleCall 不经过 dispatchAll，需在此补全 hub_name 和 task_id。
func (b *BaseHub) callSimple(ctx context.Context, addr string, req *pb.ToolRequest) (*pb.ToolResponse, error) {
	sp := b.pm.get(addr)
	if sp == nil {
		return nil, fmt.Errorf("no stream pool for addr=%s", addr)
	}
	if req.HubName == "" {
		req.HubName = b.handler.ServiceName()
	}
	if req.TaskId == "" {
		req.TaskId = newTaskID(b.handler.ServiceName())
	}
	// 复用缓存的 client，不重复分配
	return sp.client.DispatchSimple(ctx, req)
}

// ── 结果聚合 ─────────────────────────────────────────────

func (b *BaseHub) aggregate(results []DispatchResult) *pb.ToolResponse {
	var allErrors []*pb.ErrorDetail
	var parts [][]byte

	for _, r := range results {
		if r.Err != nil {
			allErrors = append(allErrors, &pb.ErrorDetail{
				Code:    "DISPATCH_ERROR",
				Message: fmt.Sprintf("[%s] %v", r.Target.Addr, r.Err),
				Field:   "target.addr",
			})
			continue
		}
		for _, resp := range r.Responses {
			allErrors = append(allErrors, resp.Errors...)
			if len(resp.Result) > 0 {
				parts = append(parts, resp.Result)
			}
		}
	}

	status := "ok"
	if len(allErrors) > 0 {
		if len(parts) > 0 {
			status = "partial"
		} else {
			status = "error"
		}
	}

	var result []byte
	switch len(parts) {
	case 0:
		result = []byte("{}")
	case 1:
		result = parts[0]
	default:
		sz := 2
		for _, p := range parts {
			sz += len(p) + 1
		}
		result = make([]byte, 0, sz)
		result = append(result, '[')
		for i, p := range parts {
			if i > 0 {
				result = append(result, ',')
			}
			result = append(result, p...)
		}
		result = append(result, ']')
	}

	return &pb.ToolResponse{
		ToolName: b.handler.ServiceName(),
		Status:   status,
		Result:   result,
		Errors:   allErrors,
	}
}

// ── 定时派发 ─────────────────────────────────────────────

func (b *BaseHub) timerLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-ticker.C:
			targets, err := b.handler.Execute(nil)
			if err != nil || len(targets) == 0 {
				continue
			}
			results := b.dispatchAll(b.ctx, targets)
			b.handler.OnResults(results)
		}
	}
}

// ── 辅助 ─────────────────────────────────────────────────

func errResp(hubName, taskID, code, msg string) *pb.ToolResponse {
	return &pb.ToolResponse{
		ToolName: hubName,
		TaskId:   taskID,
		Status:   "error",
		Result:   []byte("{}"),
		Errors:   []*pb.ErrorDetail{{Code: code, Message: msg}},
	}
}
