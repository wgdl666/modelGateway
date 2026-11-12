package httpapi

import (
	"bytes"
	"context"
	"crypto/cipher"
	"encoding/json"
	"io"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/wgdl666/kangaroo/bizerr"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/token"
)

const apiTokenID = "api_token_id"

func tokenError(c *app.RequestContext, err error) {
	code, _ := bizerr.Code(err)
	switch code {
	case token.Invalid:
		writeError(c, 400, "invalid_token_input", bizerr.Message(err), "")
	case token.NotFound:
		writeError(c, 404, "token_not_found", bizerr.Message(err), "")
	case token.Unavailable:
		writeError(c, 503, "token_unavailable", "当前环境尚未配置 API Token 加密密钥", "")
	default:
		writeError(c, 503, "token_storage_error", "API Token 服务暂不可用", "")
	}
}

func tokenCipher(ctx context.Context, c *app.RequestContext) cipher.AEAD {
	aead, err := token.Cipher(requestConfig(ctx).TokenEncryptionKey)
	if err != nil {
		tokenError(c, err)
		return nil
	}
	return aead
}

func (s *Server) apiTokens(ctx context.Context, c *app.RequestContext) {
	c.Header("Cache-Control", "no-store")
	aead := tokenCipher(ctx, c)
	if aead == nil {
		return
	}
	if string(c.Method()) == "GET" {
		rows, err := s.store.ListAPITokens(ctx)
		if err != nil {
			tokenError(c, err)
			return
		}
		writeJSON(c, 200, map[string]any{"items": rows})
		return
	}
	var input struct {
		Name      string `json:"name"`
		CreatedBy string `json:"created_by"`
	}
	// 管理入口同样校验边界，不接受额外字段或尾随 JSON。
	raw, err := io.ReadAll(io.LimitReader(c.RequestBodyStream(), 4097))
	if err != nil || len(raw) > 4096 {
		writeError(c, 400, "invalid_token_input", "无效的 Token 参数", "")
		return
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&input); err != nil {
		writeError(c, 400, "invalid_token_input", "无效的 Token 参数", "")
		return
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		writeError(c, 400, "invalid_token_input", "仅允许一个 JSON 对象", "")
		return
	}
	row, err := s.store.CreateAPIToken(ctx, aead, strings.TrimSpace(input.Name), input.CreatedBy)
	if err != nil {
		tokenError(c, err)
		return
	}
	writeJSON(c, 201, row)
}

func (s *Server) apiToken(ctx context.Context, c *app.RequestContext) {
	c.Header("Cache-Control", "no-store")
	id := c.Param("id")
	if !protocol.ValidID(id) {
		writeError(c, 400, "invalid_token_input", "无效的 Token ID", "")
		return
	}
	// 撤销不依赖解密密钥，密钥故障时仍可停用凭据。
	if string(c.Method()) == "DELETE" {
		if err := s.store.RevokeAPIToken(ctx, id); err != nil {
			tokenError(c, err)
			return
		}
		writeJSON(c, 200, map[string]bool{"revoked": true})
		return
	}
	aead := tokenCipher(ctx, c)
	if aead == nil {
		return
	}
	secret, err := s.store.RevealAPIToken(ctx, aead, id)
	if err != nil {
		tokenError(c, err)
		return
	}
	writeJSON(c, 200, map[string]string{"token": secret})
}
