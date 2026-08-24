# [논문분석] Codebase-Memory: Tree-Sitter-Based Knowledge Graphs for LLM Code Exploration via MCP

| 항목 | 내용 |
|---|---|
| 논문 | [arXiv:2603.27277v1](https://arxiv.org/abs/2603.27277) (cs.SE, 2026-03-28) |
| 저자/기관 | Martin Vogel(독립 연구자, 베를린), Falk Meyer-Eschenbach·Elias Grünewald·Felix Balzer(Charité 의료정보학연구소), Severin Kohler(FU Berlin/하이델베르크) 등 5명 |
| 실체 | 본 워크스페이스에 설치된 codebase-memory-mcp의 학술 버전. 설치본 실측 분석: [codebase-memory-mcp.md](./codebase-memory-mcp.md) |
| 분석일 | 2026-08-24 |
| 분석 목적 | memory-mcp 설계에 이식할 구조·평가 방법론 검토 |

## TL;DR

LLM 코딩 에이전트의 파일 읽기+grep 탐색은 구조적 질문("이 함수 바꾸면 뭐가 깨지나")에 토큰 수십만 개를 태운다는 문제의식에서 출발, **코드베이스 구조를 영속 지식 그래프로 만들어 MCP 도구로 노출**하는 시스템을 제시한다. tree-sitter 66개 언어 파싱 → 6단계 병렬 파이프라인 → SQLite 단일 파일 → 14개 타입드 MCP 도구. 31개 언어 실전 벤치마크에서 파일 탐색 에이전트 대비 **품질 83% vs 92%(90% 수준)를 토큰 1/10, 도구 호출 1/2.1로** 달성. 허브 탐지·호출자 랭킹 같은 그래프 네이티브 질의는 19/31개 언어에서 동등 이상. 기여의 절반은 시스템이고 나머지 절반은 "그래프가 어디서 이기고 어디서 지는지"를 가른 정직한 평가다.

---

## 1. 문제의식: 탐색 비용의 구조적 미스매치 (§1–2)

- 에이전트는 비구조화 텍스트로 코드를 탐색하지만, 개발자의 질문은 본질적으로 구조적이다(호출 그래프·의존 체인·모듈 경계·영향 분석). 텍스트 검색은 전이적(transitive) 관계를 못 잡고, 반복 참조 추적마다 토큰이 늘며 lost-context 위험이 커진다.
- 선행 연구 인용: 입력 토큰이 에이전틱 코딩 비용을 지배(캐싱을 써도), 실서비스 워크로드는 비자명한 repo에서 태스크당 수 달러. LoCoBench-Agent는 "철저한 탐색 ↔ 토큰 효율"이 파레토 충돌임을 보임 — 이 논문의 답은 "처음부터 구조적으로 관련된 정보만 준다"
- 기존 그래프 접근(CodeQL, Code Property Graph, GraphCoder, CodexGraph, RepoGraph, LocAgent, Prometheus 등)과의 차별점: **MCP 표준 인터페이스 + SQLite 제로 의존성 + 증분 동기화 + 66개 언어 단일 바이너리**. 즉 학술 기여보다 "배포 가능한 형태"가 차별점의 핵심

## 2. 시스템 설계 (§3)

### 2.1 3-스테이지 아키텍처

**Parse**(tree-sitter 66언어: 정의·시그니처·데코레이터·복잡도·호출부·임포트 8종 언어별 파서+제네릭 폴백·참조·trait 구현 추출; Go/C/C++는 LSP식 타입 해석 하이브리드) → **Build**(6단계 파이프라인) → **Serve**(MCP 14 도구). 전부 C로 구현, 상태는 SQLite 파일 하나(WAL).

### 2.2 6단계 빌드 파이프라인 (Table 3)

| 단계 | 산출 |
|---|---|
| 1. Structure | 파일 발견, Project/Package/Folder/File 노드 + 컨테인먼트 |
| 2. Extraction | pthreads 워커풀 병렬 정의 추출 (Function/Method/Class/Interface/Enum/Type + FunctionRegistry) |
| 3. Resolution | CALLS/IMPORTS/USAGES/USES_TYPE/IMPLEMENTS/INHERITS/DECORATES |
| 4. Enrichment | TESTS, HTTP 라우트 매칭, 설정 연결, git co-change |
| 5. Flush | 벌크 INSERT, 인덱스 생성 지연(deferred) |
| 6. Post-index | Louvain 커뮤니티, XXH3 파일 해시 |

워커는 per-worker 인메모리 그래프 버퍼(`cbm_gbuf_t`)에 쓰고 병합 후 일괄 flush — 임시 순번 ID를 flush 때 실제 row ID로 재매핑. 단일 트랜잭션이라 실패 시 원자성 보장.

### 2.3 호출 해석: 6단계 신뢰도 캐스케이드 (§3.4) — 이 논문의 백미

`pkg.Func` 같은 원시 callee 이름을 그래프 노드로 잇는 문제를, **전략별 신뢰도가 박힌 우선순위 캐스케이드**로 푼다:

| 순위 | 전략 | 신뢰도 |
|---|---|---|
| 1 | Import map 정확 일치 | 0.95 |
| 2 | Import map suffix 일치 | 0.85 |
| 3 | 같은 모듈 prefix 일치 | 0.90 |
| 4 | 프로젝트 유일 이름 (역인덱스, 후보 1개일 때만) | 0.75 |
| 5 | Suffix 일치 + import-거리 스코어링 | 0.55 |
| 6 | Fuzzy 문자열 유사 (최후 수단) | 0.30–0.40 |

관찰상 전략 1–3이 잘 구조화된 코드베이스 호출의 ~80%를 해석. 잔여는 4–6이 크로스모듈·동적 디스패치를 커버. **모든 엣지가 "얼마나 확신하는 연결인지"를 갖고 태어난다** — 파생 지식에 신뢰도를 함께 저장하는 패턴의 모범.

추가로 Go/C/C++는 이름 캐스케이드로 부족해서(메서드 리시버·포인터 간접·암묵 this·템플릿) 파일별 TypeRegistry + Scope 체인으로 리시버 타입을 상향 추론하는 전용 패스를 태운다. 해석된 호출은 문자열 캐스케이드를 우회하고 더 높은 신뢰 엣지를 만든다.

### 2.4 그 외 설계 포인트

- **HTTP_CALLS/ASYNC_CALLS**: 6개 프레임워크별 추출기(Python/Go/Spring/Ktor/Express/Laravel)로 REST 엔드포인트를 1급 그래프 개체화 + 신뢰도(0.0–1.0) — 마이크로서비스 경계를 넘는 그래프
- **증분 동기화** (§3.6): 파일 이벤트 → XXH3 해시 비교 → 변경 시 그 파일의 노드·엣지 삭제 후 재파싱 → 영향 커뮤니티만 재계산. XXH3 선택 이유 명시: 콘텐츠 주소 인덱싱에는 충돌 저항이 보안 요건이 아니므로 ~30GB/s 속도를 취함 — **해시도 용도별로 고른다**
- **Louvain 커뮤니티** (§3.7): CALLS류 엣지에 모듈성 최적화(γ=1.0, 내부 밀도 <1% 커뮤니티는 분리), 3–5회 반복 수렴 → Community 노드 + MEMBER_OF 엣지 → `get_architecture`의 재료. "아키텍처 요약"을 LLM이 아니라 그래프 알고리즘으로 만드는 접근

### 2.5 보안 하드닝 (§3.8) — MCP 서버의 신뢰 문제

"MCP 서버는 호스트 에이전트의 전체 권한으로 돌지만 서드파티 불투명 바이너리로 설치된다"는 위협 모델을 정면으로 다룬 드문 사례. 8층 CI 감사(위험 libc 호출 allow-list, 바이너리 문자열 감사, strace 네트워크 이그레스 감시 — localhost/DNS/GitHub만, 설치 경로 검증, 23종 적대 페이로드 MCP 강건성, vendored 의존성 SHA-256), 코드 수준(shell 인자 검증, SQLite authorizer로 ATTACH/DETACH 차단 = SQL 인젝션 기반 파일 생성 방지, realpath 컨테인먼트, ASan/UBSan), 릴리스(3단계 draft–verify–publish: Sigstore cosign + SLSA + CodeQL + VirusTotal 70+ 엔진 zero-tolerance + Defender/ClamAV + OpenSSF Scorecard + CycloneDX SBOM). memory-mcp도 사용자 기억을 다루는 만큼 최소한 경로 컨테인먼트·인젝션 방어는 이식 대상.

## 3. 평가 (§4) — "어디서 이기고 어디서 지는가"

### 3.1 설정

12개 질문 카테고리(허브 탐지·호출자 랭킹·의존 매니페스트·전체 호출 체인 추적 등) × 31개 언어 × 실제 오픈소스 repo(78 노드 HCL부터 49,398 노드 Python/Django까지). MCP Agent(14 도구) vs Explorer Agent(파일 읽기+grep), 동일 백본 Claude Opus 4.6, 수동 코드 검사로 만든 참조 답안 대비 채점(≥0.80 PASS).

### 3.2 결과 (Table 6)

| 지표 | MCP | Explorer | 차이 |
|---|---|---|---|
| 품질 | 0.83 | 0.92 | Explorer의 90% |
| 도구 호출/질문 | 2.3 | 4.8 | 2.1배 적음 |
| 토큰/질문 | ~1,000 | ~10,000 | **10배 적음** |
| 질의 지연 | <1ms | 10–30s | >100배 빠름 |

- 그래프 우위: 허브 탐지·호출자 랭킹 등 **사전 계산된 엣지를 따라가는 질의**에서 19/31 언어 동등 이상. 함수형 언어(Haskell/OCaml/Elixir)에서 격차 ~1%까지 수렴
- Explorer 우위: 라인 수준 전체 소스 컨텍스트가 필요한 질의(16/31) — **그래프가 의도적으로 저장하지 않는 것**. 최악은 매크로 헤비 C(0.58 vs 1.00) — 매크로는 AST에 없다
- 속도 비대칭의 원인: 그래프는 사전 계산(BFS via 재귀 CTE ~0.3ms)을 조회하고, Explorer는 질의 시점마다 구조를 재발견(grep→읽기→파싱 반복)한다. **색인 비용(49K 노드 6s)을 한 번 내고 이후 전 질의에 상각**하는 구조
- 시스템 성능(M3 Pro): Linux 커널 2.1M 노드 4.9M 엣지 ~3분, 증분 ~1.2s, Cypher <1ms, dead code 탐지 ~150ms

### 3.3 패러다임 비교 (Table 7)

Emb./RAG(10–30개 언어, 벡터DB 필요, 질의당 2–5K 토큰), Repo-Map/Aider(~100 언어, 영속성·구조 질의 없음), Graph+LLM/Neo4j 계열(8–14 언어, 인프라 무거움, ~5K 토큰) 대비 — 66 언어·SQLite·임베딩 모델 불요·~1K 토큰·자동 동기화·MIT. (주: 설치본 v0.10.8은 이후 로컬 임베딩을 추가했다 — 실측 분석 문서 §5 참고)

## 4. 한계와 비판적 독해

1. **평가자 편향**: 채점을 "제1저자가 참조 답안 대비 수행" — 자기 시스템 채점. 카테고리 정의도 그래프에 유리한 구조 질문 중심(12개 중 라인-수준 질의는 소수)
2. **품질 9%p 격차는 작지 않다**: "90% 품질에 1/10 비용"은 훌륭한 엔지니어링 트레이드오프지만, 정답이 중요한 태스크에서는 Explorer 병용이 전제다. 논문 스스로 §4.1에서 보완 관계를 인정 — 도구 설계 시사점: 그래프 도구는 탐색을 **대체**하는 게 아니라 **선행**한다
3. **v1 시점의 스냅샷**: 임베딩·시맨틱 엣지·커버리지 보고(::missed) 등 설치본의 핵심 진화가 논문에 없다. 논문만 읽으면 순수 구조 그래프로 오해하게 됨
4. 증분 동기화의 정합성(동시 쓰기·watch 누락 시 drift)에 대한 검증 시나리오가 없음 — 주간 전량 재해싱 같은 감사 개념 부재 (AX 정책이 이 갭을 메운다)
5. 커뮤니티 검출(γ=1.0 고정)의 안정성 — 재색인마다 커뮤니티가 흔들리면 get_architecture 답도 흔들리는데, 안정성 논의 없음

## 5. memory-mcp 이식 포인트

### ① 토큰 경제학을 1급 설계 목표로

"질의당 ~1K 토큰"이 성능 지표로 취급된다. memory-mcp의 recall 응답도 **경로+발췌+메타데이터만** 돌려주고 본문은 필요 시 읽게 하는 2단 구조로(사용자 AX 정책 원칙 3과 합치). 도구 수 최소화(2.3 calls/question이 가능했던 이유 = 도구가 타입드·목적특화)도 같은 축.

### ② 파생 지식에 신뢰도를 새겨서 저장

6단계 캐스케이드처럼, 자동 추론으로 만든 연결(기억 간 관계·중복 판정·entity 추출)은 **어떤 전략이 얼마나 확신하고 만든 것인지**를 함께 저장. 나중에 랭킹·판정 게이트가 이 신뢰도를 소비한다.

### ③ 용도별 해시 선택과 비용 상각 구조

변경 감지는 빠른 해시(XXH3류), 무결성·팀 공유는 sha256 — 용도 분리. "색인 1회 비용 → 전 질의 상각" 구조는 memory-mcp의 증류(비싼 LLM 요약을 콘텐츠 해시 캐시로 1회만) 설계와 동형.

### ④ 정직한 자기 평가 프레임

"어디서 이기고 어디서 지는가"를 표로 박는 평가 문화. memory-mcp도 recall이 강한 질의(주제 회상·중복 탐지)와 약한 질의(정밀 사실 검증 — 원문 읽기로 보완)를 문서에 명시할 것.

### ⑤ MCP 서버 보안 최소선

경로 컨테인먼트(store 루트 밖 읽기 금지), 파괴 연산의 명시적 확인 플래그, 입력 검증. 기억은 코드보다 민감하다.

## 관련 링크

- 논문: https://arxiv.org/abs/2603.27277
- 구현체: https://github.com/DeusData/codebase-memory-mcp
- 설치본 실측 분석: [codebase-memory-mcp.md](./codebase-memory-mcp.md)
- 같은 목적의 대극 설계(정본 없는 기억): [memos-memory-os-analysis.md](./memos-memory-os-analysis.md)
