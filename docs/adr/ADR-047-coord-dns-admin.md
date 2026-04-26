# ADR-047 — Mutating registry admin on coord (Season 6 Ep.5)

상태: Accepted (2026-04-27)

## 결정

DN runtime registry 의 mutation 이 coord 로:

- `POST /v1/coord/admin/dns?addr=X` — `MetaStore.AddRuntimeDN(addr)`
- `DELETE /v1/coord/admin/dns?addr=X` — `MetaStore.RemoveRuntimeDN(addr)`
- `PUT /v1/coord/admin/dns/class?addr=X&class=Y` — `MetaStore.SetRuntimeDNClass(addr, class)`

세 endpoint 모두 `requireLeader` gate (HA mode 에선 leader 만 받음). 모두 WAL 에 기록 → ADR-039 의 sync 가 follower coord 에 자동 전파.

cli `kvfs-cli dns add/remove/class --coord URL`. 기존 edge route 와 평행. `dns list --coord URL` 도 함께 (ADR-042 가 이미 추가했지만 본 ADR 에서 add/remove 와 같은 base URL 로 정렬).

## 비포함

- **URLKey rotation** — kid registry 도 mutating admin. coord 에 같은 패턴으로 추가 가능하나 현재 (ADR-028) 가 edge-side `urlkey.Signer` 를 직접 update 함. coord 측 Signer + propagate 메커니즘을 만들어야 — Ep.6 또는 후속 ADR. **본 ADR 비포함.**
- **rebalance/gc/repair admin** — Ep.2-4 가 이미 처리.

## 결과

+ DN registry 의 mutation single source = coord. cli operator 가 edge 우회 가능.
+ HA mode 일관성 자동 (WAL 통한 follower 동기화).
+ 코드 변경 ~80 LOC + 1 unit test.
- edge admin endpoints 잠시 중복 (Season 6 끝날 때 정리 검토).

## 검증

`internal/coord/coord_test.go::TestAdminMutateDNRegistry` — add → list → class → remove 라운드트립.

데모 demo-lamed.sh (히브리 ל, 12번째 letter): coord-only cluster (no edge). cli 가 모든 admin 을 coord 에 직접. dns add/remove/class 동작 + Hot/Cold 라벨 효과 검증.
