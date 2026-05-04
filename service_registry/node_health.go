package service_registry

import (
	"context"
	"log"
	"net"
	"sync"
	"time"
)

var (
	offlineMu    sync.RWMutex
	offlineAddrs = make(map[string]bool)
)

// ── 状态变更 ──────────────────────────────────────────────

func MarkOffline(addr string) {
	offlineMu.Lock()
	offlineAddrs[addr] = true
	offlineMu.Unlock()
	log.Printf("[Registry] addr=%s → offline", addr)
	notifyChange() // poolManager 收到信号后 rebuild，跳过这个地址
}

func MarkOnline(addr string) {
	offlineMu.Lock()
	delete(offlineAddrs, addr)
	offlineMu.Unlock()
	log.Printf("[Registry] addr=%s → online", addr)
	notifyChange() // poolManager 收到信号后 rebuild，重新为这个地址建流池
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
func canDial(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
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
