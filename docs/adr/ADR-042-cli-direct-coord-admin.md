# ADR-042 — kvfs-cli direct coord admin

상태: Accepted (2026-04-27)
시즌: Season 5 Ep.7 · ADR-015 분리의 사용자-도구 측 마무리

## 배경

ADR-015 (S5 Ep.1) 후속으로 메타·placement 가 모두 coord 로 이전됐다 (Ep.2 ~ Ep.6). 그러나 `kvfs-cli` 는 여전히:

- `inspect` 가 `--db ./edge-data/edge.db` 로 **edge 의 bbolt 파일을 직접 open** — coord-proxy 모드에서 그 파일은 비어있음
- 다른 admin 서브커맨드들 (rebalance, gc, dns, urlkey 등) 은 모두 **edge 의 `/v1/admin/*` 를 호출** — edge 가 thin gateway 가 됐는데 admin 만 edge 통과

결과:
- 운영자가 cli 로 메타 read 하려면 edge host 의 filesystem 에 접근해야 함 (cross-host 운영 시 짜증)
- coord 가 진실의 owner 인데 cli 가 layer 위반으로 edge 를 통해서 묻는 셈

## 결정

coord 에 read-only admin 엔드포인트 두 개 추가:

- `GET /v1/coord/admin/objects` — 전체 ObjectMeta list (single-source)
- `GET /v1/coord/admin/dns` — runtime DN list

cli 의 `inspect` 에 `--coord URL` 플래그. 설정되면:
- `--object bucket/key` → `GET /v1/coord/lookup` (이미 존재)
- 미설정 (전체 dump) → `GET /v1/coord/admin/{objects,dns}` 두 번 fetch + 합쳐 출력

기존 `--db` 경로는 그대로 유지 — 인라인 모드 + 디스크 inspect (디버그용) 보존.

## 명시적 비포함 (= 후속 ADR/ep)

본 ADR 은 **read-only inspect 만**. 다음은 미포함:

- `rebalance`, `gc`, `repair` 같은 mutate admin — coord 에 endpoint 없음 (edge 가 worker 갖고 있음). 후속 ep 에서 coord 가 worker 도 가져가면 자연 이동.
- `urlkey rotation`, `dns class set` 같은 mutating registry admin — 같은 이유. coord 가 그 책임 가져가면 함께 이전.
- `meta snapshot`, `wal stream`, `auto status` — coord 가 owner 가 된 메타 hot/auto loop 들. 후속.

이 ADR 이 cli 의 첫 read 만 떼냄으로써 패턴을 박는 의의 — 다음 mutate 엔드포인트 추가는 같은 모양 (cli `--coord` flag + coord `/v1/coord/admin/*` route).

## 비채택 대안

| 대안 | 기각 사유 |
|---|---|
| cli 가 edge 통해 coord 로 proxy (모든 admin 이 edge 거침) | layer 위반. coord 가 owner 인데 매 admin call 마다 edge 의 라우터 거치는 것은 noise. edge 가 admin 통과 책임이 없어야 함 |
| coord 에 `/v1/coord/admin/objects?since=&limit=` paginate | YAGNI. MVP 는 dump-all. 큰 cluster (객체 100k+) 가 측정으로 도달하면 paginate 추가 |
| cli `--coord` 가 edge endpoint 와 fall-back chain | cli 가 둘 다 알면 사용자가 어느 쪽을 묻는지 헷갈림. 명시적 — `--coord` 면 coord, `--db` 면 file, 둘 다 없으면 (legacy) edge endpoint 추정. cli 의 다른 서브커맨드들이 edge endpoint 를 default 로 — 본 ADR 에서 그 default 는 변경 안 함 |

## 결과

+ **운영 layering 정렬**: read admin 이 owner (coord) 에 가서 묻는다. edge 는 thin gateway 본연의 책임만.
+ **cross-host friendly**: cli host 가 edge filesystem mount 안 해도 됨. coord URL 만 알면 운영 가능.
+ **확장 패턴 박힘**: coord 의 `/v1/coord/admin/*` 가 자리 잡았으므로 다음 mutate endpoint 추가는 mechanical.
- **두 vs 한 endpoint 호출**: dump-all 은 `objects + dns` 두 번 GET. 한 번에 묶을 수도 있지만 고유 endpoint 가 더 명확.
- **mutating admin 미이전**: cli 의 절반 정도 서브커맨드는 여전히 edge 통과. 본 ADR 의 의도 (읽기부터, mutate 는 후속).

## 호환성

- `--db` 와 `--coord` 둘 다 미설정 시 default 는 `--db ./edge-data/edge.db` (변경 0).
- 새 endpoint 추가만 — 기존 coord 유저 영향 0.
- `coord.Server.Routes()` 에 두 줄 추가, `coord.go` ~25 LOC.

## 검증

`internal/coord/coord_test.go::TestAdminEndpoints_ListObjectsAndDNs` — Store 에 1 object + 1 runtime DN seed 후 두 endpoint hit, body 일치 확인.

`scripts/demo-zayin.sh` (히브리 ז): coord + edge proxy + 3 DN. PUT 객체 → cli `inspect --coord` 로 listing → 동일 출력 확인. 동시에 cli `inspect --db` 가 edge.db 비어있음 확인 (proxy 모드에서 edge 의 bbolt 가 사용 안 됨 = ADR-015 분리 완성 시각화).

## 후속

- **ADR-043 (예상)**: rebalance/gc/repair admin 의 coord 이전. coord 가 worker 도 host.
- **ADR-044 (예상)**: urlkey rotation, dns admin 의 coord 이전. cli 의 `dns add/remove`, `urlkey rotate` 가 coord 에.
