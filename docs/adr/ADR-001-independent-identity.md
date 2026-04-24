# ADR-001 — 독자적 프로젝트 identity

## 상태
Accepted · 2026-04-25

## 맥락

본 프로젝트는 **국내 한 기업의 과거 운영 사례에서 분산 파일 시스템**(원 이름 legacy DFS) 사전 연구에서 파생되었다. 기존 reference의 기술 자산 평가 매트릭스(KILL 5 / INHERIT 8 / KEEP 9 = 22건)는 `100.legacy DFS/legacy-modernized/docs/ARCH_NOTES.md §2` 에 확정.

현재 기존 reference은 프로덕션 미가동. 평가·학습·오픈소스 데모 목적의 **독립 파생 프로젝트**로 재정립 필요.

문제:
- 기존 reference 브랜드·내부 프로덕트명이 코드·문서에 산재하면 공개 배포 시 브랜드 리스크
- "legacy DFS 포크"로 간주되면 기여자·독자가 기존 reference 맥락·내부 분쟁에 얽힘
- 설계 원리를 보편 object storage 기술로 소통하려면 구체 브랜드 탈색 필수

## 결정

- 프로젝트명을 **kvfs** (Key-Value File System)로 재명명
- 기존 reference 컴포넌트명·제품명을 기능 중심 신규 이름으로 1:1 매핑 (`NAMING.md`)
  - legacy DFS → kvfs · legacy hot tier → hot · legacy meta tier → meta · legacy URL signature → UrlKey · legacy Swift gateway → swiftgate · legacy edge proxy → edge
- 원 개발사 브랜드·특정 내부 프로덕트명 **전수 redaction** (평가 repo·신규 repo 양측)
- 신규 repo = Apache 2.0 **독립 코드베이스** (기존 reference 포크 아님)
- `NAMING.md` 로 역참조 가능성 유지 (연구 목적)

## 결과

**긍정**:
- 기존 reference과 무관한 청중·기여자 유치 가능
- 공개 배포 시 브랜드·법무 리스크 최소화
- 보편 object storage 설계 레퍼런스로 자기 정의 가능

**부정**:
- 기존 reference 세부 분석(analysis_00~21)이 본 repo 바깥(`100.legacy DFS/`)에 머묾 → 맥락 참조 비용
- 기존 reference 경험자의 "어느 부분이 legacy hot tier에 해당?" 매핑 재학습 필요 (`NAMING.md` 로 완화)

**트레이드오프**:
- 물리 파일명(`installer/legacy-*/`, `source-analysis/binaries/legacy-*`)은 원형 유지 — 사전 연구 재현성 보존. 개념 이름만 kvfs/hot/meta로 통합
