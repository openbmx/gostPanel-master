package service

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"gost-panel/internal/dto"
	"gost-panel/internal/model"
)

// 这一组用例覆盖“节点上的实际状态与数据库不一致”时的安全性：
// 规则服务实际挂着哪条 chain、chain 是否还在，都要以节点上的真实配置为准。

// TestWatchdog_TransientLookupFailureNeverLeavesRuleOnStaleChain 回归：切换隧道重建规则服务时，
// 节点 API 抖了一下，旧服务没删掉；曾经会因为“删除静默跳过 + 创建因同名而跳过”留下挂在旧链路上的服务，
// 随后旧链路被清理，流量绕过隧道直连目标，而且之后再也不会被发现。
func TestWatchdog_TransientLookupFailureNeverLeavesRuleOnStaleChain(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitAFake, exitBFake := newFakeGost(t), newFakeGost(t), newFakeGost(t)
	entry := e.node("entry", entryFake)
	exitA := e.node("exit-a", exitAFake)
	exitB := e.node("exit-b", exitBFake)

	// 主隧道在入口节点不可达时被停止，节点上留下了它的 chain 和挂在上面的规则服务
	primary := e.tunnel("primary", entry, exitA, 8443, model.TunnelStatusRunning)
	if err := e.db.Model(primary).Update("status", model.TunnelStatusStopped).Error; err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}
	entryFake.put("chains", primary.ChainID, fmt.Sprintf(`{"name":%q}`, primary.ChainID))
	backup := e.tunnel("backup", entry, exitB, 8444, model.TunnelStatusRunning)
	entryFake.put("chains", backup.ChainID, fmt.Sprintf(`{"name":%q}`, backup.ChainID))
	exitBFake.put("services", backup.ServiceID, fmt.Sprintf(`{"name":%q}`, backup.ServiceID))

	rule := e.tunnelRuleWithBackups("r1", primary, []uint{backup.ID}, 10001, model.RuleStatusRunning)
	putRuleServices(entryFake, rule, primary.ChainID)
	tcp, udp := fmt.Sprintf("rule-%d-tcp", rule.ID), fmt.Sprintf("rule-%d-udp", rule.ID)
	entryFake.setFailGet(tcp, 1) // 重建时查询 tcp 子服务失败一次

	e.tick()
	if dangling := entryFake.danglingChainRefs(); len(dangling) > 0 {
		t.Fatalf("第一轮后出现了引用不存在 chain 的服务（流量直连泄漏）: %v", dangling)
	}

	e.tick()
	if dangling := entryFake.danglingChainRefs(); len(dangling) > 0 {
		t.Fatalf("第二轮后出现了引用不存在 chain 的服务（流量直连泄漏）: %v", dangling)
	}
	for _, name := range []string{tcp, udp} {
		if chain, ok := entryFake.serviceChain(name); !ok || chain != backup.ChainID {
			t.Errorf("%s 应挂在备选隧道的 chain 上，实际 %q (存在=%v)", name, chain, ok)
		}
	}

	// 第一轮里旧 chain 仍被引用，清理进入了退避；引用消失、退避到期后应被清理
	e.advance(11 * time.Second)
	e.tick()
	if entryFake.has("chains", primary.ChainID) {
		t.Error("不再被引用的旧 chain 应被清理")
	}
	if dangling := entryFake.danglingChainRefs(); len(dangling) > 0 {
		t.Fatalf("清理旧 chain 后出现了引用不存在 chain 的服务: %v", dangling)
	}
	if row := e.ruleRow(rule.ID); row.Status != model.RuleStatusRunning || row.TunnelID == nil || *row.TunnelID != backup.ID {
		t.Errorf("规则应运行在备选隧道上，实际 status=%s tunnel=%v", row.Status, row.TunnelID)
	}
}

// TestWatchdog_RuleOnWrongChainIsRebuilt 节点上的规则服务挂着另一条（仍存在的）chain，
// 与数据库记录的当前隧道不一致时，应按数据库重建到正确的 chain 上。
func TestWatchdog_RuleOnWrongChainIsRebuilt(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitAFake, exitBFake := newFakeGost(t), newFakeGost(t), newFakeGost(t)
	entry := e.node("entry", entryFake)
	exitA := e.node("exit-a", exitAFake)
	exitB := e.node("exit-b", exitBFake)
	tunnelA := e.tunnel("a", entry, exitA, 8443, model.TunnelStatusRunning)
	tunnelB := e.tunnel("b", entry, exitB, 8444, model.TunnelStatusRunning)
	for _, tn := range []*model.GostTunnel{tunnelA, tunnelB} {
		entryFake.put("chains", tn.ChainID, fmt.Sprintf(`{"name":%q}`, tn.ChainID))
	}
	exitAFake.put("services", tunnelA.ServiceID, fmt.Sprintf(`{"name":%q}`, tunnelA.ServiceID))
	exitBFake.put("services", tunnelB.ServiceID, fmt.Sprintf(`{"name":%q}`, tunnelB.ServiceID))

	rule := e.tunnelRule("r1", tunnelB, 10001, model.RuleStatusRunning)
	putRuleServices(entryFake, rule, tunnelA.ChainID) // 节点上实际挂的是 A

	e.tick()

	for _, suffix := range []string{"-tcp", "-udp"} {
		if chain, _ := entryFake.serviceChain(fmt.Sprintf("rule-%d%s", rule.ID, suffix)); chain != tunnelB.ChainID {
			t.Errorf("规则服务应按数据库挂到隧道 b 的 chain，实际 %q", chain)
		}
	}
}

// TestWatchdog_LeakIsHandledDespiteBackoff 规则服务引用的 chain 已不存在时，即使退避未到期也要立即处理
func TestWatchdog_LeakIsHandledDespiteBackoff(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitFake := newFakeGost(t), newFakeGost(t)
	entry := e.node("entry", entryFake)
	exit := e.node("exit", exitFake)
	tunnel := e.tunnel("t1", entry, exit, 8443, model.TunnelStatusStopped)
	rule := e.tunnelRule("r1", tunnel, 10001, model.RuleStatusRunning)

	// 之前多次处理失败，两类退避都已拉长到 5 分钟
	for i := 0; i < 6; i++ {
		e.sync.watchdog.backoff.fail(ruleRouteKey(rule.ID), e.clock)
		e.sync.watchdog.backoff.fail(ruleRestoreKey(rule.ID), e.clock)
	}
	// 此时规则服务又挂到了不存在的 chain 上（例如用户在两轮之间启动了规则又停了隧道）
	putRuleServices(entryFake, rule, fmt.Sprintf("tunnel-%d-chain", tunnel.ID))

	e.tick()

	if dangling := entryFake.danglingChainRefs(); len(dangling) > 0 {
		t.Fatalf("泄漏中的规则服务应被立即删除，不应等待退避: %v", dangling)
	}
}

// TestWatchdog_ResumeResetsRestoreBackoff 回归：隧道恢复后，暂停期间累积的恢复退避必须作废，
// 否则规则要再等最多 5 分钟才会恢复。
func TestWatchdog_ResumeResetsRestoreBackoff(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitFake := newFakeGost(t), newFakeGost(t)
	_, _, tunnel := runningTunnelOnNodes(e, entryFake, exitFake)
	rule := e.tunnelRule("r1", tunnel, 10001, model.RuleStatusError)

	// 暂停期间：路由退避有记录，恢复退避被拉长到 5 分钟（例如 UDP 子服务曾反复失败）
	e.sync.watchdog.backoff.fail(ruleRouteKey(rule.ID), e.clock)
	for i := 0; i < 6; i++ {
		e.sync.watchdog.backoff.fail(ruleRestoreKey(rule.ID), e.clock)
	}

	e.tick()

	for _, suffix := range []string{"-tcp", "-udp"} {
		if chain, ok := entryFake.serviceChain(fmt.Sprintf("rule-%d%s", rule.ID, suffix)); !ok || chain != tunnel.ChainID {
			t.Errorf("隧道可用后规则应立即恢复，实际 rule-%d%s 存在=%v chain=%q", rule.ID, suffix, ok, chain)
		}
	}
	if got := e.ruleStatus(rule.ID); got != model.RuleStatusRunning {
		t.Errorf("恢复后规则应为 running，实际 %s", got)
	}
}

// TestWatchdog_KeepsChainStillReferenced 已停止隧道的 chain 若仍被服务引用就不能删，
// 否则引用它的服务会绕过隧道直连目标。
func TestWatchdog_KeepsChainStillReferenced(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitFake := newFakeGost(t), newFakeGost(t)
	entry := e.node("entry", entryFake)
	exit := e.node("exit", exitFake)
	tunnel := e.tunnel("t1", entry, exit, 8443, model.TunnelStatusStopped)
	chain := fmt.Sprintf("tunnel-%d-chain", tunnel.ID)
	entryFake.put("chains", chain, fmt.Sprintf(`{"name":%q}`, chain))
	entryFake.put("services", "my-proxy", fmt.Sprintf(`{"name":"my-proxy","handler":{"type":"socks5","chain":%q}}`, chain))

	e.tick()

	if !entryFake.has("chains", chain) {
		t.Fatal("仍被引用的 chain 不应被清理")
	}
	if !entryFake.has("services", "my-proxy") {
		t.Fatal("与面板无关的服务不应被删除")
	}
}

// TestAutoFailover_DoesNotSwitchToTunnelWithoutChain 故障转移的目标隧道在数据库里是“运行中”，
// 但入口节点上没有它的 chain 时，不能拆掉正常工作的备选链路去切过去。
func TestAutoFailover_DoesNotSwitchToTunnelWithoutChain(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitAFake, exitBFake := newFakeGost(t), newFakeGost(t), newFakeGost(t)
	entry := e.node("entry", entryFake)
	exitA := e.node("exit-a", exitAFake)
	exitB := e.node("exit-b", exitBFake)
	primary := e.tunnel("primary", entry, exitA, 8443, model.TunnelStatusRunning) // chain 不在节点上
	backup := e.tunnel("backup", entry, exitB, 8444, model.TunnelStatusRunning)
	entryFake.put("chains", backup.ChainID, fmt.Sprintf(`{"name":%q}`, backup.ChainID))

	rule := e.tunnelRuleWithBackups("r1", primary, []uint{backup.ID}, 10001, model.RuleStatusRunning)
	if err := e.db.Model(rule).Update("tunnel_id", backup.ID).Error; err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}
	putRuleServices(entryFake, rule, backup.ChainID)

	e.sync.ruleService.AutoFailover()

	for _, call := range entryFake.callLog() {
		if strings.HasPrefix(call, "DELETE services") || strings.HasPrefix(call, "CREATE services") {
			t.Fatalf("目标 chain 不在节点上时不应动正在工作的规则服务，实际调用 %v", entryFake.callLog())
		}
	}
	if row := e.ruleRow(rule.ID); row.TunnelID == nil || *row.TunnelID != backup.ID || row.Status != model.RuleStatusRunning {
		t.Errorf("规则应继续运行在备选隧道上，实际 tunnel=%v status=%s", row.TunnelID, row.Status)
	}
}

// TestTunnelStart_RollbackKeepsPreexistingObjects 重新启动 error 隧道失败时，回滚只能撤销本次新建的对象：
// 节点上原有的 chain 被删会让挂在上面的规则绕过隧道直连目标。
func TestTunnelStart_RollbackKeepsPreexistingObjects(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitFake := newFakeGost(t), newFakeGost(t)
	_, _, tunnel := runningTunnelOnNodes(e, entryFake, exitFake)
	rule := e.tunnelRule("r1", tunnel, 10001, model.RuleStatusRunning)
	putRuleServices(entryFake, rule, tunnel.ChainID)
	if err := e.db.Model(tunnel).Update("status", model.TunnelStatusError).Error; err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}
	exitFake.setFailSave(true)

	if err := e.sync.ruleService.tunnelService.Start(tunnel.ID, 1, "admin", "127.0.0.1", "test"); err == nil {
		t.Fatal("保存配置失败时启动应报错")
	}
	if !entryFake.has("chains", tunnel.ChainID) {
		t.Error("回滚删掉了启动前就存在的 chain，规则会绕过隧道直连目标")
	}
	if !exitFake.has("services", tunnel.ServiceID) {
		t.Error("回滚删掉了启动前就存在的 relay")
	}
	if dangling := entryFake.danglingChainRefs(); len(dangling) > 0 {
		t.Errorf("出现了引用不存在 chain 的服务: %v", dangling)
	}
}

// TestRuleStart_PicksFirstRoutableTunnel 主隧道在数据库里是“运行中”但入口节点上没有它的 chain 时，
// 启动规则应选用链路确实存在的备选隧道，而不是挪到主隧道上再启动失败。
func TestRuleStart_PicksFirstRoutableTunnel(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitAFake, exitBFake := newFakeGost(t), newFakeGost(t), newFakeGost(t)
	entry := e.node("entry", entryFake)
	exitA := e.node("exit-a", exitAFake)
	exitB := e.node("exit-b", exitBFake)
	primary := e.tunnel("primary", entry, exitA, 8443, model.TunnelStatusRunning) // chain 不在节点上
	backup := e.tunnel("backup", entry, exitB, 8444, model.TunnelStatusRunning)
	entryFake.put("chains", backup.ChainID, fmt.Sprintf(`{"name":%q}`, backup.ChainID))
	rule := e.tunnelRuleWithBackups("r1", primary, []uint{backup.ID}, 10001, model.RuleStatusStopped)

	if err := e.sync.ruleService.Start(rule.ID, 1, "admin", "127.0.0.1", "test"); err != nil {
		t.Fatalf("有可用的备选隧道时启动不应失败: %v", err)
	}
	if row := e.ruleRow(rule.ID); row.TunnelID == nil || *row.TunnelID != backup.ID || row.Status != model.RuleStatusRunning {
		t.Fatalf("规则应在备选隧道上运行，实际 tunnel=%v status=%s", row.TunnelID, row.Status)
	}
	if chain, _ := entryFake.serviceChain(fmt.Sprintf("rule-%d-tcp", rule.ID)); chain != backup.ChainID {
		t.Errorf("规则服务应挂在备选隧道的 chain 上，实际 %q", chain)
	}
}

// TestRuleUpdate_RejectedWhenOldServicesCannotBeRemoved 编辑时节点上的旧服务删不掉，
// 必须拒绝编辑：否则旧服务继续按旧端口/旧目标转发，看门狗还会把它当成新配置已生效。
func TestRuleUpdate_RejectedWhenOldServicesCannotBeRemoved(t *testing.T) {
	e := newWatchdogEnv(t)
	fake := newFakeGost(t)
	node := e.node("n1", fake)
	rule := e.forwardRule("r1", node, 10001, model.RuleStatusError)
	putRuleServices(fake, rule, "")
	fake.setFailGet(fmt.Sprintf("rule-%d-tcp", rule.ID), 10) // 节点 API 持续抖动

	_, err := e.sync.ruleService.Update(rule.ID, &dto.UpdateRuleReq{
		Name: "renamed", ListenPort: 20001, Targets: []string{"10.0.0.8:80"},
	}, 1, "admin", "127.0.0.1", "test")
	if err == nil {
		t.Fatal("旧服务删不掉时编辑应失败")
	}
	row := e.ruleRow(rule.ID)
	if row.Name != "r1" || row.ListenPort != 10001 {
		t.Errorf("编辑失败时不应保存新配置，实际 name=%s port=%d", row.Name, row.ListenPort)
	}
	if row.Status != model.RuleStatusError {
		t.Errorf("编辑失败后应恢复原状态 error，实际 %s", row.Status)
	}
}

// TestTunnelStop_KeepsChainStillReferenced 停隧道时 chain 仍被与面板无关的服务引用，不能删
func TestTunnelStop_KeepsChainStillReferenced(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitFake := newFakeGost(t), newFakeGost(t)
	_, _, tunnel := runningTunnelOnNodes(e, entryFake, exitFake)
	entryFake.put("services", "my-proxy", fmt.Sprintf(`{"name":"my-proxy","handler":{"type":"socks5","chain":%q}}`, tunnel.ChainID))

	if err := e.sync.ruleService.tunnelService.Stop(tunnel.ID, 1, "admin", "127.0.0.1", "test"); err != nil {
		t.Fatalf("停止隧道失败: %v", err)
	}
	if !entryFake.has("chains", tunnel.ChainID) || !entryFake.has("services", "my-proxy") {
		t.Error("仍被引用的 chain 与引用它的非面板服务都不应被删除")
	}
	if got := e.tunnelStatus(tunnel.ID); got != model.TunnelStatusStopped {
		t.Errorf("隧道应为 stopped，实际 %s", got)
	}
}

// TestWatchdog_LeakingSubServiceRemovedEvenIfRebuildFails 正在泄漏的子服务要先删掉，
// 不能因为重建失败（例如未配置面板地址、观察器下发失败）而一直留着。
func TestWatchdog_LeakingSubServiceRemovedEvenIfRebuildFails(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitFake := newFakeGost(t), newFakeGost(t)
	_, _, tunnel := runningTunnelOnNodes(e, entryFake, exitFake)
	rule := e.tunnelRule("r1", tunnel, 10001, model.RuleStatusRunning)
	tcp, udp := fmt.Sprintf("rule-%d-tcp", rule.ID), fmt.Sprintf("rule-%d-udp", rule.ID)
	entryFake.put("services", tcp, fmt.Sprintf(`{"name":%q,"handler":{"type":"forward","chain":"tunnel-99-chain"}}`, tcp))
	entryFake.put("services", udp, fmt.Sprintf(`{"name":%q,"handler":{"type":"forward","chain":%q}}`, udp, tunnel.ChainID))

	// 清空面板地址：观察器无法下发，重建必然失败
	if err := e.db.Model(&model.SystemConfig{}).Where("id = 1").Update("panel_url", "").Error; err != nil {
		t.Fatalf("清空面板地址失败: %v", err)
	}

	e.tick()

	if dangling := entryFake.danglingChainRefs(); len(dangling) > 0 {
		t.Fatalf("泄漏中的子服务应先被删除，实际仍有 %v", dangling)
	}
	if got := e.ruleStatus(rule.ID); got != model.RuleStatusError {
		t.Errorf("重建失败后规则应为 error，实际 %s", got)
	}
}

// TestTunnelStart_OverwritesStaleChain 节点上残留着同名的旧 chain（例如停止时节点离线）时，
// 启动隧道要按当前跳点覆盖它，不能沿用旧跳点。
func TestTunnelStart_OverwritesStaleChain(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitFake := newFakeGost(t), newFakeGost(t)
	entry := e.node("entry", entryFake)
	exit := e.node("exit", exitFake)
	tunnel := e.tunnel("t1", entry, exit, 8443, model.TunnelStatusStopped)
	chain := fmt.Sprintf("tunnel-%d-chain", tunnel.ID)
	entryFake.put("chains", chain, fmt.Sprintf(`{"name":%q,"hops":[{"name":"hop-0","nodes":[{"name":"relay-hop-0","addr":"10.9.9.9:1"}]}]}`, chain))

	if err := e.sync.ruleService.tunnelService.Start(tunnel.ID, 1, "admin", "127.0.0.1", "test"); err != nil {
		t.Fatalf("启动隧道失败: %v", err)
	}

	entryFake.mu.Lock()
	raw := string(entryFake.chains[chain])
	entryFake.mu.Unlock()
	if strings.Contains(raw, "10.9.9.9") || !strings.Contains(raw, fmt.Sprintf("%s:8443", exit.Address)) {
		t.Errorf("启动后 chain 应指向当前出口节点，实际 %s", raw)
	}
}
