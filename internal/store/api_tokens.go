package store

import (
	"context"
	"crypto/cipher"
	"crypto/subtle"
	"strings"
	"time"

	"github.com/wgdl666/kangaroo/bizerr"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/apitoken"
	"github.com/wgdl666/modelGateway/internal/token"
)

// APITokenRecord 只公开管理元数据；只有显式查看接口返回明文。
type APITokenRecord struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	CreatedBy string     `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at"`
}

func (s *Store) CreateAPIToken(ctx context.Context, aead cipher.AEAD, name, creator string) (*APITokenRecord, error) {
	name = strings.TrimSpace(name)
	if err := token.ValidateName(name); err != nil {
		return nil, err
	}
	if strings.TrimSpace(creator) == "" || len(creator) > 256 {
		return nil, bizerr.New(token.Invalid, "创建人不能为空或超过 256 字节")
	}
	id, _, encrypted := token.Generate(aead)
	row, err := s.db.APIToken.Create().SetID(id).SetName(name).SetCreatedBy(creator).SetEncryptedToken(encrypted).Save(ctx)
	if err != nil {
		return nil, bizerr.Wrap(token.Storage, err, "创建 API Token 失败")
	}
	return &APITokenRecord{ID: row.ID, Name: row.Name, CreatedBy: row.CreatedBy, CreatedAt: row.CreatedAt}, nil
}

func (s *Store) ListAPITokens(ctx context.Context) ([]*APITokenRecord, error) {
	// 管理列表明确包含已撤销记录，便于审计；鉴权与查看始终排除软删除。
	rows, err := s.db.APIToken.Query().Select(apitoken.FieldID, apitoken.FieldName, apitoken.FieldCreatedBy, apitoken.FieldCreatedAt, apitoken.FieldDeletedAt).
		Order(ent.Desc(apitoken.FieldCreatedAt), ent.Desc(apitoken.FieldID)).All(ctx)
	if err != nil {
		return nil, bizerr.Wrap(token.Storage, err, "读取 API Token 列表失败")
	}
	result := make([]*APITokenRecord, 0, len(rows))
	for _, row := range rows {
		result = append(result, &APITokenRecord{ID: row.ID, Name: row.Name, CreatedBy: row.CreatedBy, CreatedAt: row.CreatedAt, RevokedAt: row.DeletedAt})
	}
	return result, nil
}

func (s *Store) RevealAPIToken(ctx context.Context, aead cipher.AEAD, id string) (string, error) {
	row, err := s.db.APIToken.Query().Where(apitoken.IDEQ(id), apitoken.DeletedAtIsNil()).Only(ctx)
	if ent.IsNotFound(err) {
		return "", bizerr.New(token.NotFound, "API Token 不存在或已撤销")
	}
	if err != nil {
		return "", bizerr.Wrap(token.Storage, err, "读取 API Token 失败")
	}
	return token.Reveal(aead, row.ID, row.EncryptedToken)
}

func (s *Store) AuthenticateAPIToken(ctx context.Context, aead cipher.AEAD, secret string) (string, error) {
	id := token.ID(secret)
	if id == "" {
		return "", nil
	}
	plain, err := s.RevealAPIToken(ctx, aead, id)
	if code, _ := bizerr.Code(err); code == token.NotFound {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if subtle.ConstantTimeCompare([]byte(plain), []byte(secret)) != 1 {
		return "", nil
	}
	return id, nil
}

func (s *Store) RevokeAPIToken(ctx context.Context, id string) error {
	// CAS 使重复撤销幂等，不改写首次撤销时间，也不取消已受理任务。
	_, err := s.db.APIToken.Update().Where(apitoken.IDEQ(id), apitoken.DeletedAtIsNil()).SetDeletedAt(time.Now().UTC()).Save(ctx)
	if err != nil {
		return bizerr.Wrap(token.Storage, err, "撤销 API Token 失败")
	}
	return nil
}
