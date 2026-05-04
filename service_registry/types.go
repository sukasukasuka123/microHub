package service_registry

import (
	"encoding/json"
	"fmt"
	"time"

	schema "github.com/sukasukasuka123/microHub/jsonSchema"
)

type ToolEntry struct {
	Name         string `yaml:"name"          mapstructure:"name"`
	Addr         string `yaml:"addr"          mapstructure:"addr"`
	Method       string `yaml:"method"        mapstructure:"method"`
	InputSchema  string `yaml:"input_schema"  mapstructure:"input_schema"`
	OutputSchema string `yaml:"output_schema" mapstructure:"output_schema"`
}

func (t *ToolEntry) ParseInputSchema() (*schema.SchemaNode, error) {
	return parseSchema([]byte(t.InputSchema))
}

func (t *ToolEntry) ParseOutputSchema() (*schema.SchemaNode, error) {
	return parseSchema([]byte(t.OutputSchema))
}

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

// ── HubEntry ─────────────────────────────────────────────

type HubEntry struct {
	Name         string `yaml:"name"          mapstructure:"name"`
	Addr         string `yaml:"addr"          mapstructure:"addr"`
	RegisteredAt string `yaml:"registered_at" mapstructure:"registered_at"`
}

// ── GrpcPoolConfig ────────────────────────────────────────

type GrpcPoolConfig struct {
	MinSize            int64   `mapstructure:"min_size"`
	MaxSize            int64   `mapstructure:"max_size"`
	IdleBufferFactor   float64 `mapstructure:"idle_buffer_factor"`
	SurviveTimeSec     int     `mapstructure:"survive_time_sec"`
	MonitorIntervalSec int     `mapstructure:"monitor_interval_sec"`
	MaxRetries         int     `mapstructure:"max_retries"`
	RetryIntervalMs    int     `mapstructure:"retry_interval_ms"`
	ReconnectOnGet     bool    `mapstructure:"reconnect_on_get"`
	PingIntervalSec    int     `mapstructure:"ping_interval_sec"`
	MaxWaitQueue       int64   `mapstructure:"max_wait_queue"`
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
func (c GrpcPoolConfig) PingInterval() time.Duration {
	return time.Duration(c.PingIntervalSec) * time.Second
}

// ── yaml 整体映射（只给 config_loader 用）────────────────

type registryConfig struct {
	Services struct {
		Tools []ToolEntry `mapstructure:"tools"`
		Hubs  []HubEntry  `mapstructure:"hubs"`
	} `mapstructure:"services"`
	Pool struct {
		GrpcConn GrpcPoolConfig `mapstructure:"grpc_conn"`
	} `mapstructure:"pool"`
}
