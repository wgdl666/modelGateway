// Package token 管理可恢复的调用凭据；密文绑定记录 ID，禁止跨记录替换。
package token

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/wgdl666/kangaroo/bizerr"
)

// Token 业务码集中用于存储、配置和 HTTP 边界。
const (
	Invalid     = 110001
	Unavailable = 110002
	Storage     = 110003
	NotFound    = 110004
)

func ValidateName(name string) error {
	if strings.TrimSpace(name) == "" || utf8.RuneCountInString(name) > 100 {
		return bizerr.New(Invalid, "调用方名称必填，且不能超过 100 个字符")
	}
	return nil
}

func Cipher(key string) (cipher.AEAD, error) {
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(raw) != 32 {
		return nil, bizerr.New(Unavailable, "API Token 加密密钥必须为 Base64 编码的 32 字节密钥")
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, bizerr.Wrap(Unavailable, err, "初始化 Token 加密失败")
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, bizerr.Wrap(Unavailable, err, "初始化 Token 加密失败")
	}
	return aead, nil
}

func Generate(aead cipher.AEAD) (id, secret, encrypted string) {
	id = uuid.NewString()
	random := make([]byte, 32)
	rand.Read(random)
	secret = "mg_" + id + "." + base64.RawURLEncoding.EncodeToString(random)
	encrypted = base64.StdEncoding.EncodeToString(aead.Seal(nil, nil, []byte(secret), []byte(id)))
	return
}

func Reveal(aead cipher.AEAD, id, encrypted string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		return "", bizerr.Wrap(Storage, err, "读取 Token 密文失败")
	}
	plain, err := aead.Open(nil, nil, raw, []byte(id))
	if err != nil {
		return "", bizerr.Wrap(Storage, err, "解密 Token 失败")
	}
	return string(plain), nil
}

func ID(secret string) string {
	id, suffix, ok := strings.Cut(strings.TrimPrefix(secret, "mg_"), ".")
	if !strings.HasPrefix(secret, "mg_") || !ok || len(suffix) != 43 {
		return ""
	}
	parsed, err := uuid.Parse(id)
	if err != nil || parsed.String() != id {
		return ""
	}
	return id
}
