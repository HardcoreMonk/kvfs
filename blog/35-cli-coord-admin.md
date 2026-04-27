# Episode 35 — cli 가 coord 에 직접 묻기 + S5 close

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 5 (coord 분리) · **Episode**: 7 (S5 close)
> **연결**: [ADR-042](../docs/adr/ADR-042-cli-direct-coord-admin.md) · [demo-zayin](../scripts/demo-zayin.sh)

---

## 마지막 layer 정리

ADR-015 ~ ADR-041 은 daemon 측 분리. 그러나 **cli** 는 아직 옛 모델 그대로:

```
$ kvfs-cli inspect --db ./edge-data/edge.db
```

문제:
- coord-proxy 모드에서 `edge.db` 는 비어있음 (메타는 coord 에)
- 운영자가 cli 로 inspect 하려면 edge host 의 filesystem 에 접근해야 — cross-host
  운영 시 짜증
- coord 가 진실의 owner 인데 cli 가 layer 위반 (edge 통과)

## 결정

coord 에 read-only admin 두 endpoint:

```
GET /v1/coord/admin/objects   → 전체 ObjectMeta list
GET /v1/coord/admin/dns       → runtime DN list
```

cli 의 `inspect` 에 `--coord URL` 플래그.

```
$ kvfs-cli inspect --coord http://coord1:9000
```

설정되면:
- `--object bucket/key` → `GET /v1/coord/lookup` (Ep.30 부터 존재)
- 미설정 (전체 dump) → `objects + dns` 두 번 fetch + 합쳐 출력

`--db` 경로는 그대로 — 인라인 모드 + 디스크 inspect (디버그 용) 보존.

## "왜 read 만 먼저"

ADR-042 의 명시적 비포함:

- `rebalance`, `gc`, `repair` mutate admin → coord 에 worker 없음 (edge 가
  갖고 있음). 후속 ep (Season 6) 가 worker 도 옮기면 자연 이동.
- `urlkey rotation`, `dns class set` mutating registry → 같은 이유.

이 ep 의 의의는 작은 게 아니다 — **패턴 박기**. `coord /v1/coord/admin/*` +
cli `--coord URL` 의 모양이 자리잡으면 다음 mutate endpoint 추가는 mechanical.

S6 의 7개 ep 가 이 패턴을 그대로 따라간다.

## ז zayin 데모

```
$ ./scripts/demo-zayin.sh
==> PUT /v1/o/zayin/test
{"bucket":"zayin","key":"test","size":11}

==> kvfs-cli inspect --coord http://coord1:9000
    Buckets: 1
      zayin/test (11 bytes, 1 chunks)
    DNs (3):
      dn1:8080 (class=)
      dn2:8080 (class=)
      dn3:8080 (class=)

==> kvfs-cli inspect --db ./edge-data/edge.db
    Buckets: 0
    (empty — edge.db 가 메타 owner 가 아님)

==> kvfs-cli inspect --object zayin/test --coord http://coord1:9000
    bucket: zayin
    key:    test
    size:   11
    chunks: ["dn3:8080","dn1:8080","dn2:8080"]

=== ז PASS: coord direct inspect, edge.db is empty ===
```

마지막 두 inspect 의 비교가 evidence:
- `--coord` → 데이터 있음
- `--db edge.db` → 비어있음

coord-proxy 모드에서 edge 의 bbolt 는 사용되지 않는다는 직접 증명.

## S5 회고 — 7 episodes 한 줄씩

| Ep | 무엇을 옮겼나 | 핵심 |
|---|---|---|
| 1 (aleph ℵ) | coord daemon skeleton | 4 RPC 자리만 |
| 2 (bet ב) | edge → coord 메타 client | EDGE_COORD_URL env |
| 3 (gimel ג) | coord HA via Raft | election + X-COORD-LEADER redirect |
| 4 (dalet ד) | coord-to-coord WAL repl | leader↔follower 데이터 sync |
| 5 (he ה) | transactional commit | replicate-then-commit, phantom write 0 |
| 6 (vav ו) | placement RPC | edge 의 마지막 결정도 coord 로 |
| 7 (zayin ז) | cli direct read admin | layer violation 정리 (read 만) |

7개 ep, 6개 ADR (Ep.2 만 ADR 없음 — Ep.30 본문 참조), 7개 Hebrew letter demo.

## Architecture 의 그림

S1 슬로건: "edge + dn." 끝.
S5 close 슬로건: "edge + coord (× N) + dn (× M)." 책임 명확화.

```
                        Client
                          │ HTTP + UrlKey
                          ▼
                    ┌──────────┐
                    │ kvfs-edge│  thin gateway
                    │  × N     │  - HTTP 종단
                    │          │  - UrlKey 검증
                    │          │  - chunker / EC encoder
                    └────┬─────┘
                         │ HTTP RPC + DN HTTP
              ┌──────────┴──────────┐
              ▼                     ▼
         ┌─────────┐          ┌──────────┐
         │kvfs-coord│           │ kvfs-dn  │
         │ × N      │ ◄────────┤ × M      │
         │          │  health  │ - byte   │
         │ - meta   │  beat    │   store  │
         │ - place  │          └──────────┘
         │ - HA Raft│
         │ - WAL    │
         └──────────┘
              │
              └─ bbolt: objects · dns_runtime · urlkey_secrets
```

이게 ADR-015 가 그렸던 미래. S5 7 episode 가 도달.

## 운영 측면

- **단일 coord 모드** (Ep.1~2 만 켬): single edge 와 latency 비슷, SPOF 한 자리
  옮긴 셈
- **3-coord HA + WAL repl** (Ep.3~4): leader 죽어도 데이터 살림
- **Transactional commit** (Ep.5): write loss 0, CP 시스템
- **Single-source placement** (Ep.6): topology drift 자동 해소
- **Cli direct admin** (Ep.7): cross-host 운영 친화

3-daemon 의 운영 비용 (ADR-015 -항목) 은 위 + 항목들로 갚아짐. **production-
ready 분산 storage 의 모양** — 단지 production 이 아닌 reference.

## 다음 시즌

Season 6 — operational migration. S5 가 read 책임을 옮겼다면 S6 는 mutating
admin (rebalance / GC / repair / DN registry / URLKey) 의 coord 이전. 7개
episode (Hebrew chet ח ~ nun נ).

S6 끝나면 cli 의 모든 admin 이 coord 직접. edge 는 진짜 thin — HTTP + chunker
+ urlkey 검증 + bytes-to-DN 만.

다음 글 [Ep.36 rebalance plan on coord](36-coord-rebalance-plan.md) — S6 시작.
