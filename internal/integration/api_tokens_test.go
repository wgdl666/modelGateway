package integration

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/httpapi"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/store"
)

func TestAPITokensLifecycle(t *testing.T) {
	a, b, db := stores(t)
	cfg := testConfig()
	cfg.TokenEncryptionKey = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 32)))
	runtime, err := config.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	base := startHTTPServer(t, httpapi.New(a, runtime, nil))
	replica := startHTTPServer(t, httpapi.New(b, runtime, nil))
	call := func(host, method, path, credential, body string, want int) []byte {
		t.Helper()
		req, _ := http.NewRequest(method, host+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+credential)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("%s %s: got %d want %d: %s", method, path, resp.StatusCode, want, raw)
		}
		if strings.HasPrefix(path, "/v1/admin/api-tokens") && want < 300 && resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("missing no-store")
		}
		return raw
	}
	path := "/v1/admin/api-tokens"
	call(base, "GET", path, apiToken, "", 401)
	for _, body := range []string{`{"name":" ","created_by":"tester"}`, `{"created_by":"tester"}`, `{"name":"test"}`, `{"name":"test","created_by":"tester","extra":1}`, `{} {}`} {
		call(base, "POST", path, adminToken, body, 400)
	}
	var created store.APITokenRecord
	raw := call(base, "POST", path, adminToken, `{"name":" 联调 ","created_by":"tester"}`, 201)
	if err := json.Unmarshal(raw, &created); err != nil || created.Name != "联调" {
		t.Fatalf("invalid creation %v", err)
	}
	if bytes.Contains(raw, []byte("mg_")) {
		t.Fatal("creation should remain masked")
	}
	var revealed struct {
		Token string `json:"token"`
	}
	raw = call(replica, "GET", path+"/"+created.ID, adminToken, "", 200)
	if err := json.Unmarshal(raw, &revealed); err != nil || revealed.Token == "" {
		t.Fatal("no token")
	}
	row, err := db.APIToken.Get(t.Context(), created.ID)
	if err != nil || strings.Contains(row.EncryptedToken, revealed.Token) {
		t.Fatal("plaintext stored")
	}
	if raw2 := call(base, "GET", path+"/"+created.ID, adminToken, "", 200); !bytes.Equal(raw, raw2) {
		t.Fatal("token changed across reads")
	}
	list := call(base, "GET", path, adminToken, "", 200)
	if bytes.Contains(list, []byte(revealed.Token)) || bytes.Contains(list, []byte("encrypted_token")) {
		t.Fatal("list leaked credential")
	}
	call(base, "GET", path, revealed.Token, "", 401)
	call(base, "GET", "/v1/instances", revealed.Token, "", 401)
	spec := testInstance(protocol.VideoModel, "http://127.0.0.1:8001", 1)
	registerReady(t, a, spec)
	call(replica, "GET", "/v1/models/"+spec.ModelKey, revealed.Token, "", 200)
	call(replica, "GET", "/v1/models/"+spec.ModelKey, revealed.Token+"bad", "", 401)
	// 真实受理覆盖凭据归属，不需要模型执行器或 MQ 才能证明入队事实。
	id := uuid.NewString()
	req, _ := http.NewRequest("POST", base+"/v1/gateway/requests/"+spec.ModelKey, strings.NewReader(`{"input":{"images":[{"mime_type":"image/png","data":"eA=="}]}}`))
	req.Header.Set("Authorization", "Bearer "+revealed.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", id)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 202 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("admission: %d %s", resp.StatusCode, raw)
	}
	task, err := db.Request.Get(t.Context(), id)
	if err != nil || task.APITokenID != created.ID {
		t.Fatal("missing request attribution")
	}
	var second store.APITokenRecord
	json.Unmarshal(call(base, "POST", path, adminToken, `{"name":"另一个调用方","created_by":"tester"}`, 201), &second)
	var secondSecret struct {
		Token string `json:"token"`
	}
	json.Unmarshal(call(base, "GET", path+"/"+second.ID, adminToken, "", 200), &secondSecret)
	if second.ID == created.ID || secondSecret.Token == revealed.Token {
		t.Fatal("callers share credential")
	}

	// 独立 API Token 可跨调用方读取历史与媒体，撤销仍即时阻止后续读取。
	for _, credential := range []string{revealed.Token, secondSecret.Token} {
		call(replica, "GET", "/v1/admin/requests?request_id="+id, credential, "", 200)
		call(replica, "GET", "/v1/admin/requests/"+id, credential, "", 200)
		call(replica, "GET", "/v1/admin/requests/"+id+"/media?kind=input&index=0", credential, "", 200)
		call(replica, "GET", "/v1/admin/requests/"+id+"/media?kind=output&index=0", credential, "", 404)
		call(replica, "GET", "/v1/admin/request-stats", credential, "", 401)
	}

	call(base, "DELETE", path+"/"+created.ID, adminToken, "", 200)
	call(base, "DELETE", path+"/"+created.ID, adminToken, "", 200)
	call(replica, "GET", "/v1/models/"+spec.ModelKey, revealed.Token, "", 401)
	call(replica, "GET", "/v1/admin/requests/"+id, revealed.Token, "", 401)
	call(base, "GET", path+"/"+created.ID, adminToken, "", 404)
	task, _ = db.Request.Get(t.Context(), id)
	if string(task.State) != "queued" {
		t.Fatal("revocation changed admitted task")
	}
	call(base, "GET", "/v1/models/"+spec.ModelKey, apiToken, "", 200)
	call(replica, "GET", "/v1/models/"+spec.ModelKey, secondSecret.Token, "", 200)

}
