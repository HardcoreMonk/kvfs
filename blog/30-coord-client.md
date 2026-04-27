# Episode 30 — edge 가 coord 를 처음 부른다

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 5 (coord 분리) · **Episode**: 2
> **연결**: ADR-015 (Ep.1 의 결정 본문) · [demo-bet](../scripts/demo-bet.sh)

---

## ADR 없는 episode

이 ep 의 결정은 ADR-015 (Ep.1) 에 이미 박혀있다. Ep.2 자체는 **wiring** —
새 결정 없이 코드를 쓰면 된다. ADR 의 spirit ("결정의 기록") 에 부합 하지
않으니 ADR 결번 (별도 ADR 부재) 을 의도적으로 둔다. 본 글이 그 wiring 의
설명 + trade-off 기록.

```
S5 Ep.1: coord 가 standalone 으로 산다 (edge 모름)
S5 Ep.2: edge 가 EDGE_COORD_URL 설정되면 coord 로 RPC, 미설정이면 인라인
```

backward compat 100% 가 결정의 핵심.

## CoordClient — 4 RPC × HTTP/JSON

`internal/edge/coord_client.go`:

```go
type CoordClient struct {
    baseURL string
    HTTP    *http.Client
    Timeout time.Duration
    ...
}

func (c *CoordClient) PlaceN(ctx, key, n)      ([]string, error)
func (c *CoordClient) CommitObject(ctx, meta)  error
func (c *CoordClient) LookupObject(ctx, b, k)  (*ObjectMeta, error)
func (c *CoordClient) DeleteObject(ctx, b, k)  error
func (c *CoordClient) Healthz(ctx)             error
```

코드 ~150 LOC. 새 메커니즘 0. HTTP + JSON, 그대로.

CoordClient 의 단순함은 의도적. Ep.3+ 에서 leader-redirect (`X-COORD-LEADER`)
+ retry policy 가 추가되며 점진 복잡해진다.

## edge.commitPutMeta 의 분기

핵심은 한 함수:

```go
func (s *Server) commitPutMeta(ctx, meta) error {
    if s.CoordClient != nil {
        return s.CoordClient.CommitObject(ctx, meta)  // S5 모드
    }
    if s.StrictReplication && s.Elector != nil && s.Elector.IsLeader() {
        // S4 transactional Raft (Ep.21)
        ...
    }
    return s.Store.PutObject(meta)  // S1~S4 인라인
}
```

세 갈래. **EDGE_COORD_URL 이 정해진 갈래 1번**, 이전 두 갈래는 backward compat
보존. `lookupMeta` 와 `deleteMeta` 도 같은 패턴.

## env 한 개로 모드 전환

```
EDGE_COORD_URL=                            # 인라인 (legacy)
EDGE_COORD_URL=http://coord1:9000          # coord-proxy (S5)
```

boot 시 healthz fail-fast — coord 가 안 살아있으면 edge 시작 거부.

`EDGE_AUTO=*` (auto rebalance/GC) 는 coord-proxy 모드면 자동 비활성. 이유:
edge 의 local Store 가 빈 채라 auto-trigger 가 walk 할 게 없다. Auto 의
coord 이전은 S6 의 P7-04 부터 시작.

## ב bet demo — bbolt 크기 비교

```
$ ./scripts/demo-bet.sh
==> PUT /v1/o/demo/bet-test
{"bucket":"demo","key":"bet-test","size":17,"chunks":1}
==> GET /v1/o/demo/bet-test
    body: hello from bet ep
==> verify coord owns the meta
{"bucket":"demo","key":"bet-test","size":17}
==> edge.db vs coord.db (coord should be larger / authoritative):
    edge.db  = 32768 bytes        ← 빈 DB 의 bbolt page overhead
    coord.db = 65536 bytes        ← 실제 메타 데이터
==> DELETE /v1/o/demo/bet-test
    coord lookup → 404 ✓
=== ב PASS: edge proxies all metadata operations to coord ===
```

핵심 evidence: `edge.db` 가 32K (빈 bbolt 의 minimal layout), `coord.db` 가
64K (실제 ObjectMeta 기록 후). PUT 성공의 단일 출처가 coord 라는 직접 증거.

## chunk I/O 는 그대로 edge

분리는 **메타** 만. chunk byte 를 DN 으로 PUT 하는 일은 여전히 edge 가 직접:

```
1. Client → edge: PUT (UrlKey 검증)
2. edge: chunker.Split(body) → chunks
3. edge → coord.PlaceN(chunkID, R) → DN list (Ep.6 부터, 현재는 inline placement)
4. edge → DN.PUT chunk × R/2+1 ack (인라인 coordinator 의 quorum 그대로)
5. edge → coord.CommitObject(meta)   ← NEW
6. edge → Client: 201
```

Ep.2 는 5번만 추가. 3번의 placement 는 아직 edge 인라인 (Ep.6 의 ADR-041 가
coord 로 이전). 점진 분리.

## Trade-off

+ **메타 진실 단일 출처**. 두 edge 인스턴스가 같은 coord 에 commit → 일관.
+ **edge 의 코드 책임 명확화**. edge.go 에서 commit 의 결정 분기가 한 자리.
+ Backward compat 0 비용. EDGE_COORD_URL 미설정 = S1~S4 동일.
- **Latency** — PUT 마다 edge↔coord RPC 1 회 추가 (loopback 50µs, cross-host 1ms).
- **boot fragility** — coord 안 살면 edge 도 안 뜸. demo / dev 환경의 부팅
  순서 의존 발생.

## "왜 healthz fail-fast 인가"

대안: coord 안 살아도 edge 는 뜸, RPC 실패할 때만 5xx. 채택 안 함 — 부팅
시점에 분명한 시그널이 운영자에게 더 좋다. coord 가 없는 환경이라면
EDGE_COORD_URL 자체를 unset 하는 게 명시적 (인라인 모드). 어중간하게 "coord
url 은 있는데 coord 가 죽음" 상태가 가장 디버그 어려움.

## 다음

Ep.31 — coord 가 자기끼리 Raft 시작. ADR-038. 3-coord cluster, leader 가
mutating RPC 받고 follower 는 503 + `X-COORD-LEADER` redirect. CoordClient
가 transparent 따라감. 데모 demo-gimel (ג).

여기서부터 분리의 진짜 가치가 드러난다 — 메타 owner 자체가 다중화 가능.
