package service_registry

import (
	"encoding/json"
	"sync"

	schema "github.com/sukasukasuka123/microHub/jsonSchema"
)

// ════════════════════════════════════════════════════════════
//  内存状态
// ════════════════════════════════════════════════════════════

var (
	mu       sync.RWMutex
	tools    []ToolEntry
	hubs     []HubEntry
	grpcPool GrpcPoolConfig
)

// ── 写入（只有 config_loader 调用）──────────────────────

func replaceAll(cfg registryConfig) {
	mu.Lock()
	defer mu.Unlock()
	tools = cfg.Services.Tools
	hubs = cfg.Services.Hubs
	grpcPool = cfg.Pool.GrpcConn
}

// ── Tool 查询 ─────────────────────────────────────────────

// GetAllTools 返回全部 tool，含 offline 的。
func GetAllTools() []ToolEntry {
	mu.RLock()
	defer mu.RUnlock()
	cp := make([]ToolEntry, len(tools))
	copy(cp, tools)
	return cp
}

// GetOnlineTools 返回当前 online 的 tool，供路由和 poolManager 使用。
func GetOnlineTools() []ToolEntry {
	mu.RLock()
	defer mu.RUnlock()
	offlineMu.RLock()
	defer offlineMu.RUnlock()

	result := make([]ToolEntry, 0, len(tools))
	for _, t := range tools {
		if !offlineAddrs[t.Addr] {
			result = append(result, t)
		}
	}
	return result
}

func SelectToolByName(name string) (ToolEntry, bool) {
	mu.RLock()
	defer mu.RUnlock()
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return ToolEntry{}, false
}

func SelectToolByMethod(method string) (ToolEntry, bool) {
	mu.RLock()
	defer mu.RUnlock()
	for _, t := range tools {
		if t.Method == method {
			return t, true
		}
	}
	return ToolEntry{}, false
}

func GetToolSchemaParsed(name string) (input, output *schema.SchemaNode, exists bool) {
	mu.RLock()
	defer mu.RUnlock()
	for _, t := range tools {
		if t.Name == name {
			in, _ := t.ParseInputSchema()
			out, _ := t.ParseOutputSchema()
			return in, out, true
		}
	}
	return nil, nil, false
}

func ValidateToolInput(toolName string, params json.RawMessage) error {
	in, _, exists := GetToolSchemaParsed(toolName)
	if !exists || in == nil {
		return nil
	}
	return in.Validate(params)
}

// ── Hub 查询 ──────────────────────────────────────────────

func GetAllHubs() []HubEntry {
	mu.RLock()
	defer mu.RUnlock()
	cp := make([]HubEntry, len(hubs))
	copy(cp, hubs)
	return cp
}

// ── Pool 配置查询 ─────────────────────────────────────────

func GetGrpcPoolConfig() GrpcPoolConfig {
	mu.RLock()
	defer mu.RUnlock()
	return grpcPool
}
