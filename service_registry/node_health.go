package service_registry

import (
	"context"
	"log"
	"sync"
	"time"

	pb "github.com/RedHuang-0622/microHub/proto/gen/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	offlineMu    sync.RWMutex
	offlineAddrs = make(map[string]bool)
)

// ── 状态变更 ──────────────────────────────────────────────

func MarkOffline(addr string) {
	offlineMu.Lock()
	alreadyOffline := offlineAddrs[addr]
	offlineAddrs[addr] = true
	offlineMu.Unlock()

	if alreadyOffline {
		return // ← 幂等保护
	}
	log.Printf("[Registry] addr=%s → offline", addr)
	notifyChange()
}

func MarkOnline(addr string) {
	offlineMu.Lock()
	alreadyOnline := !offlineAddrs[addr] // 已经是 online，不重复触发
	delete(offlineAddrs, addr)
	offlineMu.Unlock()

	if alreadyOnline {
		return // ← 幂等保护
	}
	log.Printf("[Registry] addr=%s → online", addr)
	notifyChange()
}

func IsOffline(addr string) bool {
	offlineMu.RLock()
	defer offlineMu.RUnlock()
	return offlineAddrs[addr]
}

// ── 后台探活 ──────────────────────────────────────────────

// StartHealthProbe 对所有 offline 地址定期做 TCP 探测，
// 连上了就调 MarkOnline，触发 poolManager 重建流池。
// 在 Init 之后调用，传入程序的根 context 控制生命周期。
func StartHealthProbe(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	go probeLoop(ctx, interval)
}

func probeLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			probeOnce()
		}
	}
}

func probeOnce() {
	offlineMu.RLock()
	addrs := make([]string, 0, len(offlineAddrs))
	for addr := range offlineAddrs {
		addrs = append(addrs, addr)
	}
	offlineMu.RUnlock()

	for _, addr := range addrs {
		if canDial(addr) {
			MarkOnline(addr)
		}
	}
}

// canDial 做一次 TCP 握手，成功即认为地址可达。
// canDial 改为 gRPC 探活，而非裸 TCP
func canDial(addr string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return false
	}
	defer conn.Close()

	// 方案 A：用标准 gRPC Health Check（需要 Tool 侧实现 health.v1）
	// hc := grpc_health_v1.NewHealthClient(conn)
	// resp, err := hc.Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	// return err == nil && resp.Status == grpc_health_v1.HealthCheckResponse_SERVING

	// 方案 B（更简单，无需改 Tool）：
	// 尝试建一条双向流，能建就算活着
	client := pb.NewHubServiceClient(conn)
	stream, err := client.DispatchStream(ctx)
	if err != nil {
		return false
	}
	_ = stream.CloseSend()
	return true
}

// ProbeAllOnStartup 在启动时同步探测所有 tool 地址，
// 不可达的直接标记 offline，让 poolManager 建池时跳过它们。
// 必须在 registry.Init 之后、hubbase.New 之前调用。
func ProbeAllOnStartup() {
	tools := GetAllTools()
	for _, t := range tools {
		if canDial(t.Addr) {
			log.Printf("[Registry] addr=%s ✓ online", t.Addr)
		} else {
			log.Printf("[Registry] addr=%s ✗ offline（不可达，跳过建池）", t.Addr)
			MarkOffline(t.Addr)
		}
	}
}
