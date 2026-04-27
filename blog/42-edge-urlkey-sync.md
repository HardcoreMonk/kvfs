# Episode 42 — edge urlkey.Signer propagation + S6 close

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 6 (operational migration) · **Episode**: 7 (S6 close)
> **연결**: [ADR-049](../docs/adr/ADR-049-edge-urlkey-propagation.md) · [demo-nun](../scripts/demo-nun.sh)

---

## 마지막 갭

[Ep.41](41-coord-urlkey.md) 끝의 화면이었다:

```
$ kvfs-cli urlkey rotate v2 --coord URL --primary
$ # PUT signed with v2 → edge → 401 (Signer 가 v2 모름)
```

coord 의 registry 는 update 됐는데 edge 의 in-memory Signer 가 모름. 이 갭이
S6 마지막 ep 의 일.

## 후보 메커니즘 셋

ADR-048 본문이 비포함 항목으로 적은 후보:

1. **Polling** — edge 가 주기적으로 coord 의 list 호출, diff 적용
2. **WAL extension** — ADR-039 의 leader→follower WAL push 에 URLKey op 추가
3. **Lazy 401-refresh** — edge 가 401 보면 그제서야 coord 에 list 호출

ADR-049 의 결정: **polling 채택**. 단순하고, ADR-049 코드 외 변경 0, ctx 취소
clean shutdown. WAL extension 은 새 hop type 추가가 필요 — 별도 ADR 의 일.
Lazy refresh 는 needs 명확해질 때 ADR-050 후보로.

## 코드

`internal/edge/urlkey_sync.go`:

```go
// SyncURLKeys: unit-testable core
func SyncURLKeys(signer *urlkey.Signer, coord *CoordClient, failFn func(error)) {
    keys, err := coord.ListURLKeys(ctx)
    if err != nil { failFn(err); return }

    coordSet := make(map[string]bool)
    for _, k := range keys {
        coordSet[k.Kid] = true
        if !signer.Has(k.Kid) {
            signer.Add(k.Kid, k.Secret)
        }
        if k.IsPrimary {
            signer.SetPrimary(k.Kid)
        }
    }
    for _, kid := range signer.Kids() {
        if !coordSet[kid] && kid != signer.Primary() {
            signer.Remove(kid)
        }
    }
}
```

세 op: Add (new kid), SetPrimary (primary changed), Remove (gone from coord).
Primary 는 절대 remove 안 함 (last-resort safety).

## Background goroutine

```go
// Server.StartURLKeyPolling — boot 시 호출
func (s *Server) StartURLKeyPolling(ctx context.Context, interval time.Duration) {
    if s.CoordClient == nil || interval <= 0 { return }  // legacy / disabled
    go func() {
        t := time.NewTicker(interval)
        defer t.Stop()
        for {
            select {
            case <-ctx.Done(): return
            case <-t.C:
                SyncURLKeys(s.Signer, s.CoordClient, func(err error) {
                    s.Log.Warn("urlkey poll failed", "err", err)
                })
            }
        }
    }()
}
```

```
EDGE_COORD_URLKEY_POLL_INTERVAL=30s   ← default
EDGE_COORD_URLKEY_POLL_INTERVAL=0     ← polling off
```

## נ nun 데모

```
$ ./scripts/demo-nun.sh
==> 1 coord + edge (poll 5s)
==> initial: kid v1 only

==> kvfs-cli urlkey rotate v2 --coord URL --primary
    rotate done at coord

==> wait 6 seconds (let edge poll)
==> sign URL with kid=v2 → PUT
    HTTP 200 ✓

==> kvfs-cli urlkey remove v1 --coord URL
==> wait 6s
==> sign URL with kid=v1 → PUT
    HTTP 401 (kid removed)

==> sign URL with kid=v2 → PUT
    HTTP 200 ✓

=== נ PASS: edge Signer auto-syncs from coord ===
```

evidence: rotate 후 6s 안에 edge 가 v2 인식, remove 후 6s 안에 v1 거부.
polling interval (5s) + RTT 안에서 propagation 완료.

## "Lazy 401-refresh" 의 매력

대안 (ADR-049 미채택): edge 가 401 받으면 그제서야 coord 에 list 호출.

장점: rare key rotation 에서 polling overhead 0. 401 은 안 그래도 비정상 path
이니 그때 한 번 더 동기화.

단점: 401 의 의미가 "auth invalid" 와 "kid 미동기화" 두 가지로 분기. 디버깅
어려움. 또 401 받기 전에 정상 PUT 들이 모두 401 되는 windowing — N concurrent
client 가 동시에 ListURLKeys 호출하는 thundering herd.

ADR-049 본문: "needs 명확해질 때 ADR-050 후보". 즉 measurement 후 결정.

## S6 회고 — 7 episodes 한 줄씩

| Ep | 무엇을 옮겼나 | 핵심 |
|---|---|---|
| 1 (chet ח) | rebalance plan | placement view single-source |
| 2 (tet ט) | rebalance apply + COORD_DN_IO | DN HTTP client 가 coord 에 |
| 3 (yod י) | GC plan + apply | 인프라 재활용, ~80 LOC |
| 4 (kaf כ) | EC repair | 같은 패턴, RS Reconstruct on coord |
| 5 (lamed ל) | DN registry mutation | WAL sync 자동 propagation |
| 6 (mem מ) | URLKey kid registry | 절반 — store 만, edge Signer 미동기 |
| 7 (nun נ) | edge Signer polling | mem 의 갭 메움, S6 close |

S6 7 episode, 7 ADR (043~049), 7 Hebrew letter demo (8th~14th).

## kvfs 의 현재 모습

```
                        Client
                          │ HTTP + UrlKey (자동 sync 된 키 set 으로 verify)
                          ▼
                    ┌──────────┐    edge: thin gateway
                    │ kvfs-edge│    - HTTP / UrlKey verify
                    │  × N     │    - chunker / EC encoder
                    │          │    - urlkey poll (background)
                    └────┬─────┘
                         │ HTTP RPC + DN HTTP
              ┌──────────┴──────────┐
              ▼                     ▼
         ┌─────────┐          ┌──────────┐
         │kvfs-coord│           │ kvfs-dn  │
         │ × N      │ ◄────────┤ × M      │
         │          │  health  │          │
         │ - meta   │  beat    │ chunks   │
         │ - place  │          │          │
         │ - HA Raft│          │          │
         │ - WAL    │          │          │
         │ - workers│          │          │
         │ - admin  │          │          │
         └──────────┘          └──────────┘
              │
              └─ bbolt: objects · dns_runtime · urlkey_secrets
```

S5 시작 (Ep.29) 에서 그렸던 미래에 도달. coord 가 모든 결정·메타·worker 의
owner. edge 는 진짜 thin gateway.

## 14개 episode 의 의미

S5 + S6 = Hebrew letter 14개, ADR 13개 (Ep.30 만 ADR 없음), 14 demo.

1700 LOC 였던 edge.go 는 ~1100 LOC (목표는 700, 추가 cleanup 가능).
internal/coord 가 ~900 LOC. 책임 분리가 코드 분포에 그대로 반영.

이 자동 chaos test (P8-01) 가 보여준 invariant — leader-loss-mid-write 시
phantom 0, quorum loss 시 503 정직, leader 교체 transparent — 모두 holds.
production-ready 분산 storage 의 patterns 가 reference 안에 모두 implemented.

## 다음

Season 7 시작. 새로 쓸 architectural primitive — failure domain hierarchy,
degraded read, tunable consistency, anti-entropy Merkle tree. ADR-050+.
Hebrew letter samekh ס 부터.

이게 학부 분산 시스템 textbook 의 "있어야 마땅한데 kvfs 엔 없는 것들". S7
끝나면 frame 2 (textbook primitive 100%) 도달.

P8-04 의 일이지만 그 전에 P8-02 (chaos suite 확장) 가 먼저 — 새 primitive 도입
전에 회귀 안전망 강화. 다음 글이 그 chaos 확장의 결과 보고일 가능성.
