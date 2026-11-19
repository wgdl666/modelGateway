// Package inputschema 用同一份数据库契约提供参数发现和运行时校验；种子只用于首次登记与迁移。
package inputschema

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

// Validate 返回已校验的对象，执行编码复用它，不再重新解析大图片 JSON。
func Validate(raw, input json.RawMessage, experimental bool) (map[string]any, error) {
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(input))
	if err != nil {
		return nil, err
	}
	return ValidateValue(raw, value, experimental)
}

// ValidateValue validates a decoded JSON value. Decode numbers as json.Number.
func ValidateValue(raw json.RawMessage, value any, experimental bool) (map[string]any, error) {
	effective, err := Effective(raw, experimental)
	if err != nil {
		return nil, err
	}
	schema, err := Compile(effective)
	if err != nil {
		return nil, err
	}
	fields, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("input must be a JSON object")
	}
	if err := schema.Validate(fields); err != nil {
		return nil, inputError(err)
	}
	return fields, nil
}

// Schema 错误只输出字段路径与规则，避免把完整图片值格式化进响应。
func inputError(err error) error {
	var validation *jsonschema.ValidationError
	if !errors.As(err, &validation) {
		return err
	}
	var issues []string
	escape := strings.NewReplacer("~", "~0", "/", "~1")
	var visit func(*jsonschema.ValidationError)
	visit = func(node *jsonschema.ValidationError) {
		if len(node.Causes) > 0 {
			for _, cause := range node.Causes {
				visit(cause)
			}
			return
		}
		path := ""
		for _, part := range node.InstanceLocation {
			path += "/" + escape.Replace(part)
		}
		keyword := strings.Join(node.ErrorKind.KeywordPath(), "/")
		if keyword == "" {
			keyword = "schema"
		}
		issues = append(issues, fmt.Sprintf("%q: %s", path, keyword))
	}
	visit(validation)
	return fmt.Errorf("input schema mismatch: %s", strings.Join(issues, "; "))
}
