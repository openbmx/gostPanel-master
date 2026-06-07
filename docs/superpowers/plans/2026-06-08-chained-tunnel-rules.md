# Chained Tunnel Rules Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Upgrade Gost Panel tunnels into ordered multi-hop chain resources while keeping rules bound to one tunnel.

**Architecture:** `GostTunnel` owns chain topology via a JSON `hops` field. `TunnelService` normalizes legacy and explicit hop data into a runtime plan, creates one relay per hop, and creates one GOST chain on the entry node. `RuleService` keeps using `tunnel.chain_id`.

**Tech Stack:** Go 1.23, GORM, SQLite JSON serializer, GOST v3 API JSON config, Vue 3, Element Plus, Vite.

---

### Task 1: Model and Repository Compatibility

**Files:**
- Modify: `internal/model/tunnel.go`
- Modify: `internal/repository/tunnel_repo.go`
- Test: `internal/model/tunnel_test.go`
- Test: `internal/repository/runtime_stats_test.go`

- [ ] Add `TunnelHop` and `GostTunnel.Hops`.
- [ ] Add `EffectiveHops`, `UsesNode`, and `LastHop` helpers.
- [ ] Update tunnel node lookup to include JSON hop nodes.
- [ ] Run `go test ./internal/model ./internal/repository`.

### Task 2: Runtime Plan Builder

**Files:**
- Modify: `internal/service/tunnel_service.go`
- Test: `internal/service/tunnel_chain_test.go`

- [ ] Add `buildTunnelRuntimePlan`.
- [ ] Preserve `relay-tunnel-{id}` for the last hop.
- [ ] Use `relay-tunnel-{id}-hop-{index}` for intermediate hops.
- [ ] Run `go test ./internal/service -run TunnelRuntimePlan`.

### Task 3: Tunnel Validation and CRUD

**Files:**
- Modify: `internal/dto/tunnel.go`
- Modify: `internal/service/tunnel_service.go`
- Test: `internal/service/tunnel_chain_test.go`

- [ ] Add `TunnelHopReq` DTO.
- [ ] Normalize explicit `hops` or legacy exit fields on create/update.
- [ ] Reject empty paths, repeated nodes, invalid protocols, and invalid relay ports.
- [ ] Keep entry node immutable on update.
- [ ] Run `go test ./internal/service`.

### Task 4: Start/Stop Multi-Hop Chains

**Files:**
- Modify: `internal/service/tunnel_service.go`
- Modify: `internal/service/sync_service.go`

- [ ] Start relay services for each hop.
- [ ] Attach observer stats only to the final hop relay.
- [ ] Create the entry chain from the ordered runtime plan.
- [ ] Roll back created services/chains on failure.
- [ ] Stop all hop relay services and the entry chain.
- [ ] Run `go test ./...`.

### Task 5: Frontend Chain Editor

**Files:**
- Modify: `web/src/views/Tunnels.vue`
- Modify: `web/src/views/Rules.vue`

- [ ] Replace the single exit selector with a hop table in tunnel create/edit.
- [ ] Add add/remove/up/down hop controls.
- [ ] Send `hops` while preserving legacy fields from the last hop.
- [ ] Render chain summaries in tunnel and rule tables.
- [ ] Run `npm run build` in `web`.

### Task 6: Final Verification

**Files:**
- Verify all modified files.

- [ ] Run `go test ./...`.
- [ ] Run `npm run build` in `web`.
- [ ] Run `git status --short` and confirm only intended files changed.
