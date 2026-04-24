# ADR-002 — 2-daemon MVP 아키텍처 (edge + dn)

## 상태
Accepted · 2026-04-25

## 맥락

MVP 목적은 **공개 오픈소스 데모** (ADR-001). 성공 지표는 학습성·재현성.

대안 검토:

| 구조 | 컴포넌트 | 데모 친화도 | 분산 스토리지 충실도 |
|---|---|:---:|:---:|
| 2-daemon | edge + dn×3 | 🟢 최고 | 🟡 coordinator 통합됨 |
| 3-daemon | edge + coordinator + dn×3 | 🟡 중 | 🟢 현실적 |
| 5-daemon | gateway, coordinator, dn×3, meta KV, config DB | 🔴 과함 | 🟢 프로덕션급 |

모든 daemon 추가는 빌드·설정·로그·문서화 비용 증가. 데모 독자는 `docker ps` 로 컨테이너 수를 먼저 본다.

3-way replication 시연에 coordinator HA 불필요 — 복제는 DN 간 발생, coordinator는 단일 프로세스로 충분.

## 결정

MVP는 **2-daemon**:

1. **kvfs-edge** — HTTP 게이트웨이 + UrlKey 검증 + **인라인 coordinator** + bbolt 메타
2. **kvfs-dn** (×3) — 로컬 파일 chunk 저장 HTTP 서버

coordinator 로직은 `internal/coordinator/` 패키지로 **library 형태**. 향후 별도 daemon으로 분리 쉽도록 인터페이스 분리.

## 결과

**긍정**:
- `docker ps` 상 4개 컨테이너 (edge + dn×3) — 첫 인상 단순
- Go goroutine + channel로 fanout·quorum 구현, 외부 메시징 불필요
- 블로그 1편 코드가 60줄로 수렴 (`WriteChunk` in coordinator)

**부정**:
- coordinator 수평 확장 불가 (edge 내부이므로 edge 대수만큼만 존재)
- edge 장애 시 전체 쓰기 중단 (데모는 이 상태 허용)

**트레이드오프**:
- Season 2에 coordinator 별도 daemon 분리는 인터페이스 경계가 이미 있어 refactor 용이
- "우리는 inline으로 시작했다, 왜 뽑는가" 블로그 소재 자연 생성

## 관련

- `docs/ARCHITECTURE.md` §컴포넌트
- `internal/coordinator/coordinator.go` — 미래 daemon 분리 경계점
- Season 2 ADR-010 (예상) — coordinator 분리 + Raft HA
