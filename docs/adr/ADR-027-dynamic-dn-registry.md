# ADR-027 — Dynamic DN Registry (edge restart 없이 DN 추가/제거)

## 상태
Accepted · 2026-04-26 · Season 3 Ep.3

## 맥락

지금까지의 모든 데모(`demo-zeta.sh`, `demo-eta.sh`, `demo-mu.sh` 등) 가 동일 패턴 반복:

```
새 DN 추가 → docker rm -f edge → docker run edge ... -e EDGE_DNS="<...,dn7:8080>"
```

edge 가 시작 시 `EDGE_DNS` env 를 1회 읽고 `coordinator.NewWithAddrs(...)` 로 placement 고정. 결과:

1. **운영자 부담** — DN 추가/제거 마다 edge 재시작
2. **무가용 윈도우** — restart 중 ~1초 PUT/GET 거부
3. **ADR-013 auto-trigger 와 단절** — auto-trigger 가 placement 변화 감지 못 함
4. **ADR-024 EC stripe rebalance 와 단절** — 동일

해결: edge 가 동작 중 DN 집합을 받을 수 있게 하고, 변경을 영속화 (재시작 후에도 유지).

### 대안 검토

| 방식 | 장점 | 단점 |
|---|---|---|
| **In-edge admin endpoint + bbolt 영속 (이번 ADR)** | edge 1개 자족, 추가 daemon 0 | multi-edge 시 동기화 필요 (ADR-022 후보) |
| External etcd / Consul | 표준 service discovery | 외부 의존 +1, kvfs 단일 의존 (bbolt) 정책 위반 |
| DN 자체가 edge 에 register | DN 시작만으로 자동 가입 | DN 인증 + heartbeat 인프라 필요 |
| File-watch (env 파일 변경 감지) | 단순 | edge 호스트 파일시스템 의존, container 환경 어색 |

ADR-013 / ADR-024 와 같은 "in-edge 단일 인스턴스" 가정 유지가 일관성 있음.

## 결정

### 핵심 모델

1. **bbolt `dns_runtime` 버킷** — `addr → registered_at` (JSON value 는 메타정보 한 줄). 비어있으면 edge boot 시 `EDGE_DNS` env 에서 초기화.
2. **`Coordinator.UpdateNodes([]Node)` 메서드** — `sync.RWMutex` 로 placer 교체. 기존 메서드 (PlaceChunk, PlaceN, ReadChunk, WriteChunk, ListChunks, ...) 모두 RLock 으로 감쌈
3. **Admin endpoints**:
   - `GET /v1/admin/dns` — 현재 DN 목록 (이미 존재; 변경 없음)
   - `POST /v1/admin/dns` body `{"addr":"dn7:8080"}` — DN 추가
   - `DELETE /v1/admin/dns?addr=dn7:8080` — DN 제거
4. **`kvfs-cli dns add|remove|list`** — admin endpoint 호출 wrapper

### Boot 시퀀스 변경

```
opens bbolt → reads dns_runtime bucket
if empty:
    seed from EDGE_DNS env
    persist to dns_runtime bucket
else:
    use bucket contents (env override 옵션 있음: EDGE_DNS_RESET=1)
```

`EDGE_DNS_RESET=1` env 가 있으면 부팅 시 bucket 을 env 로 강제 덮어씀 — disaster recovery용.

### Coordinator 인터페이스 변경

```go
// 기존: New(cfg) 시 placer 고정
// 추가: UpdateNodes(nodes []Node) 가 RWMutex.Lock 후 swap

func (c *Coordinator) UpdateNodes(nodes []Node) error {
    if len(nodes) == 0 {
        return errors.New("UpdateNodes: empty")
    }
    c.mu.Lock()
    defer c.mu.Unlock()
    c.placer = placement.New(nodes)
    return nil
}
```

기존 메서드 (PlaceChunk, PlaceN, DNs, Nodes, WriteChunk 의 `placer.Pick`, ReadChunk 의 candidates) 는 모두 `c.mu.RLock(); defer c.mu.RUnlock()` 추가.

성능 영향: RWMutex RLock 은 contention 없을 때 ~ns 단위, 무시 가능.

### Rebalance/GC/auto-trigger 와의 자동 통합

운영자가 `kvfs-cli dns add dn7:8080` →
1. edge `Coordinator.UpdateNodes(nodes_with_dn7)` 호출
2. bbolt `dns_runtime` 에 dn7 추가
3. **다음 auto-trigger cycle** 에서 rebalance.ComputePlan 이 dn7 포함 desired 계산 → 청크 자동 마이그레이션
4. 같은 cycle 의 GC 가 해당 surplus 정리

운영자 추가 명령 0번. ADR-013 + ADR-024 의 자연 결합.

### 명시적 비범위

- **multi-edge 동기화** — bbolt 는 edge 인스턴스마다 독립. 두 edge 가 다른 DN 셋 가지면 일관성 깨짐. ADR-022 (multi-edge leader election) 후보
- **DN 자가 등록** — DN 이 시작 시 edge 에 알림. DN heartbeat / health check 인프라 필요. 별도 ADR
- **인증** — admin endpoint 가 현재 unauthed (기존 패턴). production 사용 시 mTLS + RBAC 필요 (ADR-029 + 별도)
- **DN 제거 시 자동 데이터 마이그레이션** — DELETE 는 placer 만 변경. 죽은 DN 의 데이터 복구는 ADR-025 (EC repair queue) 후보. 제거 전에 운영자가 rebalance --apply 권장

## 결과

### 긍정

- **운영자 명령 1줄로 DN 추가** (`kvfs-cli dns add ...`) — edge restart 0
- **bbolt 영속** — edge crash/restart 후 DN 셋 유지
- **auto-trigger / EC rebalance 자연 통합** — 추가 명령 없이 cluster 자동 정렬
- **테스트 가능** — UpdateNodes 가 격리된 메서드, 기존 fake 와 호환
- **EDGE_DNS env backward compatible** — 기존 demo 모두 그대로 동작 (bucket 비어있으면 env seed)

### 부정

- **multi-edge 단점 그대로** — 1 edge 가정. 두 edge 가 같은 bbolt 보면 single-writer 충돌
- **DELETE 가 데이터 안전 보장 안 함** — 죽은 DN 의 청크 복구는 별도 작업. CLI 가 경고 표시
- **EDGE_DNS_RESET 의 의미 — operator override** — 잘못 쓰면 영속 상태 날아감. 명시적 env 필수

### 트레이드오프 인정

- "왜 etcd 안 씀?" — kvfs 의 의존 정책 (bbolt 1개) 유지. multi-edge 는 별도 큰 ADR
- "왜 DN 자가 등록 안 함?" — DN heartbeat / 인증 / 죽은 DN 감지 인프라 묶음. 후속 ADR
- "왜 DELETE 가 데이터 마이그레이션 안 함?" — 명시적 분리. 운영자가 안전 시점 결정

## 데모 시나리오

```
1. ./scripts/up.sh                            # 3 DN
2. ./bin/kvfs-cli dns list                    # [dn1, dn2, dn3]
3. docker run -d --name dn4 ... kvfs-dn:dev   # DN 컨테이너만 띄움
4. ./bin/kvfs-cli dns add dn4:8080            # edge 가 즉시 dn4 인지
5. ./bin/kvfs-cli dns list                    # [dn1, dn2, dn3, dn4]
6. ./bin/kvfs-cli rebalance --plan            # dn4 로 이사할 청크 표시
7. ./bin/kvfs-cli rebalance --apply           # 자동 정렬
8. docker rm -f edge && docker run ... edge   # restart
9. ./bin/kvfs-cli dns list                    # 여전히 [dn1..dn4] (bbolt 영속)
```

## 관련

- ADR-009 (Rendezvous Hashing) — placer 가 동적 노드 셋 받음
- ADR-013 (auto-trigger) — DN 변경 후 자동 정렬 트리거
- ADR-024 (EC stripe rebalance) — EC 객체 자동 정렬
- ADR-007 (UrlKey HMAC) — admin endpoint 인증 미적용 (기존 admin 패턴)
- `internal/coordinator/` — UpdateNodes + RWMutex
- `internal/store/` — dns_runtime 버킷 추가
- `internal/edge/` — POST/DELETE /v1/admin/dns 핸들러
- `cmd/kvfs-cli/` — dns 서브커맨드

## 후속 ADR 예상

- **ADR-022 — Multi-edge leader election**: 여러 edge 가 dns_runtime 동기화
- **ADR-025 — EC repair queue**: DN 제거 시 데이터 안전 복구
- **ADR-030 — DN self-registration + heartbeat**: DN 이 자기 자신을 edge 에 등록 + 죽음 감지
