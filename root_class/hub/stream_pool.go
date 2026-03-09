package hub

import (
	"context"
	"fmt"
	"log"

	pool "github.com/sukasukasuka123/TemplatePoolByGO"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/sukasukasuka123/microHub/proto/gen/proto"
	registry "github.com/sukasukasuka123/microHub/service_registry"
)

// ════════════════════════════════════════════════════════════
//  streamResource — 实现 pool.ResourceControl[*SingleStream]
// ════════════════════════════════════════════════════════════

// streamResource 封装单条 SingleStream 的生命周期，
// 供 TemplatePoolByGO 的 Pool 管理。
//
// 持有 *StreamPool 而非直接持有 *grpc.ClientConn，
// 确保 conn 的所有权唯一归属于 StreamPool，避免双重引用。
type streamResource struct {
	sp *StreamPool // 反向引用，通过 sp.Conn 访问底层连接
}

// Create 创建一条新的双向流（复用 sp.client，无额外 alloc）。
func (r *streamResource) Create() (*SingleStream, error) {
	stream, err := r.sp.client.DispatchStream(context.Background())
	if err != nil {
		return nil, fmt.Errorf("DispatchStream addr=%s: %w", r.sp.addr, err)
	}
	return NewSingleStream(stream, r.sp.addr, nil), nil
}

// Reset 检查流是否仍可用。
func (r *streamResource) Reset(s *SingleStream) error {
	if s.IsClosed() {
		return fmt.Errorf("stream closed addr=%s", r.sp.addr)
	}
	return nil
}

// Close 主动关闭流（Pool 缩容或关闭时调用）。
// 注意：不关闭 conn，conn 的生命周期由 StreamPool 管理。
func (r *streamResource) Close(s *SingleStream) error {
	s.Close()
	return nil
}

// Ping 健康检查：流已关闭则报错，Pool 会丢弃并重建。
func (r *streamResource) Ping(s *SingleStream) error {
	if s.IsClosed() {
		return fmt.Errorf("stream closed addr=%s", r.sp.addr)
	}
	state := r.sp.Conn.GetState()
	if state == connectivity.Shutdown || state == connectivity.TransientFailure {
		return fmt.Errorf("conn unhealthy state=%s addr=%s", state, r.sp.addr)
	}
	return nil
}

// ════════════════════════════════════════════════════════════
//  StreamPool — 管理单个 Tool 地址的流池
// ════════════════════════════════════════════════════════════

// StreamPool 使用 TemplatePoolByGO 管理与单个 Tool 之间的 N 条双向流。
//
// 使用方式：
//
//	sp, err := NewStreamPool("localhost:50052", cfg)
//	entry, err := sp.Get(ctx)        // 取一条流
//	task, err := entry.Conn.Send(req) // 发任务
//	sp.Put(entry)                    // 用完归还
type StreamPool struct {
	addr   string
	Conn   *grpc.ClientConn    // 导出：供 callSimple 复用底层连接
	client pb.HubServiceClient // 缓存 client，避免 callSimple 每次 alloc
	p      *pool.Pool[*SingleStream]
}

// NewStreamPool 构造流池并预热 min_size 条流。
// cfg 直接复用 registry.GrpcPoolConfig，不额外引入新配置项。
func NewStreamPool(addr string, cfg registry.GrpcPoolConfig) (*StreamPool, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	// 先构造 StreamPool（conn 所有权在此），再构造 streamResource（反向引用 sp）。
	sp := &StreamPool{addr: addr, Conn: conn, client: pb.NewHubServiceClient(conn)}
	res := &streamResource{sp: sp}
	sp.p = pool.NewPool(pool.PoolConfig{
		MinSize:          cfg.MinSize,
		MaxSize:          cfg.MaxSize,
		IdleBufferFactor: cfg.IdleBufferFactor,
		SurviveTime:      cfg.SurviveTime(),
		MonitorInterval:  cfg.MonitorInterval(),
		MaxRetries:       cfg.MaxRetries,
		RetryInterval:    cfg.RetryInterval(),
		ReconnectOnGet:   cfg.ReconnectOnGet,
	}, res)
	log.Printf("[StreamPool] addr=%s 已建立 min=%d max=%d", addr, cfg.MinSize, cfg.MaxSize)
	return sp, nil
}

// Get 从池中取一条可用流（TemplatePoolByGO 返回带 Conn 字段的 Resource）。
// 调用方必须在用完后调用 Put 归还。
func (sp *StreamPool) Get(ctx context.Context) (*pool.Resource[*SingleStream], error) {
	res, err := sp.p.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("StreamPool.Get addr=%s: %w", sp.addr, err)
	}
	return res, nil
}

// Put 归还流到池中。
func (sp *StreamPool) Put(res *pool.Resource[*SingleStream]) {
	sp.p.Put(res)
}

// Close 关闭流池及底层 conn。
func (sp *StreamPool) Close() {
	sp.p.Close()
	_ = sp.Conn.Close()
	log.Printf("[StreamPool] addr=%s 已关闭", sp.addr)
}

// Addr 返回该流池对应的 Tool 地址。
func (sp *StreamPool) Addr() string { return sp.addr }
