# ADR-037 — Chunker scratch-pool soft cap

상태: Accepted (2026-04-26)

## 배경

ADR-035 의 `chunker.scratchPool` (`internal/chunker/stream.go`) 은 sync.Pool 기반으로 16 MiB CDC 슬랩 / 4 MiB fixed 슬랩을 재사용. 이론상 GC 라운드마다 비워지지만:

- GC 라운드 사이 동시 PUT N 개 후 모두 반납 → 풀에 N 개 슬랩 잔존
- 트래픽이 spike → 다시 spike 사이의 idle 동안 GC 가 안 돌면 RSS 가 부풀어 있음
- containerd 환경의 cgroup memory limit 에 걸리면 OOM kill — 운영자에게는 의문의 죽음

ADR-035 본문에 "후속" 으로 명시.

## 결정

`scratchPoolBytes atomic.Int64` 가 Pool 안의 슬랩 cap 합계를 추적. `poolCapBytes atomic.Int64` 는 soft cap.

- **`Get`**: pool 에서 슬랩 빼면 `scratchPoolBytes.Add(-cap)` (디빗).
- **`Put`**: 디빗 후 cap 합계 + 신규 cap 이 limit 초과면 슬랩 drop (GC 회수에 맡김). 아니면 `Add(+cap)` 후 풀에 반환.
- `cap = 0` (default) = unlimited (기존 동작).

운영 측면:
- env: `EDGE_CHUNKER_POOL_CAP_BYTES` (default 0).
- metric: `kvfs_chunker_pool_bytes` (gauge, 현재 cap 합계). cgroup limit 의 1/N 로 cap 잡으면 안전.

## 비채택 대안

| 대안 | 기각 사유 |
|---|---|
| Slab 수 (개수) cap | CDC 16 MiB 와 fixed 4 MiB 가 섞이면 slab 수가 의미 없음 |
| Per-size bucket pool 두 개 | Get/Put 호출 사이트가 size 인지 필요 — API 침투. ADR-035 의 single pool 단순함 깨짐 |
| Hard cap (Get 시 block) | hot path 에 mutex/cond 도입 — sync.Pool lock-free 장점 손실 |
| GC 트리거 (`runtime.GC()`) | stop-the-world 부작용; 정상 GC 사이클 방해 |

## 결과

+ 메모리 압박 환경에서 결정적 상한 (cgroup 친화).
+ atomic counter 만 추가 — lock 0, hot path 무영향.
+ `kvfs_chunker_pool_bytes` 로 외부 검증 가능.
+ default 0 = backward compat.
- soft cap 이라 Put 시점에만 enforce — 동시 Get 폭주 시 일시적으로 cap 초과 가능 (Get 은 cap 신경 안 씀).
- Pool 에서 drop 된 슬랩은 즉시 GC 가 회수하지 않음 (다음 cycle 까지 RSS 잔존). 진정한 회수가 필요하면 `runtime.GC()` 를 별도 worker 가 idle 시 호출해야 함.

## 호환성

- `chunker.SetPoolCap(int64)` / `chunker.PoolStats() (int64, int64)` 신규 export.
- 기존 `getScratch` / `putScratch` 시그니처 무변화.
- env 미설정 시 동작 0 변경.
