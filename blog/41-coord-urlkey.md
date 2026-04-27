# Episode 41 — URLKey kid registry on coord

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 6 (operational migration) · **Episode**: 6
> **연결**: [ADR-048](../docs/adr/ADR-048-coord-urlkey-admin.md) · [demo-mem](../scripts/demo-mem.sh)

---

## 절반의 이전

DN registry 가 coord 에 정착 (Ep.40). URLKey kid registry 도 같은 패턴.
그러나 한 가지 큰 차이 — **edge 에 in-memory state 가 있다**.

```
edge.urlkey.Signer{
    primaryKid: "v1"
    secrets: { "v1": <bytes>, "v2": <bytes> }
}
```

PUT/GET 의 HMAC 검증이 이 in-memory Signer 를 사용. boot 시점에 bbolt 에서
load. 이후 admin endpoint (`POST /v1/admin/urlkey/rotate`) 가 bbolt 에 새 kid
넣고 Signer 도 update.

coord 가 owner 가 되면 bbolt 는 이전 가능. 그러나 edge 의 Signer 는?

## 두 책임 분리

ADR-048 (이 ep) = **kid registry 의 owner** 만. coord 가 list/rotate/remove
endpoint host. edge 의 in-memory Signer 갱신은 후속 (ADR-049 / Ep.42).

```
ADR-048: bbolt source-of-truth → coord
ADR-049: edge Signer 갱신 메커니즘 → polling
```

분리 이유: 각 결정의 위험을 따로 검토. 한 ADR 에 묶으면 어느 부분 깨졌는지
isolate 어려움 (ADR-038→039 와 같은 패턴).

## endpoints

```
GET    /v1/coord/admin/urlkey                      — list (open, follower OK)
POST   /v1/coord/admin/urlkey/rotate {kid, secret_hex, is_primary}
                                                    — leader-only
DELETE /v1/coord/admin/urlkey?kid=X                 — leader-only, primary 거부
```

primary kid 삭제 거부는 store-side check (ListURLKeys API 의 invariant). 운영자가
실수로 모든 client 를 잠가버리는 것 방지.

cli:
```
$ kvfs-cli urlkey list/rotate/remove --coord URL
```

`--edge` 와 평행. cli 가 response shape 두 개 자동 분기 (edge 는 `{kids, primary}`,
coord 는 raw `[]URLKeyEntry` array).

## WAL sync 자동

DN registry (ADR-047) 와 같은 메커니즘. PutURLKey / DeleteURLKey 가 WAL 에
기록 → ADR-039 sync → follower coord 자동 전파.

이 무료 의 가치 — ADR-039 이 만든 인프라 위에서 새 op 들이 추가 비용 0 으로
HA-correct.

## מ mem 데모

```
$ ./scripts/demo-mem.sh
==> 3-coord HA + edge (with EDGE_URLKEY_SECRET → kid v1 boot)
==> initial: kid v1 (primary)

==> kvfs-cli urlkey rotate v2 --coord URL --primary
    {"action":"rotate", "kid":"v2", "primary":true}
==> verify all 3 coords:
    coord1: [v1, v2(primary)]
    coord2: [v1, v2(primary)]
    coord3: [v1, v2(primary)]

==> sign URL with kid=v2 (using cli's --kid flag)
==> PUT to edge (still v1 in Signer — KNOWN GAP)
    HTTP 401: signature invalid (kid v2 unknown to edge)

    ↑ this is exactly the gap ADR-049 / Ep.42 closes

==> sign URL with kid=v1 (still in edge's Signer)
    PUT → 200

==> kvfs-cli urlkey remove v1 --coord URL
==> verify v1 gone from all 3 coords

=== מ PASS: URLKey registry on coord, but edge Signer not yet propagated ===
```

evidence: registry mutation 은 coord 에서 작동, 모든 coord 에 propagate.
**그러나 edge 의 in-memory Signer 는 아직 v1 만 알고 있음** — 새 v2 로 sign
한 URL 은 edge 가 거부.

이게 ADR-048 본문의 명시적 비포함 — Ep.42 가 메움.

## "왜 한 ep 에 안 묶었나"

ADR-048 + 049 를 한 ADR 로 할 수도 있었다. 분리한 이유:

- **저장소 mutation** (ADR-048) 와 **메모리 sync** (ADR-049) 는 위험 카테고리
  다름
- propagation 메커니즘 선택 (polling vs push vs WAL extension) 은 별도 결정 —
  ADR 본문에 trade-off 명시 가능
- 회귀 격리: ADR-048 만 적용된 상태에서도 backward compat (구 kid 는 verify 됨,
  새 kid 로 sign 한 URL 은 401 — 명확한 실패 모드)

분산 시스템의 변경은 작은 단계 + 명시적 갭 + 다음 ep 의 약속. 같은 패턴 또.

## 다음

Ep.42 — edge 의 Signer 가 coord 의 새 kid 를 polling 으로 학습. ADR-049.
30s default interval. demo-nun (נ). S6 close.

이 글이 다 끝나면 cluster 는 진짜 thin-edge: HMAC verify 까지 coord 의 자동
sync 를 따라가는 thin gateway.
