package protocol

import (
	"encoding/json"
	"errors"
	"github.com/wgdl666/modelGateway/internal/inputschema"
)

// 种子回归仅用于测试；运行时校验只能读取数据库契约。
func ValidateInput(kind string, input json.RawMessage) ([]InputImage, error) {
	return validateInput(kind, input, false)
}
func ValidateExperimentalInput(kind string, input json.RawMessage) ([]InputImage, error) {
	return validateInput(kind, input, true)
}
func validateInput(kind string, input json.RawMessage, experimental bool) ([]InputImage, error) {
	if !MediaProtocol(kind) {
		return nil, errors.New("unsupported media protocol")
	}
	raw, err := inputschema.Seed(kind)
	if err != nil {
		return nil, err
	}
	if err := inputschema.Validate(raw, input, experimental); err != nil {
		return nil, err
	}
	_, images, err := DecodeInput(input)
	return images, err
}
