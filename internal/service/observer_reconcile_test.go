package service

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gost-panel/internal/model"
	"gost-panel/internal/repository"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// fakeGostNode 模拟节点侧的 GOST API，记录面板下发的观察器配置
type fakeGostNode struct {
	server *httptest.Server

	mu           sync.Mutex
	observerBody string
	putCount     int
	postCount    int
}

func newFakeGostNode(observerExists bool) *fakeGostNode {
	f := &fakeGostNode{}
	mux := http.NewServeMux()

	// 查询单个观察器是否存在
	mux.HandleFunc("/api/config/observers/observer-global", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if observerExists {
				_, _ = w.Write([]byte(`{"data":{"name":"observer-global"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":null}`))
		case http.MethodPut:
			f.mu.Lock()
			f.putCount++
			buf := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(buf)
			f.observerBody = string(buf)
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}
	})

	mux.HandleFunc("/api/config/observers", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.postCount++
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		f.observerBody = string(buf)
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	// 健康检查
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"services":[],"chains":[]}`))
	})

	f.server = httptest.NewServer(mux)
	return f
}

func (f *fakeGostNode) close() { f.server.Close() }

func (f *fakeGostNode) snapshot() (body string, put, post int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.observerBody, f.putCount, f.postCount
}

// hostPort 从 httptest 地址中拆出主机与端口，供构造节点记录使用
func hostPort(t *testing.T, url string) (string, int) {
	t.Helper()
	trimmed := strings.TrimPrefix(url, "http://")
	parts := strings.Split(trimmed, ":")
	if len(parts) != 2 {
		t.Fatalf("无法解析测试服务器地址: %s", url)
	}
	port := 0
	for _, c := range parts[1] {
		port = port*10 + int(c-'0')
	}
	return parts[0], port
}

func newReconcileTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(
		sqlite.Open(filepath.Join(t.TempDir(), "reconcile.db")),
		&gorm.Config{Logger: gormlogger.Discard},
	)
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := db.AutoMigrate(&model.SystemConfig{}, &model.GostNode{}, &model.GostRule{}, &model.GostTunnel{}); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	return db
}

// TestReconcileObserver_PushesTokenToLegacyNode 回归：从旧版本升级时，
// 节点上仍是不带令牌的旧回调地址，面板必须主动把新地址推下去。
//
// 不这样做的话，上报接口的令牌鉴权会让所有存量规则的上报被 401 拒绝 ——
// 转发本身不受影响，但面板里的流量统计会一直停在升级那一刻，且没有任何提示。
func TestReconcileObserver_PushesTokenToLegacyNode(t *testing.T) {
	// observerExists=true 模拟节点上已存在旧版观察器
	fake := newFakeGostNode(true)
	defer fake.close()

	db := newReconcileTestDB(t)
	sysRepo := repository.NewSystemConfigRepository(db)

	cfg, err := sysRepo.Get()
	if err != nil {
		t.Fatalf("读取系统配置失败: %v", err)
	}
	cfg.PanelURL = "http://panel.example.com:39100"
	if err := sysRepo.Update(cfg); err != nil {
		t.Fatalf("写入面板地址失败: %v", err)
	}

	host, port := hostPort(t, fake.server.URL)
	node := model.GostNode{ID: 1, Name: "legacy-node", Address: host, Port: port}

	svc := NewNodeHealthService(db)
	svc.reconcileObserver(node)

	body, put, post := fake.snapshot()
	if put == 0 && post == 0 {
		t.Fatal("未向节点下发观察器配置")
	}
	// 观察器已存在时应走 PUT 覆盖，而不是跳过
	if put == 0 {
		t.Error("已存在的观察器应通过 PUT 覆盖，否则旧地址会被永久保留")
	}
	if !strings.Contains(body, "token=") {
		t.Errorf("下发的回调地址中不含上报令牌: %s", body)
	}
	if !strings.Contains(body, "/api/v1/observer/report") {
		t.Errorf("回调地址不正确: %s", body)
	}
	if !strings.Contains(body, cfg.ObserverToken) {
		t.Error("下发的令牌与系统配置中的不一致")
	}
}

// TestReconcileObserver_OnlyOncePerNode 对账成功后不应每次健康检查都重复下发
func TestReconcileObserver_OnlyOncePerNode(t *testing.T) {
	fake := newFakeGostNode(true)
	defer fake.close()

	db := newReconcileTestDB(t)
	sysRepo := repository.NewSystemConfigRepository(db)
	cfg, _ := sysRepo.Get()
	cfg.PanelURL = "http://panel.example.com:39100"
	_ = sysRepo.Update(cfg)

	host, port := hostPort(t, fake.server.URL)
	node := model.GostNode{ID: 1, Name: "n1", Address: host, Port: port}

	svc := NewNodeHealthService(db)
	for i := 0; i < 5; i++ {
		svc.reconcileObserver(node)
	}

	_, put, post := fake.snapshot()
	if put+post != 1 {
		t.Errorf("对账应只执行一次，实际下发 %d 次（健康检查每 5 秒一轮，重复下发会压垮节点）", put+post)
	}
}

// TestReconcileObserver_RetriesAfterOffline 节点掉线再上线后应重新对账，
// 因为它可能在离线期间被重装或重置过配置
func TestReconcileObserver_RetriesAfterOffline(t *testing.T) {
	fake := newFakeGostNode(true)
	defer fake.close()

	db := newReconcileTestDB(t)
	sysRepo := repository.NewSystemConfigRepository(db)
	cfg, _ := sysRepo.Get()
	cfg.PanelURL = "http://panel.example.com:39100"
	_ = sysRepo.Update(cfg)

	host, port := hostPort(t, fake.server.URL)
	node := model.GostNode{ID: 1, Name: "n1", Address: host, Port: port}

	svc := NewNodeHealthService(db)
	svc.reconcileObserver(node)

	// 模拟掉线：健康检查会清除标记
	svc.observerSynced.Delete(node.ID)
	svc.reconcileObserver(node)

	_, put, post := fake.snapshot()
	if put+post != 2 {
		t.Errorf("掉线后重新上线应再次对账，实际下发 %d 次", put+post)
	}
}

// TestReconcileObserver_NoPanelURLDoesNotSpam 未配置面板地址时不应每轮重试。
// 这是配置待办而非瞬时故障，每 5 秒重试一次只会刷屏。
func TestReconcileObserver_NoPanelURLDoesNotSpam(t *testing.T) {
	fake := newFakeGostNode(false)
	defer fake.close()

	db := newReconcileTestDB(t)
	host, port := hostPort(t, fake.server.URL)
	node := model.GostNode{ID: 1, Name: "n1", Address: host, Port: port}

	svc := NewNodeHealthService(db)
	for i := 0; i < 10; i++ {
		svc.reconcileObserver(node)
	}

	// 未配置 PanelURL，EnsureGlobalObserver 会直接返回错误，不应触达节点
	_, put, post := fake.snapshot()
	if put+post != 0 {
		t.Errorf("未配置面板地址时不应下发观察器，实际 %d 次", put+post)
	}

	// 失败后的重试受 observerRetryInterval 约束
	last, ok := svc.observerSynced.Load(node.ID)
	if !ok {
		t.Fatal("失败后应记录尝试时间以限制重试频率")
	}
	if ts, isTime := last.(time.Time); !isTime || time.Since(ts) > time.Minute {
		t.Error("记录的尝试时间不正确")
	}
}
