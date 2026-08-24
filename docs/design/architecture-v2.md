# memory-mcp v2 — episodic/knowledge 이중 저장 + hot/cold 계층

| 항목 | 내용 |
|---|---|
| 목표 | 로컬 개인 메모리 서버(Go, HTTP API) — episodic은 OpenSearch, knowledge는 GraphDB, 정본은 로컬 JSON(hot), 아카이브는 S3(cold) |
| 작성일 | 2026-08-25 |
| 대체 | [architecture.md](architecture.md)(v1 설계)와 [golang-port.md](golang-port.md)(TS 파리티 포팅)를 대체. v1의 원칙(원본-파생물 분리·판정 게이트·정직성 보고)은 계승 |
| 스택 | Go 1.25 · go-chi · swaggo(Swagger UI) · OpenSearch 2.x(+nori) · Neo4j 5 · AWS S3(`vms-holdings`, **ap-northeast-2**) · Docker Compose |

## 0. 계승되는 원칙 (v1 → v2)

1. **원본이 콘텐츠를 소유한다** — 정본은 `~/.local/dj-memory/`의 JSON 파일(hot). OpenSearch·Neo4j는 **비영속 컨테이너 위 파생물**로, 언제든 내려가도 되고 재수화(rehydration)로 전부 재구성된다. 파생물에만 존재하는 콘텐츠 금지.
2. **판정 없는 자동 덮어쓰기 없음** — knowledge의 supersede/충돌 판정은 호출 에이전트의 몫. 서버는 결정적으로만 동작(서버 내 LLM 없음).
3. **정직성** — `/status`가 인덱스 신선도·재수화 필요 여부·미통합(unconsolidated) 건수·S3 동기화 상태를 항상 보고.

## 1. 저장 계층

```
                      ┌────────────── 파생물 (비영속, docker) ──────────────┐
                      │  OpenSearch (episodic 검색)   Neo4j (knowledge 그래프) │
                      └──────────────▲──────────────────────▲───────────────┘
                                     │ 실시간 upsert + 재수화  │
┌── hot (정본, 로컬 디스크) ───────────┴──────────────────────┴───────────────┐
│ ~/.local/dj-memory/                                                        │
│   episodic/{workspace}/{team}/{project}.json    # append 지향 사건 기록     │
│   knowledge/{workspace}/{team}/{project}.json   # 노드+엣지 그래프 문서      │
│   blobs/{sha256}                                # 원본 파일 캐시(축출 가능)  │
│   manifest.json                                 # 파일별 해시·인덱스 신선도  │
└──────────────────────────────┬─────────────────────────────────────────────┘
                               │ 에이징·스냅샷·블랍 업로드
┌── cold (S3: vms-memory-mcp, ap-northeast-2, versioning on) ────────────────┐
│ s3://vms-memory-mcp/{username}/episodic/{ws}/{team}/{proj}/{yyyy-mm}.json  │
│ s3://vms-memory-mcp/{username}/knowledge/{ws}/{team}/{proj}/latest.json    │
│                                    .../snapshots/{ts}.json                 │
│ s3://vms-memory-mcp/{username}/blobs/{sha2[:2]}/{sha256}                   │
└────────────────────────────────────────────────────────────────────────────┘
```

- `{username}` 기본값은 OS 사용자명(`jin`), env `DJ_MEMORY_USERNAME`으로 교체.
- hot 쓰기는 전부 **temp+rename 원자 쓰기**. 파생물 upsert는 best-effort — 실패해도 hot 쓰기는 성공하고 manifest에 dirty 마크, 다음 재수화가 수렴시킨다.

## 2. 두 메모리 평면 — 역할 정의

| | episodic (OpenSearch) | knowledge (Neo4j) |
|---|---|---|
| 담는 것 | 사건·대화·작업 로그·관찰·문서 청크 — "그때 무슨 일이 있었나" | 개체·사실·관계·교훈 — "지금 무엇이 참인가" |
| 수명 | 유한 — 통합(consolidation) 후 cold로 가라앉음 | **영구** — 삭제 대신 상태 전이(v1 상태기계 계승) |
| 변경 모델 | append-only (수정 없음, 정정은 새 record) | 비파괴 개정 — supersede 체인, active/archived/deprecated |
| 인덱스 역할 | nori 형태소 한국어 전문검색 + 시간 범위 필터 | Cypher 그래프 순회(entity 이웃, 체인, provenance) + lucene 전문 |
| 재수화 | hot JSON → bulk index (인덱스 전체 재작성 가능) | hot JSON → `MERGE` 멱등 upsert |

**연결**: knowledge 노드/엣지는 `provenance: [episode_id...]`로 유래 episode를 가리킨다. episode가 cold로 내려가도 id는 불변이므로 링크는 깨지지 않는다(S3 아카이브에서 조회 가능).

### 2.1 episodic record

```json
{
  "id": "01JD...",            // ULID
  "kind": "event | conversation | decision | observation | document_chunk",
  "occurred_at": "2026-08-25T02:00:00Z",
  "actor": "agent | user | system",
  "text": "본문 — nori 인덱싱 대상",
  "entities": ["memory-mcp", "opensearch"],
  "refs": {"doc_sha": "...", "chunk_seq": 3},   // document_chunk일 때
  "consolidated": false,       // 통합 여부 — cold 자격 조건
  "recall_count": 0, "last_recalled": ""
}
```

### 2.2 knowledge 노드·엣지

```json
// node
{
  "id": "01JD...", "kind": "entity | fact | lesson | preference | document",
  "name": "표제", "body": "서술", "aliases": [],
  "state": "active | archived | deprecated", "trust": "user-stated | agent-inferred | imported",
  "supersedes": [], "superseded_by": "", "provenance": ["episode_id"],
  "created": "...", "updated": "...", "review_after": ""
}
// edge
{ "from": "id", "to": "id", "rel": "relates_to | derived_from | supersedes | about",
  "provenance": ["episode_id"], "confidence": 0.9 }
```

## 3. 메모리 생명주기

```
[episodic]
  ingest ──► hot(.json) + OpenSearch 색인
     │  consolidation: 에이전트가 episode들을 증류해 knowledge 승격(POST /knowledge, provenance 링크)
     ▼
  consolidated=true ──(30일 경과 or 파일 임계 초과)──► S3 {yyyy-mm}.json 배치로 이동, hot에서 제거
                                                        (OpenSearch에서도 제거 — 검색은 hot 범위만)

[knowledge]
  승격/직접 생성 ──► active ──supersede──► archived ──deprecate(reason)──► deprecated
      영구: hot에 항상 상주. 통합 실행마다 S3 snapshot 백업(latest.json + snapshots/{ts}.json)
      purge(confirm 필수) → hot에서 제거하되 S3 versioning이 백스톱

[blob(문서 원본)]
  ingest 즉시 S3 업로드(cold-first) + 로컬 blobs/ 캐시(축출 가능)
```

### 3.1 hot → cold 전환 타이밍 (결정적 규칙)

| 대상 | 조건 (AND) | 시점 |
|---|---|---|
| episode | `consolidated=true` 그리고 `occurred_at` 30일 경과(`DJ_MEMORY_EPISODIC_TTL_DAYS`) | consolidation 실행 시 |
| episode(압박) | 프로젝트 파일 > 5MB 또는 5,000건 | consolidation 실행 시 오래된 consolidated부터 |
| knowledge | 이동 없음 — 스냅샷 백업만 | consolidation 실행마다 |
| blob | 없음 — 즉시 cold | ingest 시 |

미통합(unconsolidated) episode는 **나이와 무관하게 hot에 남는다** — 증류 없이 버리는 자동 삭제는 없다(원칙 2). 대신 `/status`가 "30일 넘은 미통합 N건"을 계속 노출한다.

## 4. consolidation (통합 작업)

트리거: `POST /v1/consolidate` (에이전트가 작업 세션 종료 시 호출) 또는 `--consolidate` CLI. 서버가 하는 일은 전부 결정적:

1. **후보 제안**: 미통합 episode를 entity·시간 클러스터로 묶어 증류 후보 목록 반환 — 증류(요약→knowledge 생성) 자체는 에이전트가 API로 수행
2. **entity 통계**: dictionary 기반 entity 정규화, co-occurrence 엣지 가중 갱신
3. **에이징**: §3.1 규칙으로 cold 이동 (S3 put → hot 제거 → OpenSearch 삭제 — 이 순서, S3 성공 확인 전 로컬 삭제 금지)
4. **스냅샷**: knowledge `latest.json` + 타임스탬프 스냅샷 S3 업로드
5. **manifest 갱신** 및 결과 보고(이동 건수, 스냅샷 키, 실패 목록)

## 5. 재수화 (컨테이너는 언제든 죽는다)

컨테이너는 볼륨 없이 뜬다(의도적 비영속). 파생물의 진실성은 **manifest 대조**로 판단:

- `manifest.json`: hot 파일별 `{sha256, record_count, indexed_at}` + 인덱스별 `{last_hydrated_sha}`
- **startup 시**: OpenSearch 인덱스 존재·doc count, Neo4j 노드 count를 manifest와 대조 → 불일치 시 전체 재수화 (episodic: 인덱스 drop 후 bulk / knowledge: `MERGE` 멱등 upsert)
- **요청 진입 시(stat-gate 계승)**: manifest 나이 2초 디바운스 후, dirty 마크 또는 파일 mtime 변화 감지 시 해당 프로젝트만 부분 재수화. 파생물이 죽어 있으면 **쓰기는 hot에 성공시키고 503이 아니라 성과 보고에 `degraded: search unavailable`로 표시** — 읽기 검색만 503
- `POST /v1/reindex`: 강제 전체 재수화(재해 복구), `?verify=true`면 전량 해시 감사

## 6. 문서(document) 정의 — PDF 등 대용량 파일

**문서 1개 = blob(원본 바이트) + document 노드(knowledge) + chunk record들(episodic)**

1. `POST /v1/{ws}/{team}/{proj}/documents` (multipart) → sha256 계산
2. **blob**: S3 `blobs/{sha[:2]}/{sha}` 업로드(즉시 cold) + 로컬 캐시. 동일 sha 재업로드는 멱등
3. **추출**: 서버는 결정적 텍스트 추출만(PDF 텍스트 레이어·md·txt). OCR·요약 없음 — 스캔 PDF처럼 추출 불가면 `extractable: false`로 정직하게 보고하고 에이전트가 처리
4. **청킹**: 추출 텍스트를 ~2KB 청크로 분할, `kind: document_chunk` episode로 저장·색인. 상한 500청크 — 초과분은 잘리고 응답에 `truncated: {total, indexed}` 명시(정직성)
5. **knowledge**: `kind: document` 노드 자동 생성(파일명·sha·요약 없음). 문서에서 배운 사실은 에이전트가 별도 fact 노드로 만들고 `derived_from` 엣지로 문서 노드에 연결
6. 검색은 청크 단위로 히트 → `refs.doc_sha`로 원본 접근(`GET /documents/{sha}` → 로컬 캐시 미스 시 S3에서 재수화)

## 7. HTTP API (go-chi + swaggo)

바인딩 `127.0.0.1:8420`(로컬 도구 — 인증 없음, 외부 바인딩 금지). 모든 응답 `{success, data, error}` 봉투. Swagger UI `/swagger/index.html`, spec `/swagger/doc.json`.

| 그룹 | 엔드포인트 |
|---|---|
| episodic | `POST /v1/{ws}/{team}/{proj}/episodes` · `GET .../episodes/search?q=&from=&to=&kinds=` · `GET .../episodes/{id}` |
| knowledge | `POST .../knowledge/nodes`(supersedes 지원) · `POST .../knowledge/edges` · `GET .../knowledge/search?q=` · `GET .../knowledge/graph?entity=&depth=` · `PATCH .../knowledge/nodes/{id}`(set_state/deprecate) · `DELETE .../knowledge/nodes/{id}?confirm=true`(purge) |
| documents | `POST .../documents` · `GET /v1/documents/{sha}` · `GET /v1/documents/{sha}/chunks` |
| 운영 | `POST /v1/consolidate` · `POST /v1/reindex` · `GET /v1/status` · `GET /healthz` |

recall(검색 응답)은 v1 원칙대로 **발췌+메타+점수 구성요소만** — 본문 전문 주입 없음, `include_archived` 옵트인.

## 8. 인프라

- `deploy/docker-compose.yml`: `opensearch`(커스텀 Dockerfile — `analysis-nori` 플러그인 설치, single-node, security off, **볼륨 없음**) + `neo4j:5-community`(auth 로컬 고정, **볼륨 없음**)
- 서버 자체는 호스트에서 실행(`make run`) — 파생물만 도커
- AWS: profile `vms-holdings`, 버킷 `vms-memory-mcp`(versioning on, public 차단), 리전 `ap-northeast-2`. **주의**: profile의 기본 리전은 ap-southeast-1이므로 S3 클라이언트는 반드시 `DJ_MEMORY_S3_REGION`으로 리전을 명시 설정한다(프로파일 리전 상속 금지 — PermanentRedirect 방지)
- config: env `DJ_MEMORY_HOME`(기본 `~/.local/dj-memory`), `DJ_MEMORY_USERNAME`(기본 OS 사용자), `DJ_MEMORY_S3_BUCKET`(기본 vms-memory-mcp), `DJ_MEMORY_S3_REGION`(기본 ap-northeast-2), `AWS_PROFILE`, `DJ_MEMORY_OPENSEARCH_URL`, `DJ_MEMORY_NEO4J_URL`

## 9. 저장소 구조

```
cmd/memory-mcp/main.go
internal/
  config/       # env 로딩·검증
  ulid/         # v1 포팅
  hotstore/     # JSON 정본 — 원자 쓰기, manifest, 프로젝트 키(ws/team/proj) 검증
  episodic/     # record 도메인·검증
  knowledge/    # 노드·엣지 도메인, 상태기계, supersede
  search/       # OpenSearch 클라이언트 — nori 질의, bulk 재수화
  graph/        # Neo4j 클라이언트 — MERGE upsert, 순회 질의
  blob/         # content-addressed 캐시
  document/     # 추출·청킹
  cold/         # S3 업로드·아카이브 포맷·복원
  consolidate/  # §4 파이프라인
  rehydrate/    # §5 manifest 대조·재수화
  server/       # go-chi 핸들러 + swagger 주석
deploy/         # docker-compose, opensearch Dockerfile
test/blackbox/  # §10 시나리오
```

의존: chi, swaggo/swag+http-swagger, opensearch-go, neo4j-go-driver/v5, aws-sdk-go-v2(s3), pdf 텍스트 추출 lib. 외부 서비스 접근은 전부 인터페이스 뒤에 — 단위 테스트는 fake, 통합·블랙박스는 실물.

## 10. 검증 — 블랙박스 수용 기준

단위 테스트(80%+) 외에, **"저장돼 있어야 할 것이 실제로 다 있는가"**를 검증하는 블랙박스 시나리오(`test/blackbox/`)를 전부 통과할 때까지 수정 루프를 돈다:

1. **기동**: compose up(fresh) → `/healthz` green, swagger doc 서빙
2. **episodic**: 한국어 episode 3건 저장 → hot 파일에 존재 → `"보안을 끄고"` 저장 후 `"보안을 끄는"`으로 검색 히트(nori 형태소)
3. **knowledge**: fact 2건(+supersede 1건) 저장 → Neo4j 순회로 체인 확인 → hot 파일 일치
4. **document**: 텍스트 레이어 있는 PDF 업로드 → blob이 S3에 존재, 청크 검색 히트, document 노드 생성
5. **재수화**: `docker compose down && up` → 데이터 zero인 컨테이너 → `/status`가 drift 감지 → 재수화 후 2·3의 검색·순회 결과 동일
6. **consolidation**: 오래된 consolidated episode fixture → `/consolidate` → S3 `{yyyy-mm}.json` 생성, hot에서 제거, 미통합 건은 잔존, knowledge 스냅샷 S3 존재
7. **저하 모드**: OpenSearch만 내린 상태에서 episode 쓰기 → 성공 + degraded 보고 → 컨테이너 복구 후 재수화로 검색 가능
8. **정직성**: `/status`가 위 모든 단계에서 신선도·미통합·드리프트를 정확히 보고

S3 검증은 실버킷 `s3://vms-memory-mcp/jin/blackbox-test/...` 프리픽스 사용 후 정리.

## 11. 비목표 (v2)

- 인증·멀티유저(로컬 127.0.0.1 전용) · 서버 내 LLM/OCR/임베딩 · MCP stdio 어댑터(HTTP 우선, 어댑터는 후속) · OpenSearch/Neo4j 데이터 볼륨 영속화(의도적 비영속)
