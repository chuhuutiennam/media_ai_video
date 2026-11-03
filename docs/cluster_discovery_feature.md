# Cluster Discovery Feature – V5 FINAL SPEC (PATCHED + IMPLEMENTATION READY)

## 1. Mục tiêu
Cluster Discovery Module dùng trong app để:
- Discover service qua Kubernetes DNS (SRV/A)
- Reconcile topology (add / remove / update node)
- Enrich capability qua gRPC handshake
- Health monitoring (heartbeat)
- Event-driven updates cho business modules

---

## 2. Architecture

CONFIG
  ↓
DNS RESOLVER (K8S)
  ↓
DISCOVERY ENGINE (TOPOLOGY OWNER)
  ↓
NODE REGISTRY (STATE STORE)
  ↓
CAPABILITY FETCHER (ASYNC WORKER)
  ↓
HEALTH MONITOR (STATUS OWNER)
  ↓
EVENT BUS (NON-BLOCKING)
  ↓
BUSINESS MODULES

---

## 3. CRITICAL CLARIFICATIONS

### 3.1 Watch vs Subscribe

Watch = public API facade  
Subscribe = internal EventBus primitive

Watch filters events from Subscribe by service name.

---

### 3.2 CapabilityFetcher Ownership

DiscoveryEngine owns CapabilityFetcher.

Flow:
DNS → Upsert Node → async Fetch → UpdateCapabilities

---

### 3.3 EventBus Publish semantics

- Publish is NON-BLOCKING
- Uses buffered channels
- Slow subscribers drop events (no backpressure blocking)

---

### 3.4 Event Node safety

Event.Node is a deep copy of registry node (no shared mutation)

---

## 4. Core Types

### Node
```go
type Node struct {
    Service      string
    ID           string
    Endpoint     Endpoint
    Capabilities NodeCapabilities
    Status       NodeStatus
}
```

---

### Endpoint
```go
type Endpoint struct {
    IP   string
    Port int
}
```

---

### NodeCapabilities
```go
type NodeCapabilities struct {
    Languages []string
    Tasks     []string

    MaxStreams    int
    ActiveStreams int32
    GPULoad       float64
}
```

---

### NodeStatus
```go
type NodeStatus string

const (
    NodeStatusUp       NodeStatus = "up"
    NodeStatusDown     NodeStatus = "down"
    NodeStatusDegraded NodeStatus = "degraded"
)
```

---

### EventType
```go
type EventType string

const (
    NODE_ADDED     EventType = "NODE_ADDED"
    NODE_REMOVED   EventType = "NODE_REMOVED"
    NODE_UPDATED   EventType = "NODE_UPDATED"
    NODE_DOWN      EventType = "NODE_DOWN"
    NODE_RECOVERED EventType = "NODE_RECOVERED"
)
```

---

## 5. Core Interfaces

### DNS Resolver
```go
type DNSResolver interface {
    Resolve(service, namespace string) ([]Endpoint, error)
}
```

---

### Discovery Engine
```go
type DiscoveryEngine interface {
    Run(ctx context.Context)
}
```

---

### CapabilityFetcher
```go
type CapabilityFetcher interface {
    Fetch(ctx context.Context, ep Endpoint) (NodeCapabilities, error)
}
```

---

### Node Registry
```go
type NodeRegistry interface {
    Upsert(node *Node) error
    Delete(nodeID string) error
    Get(nodeID string) (*Node, bool)
    List(service string) []*Node

    UpdateStatus(nodeID string, status NodeStatus) error
    UpdateCapabilities(nodeID string, caps NodeCapabilities) error
    UpdateMetrics(nodeID string, activeStreams int32, gpuLoad float64) error
}
```

---

### Health Monitor
```go
type HealthMonitor interface {
    Start(ctx context.Context)
    Check(ctx context.Context, node *Node) NodeStatus
}
```

---

### EventBus
```go
type Event struct {
    Type EventType
    Node Node
}

type EventBus interface {
    Publish(Event) // non-blocking
    Subscribe(ctx context.Context, service string) (<-chan Event, error)
}
```

---

### Cluster API
```go
type SelectHint struct {
    Language string
    Task     string
}

type ClusterDiscovery interface {
    GetNodes(service string) []*Node
    SelectNode(service string, hint SelectHint) (*Node, error)
    Watch(ctx context.Context, service string) (<-chan Event, error)
}
```

---

## 6. Config

```yaml
cluster:
  poll_interval: 10s

  services:
    - name: ai-svc
      namespace: default

    - name: media-svc
      namespace: default

health:
  interval: 3s

capability:
  timeout: 2s
```

---

## 7. Concurrency Model

- DiscoveryEngine = single writer (topology)
- HealthMonitor = status only
- CapabilityFetcher = async worker
- EventBus = non-blocking fanout

---

## 8. Routing Logic

Score = 
- GPULoad (lower better)
- ActiveStreams (lower better)
- Utilization = ActiveStreams / MaxStreams

---

## 9. Watch Semantics

- Watch = facade over Subscribe
- ctx cancel closes channel
- service filter applied in Watch

---

## 10. Failure Handling

DNS down → cache fallback  
Pod crash → DOWN  
IP change → reconcile update  
Partition → DEGRADED  

---

## 11. Final Status

This spec is fully implementation-ready:
- no missing components
- no ambiguous ownership
- safe concurrency model
- deterministic event system
