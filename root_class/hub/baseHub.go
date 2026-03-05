package hub

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	pool "github.com/sukasukasuka123/TemplatePoolByGO"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/sukasukasuka123/microHub/proto/gen/proto"
	registry "github.com/sukasukasuka123/microHub/service_registry"
)

// ── gRPC 连接控制器：实现 ResourceControl[*grpc.ClientConn] ──

type grpcConnControl struct {
	addr string
}

func (g *grpcConnControl) Create() (*grpc.ClientConn, error) {
	return grpc.NewClient(g.addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
}

func (g *grpcConnControl) Reset(conn *grpc.ClientConn) error {
	if conn.GetState() == connectivity.Shutdown {
		return fmt.Errorf("grpc conn shutdown, addr=%s", g.addr)
	}
	return nil
}

func (g *grpcConnControl) Close(conn *grpc.ClientConn) error {
	return conn.Close()
}

func (g *grpcConnControl) Ping(conn *grpc.ClientConn) error {
	s := conn.GetState()
	if s == connectivity.Shutdown || s == connectivity.TransientFailure {
		return fmt.Errorf("unhealthy state=%s addr=%s", s, g.addr)
	}
	return nil
}

// ── connPoolManager：addr → Pool 映射 ────────────────────

type connPoolManager struct {
	mu    sync.RWMutex
	pools map[string]*pool.Pool[*grpc.ClientConn]
}

func newConnPoolManager() *connPoolManager {
	return &connPoolManager{
		pools: make(map[string]*pool.Pool[*grpc.ClientConn]),
	}
}

func (m *connPoolManager) rebuild(addrs []string) {
	addrSet := make(map[string]struct{}, len(addrs))
	for _, a := range addrs {
		addrSet[a] = struct{}{}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	cfg := registry.GetGrpcPoolConfig()

	for addr, p := range m.pools {
		if _, keep := addrSet[addr]; !keep {
			p.Close()
			delete(m.pools, addr)
			log.Printf("[ConnPool] 热更新：关闭连接池 addr=%s", addr)
		}
	}

	for _, addr := range addrs {
		if _, exists := m.pools[addr]; exists {
			continue
		}
		p := pool.NewPool(pool.PoolConfig{
			MinSize:          cfg.MinSize,
			MaxSize:          cfg.MaxSize,
			IdleBufferFactor: cfg.IdleBufferFactor,
			SurviveTime:      cfg.SurviveTime(),
			MonitorInterval:  cfg.MonitorInterval(),
			MaxRetries:       cfg.MaxRetries,
			RetryInterval:    cfg.RetryInterval(),
			ReconnectOnGet:   cfg.ReconnectOnGet,
		}, &grpcConnControl{addr: addr})
		m.pools[addr] = p
		log.Printf("[ConnPool] 热更新：新建连接池 addr=%s min=%d max=%d", addr, cfg.MinSize, cfg.MaxSize)
	}
}

func (m *connPoolManager) get(addr string) *pool.Pool[*grpc.ClientConn] {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.pools[addr]
}

func (m *connPoolManager) closeAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for addr, p := range m.pools {
		p.Close()
		log.Printf("[ConnPool] 关闭连接池 addr=%s", addr)
	}
	m.pools = make(map[string]*pool.Pool[*grpc.ClientConn])
}

// ── 派发相关类型 ──────────────────────────────────────────

type DispatchTarget struct {
	Addr    string
	Request *pb.ToolRequest
	Stream  bool
}

type DispatchResult struct {
	Target    DispatchTarget
	Responses []*pb.ToolResponse
	Err       error
}

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

// ── HubHandler：子类实现的接口 ───────────────────────────

type HubHandler interface {
	ServiceName() string
	Execute(req *pb.ToolRequest) ([]DispatchTarget, error)
	OnResults(results []DispatchResult)
	Addrs() []string
}

// ── BaseHub ───────────────────────────────────────────────

type BaseHub struct {
	pb.UnimplementedHubServiceServer
	handler HubHandler
	pm      *connPoolManager

	ctx    context.Context
	cancel context.CancelFunc
}

func New(handler HubHandler) *BaseHub {
	ctx, cancel := context.WithCancel(context.Background())
	b := &BaseHub{
		handler: handler,
		pm:      newConnPoolManager(),
		ctx:     ctx,
		cancel:  cancel,
	}
	b.pm.rebuild(handler.Addrs())
	return b
}

// ── 包级 Serve 函数 ───────────────────────────────────────

func Serve(addr string, handler HubHandler, loopInterval time.Duration) error {
	b := New(handler)
	return b.serve(addr, loopInterval)
}

func (b *BaseHub) ServeAsync(addr string, loopInterval time.Duration) error {
	return b.serve(addr, loopInterval)
}

func (b *BaseHub) serve(addr string, loopInterval time.Duration) error {
	if loopInterval > 0 {
		go b.dispatchLoop(loopInterval)
	}

	go b.watchRegistry()

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("[%s] 监听失败: %w", b.handler.ServiceName(), err)
	}

	srv := grpc.NewServer()
	pb.RegisterHubServiceServer(srv, b)
	log.Printf("[%s] 启动 gRPC 服务 %s", b.handler.ServiceName(), addr)

	defer func() {
		b.cancel()
		b.pm.closeAll()
	}()
	return srv.Serve(lis)
}

func (b *BaseHub) watchRegistry() {
	log.Printf("[%s] watchRegistry 已启动（阻塞模式）", b.handler.ServiceName())
	for {
		select {
		case <-b.ctx.Done():
			log.Printf("[%s] watchRegistry 已退出", b.handler.ServiceName())
			return
		case <-registry.ChangeCh():
			b.pm.rebuild(b.handler.Addrs())
			log.Printf("[%s] 连接池已热更新", b.handler.ServiceName())
		}
	}
}

// ── gRPC 服务端实现 ───────────────────────────────────────

func (b *BaseHub) DispatchSimple(ctx context.Context, req *pb.ToolRequest) (*pb.ToolResponse, error) {
	log.Printf("[%s] DispatchSimple from=%s service_name=%s method=%s",
		b.handler.ServiceName(), req.From, req.ServiceName, req.Method)

	targets, err := b.handler.Execute(req)
	if err != nil {
		// ✅ 使用结构化错误返回
		return errorResp(b.handler.ServiceName(), "EXECUTE_FAILED", err.Error(), ""), nil
	}

	results := b.sendAll(ctx, targets)
	b.handler.OnResults(results)

	// ✅ 聚合结果：收集业务数据 + 结构化错误
	var allErrors []*pb.ErrorDetail
	var aggregatedResults [][]byte

	for _, r := range results {
		if r.Err != nil {
			// 派发层错误（如连接失败）
			allErrors = append(allErrors, &pb.ErrorDetail{
				Code:    "DISPATCH_ERROR",
				Message: fmt.Sprintf("[%s] %v", r.Target.Addr, r.Err),
				Field:   "target.addr",
				Help:    "检查下游服务是否可用",
			})
			continue
		}
		for _, resp := range r.Responses {
			// 收集业务层返回的结构化错误
			allErrors = append(allErrors, resp.Errors...)
			// 收集有效业务数据（跳过空结果）
			if len(resp.Result) > 0 {
				aggregatedResults = append(aggregatedResults, resp.Result)
			}
		}
	}

	// ✅ 确定最终状态
	status := "ok"
	if len(allErrors) > 0 {
		if len(aggregatedResults) > 0 {
			status = "partial" // 部分成功
		} else {
			status = "error" // 全部失败
		}
	}

	// ✅ 聚合业务数据：单结果直接返回，多结果包裹为 JSON 数组
	var finalResult []byte
	switch len(aggregatedResults) {
	case 0:
		finalResult = []byte("{}") // 空结果返回空对象
	case 1:
		finalResult = aggregatedResults[0]
	default:
		// 简单聚合：假设每个 result 是合法 JSON，用逗号拼接成数组
		// 生产环境建议用 json.RawMessage 严格处理
		// 全程使用 []byte，避免 string 转换
		joined := bytes.Join(aggregatedResults, []byte(","))
		finalResult = make([]byte, 0, len(joined)+2)
		finalResult = append(finalResult, '[')
		finalResult = append(finalResult, joined...)
		finalResult = append(finalResult, ']')
	}

	return &pb.ToolResponse{
		ServiceName: b.handler.ServiceName(),
		Status:      status,
		Result:      finalResult, // bytes 类型，直接赋值
		Errors:      allErrors,   // ✅ 结构化错误列表
	}, nil
}

func (b *BaseHub) DispatchStream(stream pb.HubService_DispatchStreamServer) error {
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		log.Printf("[%s] DispatchStream from=%s service_name=%s method=%s",
			b.handler.ServiceName(), req.From, req.ServiceName, req.Method)

		targets, err := b.handler.Execute(req)
		if err != nil {
			_ = stream.Send(errorResp(b.handler.ServiceName(), "EXECUTE_FAILED", err.Error(), ""))
			continue
		}

		results := b.sendAll(stream.Context(), targets)
		b.handler.OnResults(results)

		for _, r := range results {
			if r.Err != nil {
				_ = stream.Send(errorResp(b.handler.ServiceName(), "DISPATCH_ERROR", r.Err.Error(), r.Target.Addr))
				continue
			}
			for _, resp := range r.Responses {
				if sendErr := stream.Send(resp); sendErr != nil {
					return sendErr
				}
			}
		}
	}
}

// ── 定时派发 ─────────────────────────────────────────────

func (b *BaseHub) dispatchLoop(interval time.Duration) {
	log.Printf("[%s] dispatch loop 已启动", b.handler.ServiceName())
	// 当前为空实现，预留定时触发逻辑
	// 如需启用，取消下方注释并实现 trigger()
	/*
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-b.ctx.Done():
				return
			case <-ticker.C:
				b.trigger()
			}
		}
	*/
}

// ── 底层发送（并发版） ────────────────────────────────────

func (b *BaseHub) sendAll(ctx context.Context, targets []DispatchTarget) []DispatchResult {
	results := make([]DispatchResult, len(targets))
	var wg sync.WaitGroup

	for i, t := range targets {
		wg.Add(1)
		go func(idx int, target DispatchTarget) {
			defer wg.Done()

			p := b.pm.get(target.Addr)
			if p == nil {
				log.Printf("[%s] sendAll: 连接池不存在 addr=%s，跳过",
					b.handler.ServiceName(), target.Addr)
				results[idx] = DispatchResult{
					Target: target,
					Err:    fmt.Errorf("no pool for addr=%s", target.Addr),
				}
				return
			}

			var resps []*pb.ToolResponse
			var err error
			if target.Stream {
				resps, err = b.callStream(ctx, target.Addr, target.Request)
			} else {
				var resp *pb.ToolResponse
				resp, err = b.callSimple(ctx, target.Addr, target.Request)
				if err == nil {
					resps = []*pb.ToolResponse{resp}
				}
			}
			results[idx] = DispatchResult{
				Target:    target,
				Responses: resps,
				Err:       err,
			}
		}(i, t)
	}

	wg.Wait()
	return results
}

func (b *BaseHub) callSimple(ctx context.Context, addr string, req *pb.ToolRequest) (*pb.ToolResponse, error) {
	p := b.pm.get(addr)
	if p == nil {
		return nil, fmt.Errorf("no pool for addr=%s", addr)
	}
	res, err := p.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("pool.Get addr=%s: %w", addr, err)
	}
	defer p.Put(res)
	return pb.NewHubServiceClient(res.Conn).DispatchSimple(ctx, req)
}

func (b *BaseHub) callStream(ctx context.Context, addr string, req *pb.ToolRequest) ([]*pb.ToolResponse, error) {
	p := b.pm.get(addr)
	if p == nil {
		return nil, fmt.Errorf("no pool for addr=%s", addr)
	}

	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	res, err := p.Get(callCtx)
	if err != nil {
		return nil, fmt.Errorf("pool.Get addr=%s: %w", addr, err)
	}
	defer func() {
		s := res.Conn.GetState()
		if s == connectivity.Shutdown || s == connectivity.TransientFailure {
			// 连接已损坏，丢弃而非归还
		} else {
			p.Put(res)
		}
	}()

	stream, err := pb.NewHubServiceClient(res.Conn).DispatchStream(callCtx)
	if err != nil {
		return nil, err
	}
	if err := stream.Send(req); err != nil {
		return nil, err
	}
	stream.CloseSend()

	var responses []*pb.ToolResponse
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return responses, err
		}
		responses = append(responses, resp)
	}
	return responses, nil
}

// ── 辅助函数：构造结构化错误响应 ─────────────────────────

// errorResp 返回包含结构化错误的 ToolResponse
// code: 错误码，如 "INTERNAL_ERROR"
// msg: 人类可读的错误描述
// field: 可选，关联的字段名（如 "params.city"）
func errorResp(serviceName, code, msg, field string) *pb.ToolResponse {
	return &pb.ToolResponse{
		ServiceName: serviceName,
		Status:      "error",
		Result:      []byte("{}"), // 错误时返回空对象，避免客户端解析异常
		Errors: []*pb.ErrorDetail{
			{
				Code:    code,
				Message: msg,
				Field:   field,
			},
		},
	}
}

// ════════════════════════════════════════════════════════════
//  公开 API：主动发送
// ════════════════════════════════════════════════════════════

func (b *BaseHub) Handler() HubHandler {
	return b.handler
}

func (b *BaseHub) Dispatch(ctx context.Context, req *pb.ToolRequest) []DispatchResult {
	targets, err := b.handler.Execute(req)
	if err != nil {
		log.Printf("[%s] Dispatch Execute 失败: %v", b.handler.ServiceName(), err)
		return nil
	}
	if len(targets) == 0 {
		log.Printf("[%s] Dispatch 无匹配目标 service=%s method=%s",
			b.handler.ServiceName(), req.ServiceName, req.Method)
		return nil
	}
	results := b.sendAll(ctx, targets)
	log.Println(" send info (DispatchTarget): ", targets)
	b.handler.OnResults(results)
	return results
}
