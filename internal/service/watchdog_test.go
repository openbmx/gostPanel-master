package service

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
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

// fakeGost 在内存里模拟节点侧 GOST 的配置 API（与真实 3.2.6/3.3.0 的响应格式一致）：
// 服务、链路、观察器的增删查，/config 汇总读取与保存。
type fakeGost struct {
	server *httptest.Server

	mu         sync.Mutex
	services   map[string]json.RawMessage
	states     map[string]string // 服务名 -> 运行状态，缺省为 ready
	chains     map[string]json.RawMessage
	observers  map[string]json.RawMessage
	failCreate map[string]bool // 创建这些服务时返回 500（模拟端口被占）
	failGet    map[string]int  // 查询单个对象时返回 502 的剩余次数（模拟节点 API 抖动）
	failSave   bool            // 保存配置时返回 500
	ghosts     []string        // 只在下一次 GET /config 里出现一次的服务（模拟快照之后被用户删掉的对象）
	calls      []string
}

func newFakeGost(t *testing.T) *fakeGost {
	t.Helper()
	f := &fakeGost{
		services:   make(map[string]json.RawMessage),
		states:     make(map[string]string),
		chains:     make(map[string]json.RawMessage),
		observers:  make(map[string]json.RawMessage),
		failCreate: make(map[string]bool),
		failGet:    make(map[string]int),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/config", f.handleConfig)
	for _, kind := range []string{"services", "chains", "observers"} {
		mux.HandleFunc("/api/config/"+kind, f.collection(kind))
		mux.HandleFunc("/api/config/"+kind+"/", f.item(kind))
	}
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// store 按类型取当前的存储（restart 会整体替换，必须每次现取）。调用方需持有 f.mu。
func (f *fakeGost) store(kind string) map[string]json.RawMessage {
	switch kind {
	case "services":
		return f.services
	case "chains":
		return f.chains
	default:
		return f.observers
	}
}

func (f *fakeGost) handleConfig(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodPost {
		f.calls = append(f.calls, "SAVE")
		if f.failSave {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":500,"msg":"save failed"}`))
			return
		}
		_, _ = w.Write([]byte(`{"msg":"OK"}`))
		return
	}
	cfg := map[string][]map[string]any{"services": {}, "chains": {}}
	for _, name := range sortedKeys(f.services) {
		var svc map[string]any
		_ = json.Unmarshal(f.services[name], &svc)
		state := f.states[name]
		if state == "" {
			state = "ready"
		}
		svc["status"] = map[string]any{"state": state}
		cfg["services"] = append(cfg["services"], svc)
	}
	for _, name := range sortedKeys(f.chains) {
		var chain map[string]any
		_ = json.Unmarshal(f.chains[name], &chain)
		cfg["chains"] = append(cfg["chains"], chain)
	}
	for _, name := range f.ghosts {
		cfg["services"] = append(cfg["services"], map[string]any{"name": name, "status": map[string]any{"state": "ready"}})
	}
	f.ghosts = nil
	_ = json.NewEncoder(w).Encode(cfg)
}

func (f *fakeGost) addGhost(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ghosts = append(f.ghosts, name)
}

func (f *fakeGost) collection(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var obj struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(body, &obj)

		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, fmt.Sprintf("CREATE %s %s", kind, obj.Name))
		if f.failCreate[obj.Name] {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":500,"msg":"listen tcp: address already in use"}`))
			return
		}
		f.store(kind)[obj.Name] = body
		delete(f.states, obj.Name)
		_, _ = w.Write([]byte(`{"msg":"OK"}`))
	}
}

func (f *fakeGost) item(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		f.mu.Lock()
		defer f.mu.Unlock()
		store := f.store(kind)
		switch r.Method {
		case http.MethodGet:
			if f.failGet[name] > 0 {
				f.failGet[name]--
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`bad gateway`))
				return
			}
			if obj, ok := store[name]; ok {
				_, _ = fmt.Fprintf(w, `{"data":%s}`, obj)
				return
			}
			_, _ = w.Write([]byte(`{"data":null}`))
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			f.calls = append(f.calls, fmt.Sprintf("UPDATE %s %s", kind, name))
			store[name] = body
			_, _ = w.Write([]byte(`{"msg":"OK"}`))
		case http.MethodDelete:
			f.calls = append(f.calls, fmt.Sprintf("DELETE %s %s", kind, name))
			delete(store, name)
			delete(f.states, name)
			_, _ = w.Write([]byte(`{"msg":"OK"}`))
		}
	}
}

// restart 模拟节点进程重启：运行时配置全部丢失
func (f *fakeGost) restart() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.services = make(map[string]json.RawMessage)
	f.states = make(map[string]string)
	f.chains = make(map[string]json.RawMessage)
	f.observers = make(map[string]json.RawMessage)
	f.calls = nil
}

func (f *fakeGost) put(kind, name, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.store(kind)[name] = json.RawMessage(body)
}

func (f *fakeGost) setState(name, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[name] = state
}

func (f *fakeGost) setFailCreate(name string, fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failCreate[name] = fail
}

func (f *fakeGost) setFailGet(name string, times int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failGet[name] = times
}

func (f *fakeGost) setFailSave(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failSave = fail
}

// serviceChain 返回服务 handler 引用的 chain；服务不存在时第二个返回值为 false
func (f *fakeGost) serviceChain(name string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, ok := f.services[name]
	if !ok {
		return "", false
	}
	var svc struct {
		Handler struct {
			Chain string `json:"chain"`
		} `json:"handler"`
	}
	_ = json.Unmarshal(raw, &svc)
	return svc.Handler.Chain, true
}

// danglingChainRefs 返回引用了节点上不存在的 chain 的服务 —— 这些服务的流量会绕过隧道直连目标
func (f *fakeGost) danglingChainRefs() []string {
	var dangling []string
	for _, name := range f.serviceNames() {
		if chain, _ := f.serviceChain(name); chain != "" && !f.has("chains", chain) {
			dangling = append(dangling, name+"->"+chain)
		}
	}
	return dangling
}

func (f *fakeGost) has(kind, name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.store(kind)[name]
	return ok
}

func (f *fakeGost) service(t *testing.T, name string) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, ok := f.services[name]
	if !ok {
		t.Fatalf("节点上缺少服务 %s", name)
	}
	var svc map[string]any
	if err := json.Unmarshal(raw, &svc); err != nil {
		t.Fatalf("解析服务 %s 失败: %v", name, err)
	}
	return svc
}

func (f *fakeGost) serviceNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return sortedKeys(f.services)
}

func (f *fakeGost) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeGost) resetCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// ==================== 测试环境 ====================

type watchdogEnv struct {
	t     *testing.T
	db    *gorm.DB
	sync  *RuleSyncService
	clock time.Time
}

func newWatchdogEnv(t *testing.T) *watchdogEnv {
	t.Helper()
	initObserverServiceTestLogger(t)

	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "watchdog.db")), &gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := db.AutoMigrate(&model.SystemConfig{}, &model.GostNode{}, &model.GostRule{}, &model.GostTunnel{}, &model.OperationLog{}); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	// 规则启动/恢复需要观察器，观察器需要面板地址
	sysRepo := repository.NewSystemConfigRepository(db)
	cfg, err := sysRepo.Get()
	if err != nil {
		t.Fatalf("读取系统配置失败: %v", err)
	}
	cfg.PanelURL = "http://panel.example.com:39100"
	if err := sysRepo.Update(cfg); err != nil {
		t.Fatalf("写入面板地址失败: %v", err)
	}

	env := &watchdogEnv{t: t, db: db, sync: NewRuleSyncService(db), clock: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	env.sync.watchdog.now = func() time.Time { return env.clock }
	return env
}

func (e *watchdogEnv) tick() { e.sync.syncAll() }

func (e *watchdogEnv) advance(d time.Duration) { e.clock = e.clock.Add(d) }

func (e *watchdogEnv) node(name string, fake *fakeGost) *model.GostNode {
	e.t.Helper()
	host, port := hostPort(e.t, fake.server.URL)
	node := &model.GostNode{Name: name, Address: host, Port: port, Status: model.NodeStatusOnline}
	if err := e.db.Create(node).Error; err != nil {
		e.t.Fatalf("创建节点失败: %v", err)
	}
	return node
}

func (e *watchdogEnv) tunnel(name string, entry, exit *model.GostNode, relayPort int, status model.TunnelStatus) *model.GostTunnel {
	e.t.Helper()
	tunnel := &model.GostTunnel{
		Name:        name,
		EntryNodeID: entry.ID,
		ExitNodeID:  exit.ID,
		Protocol:    "tcp",
		RelayPort:   relayPort,
		Hops:        []model.TunnelHop{{NodeID: exit.ID, Protocol: "tcp", RelayPort: relayPort}},
		Status:      status,
	}
	if err := e.db.Create(tunnel).Error; err != nil {
		e.t.Fatalf("创建隧道失败: %v", err)
	}
	if status != model.TunnelStatusStopped {
		tunnel.ChainID = fmt.Sprintf("tunnel-%d-chain", tunnel.ID)
		tunnel.ServiceID = fmt.Sprintf("relay-tunnel-%d", tunnel.ID)
		if err := e.db.Save(tunnel).Error; err != nil {
			e.t.Fatalf("更新隧道失败: %v", err)
		}
	}
	return tunnel
}

func (e *watchdogEnv) tunnelRule(name string, tunnel *model.GostTunnel, port int, status model.RuleStatus) *model.GostRule {
	e.t.Helper()
	rule := &model.GostRule{
		Name:            name,
		Type:            model.RuleTypeTunnel,
		TunnelID:        &tunnel.ID,
		PrimaryTunnelID: &tunnel.ID,
		ListenPort:      port,
		Targets:         []string{"10.0.0.9:80"},
		Status:          status,
	}
	if err := e.db.Create(rule).Error; err != nil {
		e.t.Fatalf("创建规则失败: %v", err)
	}
	return rule
}

func (e *watchdogEnv) forwardRule(name string, node *model.GostNode, port int, status model.RuleStatus) *model.GostRule {
	e.t.Helper()
	rule := &model.GostRule{
		Name:       name,
		Type:       model.RuleTypeForward,
		NodeID:     &node.ID,
		ListenPort: port,
		Targets:    []string{"10.0.0.9:80"},
		Status:     status,
	}
	if err := e.db.Create(rule).Error; err != nil {
		e.t.Fatalf("创建规则失败: %v", err)
	}
	return rule
}

func (e *watchdogEnv) ruleStatus(id uint) model.RuleStatus {
	e.t.Helper()
	var rule model.GostRule
	if err := e.db.First(&rule, id).Error; err != nil {
		e.t.Fatalf("读取规则失败: %v", err)
	}
	return rule.Status
}

func (e *watchdogEnv) tunnelStatus(id uint) model.TunnelStatus {
	e.t.Helper()
	var tunnel model.GostTunnel
	if err := e.db.First(&tunnel, id).Error; err != nil {
		e.t.Fatalf("读取隧道失败: %v", err)
	}
	return tunnel.Status
}

func (e *watchdogEnv) logs(action string) []model.OperationLog {
	e.t.Helper()
	var logs []model.OperationLog
	if err := e.db.Where("action = ?", action).Order("id").Find(&logs).Error; err != nil {
		e.t.Fatalf("读取操作日志失败: %v", err)
	}
	return logs
}

// runningTunnelOnNodes 在两个假节点上摆好一条正常运行的隧道
func runningTunnelOnNodes(e *watchdogEnv, entryFake, exitFake *fakeGost) (*model.GostNode, *model.GostNode, *model.GostTunnel) {
	entry := e.node("entry", entryFake)
	exit := e.node("exit", exitFake)
	tunnel := e.tunnel("t1", entry, exit, 8443, model.TunnelStatusRunning)
	entryFake.put("chains", tunnel.ChainID, fmt.Sprintf(`{"name":%q}`, tunnel.ChainID))
	exitFake.put("services", tunnel.ServiceID, fmt.Sprintf(`{"name":%q,"addr":":8443"}`, tunnel.ServiceID))
	return entry, exit, tunnel
}

// ==================== 用例 ====================

// TestWatchdog_RestoresRelayAfterExitNodeRestart 回归 issue #3：出口节点重启后 relay 丢失，
// 旧实现只检查入口 chain，面板一直显示“运行中”，用户必须手动关开隧道。
func TestWatchdog_RestoresRelayAfterExitNodeRestart(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitFake := newFakeGost(t), newFakeGost(t)
	_, _, tunnel := runningTunnelOnNodes(e, entryFake, exitFake)

	exitFake.restart()
	e.tick()

	relay := exitFake.service(t, "relay-tunnel-1")
	if relay["addr"] != ":8443" {
		t.Errorf("relay 监听地址不对: %v", relay["addr"])
	}
	if handler, _ := relay["handler"].(map[string]any); handler["type"] != "relay" {
		t.Errorf("relay handler 不对: %v", relay["handler"])
	}
	// 出口 relay 负责隧道流量统计，恢复时必须一并恢复观察器
	if relay["observer"] != "observer-global" || !exitFake.has("observers", "observer-global") {
		t.Error("恢复的 relay 没有挂上流量观察器，隧道流量统计会停止")
	}
	if got := e.tunnelStatus(tunnel.ID); got != model.TunnelStatusRunning {
		t.Errorf("恢复后隧道状态应为 running，实际 %s", got)
	}
	if logs := e.logs(model.ActionRecover); len(logs) != 1 || logs[0].ResourceID != tunnel.ID {
		t.Errorf("应记录一条隧道自动恢复日志，实际 %+v", logs)
	}

	// 恢复完成后再巡检不应重复下发
	exitFake.resetCalls()
	e.tick()
	for _, call := range exitFake.callLog() {
		if strings.HasPrefix(call, "CREATE") || strings.HasPrefix(call, "DELETE") {
			t.Errorf("配置已一致时不应再改动节点，实际调用: %v", exitFake.callLog())
			break
		}
	}
}

// TestWatchdog_RestoresChainAndRulesAfterEntryNodeRestart 入口节点重启：chain 和规则服务都没了，
// 旧实现会把隧道和规则都改写成 stopped，用户的“应当运行”意图就此丢失。
func TestWatchdog_RestoresChainAndRulesAfterEntryNodeRestart(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitFake := newFakeGost(t), newFakeGost(t)
	entry, _, tunnel := runningTunnelOnNodes(e, entryFake, exitFake)
	tunnelRule := e.tunnelRule("via-tunnel", tunnel, 10001, model.RuleStatusRunning)
	forward := e.forwardRule("direct", entry, 10002, model.RuleStatusRunning)

	entryFake.restart()
	e.tick()

	if !entryFake.has("chains", tunnel.ChainID) {
		t.Fatal("入口节点上的 chain 未恢复")
	}
	for _, suffix := range []string{"-tcp", "-udp"} {
		svc := entryFake.service(t, fmt.Sprintf("rule-%d%s", tunnelRule.ID, suffix))
		handler, _ := svc["handler"].(map[string]any)
		if handler["chain"] != tunnel.ChainID {
			t.Errorf("隧道规则恢复后必须挂回隧道 chain，实际 handler=%v", handler)
		}
		if svc["addr"] != ":10001" {
			t.Errorf("隧道规则监听地址不对: %v", svc["addr"])
		}

		direct := entryFake.service(t, fmt.Sprintf("rule-%d%s", forward.ID, suffix))
		if handler, _ := direct["handler"].(map[string]any); handler["chain"] != nil {
			t.Errorf("端口转发规则不应挂 chain，实际 handler=%v", handler)
		}
	}
	if got := e.tunnelStatus(tunnel.ID); got != model.TunnelStatusRunning {
		t.Errorf("隧道状态应保持 running，实际 %s", got)
	}
	for _, id := range []uint{tunnelRule.ID, forward.ID} {
		if got := e.ruleStatus(id); got != model.RuleStatusRunning {
			t.Errorf("规则 %d 状态应保持 running，实际 %s", id, got)
		}
	}
}

// TestWatchdog_CleansUpStoppedAndDeletedLeftovers 节点离线期间被停止/删除的资源，
// 节点重新上线后要清理；面板不认识的对象一律不碰。
func TestWatchdog_CleansUpStoppedAndDeletedLeftovers(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitFake := newFakeGost(t), newFakeGost(t)
	entry := e.node("entry", entryFake)
	exit := e.node("exit", exitFake)

	stoppedRule := e.forwardRule("stopped", entry, 10001, model.RuleStatusStopped)
	deletedRule := e.forwardRule("deleted", entry, 10002, model.RuleStatusRunning)
	if err := e.db.Delete(&model.GostRule{}, deletedRule.ID).Error; err != nil {
		t.Fatalf("删除规则失败: %v", err)
	}
	stoppedTunnel := e.tunnel("stopped-tunnel", entry, exit, 8443, model.TunnelStatusStopped)

	leftovers := []string{
		fmt.Sprintf("rule-%d-tcp", stoppedRule.ID),
		fmt.Sprintf("rule-%d-udp", stoppedRule.ID),
		fmt.Sprintf("rule-%d-tcp", deletedRule.ID),
	}
	for _, name := range leftovers {
		entryFake.put("services", name, fmt.Sprintf(`{"name":%q}`, name))
	}
	chainName, relayName := fmt.Sprintf("tunnel-%d-chain", stoppedTunnel.ID), fmt.Sprintf("relay-tunnel-%d", stoppedTunnel.ID)
	entryFake.put("chains", chainName, fmt.Sprintf(`{"name":%q}`, chainName))
	exitFake.put("services", relayName, fmt.Sprintf(`{"name":%q}`, relayName))
	// 不属于本面板的对象：编号在数据库里查不到，或者根本不是面板的命名
	entryFake.put("services", "rule-999-tcp", `{"name":"rule-999-tcp"}`)
	entryFake.put("services", "my-socks5", `{"name":"my-socks5"}`)

	e.tick()

	if got := entryFake.serviceNames(); strings.Join(got, ",") != "my-socks5,rule-999-tcp" {
		t.Errorf("入口节点上应只剩与面板无关的服务，实际 %v", got)
	}
	if entryFake.has("chains", fmt.Sprintf("tunnel-%d-chain", stoppedTunnel.ID)) {
		t.Error("已停止隧道的 chain 残留未清理")
	}
	if exitFake.has("services", fmt.Sprintf("relay-tunnel-%d", stoppedTunnel.ID)) {
		t.Error("已停止隧道的 relay 残留未清理")
	}
	if logs := e.logs(model.ActionCleanup); len(logs) != 5 {
		t.Errorf("应为每个清理掉的对象记录一条日志，实际 %d 条", len(logs))
	}
	// 清理不应改写用户设置的状态
	if got := e.ruleStatus(stoppedRule.ID); got != model.RuleStatusStopped {
		t.Errorf("已停止规则的状态被改动: %s", got)
	}
}

// TestWatchdog_BacksOffAndMarksErrorWhenRestoreFails 恢复失败时标记 error、按退避重试，
// 失败日志只记一次；条件恢复后自动转回 running。
func TestWatchdog_BacksOffAndMarksErrorWhenRestoreFails(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitFake := newFakeGost(t), newFakeGost(t)
	_, _, tunnel := runningTunnelOnNodes(e, entryFake, exitFake)

	exitFake.restart()
	exitFake.setFailCreate(tunnel.ServiceID, true)
	attempts := func() int {
		n := 0
		for _, call := range exitFake.callLog() {
			if call == "CREATE services "+tunnel.ServiceID {
				n++
			}
		}
		return n
	}

	e.tick()
	if attempts() != 1 {
		t.Fatalf("首次发现缺失应立即尝试恢复，实际尝试 %d 次", attempts())
	}
	if got := e.tunnelStatus(tunnel.ID); got != model.TunnelStatusError {
		t.Fatalf("恢复失败后隧道应标记为 error，实际 %s", got)
	}

	// 退避窗口内（10s）不重试
	e.advance(5 * time.Second)
	e.tick()
	if attempts() != 1 {
		t.Fatalf("退避期间不应重试，实际尝试 %d 次", attempts())
	}

	// 窗口过后重试，仍然失败；第二次退避为 20s
	e.advance(6 * time.Second)
	e.tick()
	if attempts() != 2 {
		t.Fatalf("退避结束后应重试，实际尝试 %d 次", attempts())
	}
	e.advance(15 * time.Second)
	e.tick()
	if attempts() != 2 {
		t.Fatalf("第二次退避应为 20s，实际尝试 %d 次", attempts())
	}
	if logs := e.logs(model.ActionRecover); len(logs) != 1 || !strings.Contains(logs[0].Details, "失败") {
		t.Errorf("连续失败只应记录一条失败日志，实际 %+v", logs)
	}

	// 端口释放后恢复成功，状态转回 running
	exitFake.setFailCreate(tunnel.ServiceID, false)
	e.advance(6 * time.Second)
	e.tick()
	if !exitFake.has("services", tunnel.ServiceID) {
		t.Fatal("条件恢复后 relay 应被重建")
	}
	if got := e.tunnelStatus(tunnel.ID); got != model.TunnelStatusRunning {
		t.Errorf("恢复成功后隧道应转回 running，实际 %s", got)
	}
}

// TestWatchdog_RestartsOnlyFailedSubService UDP 子服务失败时只重建它，
// 不能连带重建 TCP 子服务把存量连接打断。
func TestWatchdog_RestartsOnlyFailedSubService(t *testing.T) {
	e := newWatchdogEnv(t)
	fake := newFakeGost(t)
	node := e.node("n1", fake)
	rule := e.forwardRule("r1", node, 10001, model.RuleStatusRunning)

	tcp, udp := fmt.Sprintf("rule-%d-tcp", rule.ID), fmt.Sprintf("rule-%d-udp", rule.ID)
	fake.put("services", tcp, fmt.Sprintf(`{"name":%q}`, tcp))
	fake.put("services", udp, fmt.Sprintf(`{"name":%q}`, udp))
	fake.setState(udp, "failed")

	e.tick()

	calls := strings.Join(fake.callLog(), "\n")
	if strings.Contains(calls, "DELETE services "+tcp) || strings.Contains(calls, "CREATE services "+tcp) {
		t.Errorf("健康的 TCP 子服务不应被动到，调用记录:\n%s", calls)
	}
	if !strings.Contains(calls, "DELETE services "+udp) || !strings.Contains(calls, "CREATE services "+udp) {
		t.Errorf("失败的 UDP 子服务应被重建，调用记录:\n%s", calls)
	}
	if got := e.ruleStatus(rule.ID); got != model.RuleStatusRunning {
		t.Errorf("重建后规则应为 running，实际 %s", got)
	}
}

// TestWatchdog_SkipsResourceBusyWithUserOperation 用户正在操作（持有锁）时看门狗让路
func TestWatchdog_SkipsResourceBusyWithUserOperation(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitFake := newFakeGost(t), newFakeGost(t)
	_, _, tunnel := runningTunnelOnNodes(e, entryFake, exitFake)
	exitFake.restart()

	unlock := lockTunnel(tunnel.ID)
	e.tick()
	unlock()
	if exitFake.has("services", tunnel.ServiceID) {
		t.Fatal("隧道正被其他操作处理时看门狗不应介入")
	}

	e.tick()
	if !exitFake.has("services", tunnel.ServiceID) {
		t.Fatal("锁释放后的下一轮应完成恢复")
	}
}

// TestWatchdog_LeavesOfflineNodeAlone 节点离线时既不改状态也不尝试下发
func TestWatchdog_LeavesOfflineNodeAlone(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitFake := newFakeGost(t), newFakeGost(t)
	_, exit, tunnel := runningTunnelOnNodes(e, entryFake, exitFake)
	exitFake.restart()
	if err := e.db.Model(exit).Update("status", model.NodeStatusOffline).Error; err != nil {
		t.Fatalf("标记节点离线失败: %v", err)
	}

	e.tick()

	if exitFake.has("services", tunnel.ServiceID) || len(exitFake.callLog()) != 0 {
		t.Errorf("离线节点不应被访问，实际调用 %v", exitFake.callLog())
	}
	if got := e.tunnelStatus(tunnel.ID); got != model.TunnelStatusRunning {
		t.Errorf("节点离线时应保留最后已知状态，实际 %s", got)
	}
}

// TestRuleStop_StopsErrorRuleAndCleansNode error 状态的规则必须能被手动停止，
// 否则用户无法终止看门狗的重试。
func TestRuleStop_StopsErrorRuleAndCleansNode(t *testing.T) {
	e := newWatchdogEnv(t)
	fake := newFakeGost(t)
	node := e.node("n1", fake)
	rule := e.forwardRule("r1", node, 10001, model.RuleStatusError)
	tcp := fmt.Sprintf("rule-%d-tcp", rule.ID)
	fake.put("services", tcp, fmt.Sprintf(`{"name":%q}`, tcp))

	if err := e.sync.ruleService.Stop(rule.ID, 1, "admin", "127.0.0.1", "test"); err != nil {
		t.Fatalf("停止规则失败: %v", err)
	}
	if fake.has("services", tcp) {
		t.Error("停止 error 规则后节点上的服务应被删除")
	}
	if got := e.ruleStatus(rule.ID); got != model.RuleStatusStopped {
		t.Errorf("停止后规则应为 stopped，实际 %s", got)
	}
}

// TestTunnelStop_StopsErrorTunnel 同上，隧道版本
func TestTunnelStop_StopsErrorTunnel(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitFake := newFakeGost(t), newFakeGost(t)
	_, _, tunnel := runningTunnelOnNodes(e, entryFake, exitFake)
	if err := e.db.Model(tunnel).Update("status", model.TunnelStatusError).Error; err != nil {
		t.Fatalf("标记隧道异常失败: %v", err)
	}

	if err := e.sync.ruleService.tunnelService.Stop(tunnel.ID, 1, "admin", "127.0.0.1", "test"); err != nil {
		t.Fatalf("停止隧道失败: %v", err)
	}
	if entryFake.has("chains", tunnel.ChainID) || exitFake.has("services", tunnel.ServiceID) {
		t.Error("停止 error 隧道后节点上的 chain/relay 应被删除")
	}
	if got := e.tunnelStatus(tunnel.ID); got != model.TunnelStatusStopped {
		t.Errorf("停止后隧道应为 stopped，实际 %s", got)
	}
}

// TestRuleLocksAreSharedAcrossServiceInstances 回归：锁曾挂在实例上，路由层与后台同步服务
// 各自 New 的 RuleService 拿到的是不同的锁，AutoFailover 的互斥形同虚设。
func TestRuleLocksAreSharedAcrossServiceInstances(t *testing.T) {
	e := newWatchdogEnv(t)
	a, b := NewRuleService(e.db), NewRuleService(e.db)

	unlock := a.lockRule(42)
	if _, ok := b.tryLockRule(42); ok {
		t.Fatal("不同 RuleService 实例必须共享同一把规则锁")
	}
	unlock()
	release, ok := b.tryLockRule(42)
	if !ok {
		t.Fatal("锁释放后应能获取")
	}
	release()
}

func TestWatchdogBackoffSchedule(t *testing.T) {
	b := &watchdogBackoff{entries: make(map[string]*backoffEntry)}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	var delays []time.Duration
	for i := 0; i < 8; i++ {
		b.fail("k", now)
		delays = append(delays, b.entries["k"].next.Sub(now))
	}
	want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second,
		160 * time.Second, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for i := range want {
		if delays[i] != want[i] {
			t.Fatalf("第 %d 次失败后的退避应为 %v，实际 %v（全部: %v）", i+1, want[i], delays[i], delays)
		}
	}

	b.reset("k")
	if !b.ready("k", now) {
		t.Fatal("reset 后应立即可重试")
	}
}

func TestPanelObjectsOnlyMatchesPanelNames(t *testing.T) {
	snap := &nodeSnapshot{
		services: map[string]string{
			"rule-1": "", "rule-2-tcp": "", "rule-3-udp": "", "relay-tunnel-4": "", "relay-tunnel-5-hop-0": "",
			"rule-x-tcp": "", "forward-6": "", "tunnel-7": "", "relay-tunnel-0": "", "my-rule-8-tcp": "",
		},
		chains: map[string]bool{"tunnel-9-chain": true, "tunnel-a-chain": true, "custom-chain": true},
	}
	var got []string
	for _, obj := range panelObjects(snap) {
		got = append(got, fmt.Sprintf("%s#%d", obj.name, obj.id))
	}
	sort.Strings(got)
	want := "relay-tunnel-4#4,relay-tunnel-5-hop-0#5,rule-1#1,rule-2-tcp#2,rule-3-udp#3,tunnel-9-chain#9"
	if strings.Join(got, ",") != want {
		t.Errorf("面板对象识别不对:\n got %v\nwant %s", got, want)
	}
}
