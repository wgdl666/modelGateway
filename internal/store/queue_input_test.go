package store

import (
	"encoding/json"
	"testing"
)

func TestIdempotencyPreservesIntegerGenerationParameters(t *testing.T) {
	stored := json.RawMessage(`{"input":{"prompt":"draw","parameters":{"seed":9007199254740992,"enabled":false}}}`)
	if !sameInput(stored, json.RawMessage(`{"parameters":{"enabled":false,"seed":9007199254740992},"prompt":"draw"}`)) {
		t.Fatal("field order changed request identity")
	}
	if sameInput(stored, json.RawMessage(`{"prompt":"draw","parameters":{"seed":9007199254740993,"enabled":false}}`)) {
		t.Fatal("different integer seeds reused a result")
	}
}

func TestIdempotencyMatchesPostgresDecimalNormalization(t *testing.T) {
	stored := json.RawMessage(`{"input":{"parameters":{"guidance":0.001,"count":1000,"zero":0}}}`)
	if !sameInput(stored, json.RawMessage(`{"parameters":{"guidance":1e-3,"count":1.00e3,"zero":-0.0}}`)) {
		t.Fatal("PostgreSQL decimal normalization changed request identity")
	}
	if sameInput(stored, json.RawMessage(`{"parameters":{"guidance":1e999999999999999999,"count":1000,"zero":0}}`)) {
		t.Fatal("out-of-range exponent matched stored input")
	}
}
