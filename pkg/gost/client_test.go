package gost

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestClient_LookupFailureIsNotTreatedAsMissing 回归：存在性查询失败曾被当作“不存在”，
// 删除因此静默跳过，紧接着的创建又因旧对象还在而被跳过，旧配置原样留在节点上。
func TestClient_LookupFailureIsNotTreatedAsMissing(t *testing.T) {
	var mutations int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("bad gateway"))
			return
		}
		mutations++
		_, _ = w.Write([]byte(`{"msg":"OK"}`))
	}))
	defer server.Close()

	c := NewClient(&Config{APIURL: server.URL + "/api"})

	if err := c.DeleteService("rule-1-tcp"); err == nil {
		t.Error("查询失败时 DeleteService 必须报错，而不是当作已删除")
	}
	if err := c.CreateService(&ServiceConfig{Name: "rule-1-tcp"}); err == nil {
		t.Error("查询失败时 CreateService 必须报错")
	}
	if err := c.DeleteChain("tunnel-1-chain"); err == nil {
		t.Error("查询失败时 DeleteChain 必须报错")
	}
	if ok, err := c.ChainExists("tunnel-1-chain"); err == nil || ok {
		t.Errorf("查询失败时 ChainExists 应返回错误，实际 ok=%v err=%v", ok, err)
	}
	if mutations != 0 {
		t.Errorf("查询失败时不应继续发出写请求，实际 %d 次", mutations)
	}
}

func TestClient_ExistsDistinguishesMissingFromPresent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/config/chains/present":
			_, _ = w.Write([]byte(`{"data":{"name":"present"}}`))
		default:
			// GOST 3.2.6 / 3.3.0 对不存在的对象返回 200 与 data:null
			_, _ = w.Write([]byte(`{"data":null}`))
		}
	}))
	defer server.Close()

	c := NewClient(&Config{APIURL: server.URL + "/api"})
	if ok, err := c.ChainExists("present"); err != nil || !ok {
		t.Errorf("存在的链应返回 true，实际 ok=%v err=%v", ok, err)
	}
	if ok, err := c.ChainExists("missing"); err != nil || ok {
		t.Errorf("不存在的链应返回 false 且无错误，实际 ok=%v err=%v", ok, err)
	}
}
