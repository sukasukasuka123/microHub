package hub

import (
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
//
// 改动说明：
//   - 新增 rebuild(addrs) 方法，供热更新时调用：关闭已消失的池、新建新增的池
//   - get() 仅做读取，不再懒创建；未找到时返回 nil 并记录告警
//   - 原有的懒创建逻辑移入 ensurePool()，仅在 rebuild 里被调用

type connPoolManager struct {
	mu    sync.RWMutex
	pools map[string]*pool.Pool[*grpc.ClientConn]
}

func newConnPoolManager() *connPoolManager {
	return &connPoolManager{
		pools: make(map[string]*pool.Pool[*grpc.ClientConn]),
	}
}

// rebuild 根据最新的地址集合更新连接池：
//   - 新增地址 → 建池
//   - 消失地址 → 关池
//   - 已有地址 → 保持不动（不重建，避免中断正在使用的连接）
func (m *connPoolManager) rebuild(addrs []string) {
	addrSet := make(map[string]struct{}, len(addrs))
	for _, a := range addrs {
		addrSet[a] = struct{}{}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	cfg := registry.GetGrpcPoolConfig()

	// 关闭已消失的池
	for addr, p := range m.pools {
		if _, keep := addrSet[addr]; !keep {
			p.Close()
			delete(m.pools, addr)
			log.Printf("[ConnPool] 热更新：关闭连接池 addr=%s", addr)
		}
	}

	// 新建新增的池
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

// get 返回指定地址的连接池；若不存在返回 nil（调用方需判断）。
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
	// Execute 决定本次派发的目标列表
	// req == nil 表示定时触发，req != nil 表示上游调用
	Execute(req *pb.ToolRequest) ([]DispatchTarget, error)
	// OnResults 接收所有派发结果
	OnResults(results []DispatchResult)
	// Addrs 返回当前 handler 关心的所有下游地址（用于连接池热更新）
	Addrs() []string
}

// ── BaseHub ───────────────────────────────────────────────

type BaseHub struct {
	pb.UnimplementedHubServiceServer
	handler HubHandler
	pm      *connPoolManager

	// ctx / cancel 控制 dispatchLoop 和整个 Hub 的生命周期
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
	// 根据初始地址建池
	b.pm.rebuild(handler.Addrs())
	return b
}

// ── 包级 Serve 函数 ───────────────────────────────────────

func Serve(addr string, handler HubHandler, loopInterval time.Duration) error {
	b := New(handler)
	return b.serve(addr, loopInterval)
}

func (b *BaseHub) serve(addr string, loopInterval time.Duration) error {
	if loopInterval > 0 {
		go b.dispatchLoop(loopInterval)
	}

	// 热更新监听：registry 配置变化时重建连接池
	go b.watchRegistry()

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("[%s] 监听失败: %w", b.handler.ServiceName(), err)
	}

	srv := grpc.NewServer()
	pb.RegisterHubServiceServer(srv, b)
	log.Printf("[%s] 启动 gRPC 服务 %s", b.handler.ServiceName(), addr)

	// 优雅关闭：srv.Serve 返回后清理资源
	defer func() {
		b.cancel()      // 通知所有 goroutine 退出
		b.pm.closeAll() // 关闭所有连接池
	}()
	return srv.Serve(lis)
}

// watchRegistry 监听注册表变化，触发连接池热更新。
// 每隔一段时间轮询 handler.Addrs() 与当前池集合做 diff，
// 避免在 registry 层侵入回调机制。
func (b *BaseHub) watchRegistry() {
	log.Printf("[%s] watchRegistry 已启动（阻塞模式）", b.handler.ServiceName())
	for {
		// ── 核心改动：阻塞等待，无变更时不占 CPU ──
		select {
		case <-b.ctx.Done():
			log.Printf("[%s] watchRegistry 已退出", b.handler.ServiceName())
			return
		case <-registry.ChangeCh(): // 通过公有函数控制私有管道
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
		return errorResp(b.handler.ServiceName(), err.Error()), nil
	}

	results := b.sendAll(ctx, targets)
	b.handler.OnResults(results)

	result := ""
	for _, r := range results {
		if r.Err != nil {
			result += fmt.Sprintf("[%s] error: %v\n", r.Target.Addr, r.Err)
			continue
		}
		for _, resp := range r.Responses {
			result += resp.Result + "\n"
		}
	}
	return &pb.ToolResponse{
		ServiceName: b.handler.ServiceName(),
		Status:      "ok",
		Result:      result,
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
			_ = stream.Send(errorResp(b.handler.ServiceName(), err.Error()))
			continue
		}

		results := b.sendAll(stream.Context(), targets)
		b.handler.OnResults(results)

		for _, r := range results {
			if r.Err != nil {
				_ = stream.Send(errorResp(b.handler.ServiceName(), r.Err.Error()))
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

func (b *BaseHub) trigger() {
	targets, err := b.handler.Execute(nil)
	if err != nil {
		log.Printf("[%s] trigger Execute 失败: %v", b.handler.ServiceName(), err)
		return
	}
	// 传入 hub 级 ctx：hub 关闭时正在进行的 trigger 也会被取消
	results := b.sendAll(b.ctx, targets)
	b.handler.OnResults(results)
}

func (b *BaseHub) dispatchLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	log.Printf("[%s] dispatch loop 已启动，间隔 %s", b.handler.ServiceName(), interval)
	for {
		select {
		case <-b.ctx.Done():
			log.Printf("[%s] dispatch loop 已退出", b.handler.ServiceName())
			return
		case <-ticker.C:
			b.trigger()
		}
	}
}

// ── 底层发送（并发版） ────────────────────────────────────
//
// 改动说明：
//   - 原串行 for 循环改为 goroutine + WaitGroup 并发派发
//   - results 切片预分配固定槽位，每个 goroutine 写自己的下标，无需加锁
//   - pool 不存在时快速失败并记录告警，不阻塞其他 target

func (b *BaseHub) sendAll(ctx context.Context, targets []DispatchTarget) []DispatchResult {
	results := make([]DispatchResult, len(targets))
	var wg sync.WaitGroup

	for i, t := range targets {
		wg.Add(1)
		go func(idx int, target DispatchTarget) {
			defer wg.Done()

			p := b.pm.get(target.Addr)
			if p == nil {
				// 地址不在池中（热更新尚未完成或地址有误）
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
			// 连接已损坏，丢弃而非归还（池的 Reset/Close 会处理）
			// p.Discard(res) 池子没有丢弃的api
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

// ── 辅助 ─────────────────────────────────────────────────

func errorResp(serviceName, msg string) *pb.ToolResponse {
	return &pb.ToolResponse{
		ServiceName: serviceName,
		Status:      "error",
		Result:      msg,
	}
}
