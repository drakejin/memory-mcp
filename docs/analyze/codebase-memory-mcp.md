# [도구분석] codebase-memory-mcp — 메모리 관리 방법론

| 항목 | 내용 |
|---|---|
| 대상 | codebase-memory-mcp v0.10.8 ([DeusData/codebase-memory-mcp](https://github.com/DeusData/codebase-memory-mcp)) |
| 논문 | [arXiv:2603.27277v1](https://arxiv.org/abs/2603.27277) "Codebase-Memory: Tree-Sitter-Based Knowledge Graphs for LLM Code Exploration via MCP" — 별도 분석: [codebase-memory-paper-analysis.md](./codebase-memory-paper-analysis.md) |
| 분석 방법 | 로컬 설치본 실측 — 바이너리(`~/.local/bin`, 295MB arm64), 실 운영 DB 40개 12GB(`~/.cache/codebase-memory-mcp/`) 스키마·데이터 직접 조회, CLI 실행, 논문 대조 |
| 분석일 | 2026-08-24 |
| 분석 목적 | memory-mcp 설계에 이식할 **메모리 라이프사이클 / 저장 단위 / 캐싱 레이어 / 풍화·충돌 관리** 방법론 추출 |

## TL;DR

codebase-memory-mcp는 **정본(ground truth)이 존재하는 메모리**의 관리 방법론이다. 소스코드라는 원본이 항상 있으므로, 메모리를 "원본에서 계산되는 파생 인덱스 f(source)"로 정의하고 모든 라이프사이클 문제를 **노화 판정이 아니라 거울의 신선도(mirror freshness) 문제로 환원**한다. 그 결과 상태 기계·버전 체인·TTL이 전부 없고, 대신 ① 콘텐츠 해시 기반 증분 재색인, ② 언제든 폐기·재구성 가능한 파생물 계층, ③ 자기 기억의 불완전성을 1급 데이터로 보고하는 커버리지 정직성이 방법론의 전부다. 반대로 **정본이 없는 기억**(ADR, 요약, 런타임 트레이스)에 대해서는 라이프사이클 장치가 극도로 얇다(프로젝트당 1문서 전문 교체, 버전·충돌 판정 없음) — 이 갭이 memory-mcp가 채워야 할 지점이다.

---

## 0. 실물 구조 개요

### 0.1 배포 형태

- **단일 실행 파일** 295MB: C 코어(파이프라인·그래프 저장·MCP 프로토콜·Cypher 엔진·백그라운드 동기화) + 66개 tree-sitter 문법 C 소스 vendored + 그래프 시각화 웹 UI(React 19/three.js) 번들. 런타임 의존성 0
- 한 바이너리가 4가지 모드: stdio MCP 서버(기본) / `cli <tool> '{json}'` 단발 실행 / HTTP 그래프 UI(포트 9749) / `install`(43종 에이전트 클라이언트 설정 자동 주입)
- 이 머신에는 `~/.claude.json` 글로벌 mcpServers에 stdio로 등록

### 0.2 저장소 배치

```
~/.cache/codebase-memory-mcp/
  _config.db          # 전역 설정 (auto_index=false, auto_watch=true, ui_port=9749 …)
  {project}.db        # 프로젝트당 SQLite 1개 — 실측 40개, 총 12GB
  logs/               # 인덱싱 런별 로그 (coverage 전체 목록 포함)
```

### 0.3 프로젝트 DB 내부 스키마 (실측)

| 테이블 | 역할 |
|---|---|
| `nodes` | 그래프 노드. `UNIQUE(project, qualified_name)`, label + 파일경로·라인 + properties JSON |
| `edges` | 타입드 엣지. `UNIQUE(source_id, target_id, type, local_name_gen)` — IMPORTS의 local alias까지 정체성에 포함 |
| `nodes_fts` | FTS5 (unicode61, remove_diacritics 2) — 이름·경로 전문 검색 |
| `node_vectors` | 노드당 768바이트 고정 임베딩 (실측: 노드 3,088 ↔ 벡터 3,088 = 전 노드 커버) |
| `token_vectors` | 토큰별 벡터 + **IDF** — 렉시컬·시맨틱 하이브리드 랭킹 |
| `file_hashes` | rel_path → sha256 + mtime_ns + size. 증분 색인의 기준점 |
| `lsp_surface` | 파일별 defs JSON + **ref_bloom**(참조 블룸필터) + surface_sha |
| `index_coverage`(+meta) | (rel_path, kind, detail) — 파싱 실패·스킵 추적 |
| `project_summaries` | get_architecture 요약 캐시. **source_hash로 무효화** |
| `store_meta` | db_uid, mutation_gen, coverage_shadow_fp |

실측 노드 라벨 17종(Function 1,949 / Variable / Method / File / Struct / Package / Module / Folder / Class / Section / Route / Interface / Decorator / Type / Resource / Project / Branch — ads-datacat 기준), 엣지 22종(DEFINES 8,646 / CALLS 7,787 / USAGE / SIMILAR_TO / DEFINES_METHOD / TESTS / IMPORTS / CONTAINS_FILE / DEPENDS_ON / SEMANTICALLY_RELATED / IMPLEMENTS / WRITES / OVERRIDE / … / FILE_CHANGES_WITH / HAS_BRANCH).

---

## 1. 저장 단위 — "격리는 DB 파일, 정체성은 의미 경로"

### 1.1 3계층 단위

| 계층 | 단위 | 연산 |
|---|---|---|
| store | 프로젝트 = SQLite 파일 1개 | 생성(index)·삭제(delete_project = 파일 삭제)·공유(persistence 아티팩트) |
| record | 노드 = `(project, qualified_name)` | upsert·파일 단위 일괄 삭제 |
| relation | 엣지 = `(source, target, type, local_name)` | 노드 삭제에 CASCADE |

- **프로젝트가 격리의 단위**다. 다른 프로젝트와 어떤 테이블도 공유하지 않아 삭제·재색인·이동·백업이 파일 연산 하나로 끝난다. 크로스 프로젝트 연결(CROSS_HTTP_CALLS 등)은 `cross-repo-intelligence`라는 별도 모드로만, 명시 요청 시 생성된다.
- **노드의 정체성은 위치가 아니라 의미 경로**(qualified_name)다. 파일경로·라인은 속성이라, 코드가 파일을 옮겨도 정체성이 유지되고 참조 엣지가 살아남는다.
- 관계도 1급 데이터: 엣지 유니크 키에 IMPORTS의 local alias(`import foo as f`)까지 들어가 "같은 대상을 다른 이름으로 두 번 임포트"도 구분 저장한다.
- **팀 공유 단위 = 파생물 스냅샷**: `persistence: true`로 `.codebase-memory/graph.db.zst` 아티팩트를 repo에 남기면 팀원은 전체 재색인 없이 부트스트랩한다. 원본(코드)이 이미 git으로 공유되므로, 공유하는 것은 "계산 결과의 캐시"뿐이다.

### 1.2 MemCube와의 대비

MemOS의 MemCube는 페이로드에 거버넌스 속성(TTL·권한·민감도)과 행동 지표(접근 빈도·version chain)를 헤더로 붙인다. codebase-memory-mcp의 노드에는 이런 헤더가 **없다**. 정본이 소스코드에 있고 파생물은 언제든 재계산되므로, 버전·TTL·신뢰도를 노드에 붙일 이유 자체가 없다는 설계다. 저장 단위 설계는 "그 기억에 정본이 있는가"에 따라 완전히 달라진다는 것을 보여주는 대조 사례.

### 1.3 정본 없는 기억의 저장 단위 (얇은 부분)

- **ADR** (`manage_adr`): 프로젝트당 **문서 1개**. mode=get/update/sections뿐이고 update는 **전문 교체**(complete replacement). 권장 섹션 힌트(PURPOSE/STACK/ARCHITECTURE/PATTERNS/TRADEOFFS/PHILOSOPHY)만 있다. 버전 체인·항목 단위 식별·충돌 감지 없음.
- **런타임 트레이스** (`ingest_traces`): `(caller, callee, count)` 삼중항을 그래프에 오버레이. 정적 분석이 못 잡는 실호출(리플렉션·동적 디스패치)을 보강하는 훌륭한 아이디어지만, 재-ingest 시 누적/교체 정책·시효 개념이 표면에 없다.

---

## 2. 메모리 라이프사이클 — "상태 기계가 없는 라이프사이클"

### 2.1 탄생: 6단계 파이프라인 (논문 §3.3)

단일 SQLite 트랜잭션 안에서: ① Structure(파일 발견, 컨테인먼트 엣지) → ② Extraction(pthreads 워커풀 병렬 정의 추출, per-worker 인메모리 버퍼 + work-stealing) → ③ Resolution(호출·임포트·타입 사용 해석 — 6단계 신뢰도 캐스케이드) → ④ Enrichment(TESTS, HTTP 라우트 매칭, git co-change) → ⑤ Flush(벌크 INSERT, 인덱스 생성은 뒤로 미룸) → ⑥ Post-index(Louvain 커뮤니티, 파일 해시).

인덱싱 모드가 곧 **파생 지식의 선택적 생성 스위치**다: `fast`는 구조만, `moderate/full`은 유사도·시맨틱 엣지(SIMILAR_TO, SEMANTICALLY_RELATED)까지. 비싼 파생물일수록 옵트인.

### 2.2 갱신: 이벤트는 힌트, 판정은 해시

`auto_watch=true`면 파일 워처(XXH3 + adaptive polling)가 변경을 감지하고, **변경 파일에 속한 노드·엣지만 삭제 후 재파싱**한다. 영향받은 Louvain 커뮤니티만 재계산. 실측 성능(논문): 증분 재색인 ~1.2s, 전체 색인 49K 노드 ~6s.

- 파일 삭제 = 그 파일 노드 전부 삭제. **tombstone이 필요 없다** — 원본이 사라졌다는 사실 자체가 정본이므로 파생물도 그냥 지우면 된다.
- 브랜치 전환·rebase 같은 대량 변경도 같은 경로로 흡수된다(대량 diff일 뿐).

### 2.3 죽음: 상태 전이 없이 파일 삭제

`delete_project` = DB 파일 삭제. Archived/Expired 같은 완충 단계가 **없다**. MemOS가 `Generated → Activated → Merged → Archived → Expired` 5단계를 두는 것과 정반대인데, 이유가 명확하다: **잘못 지워도 잃는 것이 없다.** 원본에서 언제든 동일하게 재구성되기 때문이다. 상태 기계는 "복구 불가능한 기억"에만 필요한 장치임을 보여준다.

### 2.4 요약의 라이프사이클: 기억이 아니라 캐시

`get_architecture`의 프로젝트 요약은 `project_summaries(source_hash)`에 저장되고 소스가 바뀌면 무효화된다. "지식의 개정"이 아니라 "캐시 미스"로 처리 — 파생물 세계에서는 요약조차 상태가 아니라 캐시다.

---

## 3. 캐싱 레이어 — "모든 파생물에 무효화 키를 달아라"

실측으로 확인되는 캐시 계층과 각각의 키·무효화 조건:

| 레이어 | 캐시 키 | 무효화 트리거 | 비고 |
|---|---|---|---|
| L0 stat-gate | mtime_ns + size | 파일 stat 변화 | 해시 계산조차 생략하는 최전선 게이트 |
| L1 콘텐츠 해시 | sha256 (`file_hashes`) | 내용 실변경 | stat이 바뀌어도 내용 동일이면 재색인 skip |
| 워처 | XXH3 (논문 §3.6) | 파일시스템 이벤트 | 이벤트는 재검사 힌트일 뿐, 판정은 해시 |
| lsp_surface | surface_sha | 파일 표면(정의부) 변경 | defs JSON + ref_bloom(참조 존재 판정을 블룸필터로 근사) |
| project_summaries | source_hash | 소스 변경 | LLM/분석 요약의 재계산 게이트 |
| FTS·벡터·IDF | 재색인 시 재계산 | 소속 노드 변경 | 검색 가속용 순수 파생물 |
| coverage_shadow_fp | 커버리지 지문 (`store_meta`) | 커버리지 변화 | 커버리지 재보고 게이트 |

**발견 포인트 — 가상 파일 트릭**: `file_hashes`에 실파일이 아닌 항목이 들어 있다 (실측: `.codebase-memory/.semantic-input/git-context-v1`, `global-extension-config-v1`). git 컨텍스트·전역 설정 같은 **비파일 입력도 "가상 파일"로 직렬화해 같은 해시 테이블에 등재** — 설정이 바뀌면 파일 변경과 동일한 경로로 재색인이 트리거된다. 무효화 경로를 하나로 통일하는 우아한 방법.

전 계층을 관통하는 원칙: **파생물은 반드시 (a) 입력의 해시를 키로 갖고, (b) 통째로 폐기해도 원본에서 재구성 가능**해야 한다. AX workspace 정책의 원칙 1("저장소가 콘텐츠를 소유한다, 파생물에만 존재하는 콘텐츠 금지")과 정확히 같은 계열이다.

---

## 4. 풍화와 충돌 — "정본이 있으면 풍화는 신선도 문제로 환원된다"

### 4.1 풍화(시간 축)에 대한 답

이 도구에서 지식은 낡지 않는다. **낡는 것은 거울(파생물)뿐**이고, 그것은 판정이 아니라 동기화로 고친다:

- stale 감지 = stat-gate → 해시 diff (§3)
- stale 수리 = 파일 단위 delete + reparse (§2.2)
- 시간이 지나도 가치가 떨어지지 않으므로 TTL·감쇠·아카이브 정책이 전부 부재

유일한 "역사" 기억은 `FILE_CHANGES_WITH` 엣지(git 커밋 이력에서 파생한 동시 변경 패턴)인데, 이것조차 git이라는 정본에서 재계산되는 파생물이다.

### 4.2 충돌(공간 축)에 대한 답: 화해하지 않고 격리한다

같은 코드베이스의 여러 시점/브랜치가 동시에 존재하면? — **네임스페이스 격리**로 답한다. 실측: 이 머신에 `ads-genisys.db`, `ads-genisys-pr374.db`, `ads-genisys-layer2-split.db`, `ads-genisys-ingestion-nodata.db`가 병존한다(worktree별 별도 인덱스). 서로 다른 말을 하는 두 그래프를 화해시키는 장치는 없고, 필요도 없다 — 각각 자기 원본의 충실한 거울이면 충분하다.

충돌 개념이 존재하는 유일한 지점은 **원본 vs 거울** 사이이고(=신선도), 그 판정자는 해시다. "무엇이 사실인가"라는 질문이 "어느 쪽이 원본과 일치하는가"로 치환되므로 LLM도 사람도 판정에 필요 없다.

### 4.3 커버리지 정직성: 불완전성을 1급 데이터로

이 도구가 풍화·충돌 대신 정면으로 다루는 문제는 **자기 기억의 구멍**이다:

- 3분류 보고: `skipped`(아예 색인 안 됨 — 크기 초과/읽기·파싱 실패) / `parse_partial`(색인됐지만 특정 라인 범위의 구문이 그래프에 빠졌을 수 있음 — tree-sitter 에러 복구의 잔여) / `excluded`+`not_indexed`(gitignore 등 **의도적** 제외 — 실패가 아님)
- 문서화된 겸손: *"absence of a flag is NOT a completeness guarantee"* — 플래그 없음 ≠ 완전 색인. 플래그된 범위는 grep을 쓰라고 자기 입으로 안내
- 실측: 커버리지 갭 자체가 `{project}::missed`라는 **섀도 프로젝트 그래프**로 저장되어 `query_graph(graph="missed")`로 구조적으로 질의 가능
- 전체 목록은 런별 로그파일에, 요약은 응답에 — 응답 크기와 완전성의 트레이드오프도 명시적

"기억하고 있다"와 "기억하지 못한다는 것을 기억한다"를 분리한 설계로, 어떤 memory 시스템이든 이식할 가치가 있다.

### 4.4 detect_changes: 역방향 풍화 예보

`detect_changes`는 git diff를 심볼로 해석해 그래프를 역방향 순회, **변경의 파급 반경(blast radius)**을 계산한다. "이 변경으로 어떤 지식이 낡게 되는가"를 변경 시점에 미리 계산하는 도구 — 풍화를 사후 감지가 아니라 사전 예보로 뒤집은 역발상이다.

### 4.5 얇은 곳: 정본 없는 기억의 풍화·충돌은 미해결

ADR·트레이스·(향후) 사용자 주석 같은 **주장(assertion) 기억**은 이 방법론의 사각지대다. ADR이 낡아도 아무것도 감지하지 않고, 새 ADR은 옛 ADR을 전문 교체로 지운다(이력 소실). 코드는 바뀌었는데 ADR이 그대로면 — 그 충돌을 잡을 장치가 없다. 정본이 없는 순간 "해시로 판정"이 불가능해지고, MemOS식 상태·버전·신뢰도나 AX식 판정 게이트가 필요해진다. **codebase-memory-mcp가 의도적으로 풀지 않은 이 문제가 memory-mcp의 문제 정의다.**

---

## 5. 설치본 v0.10.8 vs 논문 (진화 추적)

| 항목 | 논문 (2026-03) | 설치본 v0.10.8 (2026-08) |
|---|---|---|
| 도구 수 | 14 | 15 (`check_index_coverage` 추가) |
| 임베딩 | 명시적으로 "No embed model" | node_vectors(768B/노드) + token_vectors/IDF 하이브리드 |
| 시맨틱 엣지 | 언급 없음 | SIMILAR_TO, SEMANTICALLY_RELATED (moderate/full 모드) |
| 노드 종류 | 코드 심볼 중심 | Section(마크다운)·Branch·Resource 추가 |
| LSP 보조 | Go/C/C++ 타입 해석 패스 | + kotlin_lsp, lsp_surface 테이블 |
| 커버리지 | 언급 약함 | 3분류 + ::missed 섀도 그래프 + shadow_fp |

반년 사이의 진화 방향이 일관되게 **"검색 품질(시맨틱)"과 "정직성(커버리지)"** 두 축이라는 점이 시사적이다.

---

## 6. memory-mcp 이식 포인트

### ① 원본–파생물 분리와 전면 재구성 가능성

기억의 정본은 사람이 읽고 편집할 수 있는 원본(파일)에 두고, 검색 인덱스·그래프·통계는 전부 파생물로. `reindex` 한 번이 곧 재해 복구가 되도록. 파생물 오염을 걱정할 필요가 없어지면 파생물 설계가 과감해질 수 있다.

### ② stat-gate → 콘텐츠 해시 → 부분 재색인의 3단 증분

mtime+size로 1차 거르고, 해시로 확정하고, 바뀐 레코드만 갱신. 이벤트(워처)는 힌트로만 쓰고 판정은 항상 해시로. 비파일 입력(설정·사전 버전)도 **가상 파일로 해시 테이블에 등재**해 무효화 경로를 단일화.

### ③ 커버리지 정직성

"색인 안 된 것·부분 색인된 것·의도적으로 뺀 것"을 구분해 1급 데이터로 보고하고, "보고 없음 ≠ 완전함"을 도구 응답에 명시. 메모리 시스템의 신뢰는 회수율이 아니라 **자기 한계의 정확한 보고**에서 나온다.

### ④ 격리 단위 = 파일 하나

store를 파일(들) 하나로 격리해 삭제·백업·공유·이동을 파일 연산으로. 팀 공유는 원본(git) + 파생물 스냅샷 아티팩트 분리 배포.

### ⑤ 반면교사 — 정본 없는 기억에는 이 방법론이 통하지 않는다

memory-mcp가 다루는 기억(사용자 발화·결정·교훈)은 대부분 정본이 따로 없는 주장 기억이다. 해시로 판정할 수 없으므로: 상태 기계(MemOS), 버전 체인·supersedes(MemOS), 판정 게이트(AX policy), 신뢰도·유래 메타데이터(MemOS MemCube)를 도입해야 한다. 단, **파생물 계층(검색 인덱스)에는 ①~④를 그대로 적용**한다 — 원본(기억 파일)과 파생물(인덱스)의 이중 구조가 두 방법론을 결합하는 지점이다.

## 관련 문서

- 논문 자체 분석: [codebase-memory-paper-analysis.md](./codebase-memory-paper-analysis.md)
- MemOS 분석: [memos-memory-os-analysis.md](./memos-memory-os-analysis.md)
- 종합 설계: [../design/architecture.md](../design/architecture.md)
