package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/modelinstance"
	"github.com/wgdl666/modelGateway/internal/inputschema"
)

var ErrInputSchemaMissing = errors.New("model input_schema is not configured; apply model-input-schema migration")

type ModelDefinition struct {
	ModelKey    string          `json:"model_key"`
	Protocol    string          `json:"protocol"`
	InputSchema json.RawMessage `json:"input_schema"`
}

func (s *Store) ModelDefinition(ctx context.Context, model string) (ModelDefinition, error) {
	row, err := s.db.ModelInstance.Query().Where(modelinstance.ModelKeyEQ(model)).
		Select(modelinstance.FieldModelKey, modelinstance.FieldProtocol, modelinstance.FieldInputSchema).First(ctx)
	if ent.IsNotFound(err) {
		return ModelDefinition{}, ErrModelUnregistered
	}
	if err != nil {
		return ModelDefinition{}, err
	}
	if len(row.InputSchema) == 0 {
		return ModelDefinition{}, ErrInputSchemaMissing
	}
	return ModelDefinition{row.ModelKey, row.Protocol, row.InputSchema}, nil
}

// ListModelDefinitions 目录按模型去重；健康和空闲容量仍只在派发阶段判断。
func (s *Store) ListModelDefinitions(ctx context.Context) ([]ModelDefinition, error) {
	rows, err := s.db.ModelInstance.Query().Where(modelinstance.DeletedAtIsNil()).
		Select(modelinstance.FieldModelKey, modelinstance.FieldProtocol, modelinstance.FieldInputSchema).
		Order(ent.Asc(modelinstance.FieldModelKey)).All(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]ModelDefinition, 0)
	for _, row := range rows {
		if len(result) > 0 && result[len(result)-1].ModelKey == row.ModelKey {
			continue
		}
		if len(row.InputSchema) == 0 {
			return nil, ErrInputSchemaMissing
		}
		result = append(result, ModelDefinition{row.ModelKey, row.Protocol, row.InputSchema})
	}
	return result, nil
}

// SetModelSchema 按模型一次更新全部登记，与新实例登记串行，避免随机取到不同契约。
func (s *Store) SetModelSchema(ctx context.Context, model string, raw json.RawMessage) error {
	if err := inputschema.CheckDefinition(raw); err != nil {
		return err
	}
	return s.serialize(ctx, func(tx *ent.Tx) error {
		count, err := tx.ModelInstance.Update().Where(modelinstance.ModelKeyEQ(model)).SetInputSchema(raw).Save(ctx)
		if err != nil {
			return err
		}
		if count == 0 {
			return ErrModelUnregistered
		}
		return nil
	})
}
