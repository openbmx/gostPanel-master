package service

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"gost-panel/internal/dto"
	"gost-panel/internal/errors"
	"gost-panel/internal/model"
)

// 这一组用例覆盖“隧道规则绝不能在 chain 缺失时运行”：
// GOST 3.2.6/3.3.0 实测，服务引用的 chain 不存在时不会报错，而是直接连接转发目标，
// 流量会绕过隧道（从入口节点直出）。

func (e *watchdogEnv) tunnelRuleWithBackups(name string, primary *model.GostTunnel, backups []uint, port int, status model.RuleStatus) *model.GostRule {
	e.t.Helper()
	rule := &model.GostRule{
		Name:            name,
		Type:            model.RuleTypeTunnel,
		TunnelID:        &primary.ID,
		PrimaryTunnelID: &primary.ID,
		BackupTunnelIDs: backups,
		ListenPort:      port,
		Targets:         []string{"10.0.0.9:80"},
		Status:          status,
		ServiceID:       "",
	}
	if err := e.db.Create(rule).Error; err != nil {
		e.t.Fatalf("创建规则失败: %v", err)
	}
	return rule
}

// putRuleServices 在节点上摆好规则的 TCP/UDP 服务，chain 为空表示端口转发
func putRuleServices(f *fakeGost, rule *model.GostRule, chain string) {
	for _, suffix := range []string{"-tcp", "-udp"} {
		name := fmt.Sprintf("rule-%d%s", rule.ID, suffix)
		handler := `{"type":"forward"}`
		if chain != "" {
			handler = fmt.Sprintf(`{"type":"forward","chain":%q}`, chain)
		}
		f.put("services", name, fmt.Sprintf(`{"name":%q,"addr":":%d","handler":%s}`, name, rule.ListenPort, handler))
	}
}

func (e *watchdogEnv) ruleRow(id uint) model.GostRule {
	e.t.Helper()
	var rule model.GostRule
	if err := e.db.First(&rule, id).Error; err != nil {
		e.t.Fatalf("读取规则失败: %v", err)
	}
	return rule
}

// TestWatchdog_PausesTunnelRuleWhenTunnelStopped 隧道被停止（chain 已删）而规则服务还在：
// 必须删掉规则服务，否则流量会绕过隧道直连目标。
func TestWatchdog_PausesTunnelRuleWhenTunnelStopped(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitFake := newFakeGost(t), newFakeGost(t)
	entry := e.node("entry", entryFake)
	exit := e.node("exit", exitFake)
	tunnel := e.tunnel("t1", entry, exit, 8443, model.TunnelStatusStopped)
	rule := e.tunnelRule("r1", tunnel, 10001, model.RuleStatusRunning)
	putRuleServices(entryFake, rule, fmt.Sprintf("tunnel-%d-chain", tunnel.ID))

	e.tick()

	if names := entryFake.serviceNames(); len(names) != 0 {
		t.Fatalf("隧道不可用时规则服务必须删除（否则直连泄漏），实际仍有 %v", names)
	}
	if got := e.ruleStatus(rule.ID); got != model.RuleStatusError {
		t.Errorf("暂停后规则应为 error（保留应当运行的意图），实际 %s", got)
	}
	logs := e.logs(model.ActionRecover)
	if len(logs) != 1 || !strings.Contains(logs[0].Details, "暂停转发") {
		t.Errorf("应记录一条暂停日志，实际 %+v", logs)
	}
}

// TestWatchdog_ReroutesRuleToBackupTunnel 主隧道不可用时切到入口节点上确有 chain 的备选隧道
func TestWatchdog_ReroutesRuleToBackupTunnel(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitAFake, exitBFake := newFakeGost(t), newFakeGost(t), newFakeGost(t)
	entry := e.node("entry", entryFake)
	exitA := e.node("exit-a", exitAFake)
	exitB := e.node("exit-b", exitBFake)
	primary := e.tunnel("primary", entry, exitA, 8443, model.TunnelStatusStopped)
	backup := e.tunnel("backup", entry, exitB, 8444, model.TunnelStatusRunning)
	entryFake.put("chains", backup.ChainID, fmt.Sprintf(`{"name":%q}`, backup.ChainID))
	exitBFake.put("services", backup.ServiceID, fmt.Sprintf(`{"name":%q}`, backup.ServiceID))

	rule := e.tunnelRuleWithBackups("r1", primary, []uint{backup.ID}, 10001, model.RuleStatusRunning)
	putRuleServices(entryFake, rule, fmt.Sprintf("tunnel-%d-chain", primary.ID))

	e.tick()

	row := e.ruleRow(rule.ID)
	if row.TunnelID == nil || *row.TunnelID != backup.ID {
		t.Fatalf("规则应切换到备选隧道 %d，实际 %v", backup.ID, row.TunnelID)
	}
	if row.PrimaryTunnelID == nil || *row.PrimaryTunnelID != primary.ID {
		t.Error("切换不应改写用户指定的主隧道")
	}
	if row.Status != model.RuleStatusRunning {
		t.Errorf("切换后规则应为 running，实际 %s", row.Status)
	}
	for _, suffix := range []string{"-tcp", "-udp"} {
		svc := entryFake.service(t, fmt.Sprintf("rule-%d%s", rule.ID, suffix))
		if handler, _ := svc["handler"].(map[string]any); handler["chain"] != backup.ChainID {
			t.Errorf("规则服务应改挂备选隧道的 chain，实际 %v", handler)
		}
	}
	if logs := e.logs(model.ActionRecover); len(logs) != 1 || !strings.Contains(logs[0].Details, "切换到隧道 backup") {
		t.Errorf("应记录一条切换日志，实际 %+v", logs)
	}
}

// TestWatchdog_ResumesPausedRuleOnceTunnelIsBack 隧道重新启动后，被暂停的规则立即恢复，
// 不必等此前累积的退避时间。
func TestWatchdog_ResumesPausedRuleOnceTunnelIsBack(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitFake := newFakeGost(t), newFakeGost(t)
	entry := e.node("entry", entryFake)
	exit := e.node("exit", exitFake)
	tunnel := e.tunnel("t1", entry, exit, 8443, model.TunnelStatusStopped)
	rule := e.tunnelRule("r1", tunnel, 10001, model.RuleStatusRunning)
	putRuleServices(entryFake, rule, fmt.Sprintf("tunnel-%d-chain", tunnel.ID))

	// 连续暂停几轮，让退避涨上去
	for i := 0; i < 3; i++ {
		e.tick()
		e.advance(time.Minute)
	}
	if got := e.ruleStatus(rule.ID); got != model.RuleStatusError {
		t.Fatalf("前置条件：规则应已被暂停，实际 %s", got)
	}

	// 用户重新启动隧道
	chain := fmt.Sprintf("tunnel-%d-chain", tunnel.ID)
	if err := e.db.Model(tunnel).Updates(map[string]any{
		"status": model.TunnelStatusRunning, "chain_id": chain, "service_id": fmt.Sprintf("relay-tunnel-%d", tunnel.ID),
	}).Error; err != nil {
		t.Fatalf("启动隧道失败: %v", err)
	}
	entryFake.put("chains", chain, fmt.Sprintf(`{"name":%q}`, chain))
	exitFake.put("services", fmt.Sprintf("relay-tunnel-%d", tunnel.ID), fmt.Sprintf(`{"name":"relay-tunnel-%d"}`, tunnel.ID))

	e.tick()

	svc := entryFake.service(t, fmt.Sprintf("rule-%d-tcp", rule.ID))
	if handler, _ := svc["handler"].(map[string]any); handler["chain"] != chain {
		t.Errorf("恢复的规则服务必须挂在隧道 chain 上，实际 %v", handler)
	}
	if got := e.ruleStatus(rule.ID); got != model.RuleStatusRunning {
		t.Errorf("隧道恢复后规则应立即转回 running，实际 %s", got)
	}
}

// TestRuleStart_RefusesWhenChainMissingOnEntry 数据库认为隧道在运行，但入口节点上没有 chain
// （例如节点刚重启、看门狗尚未补建）时，启动规则必须失败，而不是下发会直连的服务。
func TestRuleStart_RefusesWhenChainMissingOnEntry(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitFake := newFakeGost(t), newFakeGost(t)
	entry := e.node("entry", entryFake)
	exit := e.node("exit", exitFake)
	tunnel := e.tunnel("t1", entry, exit, 8443, model.TunnelStatusRunning)
	rule := e.tunnelRule("r1", tunnel, 10001, model.RuleStatusStopped)

	err := e.sync.ruleService.Start(rule.ID, 1, "admin", "127.0.0.1", "test")
	if err != errors.ErrTunnelChainNotFound {
		t.Fatalf("chain 缺失时应拒绝启动，实际 %v", err)
	}
	for _, call := range entryFake.callLog() {
		if strings.HasPrefix(call, "CREATE services") {
			t.Fatalf("chain 缺失时不应下发任何规则服务，实际调用 %v", entryFake.callLog())
		}
	}
}

// TestTunnelStop_SuspendsRulesBeforeDeletingChain 停隧道时先暂停其上的规则再删 chain，
// 否则两步之间规则的流量会绕过隧道直连目标。
func TestTunnelStop_SuspendsRulesBeforeDeletingChain(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitFake := newFakeGost(t), newFakeGost(t)
	_, _, tunnel := runningTunnelOnNodes(e, entryFake, exitFake)
	rule := e.tunnelRule("r1", tunnel, 10001, model.RuleStatusRunning)
	putRuleServices(entryFake, rule, tunnel.ChainID)

	if err := e.sync.ruleService.tunnelService.Stop(tunnel.ID, 1, "admin", "127.0.0.1", "test"); err != nil {
		t.Fatalf("停止隧道失败: %v", err)
	}

	calls := entryFake.callLog()
	chainDeleted, servicesDeleted := -1, 0
	for i, call := range calls {
		switch {
		case call == "DELETE chains "+tunnel.ChainID:
			chainDeleted = i
		case strings.HasPrefix(call, fmt.Sprintf("DELETE services rule-%d", rule.ID)):
			if chainDeleted >= 0 {
				t.Fatalf("规则服务必须在 chain 之前删除，调用顺序: %v", calls)
			}
			servicesDeleted++
		}
	}
	if chainDeleted < 0 || servicesDeleted != 2 {
		t.Fatalf("应删除 chain 与两个规则服务，调用记录: %v", calls)
	}
	if got := e.ruleStatus(rule.ID); got != model.RuleStatusError {
		t.Errorf("隧道停止后规则应为 error（等待恢复或切换），实际 %s", got)
	}
	if got := e.tunnelStatus(tunnel.ID); got != model.TunnelStatusStopped {
		t.Errorf("隧道应为 stopped，实际 %s", got)
	}
}

// TestWatchdog_StatusUpdateDoesNotOverrideConcurrentStop 回归：看门狗在巡检中途写状态时，
// 不能把用户刚刚做出的停止覆盖回 running（否则下一轮会把规则重新拉起）。
func TestWatchdog_StatusUpdateDoesNotOverrideConcurrentStop(t *testing.T) {
	e := newWatchdogEnv(t)
	fake := newFakeGost(t)
	node := e.node("n1", fake)
	rule := e.forwardRule("r1", node, 10001, model.RuleStatusError)
	entryFake, exitFake := newFakeGost(t), newFakeGost(t)
	_, _, tunnel := runningTunnelOnNodes(e, entryFake, exitFake)
	if err := e.db.Model(tunnel).Update("status", model.TunnelStatusError).Error; err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}

	// 巡检开始时读到的是 error，随后用户把它们停掉
	staleRule := e.ruleRow(rule.ID)
	var staleTunnel model.GostTunnel
	if err := e.db.First(&staleTunnel, tunnel.ID).Error; err != nil {
		t.Fatalf("读取隧道失败: %v", err)
	}
	if err := e.db.Model(&model.GostRule{}).Where("id = ?", rule.ID).Update("status", model.RuleStatusStopped).Error; err != nil {
		t.Fatalf("停止规则失败: %v", err)
	}
	if err := e.db.Model(&model.GostTunnel{}).Where("id = ?", tunnel.ID).Update("status", model.TunnelStatusStopped).Error; err != nil {
		t.Fatalf("停止隧道失败: %v", err)
	}

	e.sync.watchdog.setRuleStatus(&staleRule, model.RuleStatusRunning, "test")
	e.sync.watchdog.setTunnelStatus(&staleTunnel, model.TunnelStatusRunning, "test")

	if got := e.ruleStatus(rule.ID); got != model.RuleStatusStopped {
		t.Errorf("用户停止后的规则被看门狗改回了 %s", got)
	}
	if got := e.tunnelStatus(tunnel.ID); got != model.TunnelStatusStopped {
		t.Errorf("用户停止后的隧道被看门狗改回了 %s", got)
	}
}

// TestWatchdog_CleanupSkipsObjectAlreadyGone 快照之后对象已被用户删掉时，
// 不能再记一条“看门狗清理残留”的日志。
func TestWatchdog_CleanupSkipsObjectAlreadyGone(t *testing.T) {
	e := newWatchdogEnv(t)
	fake := newFakeGost(t)
	node := e.node("n1", fake)
	rule := e.forwardRule("r1", node, 10001, model.RuleStatusStopped)
	fake.addGhost(fmt.Sprintf("rule-%d-tcp", rule.ID))

	e.tick()

	for _, call := range fake.callLog() {
		if strings.HasPrefix(call, "DELETE") {
			t.Errorf("对象已不存在时不应再删除，实际调用 %v", fake.callLog())
		}
	}
	if logs := e.logs(model.ActionCleanup); len(logs) != 0 {
		t.Errorf("对象已不存在时不应记录清理日志，实际 %+v", logs)
	}
}

// TestWatchdog_RestoreThatBreaksAgainIsRateLimited 恢复后马上又坏的对象不能每轮重建：
// 成功的恢复同样计入退避，只有巡检确认稳定后才清零。
func TestWatchdog_RestoreThatBreaksAgainIsRateLimited(t *testing.T) {
	e := newWatchdogEnv(t)
	fake := newFakeGost(t)
	node := e.node("n1", fake)
	rule := e.forwardRule("r1", node, 10001, model.RuleStatusRunning)
	creates := func() int {
		n := 0
		for _, call := range fake.callLog() {
			if call == fmt.Sprintf("CREATE services rule-%d-tcp", rule.ID) {
				n++
			}
		}
		return n
	}

	e.tick()
	if creates() != 1 {
		t.Fatalf("首次发现缺失应立即恢复，实际 %d 次", creates())
	}

	fake.restart() // 刚恢复又丢失
	e.tick()
	if creates() != 0 {
		t.Fatalf("刚恢复过的对象 10s 内不应再次重建，实际 %d 次", creates())
	}

	e.advance(11 * time.Second)
	e.tick()
	if creates() != 1 {
		t.Fatalf("退避结束后应再次恢复，实际 %d 次", creates())
	}
}

// TestRuleUpdate_RejectsWhenEntryNodeOffline 入口节点离线时删不掉旧服务，
// 编辑运行中的规则必须直接拒绝，而不是把它改成 stopped 再让看门狗当残留清掉。
func TestRuleUpdate_RejectsWhenEntryNodeOffline(t *testing.T) {
	e := newWatchdogEnv(t)
	fake := newFakeGost(t)
	node := e.node("n1", fake)
	rule := e.forwardRule("r1", node, 10001, model.RuleStatusRunning)
	if err := e.db.Model(node).Update("status", model.NodeStatusOffline).Error; err != nil {
		t.Fatalf("标记节点离线失败: %v", err)
	}

	_, err := e.sync.ruleService.Update(rule.ID, &dto.UpdateRuleReq{
		Name: "renamed", ListenPort: 10001, Targets: []string{"10.0.0.9:80"},
	}, 1, "admin", "127.0.0.1", "test")
	if err != errors.ErrNodeOffline {
		t.Fatalf("入口节点离线时应拒绝编辑，实际 %v", err)
	}
	row := e.ruleRow(rule.ID)
	if row.Status != model.RuleStatusRunning || row.Name != "r1" {
		t.Errorf("被拒绝的编辑不应改动规则，实际 status=%s name=%s", row.Status, row.Name)
	}
}

// TestRuleUpdate_ErrorRuleSavesEditWhileTunnelDown 异常中的规则在隧道不可用时也能改名，
// 并保持 error 等待看门狗恢复。
func TestRuleUpdate_ErrorRuleSavesEditWhileTunnelDown(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitFake := newFakeGost(t), newFakeGost(t)
	entry := e.node("entry", entryFake)
	exit := e.node("exit", exitFake)
	tunnel := e.tunnel("t1", entry, exit, 8443, model.TunnelStatusStopped)
	rule := e.tunnelRule("r1", tunnel, 10001, model.RuleStatusError)

	updated, err := e.sync.ruleService.Update(rule.ID, &dto.UpdateRuleReq{
		TunnelID: &tunnel.ID, Name: "renamed", ListenPort: 10001, Targets: []string{"10.0.0.9:80"},
	}, 1, "admin", "127.0.0.1", "test")
	if err != nil {
		t.Fatalf("异常规则改名不应因隧道不可用而失败: %v", err)
	}
	row := e.ruleRow(rule.ID)
	if row.Name != "renamed" || updated.Name != "renamed" {
		t.Errorf("新配置应已保存，实际 %s", row.Name)
	}
	if row.Status != model.RuleStatusError {
		t.Errorf("隧道仍不可用，规则应保持 error 等待恢复，实际 %s", row.Status)
	}
}

// TestTunnelUpdate_RequiresStop 运行中或异常的隧道都要先停止才能编辑
func TestTunnelUpdate_RequiresStop(t *testing.T) {
	e := newWatchdogEnv(t)
	entryFake, exitFake := newFakeGost(t), newFakeGost(t)
	_, exit, tunnel := runningTunnelOnNodes(e, entryFake, exitFake)
	req := &dto.UpdateTunnelReq{Name: "renamed", Hops: []dto.TunnelHopReq{{NodeID: exit.ID, Protocol: "tcp", RelayPort: 8443}}}

	for _, status := range []model.TunnelStatus{model.TunnelStatusRunning, model.TunnelStatusError} {
		if err := e.db.Model(tunnel).Update("status", status).Error; err != nil {
			t.Fatalf("准备数据失败: %v", err)
		}
		if _, err := e.sync.ruleService.tunnelService.Update(tunnel.ID, req, 1, "admin", "127.0.0.1", "test"); err != errors.ErrTunnelRunning {
			t.Errorf("%s 状态的隧道应要求先停止，实际 %v", status, err)
		}
	}
	if !entryFake.has("chains", tunnel.ChainID) {
		t.Error("被拒绝的编辑不应动节点上的 chain")
	}
}
