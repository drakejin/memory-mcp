# 기능 인벤토리 — 전제(불변식)와 검증 매핑

| 항목 | 내용 |
|---|---|
| 목적 | 모든 기능을 나열하고, 각 기능이 "잘 작동한다"가 성립하기 위한 **전제(불변식)** 를 명시하며, 그 전제를 E2E + 저장소 검사(persistency probe)로 어떻게 증명하는지 매핑한다 |
| 작성일 | 2026-08-25 |
| 역할 | `test/e2e/`의 시나리오 정의서. 테스트 이름은 이 문서의 P-번호를 참조한다 |
| 관련 | [architecture-v2.md](architecture-v2.md) · [code-standards.md](code-standards.md) · [../spec/](../spec/README.md) |

## 1. 기능 목록

### 1.1 Episodic (사건)

| # | 기능 | 엔드포인트 | 비고 |
|---|---|---|---|
| F1 | 사건 기록 | `POST /v1/{ws}/{team}/{proj}/episodes` | hot 원자 쓰기 → 파생 색인 best-effort. 파생물 다운이어도 201 + degraded |
| F2 | 형태소 검색 | `GET .../episodes/search` | nori, 시간범위·kind 필터, 발췌+점수만(본문 미주입), recall bump |
| F3 | 단건 조회 | `GET .../episodes/{id}` | hot 미스 시 **S3 아카이브 폴백** — cold로 내려간 사건도 id로 조회된다 |

### 1.2 Knowledge (지식)

| # | 기능 | 엔드포인트 | 비고 |
|---|---|---|---|
| F4 | 노드 생성 / supersede | `POST .../knowledge/nodes` | 비파괴 개정: 패자 archived+`superseded_by`, 승자 active, supersedes 엣지 자동 |
| F5 | 상태 전이 | `PATCH .../knowledge/nodes/{id}` | 허용표 기반. deprecate는 reason 필수. active 복귀 시 `superseded_by` 해제 |
| F6 | purge | `DELETE .../knowledge/nodes/{id}?confirm=true` | archived/deprecated 완충층에서만. 남는 노드들의 체인 참조 자동 수리 |
| F7 | 엣지 생성 | `POST .../knowledge/edges` | rel·provenance·confidence |
| F8 | 지식 검색 | `GET .../knowledge/search` | fulltext. archived/deprecated는 `include_archived` 옵트인 시만 노출 |
| F9 | 그래프 순회 | `GET .../knowledge/graph?entity=&depth=` | 이웃·supersede 체인·provenance |

### 1.3 Document (문서)

| # | 기능 | 엔드포인트 | 비고 |
|---|---|---|---|
| F10 | 문서 ingest | `POST .../documents` | sha256 멱등, blob은 **cold-first**(S3 선업로드), 결정적 추출, ~2KB 청킹(상한 500, 초과는 truncated 보고), document 노드 자동 생성 |
| F11 | 원본 조회 | `GET /v1/documents/{sha}` | 로컬 캐시 미스 시 S3 복원, sha 재검증 |
| F12 | 청크 목록 | `GET /v1/documents/{sha}/chunks` | chunk_seq 순 |

### 1.4 수명·운영

| # | 기능 | 엔드포인트 | 비고 |
|---|---|---|---|
| F13 | consolidation | `POST /v1/consolidate` | ① 증류 후보 제안(미통합 episode 클러스터 — 증류 자체는 에이전트 몫) ② 에이징(§2 P4 순서) ③ knowledge 스냅샷(latest+ts) ④ dry-run |
| F14 | 재수화/감사 | `POST /v1/reindex` (`verify`) | 전체 재구성, 전량 재해싱 감사 |
| F15 | 정직성 보고 | `GET /v1/status` | 드리프트·미통합 수·stale 미통합·dirty 파일·manifest 신선도·S3 도달성/최근 아카이브·degraded |
| F16 | 헬스/문서화 | `GET /healthz` · `/swagger/*` | |

### 1.5 횡단 메커니즘 (엔드포인트 없음)

| # | 기능 | 비고 |
|---|---|---|
| F17 | 재수화 | startup + 요청 진입 stat-gate(2s 디바운스), manifest 대조, 프로젝트 단위 부분 재수화 |
| F18 | degraded 모드 | 파생물 다운 시: 쓰기 200/201+degraded, 검색 읽기만 503. 복구 후 자동 수렴 |
| F19 | hot→cold 에이징 | `consolidated=true AND TTL(30d)` 또는 파일 압박(5MB/5,000건) — 단 압박도 consolidated만 이동 |
| F20 | purge 백스톱 | S3 versioning + knowledge 스냅샷 이력 |

## 2. 전제 (P) — 이것이 성립해야 기능이 "잘 작동한다"

각 전제는 **어기면 데이터가 거짓말을 하게 되는 불변식**이다. E2E는 API 응답이 아니라 **네 저장소(hot JSON · OpenSearch · Neo4j · S3)의 실제 상태**를 단계 사이마다 검사해 증명한다.

| ID | 전제 | 어기면 생기는 일 | 검증(E2E × persistency probe) |
|---|---|---|---|
| P1 | **정본 우선** — 모든 쓰기는 hot JSON에 원자적으로 먼저 커밋. 파생물 실패는 쓰기 실패가 아니다 | 색인만 있고 원본 없는 유령 데이터 | 쓰기 직후 hot 파일을 직접 파싱해 레코드 존재 단언. OpenSearch를 내리고 쓰기 → 201 + hot 존재 + OS 부재 |
| P2 | **파생물 = f(hot)** — OS·Neo4j 내용은 hot에서 언제든 동일 재구성된다. recall bump 포함 랭킹 재료도 hot에 산다 | 컨테이너 죽으면 데이터 유실 | 검색·순회 결과 스냅샷 → 컨테이너 파괴·재생성 → 재수화 → **필드 단위 diff = 0** |
| P3 | **멱등성** — 재수화 MERGE, 같은 sha 재ingest, 같은 달 재에이징, blob 재업로드은 몇 번 해도 같은 결과 | 재시도가 중복을 만든다 | 각 연산 2회 실행 → 저장소 4곳의 개수·내용 불변 단언 |
| P4 | **에이징 순서** — S3 put 확인 → hot 삭제 → 색인 삭제. 역순·생략 없음. **미통합은 어떤 압박에도 삭제 금지**. consolidated 마킹은 에이전트만 | 증류 안 된 기억이 소멸 | 잘못된 버킷으로 강제 실패 → hot 무손실 단언. 정상 실행 → S3 객체 내용에 레코드 포함 확인 후에만 hot 부재. 미통합 30일+ 방치 → 잔존 + status 노출 |
| P5 | **상태기계** — 전이 허용표 밖 거부(409), deprecate=reason 필수, purge=confirm+완충층에서만, supersede 비파괴, purge 후 체인 참조 수리 | 판정 없는 삭제·이력 소실 | 전이 매트릭스 전체(합법 6·불법 3·게이트 3) 실행 → hot과 Neo4j **양쪽** state 일치. purge 후 남은 노드의 supersedes/superseded_by에 유령 id 없음 |
| P6 | **정직성** — 시스템은 자기 한계를 항상 보고: 드리프트·미통합·truncated·degraded·S3 도달성 | "보고 없음"을 "완전함"으로 오독 | 각 이상 상태를 만들고 status/응답 필드가 **정확한 수치**로 보고하는지(0 아님, 과소·과대 없음) |
| P7 | **검색 계약** — 본문 전문 미주입(발췌만), 조사·어미 변형 매칭, 프로젝트 스코프 격리 | 토큰 폭발·오염된 회상 | "보안을 끄고" 저장→"보안을 끄는" 히트. 응답에 text 전문 부재. 다른 proj에 동일 문장 저장 → 교차 히트 0 |
| P8 | **입력 봉쇄** — ProjectKey 검증·경로 컨테인먼트, sha 형식 검증, 루프백 바인드 | 경로 탈출·원격 노출 | `../` 류 키 400, 잘못된 sha 400, 외부 인터페이스 바인드 시도 시 기동 실패 |
| P9 | **시간 규약** — 저장은 RFC3339 UTC, 비교는 사전순 안전. Clock 주입 | TTL·에이징 오판 | 저장된 hot JSON의 시각 필드 포맷 검증. e2e가 과거 시각 레코드를 주입해 TTL 경계(29일/31일) 판정 확인 |
| P10 | **격리·동시성** — {ws}/{team}/{proj} 단위 파일·색인 스코프·그래프 키 격리, hot 쓰기 레이스 없음 | 팀 간 기억 혼선 | 동일 id 패턴을 두 프로젝트에 병행 기록 → 파일·검색·그래프 교차 오염 0. 동시 append N건 → 유실 0 |
| P11 | **provenance 영속** — knowledge→episode 링크는 episode가 cold로 가도 살아있다 | 유래 추적 단절 | 노드 provenance의 episode를 에이징 → `GET episodes/{id}`가 S3 폴백으로 복원 |
| P12 | **blob cold-first** — 문서 원본은 ingest 시점에 이미 S3에 있다. 로컬 캐시는 축출 가능 | 캐시 축출=원본 유실 | ingest 직후 S3 존재(서버 아닌 aws CLI로), 로컬 캐시 삭제 후 조회 → 바이트 동일 복원 |

## 3. 갭 (G) — v1에 있었으나 v2에 없는 것. **전제가 아니라 로드맵이다**

솔직하게: 사용자가 물은 "충돌·풍화 대처"의 일부는 v2에 아직 없다. E2E는 이것들을 검증하지 않는다(없는 것을 있다고 못 박는 테스트 금지).

| ID | v1 기능 | v2 현황 |
|---|---|---|
| G1 | **충돌 게이트** — remember 시 이웃 감지 → `possible_conflicts`(판정 재료) 반환, pair-hash 멱등 가드, 미해결 충돌 status 노출 | 없음. 충돌 처리는 에이전트가 supersede를 **명시 호출**할 때만. 유사 노드 자동 표면화 없음 |
| G2 | **풍화 랭킹** — `score = text × trust_w × state_w × (1+freshness+heat)` | 부분. 이산 버전만: archived/deprecated는 검색에서 옵트인 제외(F8), recall_count는 기록만 되고 랭킹 미반영. 연속 가중치 없음 |
| G3 | **review_after 도래 보고** — 재검토 기한 지난 기억을 status가 노출 | 필드는 저장·왕복되나 status에 미노출 |
| G4 | **heat 기반 compaction 후보** — 오래되고 회상 안 된 episode 증류 제안 | 부분. 후보 제안은 있으나(F13) 기준이 미통합 클러스터뿐, recall heat 미반영 |

## 4. 디렉토리 재구성 매핑

레이어드 헥사고날. **레이어 간 의존은 인터페이스로만** — 구현 struct는 자기 패키지 밖으로 나가지 않고, `New`는 인터페이스를 반환하며, 조립은 `app`에서만 한다.

```
cmd/server/http/                     ← cmd/memory-mcp        (main: flag/env → app 호출만)
internal/app/httpserver/             ← internal/server 중 조립·기동 (composition root, router, startup, graceful)
internal/handler/app/httpserver/     ← internal/server 중 핸들러·검증·봉투·apierr·degraded
internal/service/episode/            ← episodic 도메인 + 기록/검색/조회 오케스트레이션 (핸들러에서 추출)
internal/service/knowledge/          ← knowledge 도메인·상태기계 + 노드/엣지/순회 오케스트레이션
internal/service/document/           ← internal/document
internal/service/consolidate/        ← internal/consolidate
internal/service/rehydrate/          ← internal/rehydrate
internal/external/persistence/hotstore/  ← internal/hotstore   (정본 JSON — 유일한 persistence)
internal/external/persistence/blob/      ← internal/blob       (컨텐츠 주소 캐시)
internal/external/memory/episode/        ← internal/search     (OpenSearch — 비영속 파생물이므로 "memory")
internal/external/memory/knowledge/      ← internal/graph      (Neo4j — 동상)
internal/external/thirdparty/cold/       ← internal/cold       (S3)
internal/x/errs/                     ← internal/errs
internal/x/ulid/                     ← internal/ulid
internal/x/config/                   ← internal/config
internal/x/projectkey/               ← hotstore.ProjectKey 승격 (모든 레이어가 쓰는 식별자)
```

파생물(OpenSearch·Neo4j)을 `memory/`로 분류하는 근거: 이 시스템에서 그것들은 **폐기 가능한 비영속 캐시**다(§architecture-v2 0.1). persistence는 정본인 hot JSON뿐이다.

### 4.1 레이어 규칙

1. 의존 방향: `handler → service → (service가 선언한 포트)` ← `external`이 구현. `app`만 전부 import해 조립한다. 역방향 import는 컴파일 타임에 없어야 한다.
2. **포트는 소비자(service)가 선언한다** — external의 넓은 Client를 service가 import하지 않는다. service 패키지 안에 자신이 쓸 만큼의 narrow interface를 정의하고, external의 Client가 그것을 구조적으로 만족한다(duck typing, code-standards §1.1).
3. handler는 service의 **인터페이스**(`episode.Service` 등)만 받는다. service 구현 struct·external Client가 handler 시그니처에 등장하면 위반.
4. 레이어를 넘는 값은 도메인 값 타입(Record, Node…)과 에러(`*errs.Error`)뿐. 구현 struct 전달 금지.
5. 에러 규약은 기존대로: handler만 `apierr.From`으로 변환한다.

## 5. E2E 스위트 구성 (`test/e2e/`, build tag `e2e`)

- **4-store inspector**를 공유 헬퍼로: hot(JSON 직접 파싱) · OpenSearch(HTTP _search 직격) · Neo4j(cypher-shell) · S3(aws CLI). 서버 API를 거치지 않고 저장소를 직접 보는 것이 요점 — 서버의 자기 보고를 서버로 검증하면 순환이다.
- 시나리오는 P1~P12와 1:1. 테스트명 `TestP01_SourceOfTruth` 형식으로 문서 역참조.
- 기존 `test/blackbox`(§10 수용 8단계)는 그대로 유지 — blackbox는 "스펙 수용", e2e는 "전제 불변식"으로 역할이 다르다.
- 실패 주입: 컨테이너 stop/재생성(P1·P2·P18), 잘못된 버킷 env로 별도 서버 기동(P4), Clock 주입 대신 과거 `occurred_at` 레코드 직접 기록(P9).
