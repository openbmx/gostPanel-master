package service

import (
	stderrors "errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gost-panel/internal/model"
	"gost-panel/internal/repository"
	"gost-panel/internal/utils"
	"gost-panel/pkg/gost"
	"gost-panel/pkg/logger"

	"gorm.io/gorm"
)

// Watchdog 看门狗：把节点上的运行时配置对齐到面板数据库里的期望状态。
//
// 为什么需要它（issue #3 / #5）：面板下发的服务和链路只存在于节点 GOST 进程的内存里。
// 安装脚本固定的 GOST 3.2.6 把 SaveConfig 写到工作目录下的 gost.yaml，而进程启动时
// 只读 -C 指定的 config.yaml，所以节点进程或主机一重启，隧道和规则就全部丢失，
// 面板却可能还显示“运行中”。
//
// 也不能指望节点自己持久化：GOST 3.3.0 起会把配置写回 config.yaml，开机时只要其中
// 任何一个端口被别的程序占用，进程就直接退出，连 API 都起不来，节点从此失联。
// 因此由面板充当唯一可信的状态源，只要节点 API 可达，每轮巡检都会：
//   - 补建应当运行的规则/隧道在节点上缺失的对象，重建状态为 failed 的服务；
//   - 隧道规则所走的隧道不可用（被停止，或入口节点上缺 chain）时，切到可用的备选隧道，
//     没有就暂停转发 —— GOST 遇到不存在的 chain 会直接连接目标，流量会绕过隧道；
//   - 清理已停止、已删除资源在节点上的残留（例如节点离线期间被停止的规则）；
//   - 恢复失败时把状态记为 error，并按指数退避重试。
//
// 只处理面板自己命名的对象（rule-<id>*、relay-tunnel-<id>*、tunnel-<id>-chain），
// 数据库里查不到编号的对象一律不碰，避免误删节点上与本面板无关的配置。
type Watchdog struct {
	ruleRepo      *repository.RuleRepository
	tunnelRepo    *repository.TunnelRepository
	ruleService   *RuleService
	tunnelService *TunnelService
	logService    *LogService
	backoff       *watchdogBackoff
	now           func() time.Time
}

const (
	// serviceStateFailed GOST 报告的服务失败状态
	serviceStateFailed = "failed"

	// 两次恢复尝试之间的最短间隔：10s 起步，逐次翻倍，最长 5 分钟。
	// 巡检确认对象已稳定（无需任何动作）后才清零。
	watchdogBackoffBase = 10 * time.Second
	watchdogBackoffMax  = 5 * time.Minute

	// watchdogOperator 写操作日志时的操作者，与故障转移保持一致
	watchdogOperator = "system"
)

var (
	ruleServicePattern = regexp.MustCompile(`^rule-(\d+)(?:-tcp|-udp)?$`)
	tunnelRelayPattern = regexp.MustCompile(`^relay-tunnel-(\d+)(?:-hop-\d+)?$`)
	tunnelChainPattern = regexp.MustCompile(`^tunnel-(\d+)-chain$`)
)

// NewWatchdog 创建看门狗，复用调用方的 RuleService（及其内部的 TunnelService）
func NewWatchdog(db *gorm.DB, ruleService *RuleService) *Watchdog {
	return &Watchdog{
		ruleRepo:      repository.NewRuleRepository(db),
		tunnelRepo:    repository.NewTunnelRepository(db),
		ruleService:   ruleService,
		tunnelService: ruleService.tunnelService,
		logService:    NewLogService(db),
		backoff:       &watchdogBackoff{entries: make(map[string]*backoffEntry)},
		now:           time.Now,
	}
}

// ==================== 节点快照 ====================

// nodeSnapshot 某个在线节点在一轮巡检中读到的运行时配置
type nodeSnapshot struct {
	node          *model.GostNode
	client        *gost.Client
	services      map[string]string // 服务名 -> 运行状态
	serviceChains map[string]string // 服务名 -> 它的 handler 引用的 chain（没有则为空）
	chains        map[string]bool
}

func newNodeSnapshot(node *model.GostNode, client *gost.Client, cfg *gost.GostConfig) *nodeSnapshot {
	snap := &nodeSnapshot{
		node:          node,
		client:        client,
		services:      make(map[string]string, len(cfg.Services)),
		serviceChains: make(map[string]string, len(cfg.Services)),
		chains:        make(map[string]bool, len(cfg.Chains)),
	}
	for _, svc := range cfg.Services {
		// 部分 GOST 版本/场景下 /config 只返回服务定义、不带运行时 status。
		// 服务对象存在就不能当作缺失，否则会把正在转发的规则反复重建。
		state := "configured"
		if svc.Status != nil && svc.Status.State != "" {
			state = svc.Status.State
		}
		snap.services[svc.Name] = state
		if svc.Handler != nil {
			snap.serviceChains[svc.Name] = svc.Handler.Chain
		}
	}
	for _, chain := range cfg.Chains {
		snap.chains[chain.Name] = true
	}
	return snap
}

func (s *nodeSnapshot) has(kind objectKind, name string) bool {
	if kind == kindTunnelChain {
		return s.chains[name]
	}
	_, ok := s.services[name]
	return ok
}

// serviceHealthy 服务存在、未失败，且挂在期望的 chain 上（端口转发期望为空）
func (s *nodeSnapshot) serviceHealthy(name, chain string) bool {
	state, ok := s.services[name]
	return ok && state != serviceStateFailed && s.serviceChains[name] == chain
}

// leaks 服务引用了节点上不存在的 chain：GOST 此时会让它的流量绕过隧道直连目标
func (s *nodeSnapshot) leaks(name string) bool {
	if _, ok := s.services[name]; !ok {
		return false
	}
	chain := s.serviceChains[name]
	return chain != "" && !s.chains[chain]
}

// chainInUse 节点上是否还有服务引用这条 chain
func (s *nodeSnapshot) chainInUse(chain string) bool {
	for _, c := range s.serviceChains {
		if c == chain {
			return true
		}
	}
	return false
}

// fetchNodeSnapshot 读取单个节点当前的运行时配置
func fetchNodeSnapshot(node *model.GostNode) (*nodeSnapshot, error) {
	client := utils.GetGostClient(node)
	cfg, err := client.GetConfig()
	if err != nil {
		return nil, err
	}
	return newNodeSnapshot(node, client, cfg), nil
}

// collectNodeSnapshots 并发读取所有非离线节点的运行时配置。
// 读取失败的节点不在结果里：它们承载的规则/隧道本轮既不处理也不改状态。
func collectNodeSnapshots(nodes []model.GostNode) map[uint]*nodeSnapshot {
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		snaps = make(map[uint]*nodeSnapshot, len(nodes))
	)
	for i := range nodes {
		node := &nodes[i]
		if node.Status == model.NodeStatusOffline {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap, err := fetchNodeSnapshot(node)
			if err != nil {
				logger.Debugf("[Watchdog] 获取节点 %d (%s) 配置失败: %v", node.ID, node.Name, err)
				return
			}
			mu.Lock()
			snaps[node.ID] = snap
			mu.Unlock()
		}()
	}
	wg.Wait()
	return snaps
}

// freshSnapshots 持锁后重新读取指定节点的配置。本轮开始时的快照可能早于
// 刚刚完成的手动操作，基于旧快照动手会重复下发甚至误删。
func freshSnapshots(nodeIDs []uint, nodes map[uint]*model.GostNode) map[uint]*nodeSnapshot {
	snaps := make(map[uint]*nodeSnapshot, len(nodeIDs))
	for _, id := range nodeIDs {
		node := nodes[id]
		if node == nil || node.Status == model.NodeStatusOffline {
			continue
		}
		if snap, err := fetchNodeSnapshot(node); err == nil {
			snaps[id] = snap
		}
	}
	return snaps
}

// ==================== 退避 ====================

// watchdogBackoff 按资源记录恢复尝试，安排下一次允许尝试的时间。
//
// 成功的恢复也计入尝试次数：如果对象恢复后马上又坏（例如服务一创建就进入 failed），
// 不能每 5 秒重建一次。只有巡检确认对象已经稳定时才 reset。
type watchdogBackoff struct {
	mu      sync.Mutex
	entries map[string]*backoffEntry
}

type backoffEntry struct {
	attempts int // 自上次稳定以来的尝试次数，决定退避时长
	failures int // 连续失败次数，用于只在首次失败时写操作日志
	next     time.Time
}

func (b *watchdogBackoff) ready(key string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.entries[key]
	return !ok || !now.Before(e.next)
}

func (b *watchdogBackoff) record(key string, now time.Time, failed bool) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.entries[key]
	if !ok {
		e = &backoffEntry{}
		b.entries[key] = e
	}
	e.attempts++
	if failed {
		e.failures++
	} else {
		e.failures = 0
	}
	delay := watchdogBackoffMax
	if e.attempts <= 6 {
		if d := watchdogBackoffBase << (e.attempts - 1); d < watchdogBackoffMax {
			delay = d
		}
	}
	e.next = now.Add(delay)
	return e.failures
}

// fail 记录一次失败的尝试，返回这是连续第几次失败
func (b *watchdogBackoff) fail(key string, now time.Time) int {
	return b.record(key, now, true)
}

// attempt 记录一次成功的尝试
func (b *watchdogBackoff) attempt(key string, now time.Time) {
	b.record(key, now, false)
}

// reset 对象已稳定，清除记录，下次出问题时立即处理；返回此前是否有记录
func (b *watchdogBackoff) reset(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.entries[key]
	delete(b.entries, key)
	return ok
}

// prune 丢弃不再需要的记录（资源已停止/删除），避免下次出问题时沿用旧的失败计数
func (b *watchdogBackoff) prune(keep func(key string) bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for key := range b.entries {
		if !keep(key) {
			delete(b.entries, key)
		}
	}
}

func tunnelKey(id uint) string      { return fmt.Sprintf("tunnel:%d", id) }
func ruleRestoreKey(id uint) string { return fmt.Sprintf("rule:%d", id) }
func ruleRouteKey(id uint) string   { return fmt.Sprintf("rule-route:%d", id) }
func cleanupKey(nodeID uint, name string) string {
	return fmt.Sprintf("cleanup:%d:%s", nodeID, name)
}

// ==================== 期望状态 ====================

// desiredState 本轮巡检时数据库里的期望状态
type desiredState struct {
	nodes          map[uint]*model.GostNode
	tunnels        map[uint]*model.GostTunnel
	rules          map[uint]*model.GostRule
	deletedTunnels map[uint]bool
	deletedRules   map[uint]bool
}

func (w *Watchdog) loadDesiredState(nodes []model.GostNode) (*desiredState, error) {
	state := &desiredState{
		nodes:          make(map[uint]*model.GostNode, len(nodes)),
		tunnels:        make(map[uint]*model.GostTunnel),
		rules:          make(map[uint]*model.GostRule),
		deletedTunnels: make(map[uint]bool),
		deletedRules:   make(map[uint]bool),
	}
	for i := range nodes {
		state.nodes[nodes[i].ID] = &nodes[i]
	}

	tunnels, _, err := w.tunnelRepo.List(nil)
	if err != nil {
		return nil, err
	}
	for i := range tunnels {
		state.tunnels[tunnels[i].ID] = &tunnels[i]
	}

	rules, _, err := w.ruleRepo.List(nil)
	if err != nil {
		return nil, err
	}
	for i := range rules {
		state.rules[rules[i].ID] = &rules[i]
	}

	deletedTunnels, err := w.tunnelRepo.ListDeletedIDs()
	if err != nil {
		return nil, err
	}
	for _, id := range deletedTunnels {
		state.deletedTunnels[id] = true
	}

	deletedRules, err := w.ruleRepo.ListDeletedIDs()
	if err != nil {
		return nil, err
	}
	for _, id := range deletedRules {
		state.deletedRules[id] = true
	}
	return state, nil
}

// ==================== 一轮对账 ====================

// Reconcile 执行一轮对账。nodes 为全部节点，snapshots 为本轮成功读取的节点配置。
func (w *Watchdog) Reconcile(nodes []model.GostNode, snapshots map[uint]*nodeSnapshot) {
	state, err := w.loadDesiredState(nodes)
	if err != nil {
		logger.Errorf("[Watchdog] 读取期望状态失败: %v", err)
		return
	}

	// 1. 先清理规则残留：已删除规则的服务可能正占着新规则要用的端口
	cleaned := w.cleanupStale(state, snapshots, kindRuleService)

	// 2. 再恢复隧道：规则服务通过名称引用隧道的 chain
	for _, id := range sortedIDs(state.tunnels) {
		if t := state.tunnels[id]; t.Status.WantsRunning() {
			w.reconcileTunnel(t, state, snapshots)
		}
	}

	// 3. 再处理规则：包括把走在不可用隧道上的规则切走或暂停
	for _, id := range sortedIDs(state.rules) {
		if r := state.rules[id]; r.Status.WantsRunning() {
			w.reconcileRule(r, state, snapshots)
		}
	}

	// 4. 最后清理隧道残留。chain 必须在第 3 步处理完引用它的规则之后才能删，
	//    否则这些规则的流量会在两步之间绕过隧道直连目标。
	for key := range w.cleanupStale(state, snapshots, kindTunnelRelay, kindTunnelChain) {
		cleaned[key] = true
	}

	w.pruneBackoff(state, cleaned)
}

// pruneBackoff 丢弃已停止/已删除资源的退避记录，以及本轮已不再出现的清理记录
func (w *Watchdog) pruneBackoff(state *desiredState, cleanupSeen map[string]bool) {
	active := make(map[string]bool)
	for id, t := range state.tunnels {
		if t.Status.WantsRunning() {
			active[tunnelKey(id)] = true
		}
	}
	for id, r := range state.rules {
		if r.Status.WantsRunning() {
			active[ruleRestoreKey(id)] = true
			active[ruleRouteKey(id)] = true
		}
	}
	w.backoff.prune(func(key string) bool { return active[key] || cleanupSeen[key] })
}

// ==================== 清理残留 ====================

type objectKind int

const (
	kindRuleService objectKind = iota
	kindTunnelRelay
	kindTunnelChain
)

// panelObject 节点上一个由面板命名的对象
type panelObject struct {
	kind objectKind
	id   uint // 所属规则/隧道 ID
	name string
}

func (o panelObject) isRule() bool { return o.kind == kindRuleService }

// cleanupStale 删除节点上指定类型中多余的面板对象，返回本轮涉及的清理退避键
func (w *Watchdog) cleanupStale(state *desiredState, snapshots map[uint]*nodeSnapshot, kinds ...objectKind) map[string]bool {
	seen := make(map[string]bool)
	for _, nodeID := range sortedIDs(snapshots) {
		snap := snapshots[nodeID]
		for _, obj := range panelObjects(snap) {
			if !containsKind(kinds, obj.kind) || !w.isStale(obj, nodeID, state) {
				continue
			}
			seen[cleanupKey(nodeID, obj.name)] = true
			w.cleanupObject(obj, snap.node)
		}
	}
	return seen
}

func containsKind(kinds []objectKind, kind objectKind) bool {
	for _, k := range kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// panelObjects 列出节点上由面板命名的服务与链路
func panelObjects(snap *nodeSnapshot) []panelObject {
	var objs []panelObject
	for _, name := range sortedKeys(snap.services) {
		if id, ok := matchID(ruleServicePattern, name); ok {
			objs = append(objs, panelObject{kind: kindRuleService, id: id, name: name})
		} else if id, ok := matchID(tunnelRelayPattern, name); ok {
			objs = append(objs, panelObject{kind: kindTunnelRelay, id: id, name: name})
		}
	}
	for _, name := range sortedKeys(snap.chains) {
		if id, ok := matchID(tunnelChainPattern, name); ok {
			objs = append(objs, panelObject{kind: kindTunnelChain, id: id, name: name})
		}
	}
	return objs
}

// isStale 判断节点上的面板对象是否应当删除。
// 资源已删除或已停止 → 删除；仍应运行但对象不该出现在这个节点上（例如编辑隧道时
// 旧中转节点离线、没删掉的 relay）→ 删除；数据库里查不到编号 → 不是本面板的，不碰。
func (w *Watchdog) isStale(obj panelObject, nodeID uint, state *desiredState) bool {
	if obj.isRule() {
		if state.deletedRules[obj.id] {
			return true
		}
		rule := state.rules[obj.id]
		if rule == nil {
			return false
		}
		return ruleObjectStale(rule, state.tunnels[derefUint(rule.TunnelID)], nodeID)
	}

	if state.deletedTunnels[obj.id] {
		return true
	}
	tunnel := state.tunnels[obj.id]
	if tunnel == nil {
		return false
	}
	return tunnelObjectStale(tunnel, obj, nodeID)
}

// ruleObjectStale 判断规则服务在该节点上是否多余（规则本身存在时）
func ruleObjectStale(rule *model.GostRule, tunnel *model.GostTunnel, nodeID uint) bool {
	if !rule.Status.WantsRunning() {
		return true
	}
	entryID := ruleEntryNodeID(rule, tunnel)
	return entryID != 0 && entryID != nodeID
}

// tunnelObjectStale 判断隧道对象在该节点上是否多余（隧道本身存在时）
func tunnelObjectStale(tunnel *model.GostTunnel, obj panelObject, nodeID uint) bool {
	if !tunnel.Status.WantsRunning() {
		return true
	}
	if obj.kind == kindTunnelChain {
		return nodeID != tunnel.EntryNodeID
	}
	expectedNode, ok := tunnelRelayNames(tunnel)[obj.name]
	return !ok || expectedNode != nodeID
}

// cleanupObject 持锁复核后删除节点上的残留对象
func (w *Watchdog) cleanupObject(obj panelObject, node *model.GostNode) {
	key := cleanupKey(node.ID, obj.name)
	if !w.backoff.ready(key, w.now()) {
		return
	}

	var (
		unlock func()
		ok     bool
	)
	if obj.isRule() {
		unlock, ok = w.ruleService.tryLockRule(obj.id)
	} else {
		unlock, ok = tryLockTunnel(obj.id)
	}
	if !ok {
		return // 正被用户操作处理，下一轮再看
	}
	defer unlock()

	// 持锁后按数据库最新状态复核：用户可能刚刚启动了它
	resourceType, resourceName, stale := w.recheckStale(obj, node.ID)
	if !stale {
		return
	}
	// 再按节点最新配置复核：本轮快照可能早于用户刚完成的停止/删除，对象其实已经不在了
	snap, err := fetchNodeSnapshot(node)
	if err != nil || !snap.has(obj.kind, obj.name) {
		w.backoff.reset(key)
		return
	}
	// 还有服务引用这条 chain 时不能删，否则它们的流量会绕过隧道直连目标。
	// 正常情况下引用它的规则已在第 3 步被切走或暂停；走到这里说明节点与数据库不一致，下一轮再看
	if obj.kind == kindTunnelChain && snap.chainInUse(obj.name) {
		// 可能是与面板无关的服务在用它，会一直存在：按退避复查，只提示一次
		if w.backoff.fail(key, w.now()) == 1 {
			logger.Warnf("[Watchdog] 节点 %s 上的 %s 仍被服务引用，暂不清理", node.Name, obj.name)
		}
		return
	}

	if obj.kind == kindTunnelChain {
		err = snap.client.DeleteChain(obj.name)
	} else {
		err = snap.client.DeleteService(obj.name)
	}
	if err != nil {
		w.backoff.fail(key, w.now())
		logger.Warnf("[Watchdog] 清理节点 %s 上的残留对象 %s 失败: %v", node.Name, obj.name, err)
		return
	}
	_ = snap.client.SaveConfig()
	w.backoff.reset(key)

	logger.Infof("[Watchdog] 已清理节点 %s 上的残留对象 %s", node.Name, obj.name)
	w.record(model.ActionCleanup, resourceType, obj.id,
		fmt.Sprintf("看门狗清理%s %s 在节点 %s 上的残留：%s", resourceTypeText(resourceType), resourceName, node.Name, obj.name))
}

// recheckStale 从数据库重新读取资源后复核是否仍应清理
func (w *Watchdog) recheckStale(obj panelObject, nodeID uint) (resourceType, resourceName string, stale bool) {
	if obj.isRule() {
		resourceType = model.ResourceTypeRule
		rule, err := w.ruleRepo.FindByID(obj.id)
		if err != nil {
			// 只有确认已删除才清理；数据库出错或查无此 ID 都不动
			deleted := stderrors.Is(err, gorm.ErrRecordNotFound) && w.isDeleted(obj)
			return resourceType, fmt.Sprintf("#%d（已删除）", obj.id), deleted
		}
		return resourceType, rule.Name, ruleObjectStale(rule, rule.Tunnel, nodeID)
	}

	resourceType = model.ResourceTypeTunnel
	tunnel, err := w.tunnelRepo.FindByID(obj.id)
	if err != nil {
		deleted := stderrors.Is(err, gorm.ErrRecordNotFound) && w.isDeleted(obj)
		return resourceType, fmt.Sprintf("#%d（已删除）", obj.id), deleted
	}
	return resourceType, tunnel.Name, tunnelObjectStale(tunnel, obj, nodeID)
}

func (w *Watchdog) isDeleted(obj panelObject) bool {
	var (
		deleted bool
		err     error
	)
	if obj.isRule() {
		deleted, err = w.ruleRepo.IsDeleted(obj.id)
	} else {
		deleted, err = w.tunnelRepo.IsDeleted(obj.id)
	}
	return err == nil && deleted
}

// ==================== 隧道 ====================

// reconcileTunnel 检查一条应当运行的隧道，缺什么补什么，并校正状态
func (w *Watchdog) reconcileTunnel(t *model.GostTunnel, state *desiredState, snapshots map[uint]*nodeSnapshot) {
	key := tunnelKey(t.ID)
	plan, err := tunnelPlan(t, state.nodes)
	if err != nil {
		// 节点记录缺失或地址为空，无从恢复，只能标记异常
		w.setTunnelStatus(t, model.TunnelStatusError, fmt.Sprintf("无法生成运行计划: %v", err))
		return
	}

	drift, verified := tunnelDrift(t, plan, snapshots)
	if len(drift) == 0 {
		w.backoff.reset(key)
		if verified {
			w.setTunnelStatus(t, model.TunnelStatusRunning, "各节点上的 relay 与 chain 均已就绪")
		}
		return
	}
	if !w.backoff.ready(key, w.now()) {
		return
	}

	unlock, ok := tryLockTunnel(t.ID)
	if !ok {
		return
	}
	defer unlock()

	// 持锁后重新加载：用户可能刚刚停止或删除了它
	fresh, err := w.tunnelRepo.FindByID(t.ID)
	if err != nil || !fresh.Status.WantsRunning() {
		return
	}
	if plan, err = tunnelPlan(fresh, state.nodes); err != nil {
		w.setTunnelStatus(fresh, model.TunnelStatusError, fmt.Sprintf("无法生成运行计划: %v", err))
		return
	}

	snaps := freshSnapshots(tunnelNodeIDs(fresh), state.nodes)
	restored, err := w.tunnelService.restoreRuntime(fresh, plan, snaps)
	if err != nil {
		failures := w.backoff.fail(key, w.now())
		logger.Warnf("[Watchdog] 恢复隧道 %d (%s) 失败（连续第 %d 次）: %v", fresh.ID, fresh.Name, failures, err)
		w.setTunnelStatus(fresh, model.TunnelStatusError, err.Error())
		if failures == 1 {
			w.record(model.ActionRecover, model.ResourceTypeTunnel, fresh.ID,
				fmt.Sprintf("看门狗恢复隧道 %s 失败：%v（将自动重试）", fresh.Name, err))
		}
		return
	}
	w.backoff.attempt(key, w.now())

	if len(restored) > 0 {
		logger.Infof("[Watchdog] 已恢复隧道 %d (%s): %s", fresh.ID, fresh.Name, strings.Join(restored, ", "))
		w.record(model.ActionRecover, model.ResourceTypeTunnel, fresh.ID,
			fmt.Sprintf("看门狗自动恢复隧道 %s：重新下发 %s", fresh.Name, strings.Join(restored, "、")))
	}
	if len(snaps) == len(tunnelNodeIDs(fresh)) {
		w.setTunnelStatus(fresh, model.TunnelStatusRunning, "已自动恢复")
	}
}

// tunnelPlan 生成隧道的运行计划；chain 名称以数据库记录为准，与规则引用的保持一致
func tunnelPlan(t *model.GostTunnel, nodes map[uint]*model.GostNode) (*tunnelRuntimePlan, error) {
	plan, err := buildTunnelRuntimePlan(t, nodes)
	if err != nil {
		return nil, err
	}
	plan.Chain.Name = tunnelChainName(t)
	return plan, nil
}

// tunnelChainName 隧道在入口节点上的 chain 名称
func tunnelChainName(t *model.GostTunnel) string {
	if t.ChainID != "" {
		return t.ChainID
	}
	return fmt.Sprintf("tunnel-%d-chain", t.ID)
}

// tunnelDrift 返回隧道在各在线节点上缺失或失败的对象；
// verified 表示隧道涉及的所有节点本轮都读到了配置（能够确认整条链路完好）。
func tunnelDrift(t *model.GostTunnel, plan *tunnelRuntimePlan, snapshots map[uint]*nodeSnapshot) (drift []string, verified bool) {
	verified = true
	for _, relay := range plan.Relays {
		snap := snapshots[relay.NodeID]
		if snap == nil {
			verified = false
			continue
		}
		if state, ok := snap.services[relay.Service.Name]; !ok || state == serviceStateFailed {
			drift = append(drift, relay.Service.Name)
		}
	}
	if snap := snapshots[t.EntryNodeID]; snap == nil {
		verified = false
	} else if !snap.chains[plan.Chain.Name] {
		drift = append(drift, plan.Chain.Name)
	}
	return drift, verified
}

// tunnelRelayNames 返回隧道各 relay 服务名及其所在节点（与 buildTunnelRuntimePlan 的命名一致）
func tunnelRelayNames(t *model.GostTunnel) map[string]uint {
	hops := t.EffectiveHops()
	names := make(map[string]uint, len(hops))
	for i, hop := range hops {
		name := fmt.Sprintf("relay-tunnel-%d-hop-%d", t.ID, i)
		if i == len(hops)-1 {
			name = fmt.Sprintf("relay-tunnel-%d", t.ID)
		}
		names[name] = hop.NodeID
	}
	return names
}

// tunnelNodeIDs 返回隧道涉及的全部节点（入口 + 各跳），去重
func tunnelNodeIDs(t *model.GostTunnel) []uint {
	seen := map[uint]bool{t.EntryNodeID: true}
	ids := []uint{t.EntryNodeID}
	for _, hop := range t.EffectiveHops() {
		if !seen[hop.NodeID] {
			seen[hop.NodeID] = true
			ids = append(ids, hop.NodeID)
		}
	}
	return ids
}

// setTunnelStatus 以 CAS 方式改写状态：只有数据库里仍是 t.Status 时才生效，
// 不会覆盖用户在本轮巡检期间做出的停止/编辑。
func (w *Watchdog) setTunnelStatus(t *model.GostTunnel, status model.TunnelStatus, reason string) {
	if t.Status == status {
		return
	}
	changed, err := w.tunnelRepo.CompareAndSetStatus(t.ID, t.Status, status)
	if err != nil {
		logger.Errorf("[Watchdog] 更新隧道 %d 状态失败: %v", t.ID, err)
		return
	}
	if !changed {
		logger.Debugf("[Watchdog] 隧道 %d 状态已被并发修改，放弃改为 %s", t.ID, status)
		return
	}
	logger.Infof("[Watchdog] 隧道 %d (%s) 状态变更: %s -> %s（%s）", t.ID, t.Name, t.Status, status, reason)
	t.Status = status
}

// ==================== 规则 ====================

// reconcileRule 检查一条应当运行的规则：路由不安全就切走或暂停，缺服务或挂错链路就重建，并校正状态
func (w *Watchdog) reconcileRule(r *model.GostRule, state *desiredState, snapshots map[uint]*nodeSnapshot) {
	tunnel := state.tunnels[derefUint(r.TunnelID)]
	snap := snapshots[ruleEntryNodeID(r, tunnel)]
	if snap == nil {
		// 入口节点离线或本轮读取失败：保留最后已知状态，等它回来再说
		return
	}

	// 规则服务正挂在节点上不存在的 chain 上：流量此刻就在绕过隧道直连目标，不等退避，立即处理
	if ruleLeaks(r, snap) {
		w.handleRule(r.ID, state)
		return
	}

	routeKey, restoreKey := ruleRouteKey(r.ID), ruleRestoreKey(r.ID)
	if r.Type == model.RuleTypeTunnel && !tunnelRouteSafe(tunnel, snap) {
		if w.backoff.ready(routeKey, w.now()) {
			w.handleRule(r.ID, state)
		}
		return
	}
	// 隧道刚从不可用恢复：暂停期间累积的退避（包括恢复退避）一律作废，立即按正常路径恢复
	if w.backoff.reset(routeKey) {
		w.backoff.reset(restoreKey)
	}

	needsRestore, observed := ruleDrift(r, expectedRuleChain(r, tunnel), snap)
	if !needsRestore {
		w.backoff.reset(restoreKey)
		w.setRuleStatus(r, observed, "入口节点上的服务已就绪")
		return
	}
	if w.backoff.ready(restoreKey, w.now()) {
		w.handleRule(r.ID, state)
	}
}

// handleRule 持锁、重新加载规则与入口节点配置后，执行切换/暂停/恢复
func (w *Watchdog) handleRule(id uint, state *desiredState) {
	unlock, ok := w.ruleService.tryLockRule(id)
	if !ok {
		return
	}
	defer unlock()

	// 用户可能刚刚停止、编辑或启动了它
	rule, err := w.ruleRepo.FindByID(id)
	if err != nil || !rule.Status.WantsRunning() {
		return
	}
	entryID := ruleEntryNodeID(rule, rule.Tunnel)
	snap := freshSnapshots([]uint{entryID}, state.nodes)[entryID]
	if snap == nil {
		return
	}

	if rule.Type == model.RuleTypeTunnel && !tunnelRouteSafe(rule.Tunnel, snap) {
		w.rerouteRule(rule, snap)
		return
	}
	if w.backoff.reset(ruleRouteKey(id)) {
		w.backoff.reset(ruleRestoreKey(id))
	}

	chainID := expectedRuleChain(rule, rule.Tunnel)
	needsRestore, observed := ruleDrift(rule, chainID, snap)
	if !needsRestore {
		w.backoff.reset(ruleRestoreKey(id))
		w.setRuleStatus(rule, observed, "入口节点上的服务已就绪")
		return
	}

	restored, err := w.ruleService.restoreRuleServices(snap.client, rule, chainID, snap)
	if len(restored) > 0 {
		logger.Infof("[Watchdog] 已恢复规则 %d (%s): 节点 %s 上的 %s", rule.ID, rule.Name, snap.node.Name, strings.Join(restored, ", "))
		w.record(model.ActionRecover, model.ResourceTypeRule, rule.ID,
			fmt.Sprintf("看门狗自动恢复规则 %s：在节点 %s 上重新下发 %s", rule.Name, snap.node.Name, strings.Join(restored, "、")))
	}
	if err != nil {
		failures := w.backoff.fail(ruleRestoreKey(id), w.now())
		logger.Warnf("[Watchdog] 恢复规则 %d (%s) 失败（连续第 %d 次）: %v", rule.ID, rule.Name, failures, err)
		w.setRuleStatus(rule, model.RuleStatusError, err.Error())
		if failures == 1 {
			w.record(model.ActionRecover, model.ResourceTypeRule, rule.ID,
				fmt.Sprintf("看门狗恢复规则 %s 失败：%v（将自动重试）", rule.Name, err))
		}
		return
	}
	w.backoff.attempt(ruleRestoreKey(id), w.now())
	w.setRuleStatus(rule, model.RuleStatusRunning, "已自动恢复")
}

// rerouteRule 规则当前所走的隧道不可用：按启动时的优先级（主隧道 → 当前 → 备选）
// 切到第一条入口节点上确有 chain 的可用隧道；一条都没有就暂停转发（fail closed）。
// 调用方必须持有该规则的锁。
func (w *Watchdog) rerouteRule(rule *model.GostRule, snap *nodeSnapshot) {
	key := ruleRouteKey(rule.ID)
	base := fmt.Sprintf("rule-%d", rule.ID)
	fromName := "（无）"
	if rule.Tunnel != nil {
		fromName = rule.Tunnel.Name
	}

	var selected *model.GostTunnel
	for _, t := range w.ruleService.availableTunnels(rule) {
		if snap.chains[tunnelChainName(t)] {
			selected = t
			break
		}
	}

	if selected != nil {
		if rule.TunnelID == nil || *rule.TunnelID != selected.ID {
			_ = w.ruleRepo.UpdateFields(&model.GostRule{}, rule.ID, map[string]any{"tunnel_id": selected.ID})
			rule.TunnelID = &selected.ID
		}
		// 整组服务按新链路重建（buildAndStartService 会先删掉引用旧链路的服务，删不掉就中止）
		if err := w.ruleService.buildAndStartService(snap.client, rule, base, tunnelChainName(selected)); err != nil {
			failures := w.backoff.fail(key, w.now())
			logger.Warnf("[Watchdog] 规则 %d (%s) 切换到隧道 %s 失败: %v", rule.ID, rule.Name, selected.Name, err)
			w.setRuleStatus(rule, model.RuleStatusError, err.Error())
			if failures == 1 {
				w.record(model.ActionRecover, model.ResourceTypeRule, rule.ID,
					fmt.Sprintf("看门狗将规则 %s 切换到隧道 %s 失败：%v（将自动重试）", rule.Name, selected.Name, err))
			}
			return
		}
		w.backoff.reset(key)
		logger.Infof("[Watchdog] 规则 %d (%s) 已切换到隧道 %s（原隧道 %s 不可用）", rule.ID, rule.Name, selected.Name, fromName)
		w.record(model.ActionRecover, model.ResourceTypeRule, rule.ID,
			fmt.Sprintf("看门狗将规则 %s 切换到隧道 %s 并恢复转发（原隧道 %s 不可用）", rule.Name, selected.Name, fromName))
		return
	}

	// 没有可用隧道：删除节点上的规则服务，宁可中断也不能让流量绕过隧道直连目标。
	// 删除失败的服务若仍在泄漏，下一轮会跳过退避立即再试
	var removed []string
	for _, name := range ruleServiceNames(rule) {
		if _, ok := snap.services[name]; !ok {
			continue
		}
		if err := snap.client.DeleteService(name); err != nil {
			logger.Warnf("[Watchdog] 暂停规则 %s 时删除服务 %s 失败: %v", rule.Name, name, err)
			continue
		}
		removed = append(removed, name)
	}
	if len(removed) > 0 {
		_ = snap.client.SaveConfig()
	}
	w.backoff.fail(key, w.now())
	w.setRuleStatus(rule, model.RuleStatusError, "所走隧道不可用")
	if len(removed) > 0 {
		logger.Warnf("[Watchdog] 规则 %d (%s) 所走隧道 %s 不可用，已暂停转发", rule.ID, rule.Name, fromName)
		w.record(model.ActionRecover, model.ResourceTypeRule, rule.ID,
			fmt.Sprintf("规则 %s 所走的隧道 %s 不可用（未运行或入口节点上缺少链路），已暂停转发，避免流量绕过隧道直连目标；隧道恢复后自动重启", rule.Name, fromName))
	}
}

// tunnelRouteSafe 隧道规则能否安全地走这条隧道：隧道仍应运行，且入口节点上确实有它的 chain。
// GOST 遇到不存在的 chain 不会报错而是直接连接目标，所以 chain 缺失时绝不能留着规则服务。
func tunnelRouteSafe(t *model.GostTunnel, snap *nodeSnapshot) bool {
	return t != nil && t.Status.WantsRunning() && snap.chains[tunnelChainName(t)]
}

// expectedRuleChain 规则服务应当挂的 chain：隧道转发为当前隧道的 chain，端口转发为空
func expectedRuleChain(r *model.GostRule, tunnel *model.GostTunnel) string {
	if r.Type == model.RuleTypeTunnel && tunnel != nil {
		return tunnelChainName(tunnel)
	}
	return ""
}

// ruleServiceNames 规则在入口节点上可能存在的全部服务名（含不带协议后缀的旧结构）
func ruleServiceNames(r *model.GostRule) []string {
	base := fmt.Sprintf("rule-%d", r.ID)
	return []string{base, base + "-tcp", base + "-udp"}
}

// ruleLeaks 规则在入口节点上是否有服务引用着不存在的 chain
func ruleLeaks(r *model.GostRule, snap *nodeSnapshot) bool {
	for _, name := range ruleServiceNames(r) {
		if snap.leaks(name) {
			return true
		}
	}
	return false
}

// ruleDrift 判断规则在入口节点上是否需要重建：子服务缺失、失败、挂错了 chain，
// 或残留着不带协议后缀的旧单服务结构。不需要时一并给出观察到的状态。
func ruleDrift(r *model.GostRule, chain string, snap *nodeSnapshot) (needsRestore bool, observed model.RuleStatus) {
	base := fmt.Sprintf("rule-%d", r.ID)
	if _, ok := snap.services[base]; ok {
		return true, ""
	}
	for _, name := range []string{base + "-tcp", base + "-udp"} {
		if !snap.serviceHealthy(name, chain) {
			return true, ""
		}
	}
	return false, model.RuleStatusRunning
}

// ruleEntryNodeID 返回规则实际运行所在的入口节点：
// 端口转发取 rule.NodeID，隧道转发取当前隧道的入口节点；无法确定时返回 0。
func ruleEntryNodeID(r *model.GostRule, tunnel *model.GostTunnel) uint {
	if r.Type == model.RuleTypeTunnel {
		if tunnel != nil {
			return tunnel.EntryNodeID
		}
		return 0
	}
	return derefUint(r.NodeID)
}

// setRuleStatus 以 CAS 方式改写状态：只有数据库里仍是 r.Status 时才生效，
// 不会覆盖用户在本轮巡检期间做出的停止/编辑。
func (w *Watchdog) setRuleStatus(r *model.GostRule, status model.RuleStatus, reason string) {
	if r.Status == status {
		return
	}
	changed, err := w.ruleRepo.CompareAndSetStatus(r.ID, r.Status, status)
	if err != nil {
		logger.Errorf("[Watchdog] 更新规则 %d 状态失败: %v", r.ID, err)
		return
	}
	if !changed {
		logger.Debugf("[Watchdog] 规则 %d 状态已被并发修改，放弃改为 %s", r.ID, status)
		return
	}
	logger.Infof("[Watchdog] 规则 %d (%s) 状态变更: %s -> %s（%s）", r.ID, r.Name, r.Status, status, reason)
	r.Status = status
}

// ==================== 工具 ====================

func (w *Watchdog) record(action, resourceType string, resourceID uint, details string) {
	w.logService.Record(0, watchdogOperator, action, resourceType, resourceID, details, "", "")
}

func resourceTypeText(resourceType string) string {
	if resourceType == model.ResourceTypeTunnel {
		return "隧道"
	}
	return "规则"
}

func matchID(pattern *regexp.Regexp, name string) (uint, bool) {
	m := pattern.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	id, err := strconv.ParseUint(m[1], 10, 32)
	if err != nil || id == 0 {
		return 0, false
	}
	return uint(id), true
}

func derefUint(p *uint) uint {
	if p == nil {
		return 0
	}
	return *p
}

func sortedIDs[V any](m map[uint]V) []uint {
	ids := make([]uint, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
