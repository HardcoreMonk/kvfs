# ADR-003 — 내·외부 통신 HTTP REST

## 상태
Accepted · 2026-04-25

## 맥락

kvfs에는 두 가지 통신 경로:

1. **Client ↔ edge** — 외부. HTTP가 자연스러움
2. **edge ↔ dn** — 내부. 다음 선택지 검토:

| 옵션 | 장점 | 단점 | 데모 친화 |
|---|---|---|:---:|
| HTTP REST | curl·tcpdump 디버그 / stdlib only | 헤더 오버헤드 | 🟢 |
| gRPC | 타입 안전·streaming·schema | protoc 툴체인 / 생성 코드 | 🟡 |
| Raw TCP + 자체 binary | 최고 성능 | decoder 없음, 디버그 지옥 | 🔴 |
| HTTP/2 + msgpack | 효율+semantics | 라이브러리 의존, 직관성↓ | 🟡 |

사전 연구에서, 커스텀 바이너리 프로토콜은 Wireshark decoder 부재 시 디버깅 비용이 폭증하는 패턴이 반복적으로 관찰되었다.

데모 목적에선 **보이는 프로토콜**이 우선.

## 결정

**모든 통신 HTTP REST** (client↔edge, edge↔dn, 관리 API 동일):

- Client↔edge: S3 스타일 — `PUT /v1/o/{bucket}/{key}?sig=...&exp=...`
- edge↔dn: 내부 API — `PUT /chunk/{id}`, `GET /chunk/{id}`, `DELETE /chunk/{id}`, `GET /healthz`
- 관리 API: `GET /v1/admin/objects`, `GET /v1/admin/dns`
- 시리얼화: JSON metadata (headers + body) + raw bytes body

gRPC·raw TCP는 **Season 2+ 최적화 소재**. 현재 데이터 경로 처리량 목표는 "랩톱에서 동작"이라 HTTP 오버헤드 무의미.

## 결과

**긍정**:
- `curl -v http://dn1:8080/chunk/<sha256>` 한 줄로 각 계층 검증
- tcpdump·Wireshark 모든 decoder 기본 동작
- Go `net/http` stdlib 만으로 전체 구현 (외부 의존 0)
- 독자가 이미 아는 프로토콜 — 학습 곡선 없음

**부정**:
- HTTP/1.1 헤더 파싱·keep-alive 오버헤드 (gRPC 대비 수십% 느림)
- streaming 흉내 불가 (chunked transfer-encoding만 지원, bi-directional 없음)
- multipart upload 구현 시 복잡도 증가

**트레이드오프**:
- 프로덕션 성능 요구 발생 시 gRPC 마이그레이션 → Season 2+ ADR-012 예상
- "HTTP → gRPC 마이그레이션 비용" 자체가 블로그 소재

## 관련

- `internal/edge/edge.go` — HTTP 핸들러
- `internal/dn/dn.go` — HTTP 핸들러
- `internal/coordinator/coordinator.go` — HTTP 클라이언트 fanout
