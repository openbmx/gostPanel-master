# Chained Tunnel Rules Design

## Goal

Add chained forwarding by upgrading tunnels from a single entry/exit pair into an ordered relay path. Rules continue to reference one tunnel, and the tunnel owns the GOST chain topology.

## Current Boundary

The current tunnel model stores `entry_node_id`, `exit_node_id`, `protocol`, and `relay_port`. Starting a tunnel creates one relay service on the exit node and one chain on the entry node. Tunnel rules then attach their forwarding services to `tunnel.chain_id`.

That boundary is correct and should stay intact. Rule services should not compose multiple tunnels, because that would move topology, failover, cleanup, and status logic into the rule layer.

## Data Model

`GostTunnel` gains a JSON `hops` field:

```go
type TunnelHop struct {
    NodeID    uint   `json:"node_id"`
    Protocol  string `json:"protocol"`
    RelayPort int    `json:"relay_port"`
}
```

`hops` is ordered. The effective path is:

`entry_node_id -> hops[0] -> hops[1] -> ... -> hops[last] -> rule targets`

Compatibility rule:

- If `hops` is empty, the legacy fields describe a one-hop tunnel using `exit_node_id`, `protocol`, and `relay_port`.
- If `hops` is present, the final hop is mirrored into `exit_node_id`, `protocol`, and `relay_port` so existing list displays and old API clients remain usable.

## Runtime Plan

Starting a tunnel builds a runtime plan:

- One relay service per hop.
- The final hop uses service name `relay-tunnel-{id}` for compatibility with existing observer parsing.
- Intermediate hops use `relay-tunnel-{id}-hop-{index}` and do not enable observer stats to avoid double counting.
- One chain on the entry node named `tunnel-{id}-chain`, with one GOST hop per configured tunnel hop.

If any step fails, the service deletes every relay or chain it already created and marks the tunnel as `error`.

## Validation

Tunnel creation and update validate:

- At least one hop exists, either explicit `hops` or legacy exit fields.
- Entry node exists and is not repeated in hops.
- Hop nodes exist.
- Hop nodes are not repeated.
- Protocol is one of the existing supported GOST listener/dialer protocol values.
- Relay ports are in `1..65535`.
- Running tunnels cannot be edited.

Entry node changes remain unsupported during update. This avoids moving existing rule listening ports to another runtime node and creating hidden port conflicts.

## Rule Behavior

Rule behavior remains unchanged:

- `forward` rules run directly on `node_id`.
- `tunnel` rules run on the tunnel entry node and attach their TCP/UDP services to `tunnel.chain_id`.
- Primary and backup tunnel failover still switches whole tunnel resources, not individual hops.

## UI

`Tunnels.vue` becomes the chain editor:

- Create dialog selects an entry node.
- A hop table lets the user add, remove, and reorder hop nodes.
- Each hop row has node, protocol, and relay port.
- The last hop is the exit.
- Running tunnels are not editable.

`Rules.vue` remains simple and displays a tunnel path summary instead of only a single entry/exit pair.

## Verification

Backend tests cover:

- Legacy tunnels produce one effective hop.
- Explicit hop lists produce ordered effective hops.
- Runtime plan emits the right relay service names and chain hops.
- Tunnel node lookup includes JSON hop nodes.
- Rule runtime node and port-conflict behavior stays based on tunnel entry node.

Frontend verification uses `npm run build`.
