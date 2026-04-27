# Episode 40 — DN registry mutation on coord

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 6 (operational migration) · **Episode**: 5
> **연결**: [ADR-047](../docs/adr/ADR-047-coord-dns-admin.md) · [demo-lamed](../scripts/demo-lamed.sh)

---

## DN list 의 진짜 owner

Ep.34 (ADR-041) 가 placement 결정을 coord 로. 그러나 DN registry 자체의 mutation
(add/remove/class) 은 여전히 edge 에 있었다 — `POST /v1/admin/dns?addr=X`.

```
운영자 → cli "dns add dn4" → edge → edge.Store.AddRuntimeDN(dn4)
                                  → edge.Coord.UpdateNodes(...)
                                  → ack
```

문제:
- coord 의 placement 가 edge 의 DN list 와 분기 가능 (edge 가 추가했지만 coord
  가 모름)
- ADR-015 의 single source 원칙: DN list 도 coord 가 owner 여야

## endpoints

```
POST   /v1/coord/admin/dns?addr=X       — Add
DELETE /v1/coord/admin/dns?addr=X       — Remove
PUT    /v1/coord/admin/dns/class?addr=X&class=Y  — class label set
```

세 endpoint 모두 `requireLeader` gate. HA mode 에서 leader 만 받음.
follower 는 503 + `X-COORD-LEADER` redirect.

cli:
```
$ kvfs-cli dns add dn4:8080 --coord URL
$ kvfs-cli dns remove dn4:8080 --coord URL
$ kvfs-cli dns class dn4:8080 hot --coord URL
```

## WAL 자동 sync

ADR-039 (Ep.32) 가 coord-to-coord WAL replication 을 만들어둔 덕분:

```
add(dn4) → leader.MetaStore.AddRuntimeDN → bbolt + WAL append → walHook
                                                                  ↓
                                                  ReplicateEntry to peers
                                                                  ↓
                                       follower.ApplyEntry(addRuntimeDN) → 자기 bbolt 도 update
```

cli 한 번 호출 → leader coord 가 받음 → WAL 통해 follower coord 들에 자동
전파. 운영자는 한 곳에만 명령, HA 일관성 자동.

## ל lamed 데모

```
$ ./scripts/demo-lamed.sh
==> 3-coord HA + 3 DN + edge
==> initial coord DN list: dn1, dn2, dn3

==> kvfs-cli dns add dn4:8080 --coord http://coord1:9000
    {"action":"add", "addr":"dn4:8080"}

==> verify all 3 coord bbolts have dn4 (WAL replication)
    coord1.dns_runtime: [dn1, dn2, dn3, dn4]
    coord2.dns_runtime: [dn1, dn2, dn3, dn4]
    coord3.dns_runtime: [dn1, dn2, dn3, dn4]

==> kvfs-cli dns class dn4 hot --coord http://coord1:9000

==> kvfs-cli dns remove dn4 --coord http://coord1:9000
    {"action":"remove", "addr":"dn4:8080"}

==> verify removal propagated
    all 3 coord: dn1, dn2, dn3

=== ל PASS: DN registry mutations propagate via WAL ===
```

evidence: leader 가 받은 mutation 이 모든 coord 에 전파. ADR-039 의 sync
infrastructure 가 새 admin op 에도 그대로 작동.

## 명시적 비포함

ADR-047 에서 안 한 일:

- **URLKey rotation** — 이것도 mutating registry 지만 edge-side `urlkey.Signer`
  의 in-memory state 도 update 해야 함. 별도 ep (ADR-048 + 049) 의 일.
- **rebalance/gc/repair admin** — 이미 Ep.36~39 에서 처리.

분리 이유: URLKey 는 단순 bbolt update 이상이다. edge 가 verify 시점에
in-memory Signer 를 사용하므로, signer 에 새 kid 를 어떻게 알리는지 (poll? push?)
가 별도 결정.

## "왜 follower-OK 가 아닌 leader-only"

DN add/remove/class 는 cluster topology 결정. 두 coord 가 동시에 다른 add
받으면 분기 가능. requireLeader 가 그 race 를 직렬화.

대안: follower 가 받아 leader 로 internally proxy. 채택 안 함 — ADR-038 의
직접-redirect 패턴 일관성. cli 가 transparent retry 하면 끝.

## 다음

Ep.41 — URLKey kid registry 도 coord 로. ADR-048. 같은 패턴이지만 propagation
이 trickier. demo-mem (מ).
