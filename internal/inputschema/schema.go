// Package inputschema 用同一份数据库契约提供参数发现和运行时校验；种子只用于首次登记与迁移。
package inputschema

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed seeds/*.json
var seeds embed.FS

var compiled sync.Map

// Seed 只在模型首次登记时提供初始值；请求路径不能在数据库缺少定义时回退到种子。
func Seed(kind string) (json.RawMessage, error) {
	return seeds.ReadFile("seeds/" + kind + ".json")
}

// Effective 按现有凭据权限展开实验字段，对外返回的就是本次实际使用的校验规则。
func Effective(raw json.RawMessage, experimental bool) (json.RawMessage, error) {
	var doc map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&doc); err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, fmt.Errorf("input_schema must be a JSON Schema object")
	}
	if experimental {
		properties, _ := doc["properties"].(map[string]any)
		if extra, ok := doc["x-experimental-properties"].(map[string]any); ok {
			if properties == nil {
				properties = map[string]any{}
				doc["properties"] = properties
			}
			for name, value := range extra {
				properties[name] = value
			}
		}
	}
	delete(doc, "x-experimental-properties")
	return json.Marshal(doc)
}

// 数据库保存完整定义，只允许文档内部引用，避免校验时再读取文件或远端资源。
type localOnly struct{}

func (localOnly) Load(url string) (any, error) {
	return nil, fmt.Errorf("external schema reference is not supported: %s", url)
}

func Compile(raw json.RawMessage) (*jsonschema.Schema, error) {
	key := sha256.Sum256(raw)
	if cached, ok := compiled.Load(key); ok {
		return cached.(*jsonschema.Schema), nil
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(localOnly{})
	const resource = "https://gateway.invalid/input.json"
	if err := c.AddResource(resource, doc); err != nil {
		return nil, err
	}
	schema, err := c.Compile(resource)
	if err != nil {
		return nil, err
	}
	actual, _ := compiled.LoadOrStore(key, schema)
	return actual.(*jsonschema.Schema), nil
}

func CheckDefinition(raw json.RawMessage) error {
	for _, experimental := range []bool{false, true} {
		effective, err := Effective(raw, experimental)
		if err != nil {
			return err
		}
		if _, err := Compile(effective); err != nil {
			return err
		}
	}
	return nil
}

func Validate(raw, input json.RawMessage, experimental bool) error {
	effective, err := Effective(raw, experimental)
	if err != nil {
		return err
	}
	schema, err := Compile(effective)
	if err != nil {
		return err
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(input))
	if err != nil {
		return err
	}
	return schema.Validate(value)
}
