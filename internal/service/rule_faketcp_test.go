package service

import (
	"encoding/json"
	stderrors "errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"gost-panel/internal/dto"
	"gost-panel/internal/errors"
	"gost-panel/internal/model"
)

// 这一组用例覆盖「UDP 伪TCP直通」：UDP 子服务改挂规则自己的伪 TCP 链路（入口 chain → 出口 ftcp 服务），
// TCP 子服务仍走隧道。伪 TCP 的 chain 缺失时 GOST 会让 UDP 直连目标，所以 chain 必须先于 UDP 子服务就位、
// 晚于它删除；出口服务要跟着当前隧道的出口走，看门狗不能把它当成入口节点之外的残留删掉。

func (e *watchdogEnv) fakeTCPRule(name string, tunnel *model.GostTunnel, port, fakeTCPPort int, status model.RuleStatus) *model.GostRule {
	e.t.Helper()
	rule := e.tunnelRule(name, tunnel, port, status)
	if err := e.db.Model(rule).Updates(map[string]any{"udp_fake_tcp": true, "udp_fake_tcp_port": fakeTCPPort}).Error; err != nil {
		e.t.Fatalf("开启 UDP 伪TCP直通失败: %v", err)
	}
	rule.UDPFakeTCP, rule.UDPFakeTCPPort = true, fakeTCPPort
	return rule
}

func (f *fakeGost) chainConfig(t *testing.T, name string) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, ok := f.chains[name]
	if !ok {
		t.Fatalf("节点上缺少 chain %s", name)
	}
	var chain map[string]any
	if err := json.Unmarshal(raw, &chain); err != nil {
		t.Fatalf("解析 chain %s 失败: %v", name, err)
	}
	return chain
}

// chainNode 取 chain 第一跳的第一个节点
func chainNode(t *testing.T, chain map[string]any) map[string]any {
	t.Helper()
	hops, _ := chain["hops"].([]any)
	if len(hops) == 0 {
		t.Fatalf("chain 没有跳: %v", chain)
	}
	nodes, _ := hops[0].(map[string]any)["nodes"].([]any)
	if len(nodes) == 0 {
		t.Fatalf("chain 第一跳没有节点: %v", chain)
	}
	return nodes[0].(map[string]any)
}

func field(m map[string]any, keys ...string) any {
	var cur any = m
	for _, k := range keys {
		next, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = next[k]
	}
	return cur
}

// callIndex 返回调用记录里第一次出现 call 的位置，没有则为 -1
func callIndex(f *fakeGost, call string) int {
	return slices.Index(f.callLog(), call)
}

func startRule(t *testing.T, e *watchdogEnv, rule *model.GostRule) {
	t.Helper()
	if err := e.sync.ruleService.Start(rule.ID, 1, "admin", "127.0.0.1", "test"); err != nil {
		t.Fatalf("启动规则失败: %v", err)
	}
}

func TestFakeTCPRule_StartDeploysExitServiceBeforeUDPSubService(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitFake := newFakeGost(t), newFakeGost(t)
	_, exit, tunnel := runningTunnelOnNodes(e, entryFake, exitFake)
	rule := e.fakeTCPRule("game", tunnel, 27015, 0, model.RuleStatusStopped)

	startRule(t, e, rule)

	svcName, chainName := ruleFakeTCPServiceName(rule.ID), ruleFakeTCPChainName(rule.ID)
	// 出口节点：伪 TCP 服务，端口默认同监听端口，目标就是规则的目标
	svc := exitFake.service(t, svcName)
	if svc["addr"] != ":27015" || field(svc, "listener", "type") != "ftcp" || field(svc, "handler", "type") != "forward" {
		t.Errorf("出口伪 TCP 服务不对: %v", svc)
	}
	if nodes, _ := field(svc, "forwarder", "nodes").([]any); len(nodes) != 1 || nodes[0].(map[string]any)["addr"] != "10.0.0.9:80" {
		t.Errorf("出口伪 TCP 服务应转发到规则目标: %v", svc["forwarder"])
	}

	// 入口节点：chain 指向出口的伪 TCP 端口；forward 连接器 + ftcp 拨号器，每个 UDP 包一个伪 TCP 报文
	node := chainNode(t, entryFake.chainConfig(t, chainName))
	if node["addr"] != fmt.Sprintf("%s:27015", exit.Address) || field(node, "connector", "type") != "forward" || field(node, "dialer", "type") != "ftcp" {
		t.Errorf("伪 TCP chain 不对: %v", node)
	}

	// UDP 子服务改挂伪 TCP 链路，TCP 子服务仍走隧道
	if chain, _ := entryFake.serviceChain(fmt.Sprintf("rule-%d-udp", rule.ID)); chain != chainName {
		t.Errorf("UDP 子服务应挂 %s，实际 %q", chainName, chain)
	}
	if chain, _ := entryFake.serviceChain(fmt.Sprintf("rule-%d-tcp", rule.ID)); chain != tunnel.ChainID {
		t.Errorf("TCP 子服务应仍走隧道 %s，实际 %q", tunnel.ChainID, chain)
	}

	// chain 先于引用它的 UDP 子服务创建；任何时刻都不能有服务引用不存在的 chain
	chainAt := callIndex(entryFake, "CREATE chains "+chainName)
	udpAt := callIndex(entryFake, fmt.Sprintf("CREATE services rule-%d-udp", rule.ID))
	if chainAt < 0 || udpAt < 0 || chainAt > udpAt {
		t.Errorf("伪 TCP chain 必须先于 UDP 子服务创建，调用顺序 %v", entryFake.callLog())
	}
	if dangling := entryFake.danglingChainRefs(); len(dangling) > 0 {
		t.Errorf("出现了引用不存在 chain 的服务: %v", dangling)
	}
	if got := e.ruleStatus(rule.ID); got != model.RuleStatusRunning {
		t.Errorf("启动后规则应为 running，实际 %s", got)
	}
}

func TestFakeTCPRule_StopRemovesChainOnlyAfterUDPSubService(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitFake := newFakeGost(t), newFakeGost(t)
	_, _, tunnel := runningTunnelOnNodes(e, entryFake, exitFake)
	rule := e.fakeTCPRule("game", tunnel, 27015, 30015, model.RuleStatusStopped)
	startRule(t, e, rule)
	if svc := exitFake.service(t, ruleFakeTCPServiceName(rule.ID)); svc["addr"] != ":30015" {
		t.Fatalf("单独设置的出口端口没生效: %v", svc["addr"])
	}
	entryFake.resetCalls()

	if err := e.sync.ruleService.Stop(rule.ID, 1, "admin", "127.0.0.1", "test"); err != nil {
		t.Fatalf("停止规则失败: %v", err)
	}

	if exitFake.has("services", ruleFakeTCPServiceName(rule.ID)) {
		t.Error("停止后出口节点上的伪 TCP 服务应删除")
	}
	if entryFake.has("chains", ruleFakeTCPChainName(rule.ID)) {
		t.Error("停止后入口节点上的伪 TCP chain 应删除")
	}
	udpAt := callIndex(entryFake, fmt.Sprintf("DELETE services rule-%d-udp", rule.ID))
	chainAt := callIndex(entryFake, "DELETE chains "+ruleFakeTCPChainName(rule.ID))
	if udpAt < 0 || chainAt < 0 || udpAt > chainAt {
		t.Errorf("UDP 子服务必须先于伪 TCP chain 删除，调用顺序 %v", entryFake.callLog())
	}
	if !exitFake.has("services", tunnel.ServiceID) || !entryFake.has("chains", tunnel.ChainID) {
		t.Error("停止规则不应动隧道本身")
	}
}

// 回归：伪 TCP 服务在出口节点上、不在规则的入口节点上，看门狗不能把它当成残留删掉
func TestWatchdog_FakeTCPRule_LeavesHealthyObjectsAlone(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitFake := newFakeGost(t), newFakeGost(t)
	_, _, tunnel := runningTunnelOnNodes(e, entryFake, exitFake)
	rule := e.fakeTCPRule("game", tunnel, 27015, 0, model.RuleStatusStopped)
	startRule(t, e, rule)
	entryFake.resetCalls()
	exitFake.resetCalls()

	for i := 0; i < 3; i++ {
		e.tick()
		e.advance(watchdogBackoffMax)
	}

	for _, f := range []*fakeGost{entryFake, exitFake} {
		for _, call := range f.callLog() {
			if strings.HasPrefix(call, "DELETE") || strings.HasPrefix(call, "CREATE") {
				t.Fatalf("一切正常时看门狗不应增删对象，实际调用 %v", f.callLog())
			}
		}
	}
	if !exitFake.has("services", ruleFakeTCPServiceName(rule.ID)) {
		t.Fatal("出口节点上运行中的伪 TCP 服务被误删")
	}
	if got := e.ruleStatus(rule.ID); got != model.RuleStatusRunning {
		t.Errorf("规则应保持 running，实际 %s", got)
	}
}

func TestWatchdog_FakeTCPRule_RestoresExitServiceAfterExitRestart(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitFake := newFakeGost(t), newFakeGost(t)
	_, _, tunnel := runningTunnelOnNodes(e, entryFake, exitFake)
	rule := e.fakeTCPRule("game", tunnel, 27015, 0, model.RuleStatusStopped)
	startRule(t, e, rule)

	exitFake.restart()
	e.tick()

	if svc := exitFake.service(t, ruleFakeTCPServiceName(rule.ID)); field(svc, "listener", "type") != "ftcp" {
		t.Errorf("恢复的伪 TCP 服务不对: %v", svc)
	}
	if !exitFake.has("services", tunnel.ServiceID) {
		t.Error("隧道 relay 也应恢复")
	}
	if got := e.ruleStatus(rule.ID); got != model.RuleStatusRunning {
		t.Errorf("恢复后规则应为 running，实际 %s", got)
	}
	found := false
	for _, log := range e.logs(model.ActionRecover) {
		if log.ResourceType == model.ResourceTypeRule && strings.Contains(log.Details, ruleFakeTCPServiceName(rule.ID)) {
			found = true
		}
	}
	if !found {
		t.Errorf("应记录恢复伪 TCP 服务的日志，实际 %+v", e.logs(model.ActionRecover))
	}
}

func TestWatchdog_FakeTCPRule_RestoresChainBeforeUDPSubServiceAfterEntryRestart(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitFake := newFakeGost(t), newFakeGost(t)
	_, _, tunnel := runningTunnelOnNodes(e, entryFake, exitFake)
	rule := e.fakeTCPRule("game", tunnel, 27015, 0, model.RuleStatusStopped)
	startRule(t, e, rule)

	entryFake.restart()
	for i := 0; i < 3 && !entryFake.has("services", fmt.Sprintf("rule-%d-udp", rule.ID)); i++ {
		e.tick()
		e.advance(watchdogBackoffMax)
	}

	chainName := ruleFakeTCPChainName(rule.ID)
	if chain, ok := entryFake.serviceChain(fmt.Sprintf("rule-%d-udp", rule.ID)); !ok || chain != chainName {
		t.Fatalf("UDP 子服务应恢复并挂 %s，实际 %q (存在=%v)", chainName, chain, ok)
	}
	if chain, _ := entryFake.serviceChain(fmt.Sprintf("rule-%d-tcp", rule.ID)); chain != tunnel.ChainID {
		t.Errorf("TCP 子服务应恢复并挂隧道 chain，实际 %q", chain)
	}
	chainAt := callIndex(entryFake, "CREATE chains "+chainName)
	udpAt := callIndex(entryFake, fmt.Sprintf("CREATE services rule-%d-udp", rule.ID))
	if chainAt < 0 || udpAt < 0 || chainAt > udpAt {
		t.Errorf("恢复时伪 TCP chain 必须先于 UDP 子服务创建，调用顺序 %v", entryFake.callLog())
	}
	if dangling := entryFake.danglingChainRefs(); len(dangling) > 0 {
		t.Errorf("出现了引用不存在 chain 的服务: %v", dangling)
	}
}

// 主隧道不可用、规则切到出口不同的备选隧道：伪 TCP 服务跟着搬到新出口，chain 改指新出口，旧出口上的被清理
func TestWatchdog_FakeTCPRule_FollowsFailoverToAnotherExit(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitAFake, exitBFake := newFakeGost(t), newFakeGost(t), newFakeGost(t)
	entry := e.node("entry", entryFake)
	exitA := e.node("exit-a", exitAFake)
	exitB := e.node("exit-b", exitBFake)
	// 两个假节点都在 127.0.0.1 上，换个写法好区分 chain 指向的是哪个出口
	if err := e.db.Model(exitB).Update("address", "localhost").Error; err != nil {
		t.Fatalf("更新节点地址失败: %v", err)
	}
	primary := e.tunnel("primary", entry, exitA, 8443, model.TunnelStatusRunning)
	backup := e.tunnel("backup", entry, exitB, 8444, model.TunnelStatusRunning)
	for _, tn := range []*model.GostTunnel{primary, backup} {
		entryFake.put("chains", tn.ChainID, fmt.Sprintf(`{"name":%q}`, tn.ChainID))
	}
	exitAFake.put("services", primary.ServiceID, fmt.Sprintf(`{"name":%q}`, primary.ServiceID))
	exitBFake.put("services", backup.ServiceID, fmt.Sprintf(`{"name":%q}`, backup.ServiceID))

	rule := e.tunnelRuleWithBackups("game", primary, []uint{backup.ID}, 27015, model.RuleStatusStopped)
	if err := e.db.Model(rule).Update("udp_fake_tcp", true).Error; err != nil {
		t.Fatalf("开启 UDP 伪TCP直通失败: %v", err)
	}
	startRule(t, e, rule)
	if !exitAFake.has("services", ruleFakeTCPServiceName(rule.ID)) {
		t.Fatal("启动后伪 TCP 服务应在主隧道出口上")
	}

	// 主隧道被停止、入口上的 chain 没了
	if err := e.db.Model(primary).Update("status", model.TunnelStatusStopped).Error; err != nil {
		t.Fatalf("停止主隧道失败: %v", err)
	}
	entryFake.mu.Lock()
	delete(entryFake.chains, primary.ChainID)
	entryFake.mu.Unlock()

	e.tick()
	e.advance(watchdogBackoffMax)
	e.tick() // 切换发生在上一轮第 3 步，旧出口上的伪 TCP 服务在这一轮第 1 步才成为残留

	if row := e.ruleRow(rule.ID); row.TunnelID == nil || *row.TunnelID != backup.ID || row.Status != model.RuleStatusRunning {
		t.Fatalf("规则应切到备选隧道并运行，实际 tunnel=%v status=%s", row.TunnelID, row.Status)
	}
	if !exitBFake.has("services", ruleFakeTCPServiceName(rule.ID)) {
		t.Error("伪 TCP 服务应建到备选隧道的出口上")
	}
	if exitAFake.has("services", ruleFakeTCPServiceName(rule.ID)) {
		t.Error("旧出口上的伪 TCP 服务应被清理")
	}
	if node := chainNode(t, entryFake.chainConfig(t, ruleFakeTCPChainName(rule.ID))); node["addr"] != "localhost:27015" {
		t.Errorf("伪 TCP chain 应改指新出口，实际 %v", node["addr"])
	}
	if dangling := entryFake.danglingChainRefs(); len(dangling) > 0 {
		t.Errorf("出现了引用不存在 chain 的服务: %v", dangling)
	}
}

func TestWatchdog_CleansFakeTCPLeftovers(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitFake := newFakeGost(t), newFakeGost(t)
	_, _, tunnel := runningTunnelOnNodes(e, entryFake, exitFake)

	// 已停止的规则（例如停止时出口节点离线）留下的对象
	stopped := e.fakeTCPRule("stopped", tunnel, 27015, 0, model.RuleStatusStopped)
	exitFake.put("services", ruleFakeTCPServiceName(stopped.ID), fmt.Sprintf(`{"name":%q}`, ruleFakeTCPServiceName(stopped.ID)))
	entryFake.put("chains", ruleFakeTCPChainName(stopped.ID), fmt.Sprintf(`{"name":%q}`, ruleFakeTCPChainName(stopped.ID)))

	// 运行中但已关掉 UDP 伪TCP直通的规则留下的对象
	plain := e.tunnelRule("plain", tunnel, 27016, model.RuleStatusRunning)
	putRuleServices(entryFake, plain, tunnel.ChainID)
	exitFake.put("services", ruleFakeTCPServiceName(plain.ID), fmt.Sprintf(`{"name":%q}`, ruleFakeTCPServiceName(plain.ID)))
	entryFake.put("chains", ruleFakeTCPChainName(plain.ID), fmt.Sprintf(`{"name":%q}`, ruleFakeTCPChainName(plain.ID)))

	e.tick()

	for _, r := range []*model.GostRule{stopped, plain} {
		if exitFake.has("services", ruleFakeTCPServiceName(r.ID)) || entryFake.has("chains", ruleFakeTCPChainName(r.ID)) {
			t.Errorf("规则 %s 的伪 TCP 残留应被清理", r.Name)
		}
	}
	if !exitFake.has("services", tunnel.ServiceID) || !entryFake.has("chains", tunnel.ChainID) {
		t.Error("清理不应动隧道本身")
	}
}

// 伪 TCP chain 仍被服务引用时不能删：删了那个服务的 UDP 就会直连目标
func TestWatchdog_KeepsFakeTCPChainWhileReferenced(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitFake := newFakeGost(t), newFakeGost(t)
	_, _, tunnel := runningTunnelOnNodes(e, entryFake, exitFake)
	rule := e.tunnelRule("plain", tunnel, 27016, model.RuleStatusRunning)
	chainName := ruleFakeTCPChainName(rule.ID)
	entryFake.put("chains", chainName, fmt.Sprintf(`{"name":%q}`, chainName))
	entryFake.put("services", "user-svc", fmt.Sprintf(`{"name":"user-svc","handler":{"type":"forward","chain":%q}}`, chainName))

	e.tick()

	if !entryFake.has("chains", chainName) {
		t.Fatal("仍被服务引用的伪 TCP chain 被删掉了")
	}
}

func TestRuleCreateValidatesUDPFakeTCP(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitFake := newFakeGost(t), newFakeGost(t)
	entry, _, tunnel := runningTunnelOnNodes(e, entryFake, exitFake)
	svc := e.sync.ruleService

	create := func(req dto.CreateRuleReq) (*model.GostRule, error) {
		req.Name = "r"
		req.Targets = []string{"10.0.0.9:80"}
		return svc.Create(&req, 1, "admin", "127.0.0.1", "test")
	}

	rule, err := create(dto.CreateRuleReq{Type: "tunnel", TunnelID: &tunnel.ID, ListenPort: 27015, UDPFakeTCP: true})
	if err != nil {
		t.Fatalf("创建开启伪 TCP 的规则失败: %v", err)
	}
	if !rule.UDPFakeTCP || rule.UDPFakeTCPPort != 0 || ruleFakeTCPPort(rule) != 27015 {
		t.Errorf("未设置出口端口时应沿用监听端口: %+v", rule)
	}

	// 出口端口与隧道出口的 relay 端口相同：两者会在出口节点上抢同一个 TCP 端口
	for _, req := range []dto.CreateRuleReq{
		{Type: "tunnel", TunnelID: &tunnel.ID, ListenPort: 27016, UDPFakeTCP: true, UDPFakeTCPPort: 8443},
		{Type: "tunnel", TunnelID: &tunnel.ID, ListenPort: 8443, UDPFakeTCP: true},
	} {
		var biz *errors.BizError
		if _, err = create(req); !stderrors.As(err, &biz) || biz.Code != errors.ErrRuleUDPFakeTCP.Code {
			t.Errorf("出口端口与 relay 端口冲突应被拒绝，实际 %v", err)
		}
	}

	// 端口转发没有出口节点，开关不生效
	forward, err := create(dto.CreateRuleReq{Type: "forward", NodeID: &entry.ID, ListenPort: 27017, UDPFakeTCP: true, UDPFakeTCPPort: 30000})
	if err != nil {
		t.Fatalf("创建端口转发规则失败: %v", err)
	}
	if forward.UDPFakeTCP || forward.UDPFakeTCPPort != 0 {
		t.Errorf("端口转发不应开启伪 TCP: %+v", forward)
	}

	// 编辑时开关与端口都要落库
	if _, err = svc.Update(rule.ID, &dto.UpdateRuleReq{
		TunnelID: &tunnel.ID, Name: "r", ListenPort: 27015, Targets: []string{"10.0.0.9:80"},
		UDPFakeTCP: true, UDPFakeTCPPort: 30015,
	}, 1, "admin", "127.0.0.1", "test"); err != nil {
		t.Fatalf("编辑规则失败: %v", err)
	}
	if row := e.ruleRow(rule.ID); !row.UDPFakeTCP || row.UDPFakeTCPPort != 30015 {
		t.Errorf("编辑后的伪 TCP 配置没有保存: udp_fake_tcp=%v port=%d", row.UDPFakeTCP, row.UDPFakeTCPPort)
	}
}

// 停止隧道会暂停走它的规则；伪 TCP 链路要一并撤掉（停用期间隧道可能被改成别的出口），
// 隧道恢复后由看门狗按当时的出口重建
func TestTunnelStop_RemovesFakeTCPPathAndWatchdogRebuildsIt(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitFake := newFakeGost(t), newFakeGost(t)
	_, _, tunnel := runningTunnelOnNodes(e, entryFake, exitFake)
	rule := e.fakeTCPRule("game", tunnel, 27015, 0, model.RuleStatusStopped)
	startRule(t, e, rule)
	tunnelService := e.sync.ruleService.tunnelService

	if err := tunnelService.Stop(tunnel.ID, 1, "admin", "127.0.0.1", "test"); err != nil {
		t.Fatalf("停止隧道失败: %v", err)
	}
	if exitFake.has("services", ruleFakeTCPServiceName(rule.ID)) || entryFake.has("chains", ruleFakeTCPChainName(rule.ID)) {
		t.Fatal("停止隧道后规则的伪 TCP 链路应撤掉")
	}
	if dangling := entryFake.danglingChainRefs(); len(dangling) > 0 {
		t.Errorf("出现了引用不存在 chain 的服务: %v", dangling)
	}
	if got := e.ruleStatus(rule.ID); got != model.RuleStatusError {
		t.Fatalf("隧道停止后规则应为 error，实际 %s", got)
	}

	if err := tunnelService.Start(tunnel.ID, 1, "admin", "127.0.0.1", "test"); err != nil {
		t.Fatalf("重新启动隧道失败: %v", err)
	}
	e.tick()

	if !exitFake.has("services", ruleFakeTCPServiceName(rule.ID)) {
		t.Error("隧道恢复后看门狗应重建出口上的伪 TCP 服务")
	}
	if chain, _ := entryFake.serviceChain(fmt.Sprintf("rule-%d-udp", rule.ID)); chain != ruleFakeTCPChainName(rule.ID) {
		t.Errorf("UDP 子服务应重新挂上伪 TCP 链路，实际 %q", chain)
	}
	if got := e.ruleStatus(rule.ID); got != model.RuleStatusRunning {
		t.Errorf("隧道恢复后规则应为 running，实际 %s", got)
	}
}
