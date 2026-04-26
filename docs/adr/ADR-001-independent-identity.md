# ADR-001 — 독자적 프로젝트 identity

## 상태
Accepted · 2026-04-25

## 맥락

본 프로젝트는 분산 object storage 설계 원리를 다루는 **독립 학습·연구 프로젝트**. Apache 2.0 라이선스 하에 clean-slate Go 구현으로 출발.

설계 의사결정의 일부는 운영 분산 파일 시스템에서 흔히 마주치는 트레이드오프(presigned URL, content-addressable chunk, EC repair 등)에서 출발하지만, 코드·테스트·문서는 모두 본 repo에서 새로 작성된다.

목적:
- 공개 배포 가능한 보편 object storage 학습 자료
- 독립 정체성 — 외부 시스템 호환·계승 부담 없음

## 결정

- 프로젝트명을 **kvfs** (Key-Value File System)로 명명
- 신규 repo = Apache 2.0 **독립 코드베이스** (clean slate)
- 모든 컴포넌트·식별자·문서 표기를 본 프로젝트 자체 어휘로만 통일

## 결과

**긍정**:
- 보편 object storage 설계 레퍼런스로 자기 정의 가능
- 공개 배포 시 브랜드·법무 리스크 없음
- 기여자가 외부 컨텍스트 없이 onboarding 가능

**부정**:
- 학습 맥락(왜 이 결정을 했는가)을 매번 ADR에 다시 적어야 함 — 본 시리즈가 그 비용을 흡수

**트레이드오프**:
- 모든 컴포넌트 명명을 기능 기반(edge, dn, hot, meta, …)으로 통일 — 도메인 직관 ↑, 마케팅성 brand 이름 ↓
