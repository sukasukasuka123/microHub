package jsonSchema

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ════════════════════════════════════════════════════════════
//  Schema 定义：无歧义递归结构
// ════════════════════════════════════════════════════════════

// SchemaType 支持的类型枚举
type SchemaType string

const (
	TypeString  SchemaType = "string"
	TypeInteger SchemaType = "integer"
	TypeNumber  SchemaType = "number"
	TypeBoolean SchemaType = "boolean"
	TypeObject  SchemaType = "object"
	TypeArray   SchemaType = "array"
)

// SchemaNode 自定义 schema 的最小递归单元
// 核心规则：
//   - 每个节点必须有 type 字段
//   - object 类型的子字段必须放在 data 中（map）
//   - array 类型的元素类型必须放在 items 中（单节点）
//   - 叶子节点（string/integer 等）直接结束，不再嵌套
type SchemaNode struct {
	Type     SchemaType             `json:"type"`               // 必填：节点类型
	Data     map[string]*SchemaNode `json:"data,omitempty"`     // object 专用：字段定义映射
	Items    *SchemaNode            `json:"items,omitempty"`    // array 专用：元素类型定义
	Default  interface{}            `json:"default,omitempty"`  // 可选：默认值
	Required []string               `json:"required,omitempty"` // object 专用：必填字段列表
	Min      *float64               `json:"min,omitempty"`      // number/integer 可选：最小值
	Max      *float64               `json:"max,omitempty"`      // number/integer 可选：最大值
	Enum     []interface{}          `json:"enum,omitempty"`     // 可选：枚举值列表
}

// IsLeaf 判断是否为叶子节点（可直接取值，无需递归）
func (n *SchemaNode) IsLeaf() bool {
	return n.Type != TypeObject && n.Type != TypeArray
}

// IsObject 判断是否为对象类型
func (n *SchemaNode) IsObject() bool {
	return n.Type == TypeObject
}

// IsArray 判断是否为数组类型
func (n *SchemaNode) IsArray() bool {
	return n.Type == TypeArray
}

// GetField 安全获取 object 的子字段定义
func (n *SchemaNode) GetField(name string) (*SchemaNode, bool) {
	if !n.IsObject() || n.Data == nil {
		return nil, false
	}
	field, ok := n.Data[name]
	return field, ok
}

// GetFieldType 递归获取字段路径的类型
// 支持路径："user.name" → "string", "tags[0]" → "string"
// 返回：(类型, 是否找到)
func (n *SchemaNode) GetFieldType(path string) (SchemaType, bool) {
	if path == "" {
		return n.Type, true
	}
	return n.resolvePath(splitPath(path))
}

// resolvePath 内部递归解析路径
func (n *SchemaNode) resolvePath(parts []string) (SchemaType, bool) {
	if len(parts) == 0 {
		return n.Type, true
	}
	key := parts[0]
	rest := parts[1:]

	// 处理数组索引：user[0].name → key="user", index=0, rest=["name"]
	if strings.HasSuffix(key, "]") && strings.Contains(key, "[") {
		if !n.IsArray() || n.Items == nil {
			return "", false
		}
		// 提取索引（简单实现，生产环境需校验合法性）
		return n.Items.resolvePath(rest)
	}

	// 处理 object 字段
	if !n.IsObject() || n.Data == nil {
		return "", false
	}
	child, ok := n.Data[key]
	if !ok {
		return "", false
	}
	return child.resolvePath(rest)
}

// Validate 对原始 JSON 数据进行校验（基础版）
// 返回：error = nil 表示校验通过
func (n *SchemaNode) Validate(data json.RawMessage) error {
	if n == nil {
		return nil
	}

	// 1️⃣ 解析原始数据为 interface{}
	var val interface{}
	if err := json.Unmarshal(data, &val); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}

	return n.validateValue(val, "")
}

// validateValue 递归校验具体值
func (n *SchemaNode) validateValue(val interface{}, path string) error {
	// null 值处理：如果非必填且无默认值，允许 null
	if val == nil {
		return nil
	}

	// 2️⃣ 枚举校验（优先于类型校验）
	if len(n.Enum) > 0 {
		if !contains(n.Enum, val) {
			return fmt.Errorf("value not in enum at %s", formatPath(path))
		}
		return nil
	}

	// 3️⃣ 类型校验
	switch n.Type {
	case TypeString:
		if _, ok := val.(string); !ok {
			return fmt.Errorf("expected string at %s, got %T", formatPath(path), val)
		}

	case TypeInteger:
		if err := validateInteger(val, path); err != nil {
			return err
		}
		// 范围校验
		if n.Min != nil || n.Max != nil {
			if f, ok := toFloat64(val); ok {
				if n.Min != nil && f < *n.Min {
					return fmt.Errorf("value < min (%.0f) at %s", *n.Min, formatPath(path))
				}
				if n.Max != nil && f > *n.Max {
					return fmt.Errorf("value > max (%.0f) at %s", *n.Max, formatPath(path))
				}
			}
		}

	case TypeNumber:
		if _, ok := toFloat64(val); !ok {
			return fmt.Errorf("expected number at %s, got %T", formatPath(path), val)
		}

	case TypeBoolean:
		if _, ok := val.(bool); !ok {
			return fmt.Errorf("expected boolean at %s, got %T", formatPath(path), val)
		}

	case TypeObject:
		if err := validateObject(val, n, path); err != nil {
			return err
		}

	case TypeArray:
		if err := validateArray(val, n, path); err != nil {
			return err
		}
	}
	return nil
}

// validateObject 校验 object 类型
func validateObject(val interface{}, schema *SchemaNode, path string) error {
	obj, ok := val.(map[string]interface{})
	if !ok {
		return fmt.Errorf("expected object at %s, got %T", formatPath(path), val)
	}

	// 必填字段检查
	for _, req := range schema.Required {
		if _, exists := obj[req]; !exists {
			return fmt.Errorf("missing required field '%s' at %s", req, formatPath(path))
		}
	}

	// 递归校验每个字段
	for key, fieldVal := range obj {
		fieldSchema, ok := schema.GetField(key)
		if !ok {
			continue // 未知字段：可选策略（忽略/警告/报错）
		}
		fieldBytes, _ := json.Marshal(fieldVal) // 重新序列化供递归校验
		if err := fieldSchema.Validate(fieldBytes); err != nil {
			return err
		}
	}
	return nil
}

// validateArray 校验 array 类型
func validateArray(val interface{}, schema *SchemaNode, path string) error {
	arr, ok := val.([]interface{})
	if !ok {
		return fmt.Errorf("expected array at %s, got %T", formatPath(path), val)
	}
	if schema.Items == nil {
		return nil // 无 items 定义，跳过元素校验
	}
	// 递归校验每个元素
	for _, item := range arr {
		itemBytes, _ := json.Marshal(item)
		if err := schema.Items.Validate(itemBytes); err != nil {
			return err
		}
	}
	return nil
}

// validateInteger 专门校验 integer 类型（json 默认解析为 float64）
func validateInteger(val interface{}, path string) error {
	switch v := val.(type) {
	case int, int8, int16, int32, int64:
		return nil
	case float64:
		// json.Unmarshal 将整数解析为 float64，需判断是否为整数值
		if v == float64(int64(v)) {
			return nil
		}
		return fmt.Errorf("expected integer at %s, got float %f", formatPath(path), v)
	default:
		return fmt.Errorf("expected integer at %s, got %T", formatPath(path), val)
	}
}

// ════════════════════════════════════════════════════════════
//  工具函数
// ════════════════════════════════════════════════════════════

// splitPath 分割字段路径：支持 "user.name" 和 "tags[0].id"
func splitPath(path string) []string {
	var parts []string
	var current strings.Builder
	inBracket := false

	for _, ch := range path {
		switch ch {
		case '[':
			inBracket = true
			current.WriteRune(ch)
		case ']':
			inBracket = false
			current.WriteRune(ch)
			parts = append(parts, current.String())
			current.Reset()
		case '.':
			if inBracket {
				current.WriteRune(ch)
			} else {
				if current.Len() > 0 {
					parts = append(parts, current.String())
					current.Reset()
				}
			}
		default:
			current.WriteRune(ch)
		}
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}
	return parts
}

// joinPath 拼接路径
func joinPath(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "." + child
}

// formatPath 格式化路径用于错误提示
func formatPath(path string) string {
	if path == "" {
		return "$root"
	}
	return "$." + path
}

// contains 检查切片是否包含某值（用于 enum 校验）
func contains(slice []interface{}, val interface{}) bool {
	for _, v := range slice {
		if v == val {
			return true
		}
		// 特殊处理：json 数字 vs Go 数字
		if f1, ok1 := toFloat64(v); ok1 {
			if f2, ok2 := toFloat64(val); ok2 && f1 == f2 {
				return true
			}
		}
	}
	return false
}

// toFloat64 安全转换数值为 float64
func toFloat64(val interface{}) (float64, bool) {
	switch v := val.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case int32:
		return float64(v), true
	default:
		return 0, false
	}
}
