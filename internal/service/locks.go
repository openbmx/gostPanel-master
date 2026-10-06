package service

import "sync"

// 规则与隧道的事务锁，串行化同一资源上的启动/停止/编辑/删除/故障转移/看门狗操作。
//
// 必须是包级别的：路由层与后台同步服务各自 New 了自己的 RuleService / TunnelService，
// 锁若挂在实例上，后台任务和用户手动操作拿到的是两把不同的锁，根本起不到互斥作用
// （此前 AutoFailover 里的 tryLockRule 就因此形同虚设）。
var (
	ruleLocks   sync.Map // map[uint]*sync.Mutex
	tunnelLocks sync.Map // map[uint]*sync.Mutex
)

// lockByID 获取指定资源的锁，返回解锁函数。
func lockByID(locks *sync.Map, id uint) func() {
	v, _ := locks.LoadOrStore(id, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// tryLockByID 尝试获取指定资源的锁；锁被占用（资源正被其他操作处理）时返回 false，
// 调用方应跳过本次操作，等下一轮再试。
func tryLockByID(locks *sync.Map, id uint) (func(), bool) {
	v, _ := locks.LoadOrStore(id, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	if !mu.TryLock() {
		return nil, false
	}
	return mu.Unlock, true
}

// lockTunnel 获取指定隧道的事务锁，返回解锁函数。
func lockTunnel(id uint) func() {
	return lockByID(&tunnelLocks, id)
}

// tryLockTunnel 尝试获取隧道事务锁，语义同 tryLockByID。
func tryLockTunnel(id uint) (func(), bool) {
	return tryLockByID(&tunnelLocks, id)
}
