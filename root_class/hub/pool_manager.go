package hub

import (
	"log"
	"sync"

	registry "github.com/sukasukasuka123/microHub/service_registry"
)

type poolManager struct {
	mu    sync.RWMutex
	pools map[string]*StreamPool
}

func newPoolManager() *poolManager {
	return &poolManager{pools: make(map[string]*StreamPool)}
}

// rebuild 根据最新在线地址列表热更新流池。
// 多余的关闭，缺少的新建，已存在的不动。
func (m *poolManager) rebuild(addrs []string) {
	cfg := registry.GetGrpcPoolConfig()

	addrSet := make(map[string]struct{}, len(addrs))
	for _, a := range addrs {
		addrSet[a] = struct{}{}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for addr, sp := range m.pools {
		if _, keep := addrSet[addr]; !keep {
			sp.Close()
			delete(m.pools, addr)
			log.Printf("[poolManager] 关闭流池 addr=%s", addr)
		}
	}

	for _, addr := range addrs {
		if _, exists := m.pools[addr]; exists {
			continue
		}
		sp, err := NewStreamPool(addr, cfg, defaultHeartbeatCallback())
		if err != nil {
			log.Printf("[poolManager] 新建流池失败 addr=%s: %v", addr, err)
			continue
		}
		m.pools[addr] = sp
		log.Printf("[poolManager] 新建流池 addr=%s", addr)
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

// defaultHeartbeatCallback 默认策略：
// 心跳失败直接标记 offline，由 registry 的探活 goroutine 负责恢复。
func defaultHeartbeatCallback() HeartbeatFailCallback {
	return func(addr string, err error) UnhealthyAction {
		// 已经是 offline 就不重复打日志和触发 rebuild
		if registry.IsOffline(addr) {
			return ActionIgnore // ← 新增：已离线则忽略后续重复回调
		}
		log.Printf("[poolManager] 心跳失败 addr=%s: %v，标记 offline", addr, err)
		return ActionMarkOffline
	}
}
