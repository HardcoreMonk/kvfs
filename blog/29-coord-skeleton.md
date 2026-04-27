# Episode 29 — kvfs-coord 의 첫 호흡

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 5 (coord 분리) · **Episode**: 1
> **연결**: [ADR-015](../docs/adr/ADR-015-coordinator-daemon-split.md) · [demo-aleph](../scripts/demo-aleph.sh)

---

## "2 daemon" 슬로건의 종료

S1 부터 외쳐온 단순함의 만트라 — "edge + dn 끝." 그 단순함이 30 episode 분량의
교육 자산을 만들었다. 그러나 ADR-015 가 그 슬로건을 깬다.

```
S1~S4:  Client → kvfs-edge → kvfs-dn × N
S5~  :  Client → kvfs-edge → kvfs-coord (× N) → kvfs-dn × N
```

왜 굳이 daemon 을 하나 더 늘리나? 같은 답을 두 번 한다.

1. **bbolt 는 한 프로세스 내 single writer**. 인라인 fsync 480 ops/s, group
   commit (Ep.22) 7400 ops/s. 그 위로 못 올라감.
2. **Placement 결정은 단일 출처여야 한다.** multi-edge HA (Ep.13) 에서 두 edge
   가 같은 chunk_id 에 대해 다른 시점의 DN topology 를 보면 placement 분기 가능.
   ADR-031 의 leader election 만으로는 메타 동시성에 부족.

S4 까지 wrest 하던 이 두 천장이 분리 결정의 트리거. ADR-015 본문이 정리.

## ℵ aleph — 최소 분리

Hebrew letter 시작 (그리스 α-ω 가 S4 close 에서 소진). Aleph = 1, 시작.

`scripts/demo-aleph.sh` 가 보여주는 것: **coord 가 edge 없이도 살아있다**.

```
docker run kvfs-coord:dev   # 포트 :9000
```

이 daemon 의 책임 (Ep.1 시점):
- placement 계산 — 같은 알고리즘 (HRW), 다른 위치
- 메타 owner — `coord.db` (bbolt) 가 ObjectMeta 의 진실 출처
- 4개 RPC: `place`, `commit`, `lookup`, `delete`

edge 는 아직 모른다. backward compat 100% — `EDGE_COORD_URL` 미설정이면 S1~S4
와 동일하게 인라인 모드.

## 4개 RPC 의 의미

```
POST /v1/coord/place    {key, n}        → {addrs}
POST /v1/coord/commit   {meta}          → {ok, version}
GET  /v1/coord/lookup?bucket=...&key=...→ ObjectMeta | 404
POST /v1/coord/delete   {bucket, key}   → {ok}
```

**4 개 가 메타 ownership 의 전부**. PUT 은 coord 에 commit, GET 은 coord 에
lookup, DELETE 는 coord 에 delete. chunk I/O (실제 byte) 는 edge 가 DN 으로
계속 직접 — coord 는 메타만.

이 분기가 ADR-015 의 정수: **coord = 메타·결정 의 owner, edge = bytes·HTTP
의 owner**. 같은 책임을 한 프로세스 안에서 fight 하지 않게.

## 코드는 어떻게 단순한가

`internal/coord/coord.go` 의 4 핸들러 합 ~150 LOC:

```go
func (s *Server) handlePlace(w http.ResponseWriter, r *http.Request) {
    var req PlaceRequest; json.NewDecoder(r.Body).Decode(&req)
    addrs := s.Placer.PlaceN(req.Key, req.N)
    writeJSON(w, http.StatusOK, PlaceResponse{Addrs: addrs})
}

func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request) {
    if s.requireLeader(w) { return }   // Ep.3 부터 의미 있음, Ep.1 은 noop
    var req CommitRequest; ...
    if err := s.Store.PutObject(req.Meta); err != nil { ... }
    writeJSON(w, http.StatusOK, CommitResponse{OK: true, Version: req.Meta.Version})
}
```

handlePlace 는 leader 체크 없음 — placement 는 read-only computation. handleCommit
은 leader 체크 있음 (Ep.3 의 HA 모드 대비). Ep.1 단일-coord 환경에서는 noop.

## 데모: 5 분 만에 확인

```
$ ./scripts/demo-aleph.sh
==> POST /v1/coord/place {key:abc-chunk, n:3}
    placed → dn1:8080,dn2:8080,dn3:8080
    deterministic across two calls ✓
==> POST /v1/coord/commit
    {"ok":true,"version":1}
==> GET /v1/coord/lookup
    {"bucket":"demo","key":"ep1/hello","size":11,"chunks":1}
==> POST /v1/coord/delete
    post-delete lookup → 404 ✓
=== ℵ PASS: coord daemon owns placement + meta independently ===
```

이 한 page 가 coord 의 전부. 메타 owner 가 분리됐다는 evidence — `coord.db`
파일 안에 객체 존재, edge 의 `edge.db` 는 아직 빈 채.

## 데이터 모델은 그대로

ADR-015 가 명시: **모든 데이터 모델 그대로**. ObjectMeta · ChunkRef · Stripe ·
WAL — coord 가 owner 일 뿐 schema 동일. migration 0줄.

이게 분리의 미덕. 새 책임 분배 + 동일 데이터 = "옛 edge.db 를 coord.db 로
이름만 바꾸면 동작" 정도의 단순성. 첫 마이그레이션 = 단일 coord = single
edge 와 동등.

## 비대칭 그대로

> S1 의 "edge 가 똑똑하고 dn 은 멍청하다" 는 여전. 이제 그 똑똑함의 대부분이
> coord 로 이전. dn 은 byte container, edge 는 thin gateway, coord 가 brain.

| | edge (S1~S4) | edge (S5~) | coord |
|---|---|---|---|
| chunker / EC encode | ✓ | ✓ | — |
| HMAC URL 검증 | ✓ | ✓ | — |
| placement 결정 | ✓ | (RPC) | ✓ |
| 메타 commit | ✓ | (RPC) | ✓ |
| WAL · bbolt owner | ✓ | (legacy, 비활성) | ✓ |
| rebalance/GC/repair | ✓ | (Ep.36~ 까지 점진) | (Ep.36~ 점진) |

S5 Ep.1~7 은 read 책임 (lookup, place) 을 옮기고, S6 가 admin 책임 (rebalance/
gc/repair/dns/urlkey) 를 옮긴다. 두 시즌이 분리 작업의 양면.

## 운영 비용

ADR-015 의 - 항목:
- daemon 종류 2 → 3. systemd, health, deploy 모두 ×3/2.
- 매 PUT/GET 이 edge↔coord RPC 1 회 추가. loopback ~50µs, cross-host ~1ms.
- "2 daemon" 슬로건 폐기.

이 비용을 받아들이는 대가: **horizontal scale 가능**, **placement 일관성 강화**,
**책임 명확화**. S5 끝나면 edge.go 1700 → ~700 LOC 추정 (실측: ~900 LOC, S6 까지 가야 700).

## 다음

Ep.30 — edge 가 처음으로 coord 를 호출한다. `EDGE_COORD_URL` env 도입,
`internal/edge/coord_client.go`. backward compat 보존 (env 미설정 = S1~S4 모드).
demo-bet (ב, 2번째 letter).
