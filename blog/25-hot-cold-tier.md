# Episode 25 — Hot/Cold tier: 노드 부분집합으로 만든 계층

> **Series**: kvfs — distributed object storage from scratch
> **Season**: P4-09 wave + P5-04 · **Episode**: 12
> **연결**: `internal/coordinator/` `PlaceNFromAddrs` · `internal/edge/edge.go` `writeChunkPreferClass` · `internal/rebalance/` ClassResolver

---

## 한 알고리즘, 두 종류 디스크

S3 에는 Standard, Standard-IA, Glacier 등 객체 storage class 가 있다. kvfs 가 같은 걸 만들면 "DN 마다 등급, 등급별 다른 알고리즘" 이 자연스러운 첫 생각. 그러나 그 길 끝엔 코드 폭증.

대신 채택: **placement 알고리즘은 그대로, 노드 부분집합만 다르게**.

```
모든 DN: dn1, dn2, dn3, dn4, dn5, dn6  (6개)
  hot  라벨: dn1, dn2, dn3              (3개)
  cold 라벨: dn4, dn5, dn6              (3개)

신규 PUT (default class=hot):
  HRW.Pick(chunk_id, R=3, subset=hot DN)
  → top 3 of {dn1, dn2, dn3}
  → cold DN 들은 후보에서 아예 제외
```

기존 `placement.Pick` 알고리즘 그대로 — input 이 부분집합으로 좁아질 뿐. 코드 추가 최소.

## 새 함수 두 개

`internal/placement/placement.go`:
```go
func PickFromNodes(key string, n int, nodes []Node) []Node {
    // 기존 Pick 의 stateless 버전 — Placer 인스턴스 없이도 호출 가능.
}
```

`internal/coordinator/coordinator.go`:
```go
func (c *Coordinator) PlaceNFromAddrs(key string, n int, addrs []string) []string {
    nodes := make([]placement.Node, len(addrs))
    for i, a := range addrs { nodes[i] = placement.Node{ID: a, Addr: a} }
    out := placement.PickFromNodes(key, n, nodes)
    // ... 추출 addrs
}
```

`PlaceN` 의 가까운 친척. subset 은 caller 가 결정.

## DN class 라벨

`internal/store/store.go` 에 `dns_runtime` 버킷이 이미 있음 ([Ep.7 ADR-027](../docs/adr/ADR-027-dynamic-dn-registry.md)). 거기에 class field 만 추가:

```go
// {addr → JSON {"registered_at": ts, "class": "hot"}}
SetRuntimeDNClass(addr, class string) error
RuntimeDNClass(addr string) (string, error)
ListRuntimeDNsByClass(class string) ([]string, error)
```

admin endpoint:
```
PUT /v1/admin/dns/class?addr=dn1:8080&class=hot
```

기본 class = "" (대문자 없음). 라벨 없는 DN 들은 "default pool" 로 취급, `EDGE_PLACEMENT_PREFER` 가 hot 일 때 후보 안 됨.

## 신규 PUT 의 bias

`internal/edge/edge.go`:

```go
type Server struct {
    ...
    PlacementPreferClass string  // env: EDGE_PLACEMENT_PREFER
}

func (s *Server) writeChunkPreferClass(ctx, chunkID, data) ([]string, error) {
    if s.PlacementPreferClass == "" {
        return s.Coord.WriteChunk(ctx, chunkID, data)  // 기존 path
    }
    classDNs, _ := s.Store.ListRuntimeDNsByClass(s.PlacementPreferClass)
    if len(classDNs) < s.Coord.ReplicationFactor() {
        return s.Coord.WriteChunk(ctx, chunkID, data)  // fallback (deadlock 방지)
    }
    targets := s.Coord.PlaceNFromAddrs(chunkID, R, classDNs)
    return s.Coord.WriteChunkToAddrs(ctx, chunkID, data, targets)
}
```

3 case: 라벨 미설정 / 라벨 설정 + 충분한 DN / 라벨 설정 + DN 부족 (fallback 으로 default pool).

## P5-04: Rebalance 통합

신규 PUT 만 bias 받으면 부족하다 — **기존 cold-tier 객체가 hot DN 으로 옮겨졌다 다시 rebalance 되어야** class 일관성. 그래서 `ObjectMeta.Class` 필드 추가 + rebalance 가 인지:

```go
type ObjectMeta struct {
    ...
    Class string `json:"class,omitempty"`  // PUT 시점 의 class label
}

// rebalance.ComputePlan signature 가 ClassResolver 받음
func ComputePlan(coord Coordinator, st ObjectStore, class ClassResolver) (Plan, error) {
    for _, obj := range objs {
        if obj.Class != "" {
            // class 의 DN subset 으로 desired placement 계산
            subset := class.ListRuntimeDNsByClass(obj.Class)
            if len(subset) >= R {
                desired = coord.PlaceNFromAddrs(chunkID, R, subset)
            }
        }
        // ... 기존 vs desired diff 로 migration plan
    }
}
```

resolver 가 R 미만 DN 만 알면 default placement fallback (deadlock 방지). 안 그러면 hot DN 1개일 때 "옮겨야 한다" 면서 옮길 곳 없는 무한 loop.

테스트 둘:
- `TestComputePlan_ClassFiltered`: hot 라벨 객체가 dn* 에 있으면 hot* 로 migrate.
- `TestComputePlan_ClassUndersized_FallsBackToDefault`: hot DN 1개일 때 0 migration.

## tradeoff

- **이 모델의 한계**: lifecycle policy 없음. 객체가 "30일 후 cold 로" 같은 자동 transition 안 됨. PUT 시점 의 class 가 영구 라벨.
- **확장 여지**: `kvfs-cli class set <bucket> <key> cold` 같은 admin 으로 라벨 변경 → 다음 rebalance 가 자동 migration. 코드 ~10 LOC 면 끝.
- **Storage class enforcement 안 함**: hot 라벨 객체가 cold DN 에 있어도 GET 자체는 됨 (rebalance 가 알아서 옮김). 격리 (예: cold DN 의 더 느린 디스크) 는 운영자가 hardware 로.

## demo

```
$ ./scripts/up.sh
$ kvfs-cli dns class set --addr dn1:8080 --class hot
$ kvfs-cli dns class set --addr dn4:8080 --class cold
$ EDGE_PLACEMENT_PREFER=hot kvfs-cli put hot-bucket key1 ./big.dat
$ kvfs-cli inspect hot-bucket key1
{
  "class": "hot",
  "chunks": [{ "replicas": ["dn1:8080", "dn2:8080", "dn3:8080"] }]
}
$ EDGE_PLACEMENT_PREFER=cold kvfs-cli put cold-bucket key1 ./bigger.dat
$ kvfs-cli inspect cold-bucket key1
{
  "class": "cold",
  "chunks": [{ "replicas": ["dn4:8080", "dn5:8080", "dn6:8080"] }]
}
```

## 다음

[Ep.26 NFS deferred](26-nfs-deferred.md): "왜 안 했는가" — 종종 "안 한 결정" 이 더 가르친다.
