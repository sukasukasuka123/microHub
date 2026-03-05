package service_registry

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
	schema "github.com/sukasukasuka123/microHub/jsonSchema"
)

// ════════════════════════════════════════════════════════════
//  服务元数据（✅ 移除 sync.Once，避免 copylocks）
// ════════════════════════════════════════════════════════════

type ToolEntry struct {
	Name         string `yaml:"name"           mapstructure:"name"`
	Addr         string `yaml:"addr"           mapstructure:"addr"`
	Method       string `yaml:"method"         mapstructure:"method"`
	InputSchema  string `yaml:"input_schema"   mapstructure:"input_schema"`
	OutputSchema string `yaml:"output_schema"  mapstructure:"output_schema"`
	// ✅ 移除 parseOnce / inputSchemaParsed / outputSchemaParsed
	// Demo 场景：每次解析开销可接受，避免锁拷贝问题
}

// ParseInputSchema 解析 input_schema（无缓存，简单可靠）
func (t *ToolEntry) ParseInputSchema() (*schema.SchemaNode, error) {
	if t.InputSchema == "" {
		return nil, nil
	}
	return parseSchema([]byte(t.InputSchema))
}

// ParseOutputSchema 解析 output_schema（无缓存）
func (t *ToolEntry) ParseOutputSchema() (*schema.SchemaNode, error) {
	if t.OutputSchema == "" {
		return nil, nil
	}
	return parseSchema([]byte(t.OutputSchema))
}

// parseSchema 通用解析函数
func parseSchema(raw []byte) (*schema.SchemaNode, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var node schema.SchemaNode
	if err := json.Unmarshal(raw, &node); err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}
	return &node, nil
}

type HubEntry struct {
	Name         string `yaml:"name"          mapstructure:"name"`
	Addr         string `yaml:"addr"          mapstructure:"addr"`
	RegisteredAt string `yaml:"registered_at" mapstructure:"registered_at"`
}

// ════════════════════════════════════════════════════════════
//  连接池配置
// ════════════════════════════════════════════════════════════

type GrpcPoolConfig struct {
	MinSize            int64   `mapstructure:"min_size"`
	MaxSize            int64   `mapstructure:"max_size"`
	IdleBufferFactor   float64 `mapstructure:"idle_buffer_factor"`
	SurviveTimeSec     int     `mapstructure:"survive_time_sec"`
	MonitorIntervalSec int     `mapstructure:"monitor_interval_sec"`
	MaxRetries         int     `mapstructure:"max_retries"`
	RetryIntervalMs    int     `mapstructure:"retry_interval_ms"`
	ReconnectOnGet     bool    `mapstructure:"reconnect_on_get"`
}

func (c GrpcPoolConfig) SurviveTime() time.Duration {
	return time.Duration(c.SurviveTimeSec) * time.Second
}
func (c GrpcPoolConfig) MonitorInterval() time.Duration {
	return time.Duration(c.MonitorIntervalSec) * time.Second
}
func (c GrpcPoolConfig) RetryInterval() time.Duration {
	return time.Duration(c.RetryIntervalMs) * time.Millisecond
}

// ════════════════════════════════════════════════════════════
//  YAML 整体映射
// ════════════════════════════════════════════════════════════

type registryConfig struct {
	Services struct {
		Tools []ToolEntry `mapstructure:"tools"`
		Hubs  []HubEntry  `mapstructure:"hubs"`
	} `mapstructure:"services"`
	Pool struct {
		GrpcConn GrpcPoolConfig `mapstructure:"grpc_conn"`
	} `mapstructure:"pool"`
}

// ════════════════════════════════════════════════════════════
//  注册表状态（✅ ToolEntry 现在是纯数据，可安全拷贝）
// ════════════════════════════════════════════════════════════

var (
	mu       sync.RWMutex
	tools    []ToolEntry
	Hubs     []HubEntry
	grpcPool GrpcPoolConfig

	passedMu    sync.RWMutex
	passedTools = make(map[string]bool)
	changeCh    = make(chan struct{}, 1)
)

func ChangeCh() <-chan struct{} {
	return changeCh
}

// ════════════════════════════════════════════════════════════
//  Tool API（✅ 现在可安全按值返回）
// ════════════════════════════════════════════════════════════

func GetAllTools() []ToolEntry {
	mu.RLock()
	defer mu.RUnlock()
	cp := make([]ToolEntry, len(tools))
	copy(cp, tools) // ✅ 纯数据拷贝，安全
	return cp
}

func SelectToolByName(name string) (ToolEntry, bool) {
	mu.RLock()
	defer mu.RUnlock()
	for _, t := range tools { // ✅ 纯数据，range 拷贝安全
		if t.Name == name {
			return t, true // ✅ 按值返回，安全
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

func GetToolSchema(name string) (input, output string, exists bool) {
	mu.RLock()
	defer mu.RUnlock()
	for _, t := range tools {
		if t.Name == name {
			return t.InputSchema, t.OutputSchema, true
		}
	}
	return "", "", false
}

func GetToolSchemaParsed(name string) (input, output *schema.SchemaNode, exists bool) {
	mu.RLock()
	defer mu.RUnlock()
	for _, t := range tools {
		if t.Name == name {
			in, _ := t.ParseInputSchema() // ✅ 无缓存，每次解析
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

// ════════════════════════════════════════════════════════════
//  Hub API
// ════════════════════════════════════════════════════════════

func GetAllHubs() []HubEntry {
	mu.RLock()
	defer mu.RUnlock()
	cp := make([]HubEntry, len(Hubs))
	copy(cp, Hubs)
	return cp
}

// ════════════════════════════════════════════════════════════
//  连接池配置 API
// ════════════════════════════════════════════════════════════

func GetGrpcPoolConfig() GrpcPoolConfig {
	mu.RLock()
	defer mu.RUnlock()
	return grpcPool
}

// ════════════════════════════════════════════════════════════
//  测试状态 API
// ════════════════════════════════════════════════════════════

func MarkPassed(toolName string) {
	passedMu.Lock()
	defer passedMu.Unlock()
	passedTools[toolName] = true
	log.Printf("[Registry] tool=%s 已通过测试", toolName)
}

func MarkFailed(toolName string) {
	passedMu.Lock()
	defer passedMu.Unlock()
	passedTools[toolName] = false
	log.Printf("[Registry] tool=%s 测试失败，已移除", toolName)
}

func IsPassed(toolName string) bool {
	passedMu.RLock()
	defer passedMu.RUnlock()
	return passedTools[toolName]
}

func GetPassedTools() []ToolEntry {
	mu.RLock()
	defer mu.RUnlock()
	passedMu.RLock()
	defer passedMu.RUnlock()
	var result []ToolEntry
	for _, t := range tools { // ✅ 纯数据，append 安全
		if passedTools[t.Name] {
			result = append(result, t)
		}
	}
	return result
}

// ════════════════════════════════════════════════════════════
//  打印
// ════════════════════════════════════════════════════════════

func PrintRegistry() {
	mu.RLock()
	defer mu.RUnlock()
	passedMu.RLock()
	defer passedMu.RUnlock()

	fmt.Println("\n=== tools ===")
	for i, t := range tools { // ✅ 纯数据，range 安全
		mark := "[ ]"
		if passedTools[t.Name] {
			mark = "[✓]"
		}
		in, _ := t.ParseInputSchema()
		out, _ := t.ParseOutputSchema()
		fmt.Printf("  %s [%d] name=%-10s addr=%-22s method=%-10s",
			mark, i, t.Name, t.Addr, t.Method)
		if in != nil {
			fmt.Printf(" in_type=%s", in.Type)
		}
		if out != nil {
			fmt.Printf(" out_type=%s", out.Type)
		}
		fmt.Println()
	}

	fmt.Println("=== pool.grpc_conn ===")
	fmt.Printf("  min=%d max=%d idle_buf=%.2f survive=%ds monitor=%ds retries=%d retry_ms=%d reconnect=%v\n",
		grpcPool.MinSize, grpcPool.MaxSize, grpcPool.IdleBufferFactor,
		grpcPool.SurviveTimeSec, grpcPool.MonitorIntervalSec,
		grpcPool.MaxRetries, grpcPool.RetryIntervalMs, grpcPool.ReconnectOnGet,
	)
}

// ════════════════════════════════════════════════════════════
//  内部函数
// ════════════════════════════════════════════════════════════

func replaceAll(cfg registryConfig) {
	mu.Lock()
	defer mu.Unlock()
	// ✅ 纯数据赋值，无需重置缓存
	tools = cfg.Services.Tools
	Hubs = cfg.Services.Hubs
	grpcPool = cfg.Pool.GrpcConn
}

func readConfig(configPath string) (registryConfig, error) {
	var cfg registryConfig
	viper.SetConfigFile(configPath)
	viper.SetConfigType("yaml")
	if err := viper.ReadInConfig(); err != nil {
		return cfg, err
	}
	return cfg, viper.Unmarshal(&cfg)
}

func onConfigUpdated(cfg registryConfig) {
	replaceAll(cfg)
	log.Printf("[Registry] 注册表已更新 — tools=%d Hubs=%d",
		len(cfg.Services.Tools), len(cfg.Services.Hubs))
	PrintRegistry()

	select {
	case changeCh <- struct{}{}:
	default:
	}
}

func Watch(configPath string) {
	viper.OnConfigChange(func(e fsnotify.Event) {
		log.Printf("[Registry] 检测到 yaml 变化: %s", e.Name)
		cfg, err := readConfig(configPath)
		if err != nil {
			log.Printf("[Registry] 解析失败，跳过: %v", err)
			return
		}
		onConfigUpdated(cfg)
	})
	viper.WatchConfig()
	log.Println("[Registry] 开始监听 yaml 变化...")
}

func Init(configPath string) error {
	cfg, err := readConfig(configPath)
	if err != nil {
		return fmt.Errorf("[Registry] 首次加载失败: %w", err)
	}
	onConfigUpdated(cfg)
	Watch(configPath)
	return nil
}
