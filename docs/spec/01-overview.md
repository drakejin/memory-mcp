# 01 — 시스템 개요: 이중 평면 메모리

정본(hot JSON) 하나와 파생 인덱스 둘, 콜드 아카이브 하나로 이루어진 memory-mcp의 전체 지도 — 요청 하나가 어디를 지나고, 무엇이 죽어도 되는지.

| 항목 | 내용 |
|---|---|
| 관련 코드 | `cmd/memory-mcp/main.go` · `internal/server/{server,deps,startup,degraded,respond}.go` · `internal/server/apierr` · `internal/errs` · `internal/config/config.go` · `internal/hotstore` · `internal/search` · `internal/graph` · `internal/cold` · `internal/rehydrate` |
| 관련 스펙 | [02-storage-model](02-storage-model.md) · [03-lifecycle](03-lifecycle.md) · [04-episodic-search](04-episodic-search.md) · [05-knowledge-graph](05-knowledge-graph.md) · [06-documents](06-documents.md) · [07-rehydration](07-rehydration.md) · [08-http-api](08-http-api.md) · [09-code-structure](09-code-structure.md) · [10-operations](10-operations.md) · [11-testing](11-testing.md) · 인덱스: [README](README.md) |
| 상태 | 구현됨 — Go 1.25.9, `go-chi/chi/v5`, `opensearch-go/v4`, `neo4j-go-driver/v5`, `aws-sdk-go-v2`. 본 문서는 `docs/design/architecture-v2.md`(무엇을) · `docs/design/code-standards.md`(어떻게)가 아니라 **현재 소스 코드**를 기술한다 |

![memory-mcp 시스템 맵 — 클라이언트에서 chi HTTP 서버를 지나 hot 정본에 원자 쓰기를 하고, 파생 인덱스(OpenSearch·Neo4j)에 best-effort로 upsert하며, 아래에 S3 콜드 아카이브가 놓이고, 재수화 화살표가 hot에서 컨테이너로 올라가는 구조](assets/01-overview.svg)

---

## 1. 문제 — 정본 있는 기억 vs 주장 기억

에이전트 기억은 두 종류다.

- **정본 있는 기억(ground-truth memory)**: 코드 인덱스, 문서 색인처럼 **디스크 어딘가에 원본 파일이 이미 존재**하는 기억. 인덱스가 틀리면 원본을 다시 읽어 고치면 된다. 동기화 문제일 뿐 소유권 문제가 아니다.
- **주장 기억(claim memory)**: "사용자가 X를 선호한다", "Y로 결정했다", "Z를 시도했다가 실패했다". **원본 파일이 존재하지 않는다.** 누군가 정본을 소유하지 않으면, 인덱스가 날아가는 순간 기억 자체가 사라진다.

memory-mcp가 다루는 것은 후자다. 그래서 서버는 검색 엔진을 "저장소"로 쓰지 않고, **자기 손으로 정본을 만들어 로컬 JSON 파일에 쓴다.** OpenSearch와 Neo4j는 그 정본을 조회 가능한 형태로 비춘 **뷰**일 뿐이다.

이 판단이 코드에 직접 박혀 있다 (`internal/hotstore/hotstore.go` 패키지 주석):

> OpenSearch and Neo4j are derived, disposable views; content that exists only in a derived store is a bug.

## 2. 원칙 — 정본 하나, 나머지는 전부 파생물

### 2.1 세 가지 불변 원칙

| # | 원칙 | 코드에서의 실체 |
|---|---|---|
| 1 | **원본이 콘텐츠를 소유한다** | `hotstore.Client`가 유일한 쓰기 정본. `search.Client`·`graph.Client`는 파생. 파생 컨테이너는 볼륨 없이 뜬다(`deploy/docker-compose.yml`) |
| 2 | **판정 없는 자동 덮어쓰기 없음** | 서버 안에 LLM·요약·OCR·임베딩이 없다. supersede 대상 결정은 요청 본문의 `supersedes[]`로 **호출 에이전트가** 지정한다 (`handleCreateNode`). `consolidated=true`도 마찬가지로 추론하지 않고, 에이전트가 `provenance[]`에 적은 episode에만 `promoteProvenance`가 찍는다 |
| 3 | **정직성** | `GET /v1/status`가 `StatusReport{Drift, Unconsolidated, StaleUnconsolidated, ManifestUpdatedAt, DirtyFiles, S3, Degraded}`를 항상 반환. 문서 청킹이 잘리면 `truncated:{total,indexed}`, 텍스트 추출 불가면 `extractable:false` |

### 2.2 쓰기 순서 규칙 (코드에 강제되어 있음)

```
hot 쓰기  ──실패──► apierr.From(Kind) → 4xx/5xx, 요청 실패    (정본이 안 써지면 없던 일)
   │성공
   ├─► 파생 upsert ──실패──► 2xx + degraded[] + manifest dirty   (요청은 성공)
   └─► S3 put ──확인 후에만──► hot 삭제 ──► 파생 삭제      (에이징의 철칙)
```

- `handleCreateEpisode`: `HotStore.AppendEpisode` 실패 → `apierr.From`이 도메인 Kind를 상태 코드로 옮긴다(중복 ULID = `KindConflict` → 409, 파일시스템 실패 = `KindInternal` → 500). 그 뒤의 `EpisodeIndex.IndexRecords` 실패 → **여전히 201**, 응답에 `degraded:["search unavailable"]`, `markDirty(PlaneEpisodic)`.
- `handleCreateNode`: `HotStore.UpdateKnowledge` 실패 → `apierr.From`(`knowledge.Supersede`의 판정을 그대로 옮긴다 — supersede 대상 없음 = `KindNotFound` → 404, 자기 자신을 supersede = `KindInvalid` → 400, 이미 있는 노드 id = `KindConflict` → 409, IO = 500). `mirrorKnowledge`의 Neo4j MERGE 실패 → 201 + `degraded:["graph unavailable"]` + `markDirty(PlaneKnowledge)`.
- `consolidate`의 `ageProject`: `ColdArchiver.ArchiveEpisodes` → `HotStore.RemoveEpisodes` → `EpisodeIndexer.DeleteRecords` 순서를 지킨다. S3 put이 실패하면 hot은 건드리지 않고 `Report.Failures`에만 남긴다. 반대로 hot 삭제가 실패해도 콜드 사본은 이미 존재하므로(안전한 방향) 다음 실행이 멱등하게 재아카이브한다.

## 3. 두 메모리 평면

| | **episodic** | **knowledge** |
|---|---|---|
| 담는 것 | 사건·대화·결정·관찰·문서 청크 — "그때 무슨 일이 있었나" | 개체·사실·교훈·선호·문서 — "지금 무엇이 참인가" |
| 도메인 타입 | `episodic.Record` (`Kind`: `event`\|`conversation`\|`decision`\|`observation`\|`document_chunk`) | `knowledge.Node` / `knowledge.Edge` / `knowledge.Graph` |
| hot 정본 | `episodic/{ws}/{team}/{proj}.json` — Record 배열 | `knowledge/{ws}/{team}/{proj}.json` — `{nodes, edges}` 문서 |
| 파생 저장소 | OpenSearch 인덱스 `search.IndexName` = `dj-memory-episodic` (전 프로젝트 공유, `workspace`/`team`/`project` keyword로 스코프) | Neo4j `:KnowledgeNode` 라벨 + `:REL` 관계 (`rel` 속성에 관계 종류 저장, MERGE 매칭 키는 `(from, to, rel)`) |
| 변경 모델 | append 전용. `UpdateEpisodes`는 `consolidated` 플래그와 recall 통계만 갱신 | 비파괴 개정. `knowledge.Transition`으로 `active ↔ archived ↔ deprecated`, `knowledge.Supersede`로 승계 체인 |
| 수명 | 유한 — `consolidated=true` + TTL 경과 시 S3로 가라앉고 hot·인덱스에서 제거 | 영구 — hot에 항상 상주. 이동 없음, consolidation마다 S3 스냅샷만 |
| 질의 | nori 형태소 한국어 전문검색 + `occurred_at` 범위 + `kinds` 필터 (`search.Query{Text,From,To,Kinds,Size}`) | Cypher 순회: `Neighborhood(ctx,key,entity,depth)`, `SupersedeChain(ctx,key,id)`, lucene fulltext `Search(ctx,key,q,includeArchived)` |
| 전량 재수화 | `Drop` → `EnsureIndex` → `IndexRecords` 벌크 | `Clear` → `UpsertNodes`/`UpsertEdges` MERGE 재생 |
| 부분 재수화 | `EnsureIndex` → `DeleteProject` → `IndexRecords` (그 프로젝트만) | `UpsertNodes`/`UpsertEdges` → `DeleteMissing(keep)` (그 프로젝트만) |

부분 경로가 따로 있는 이유는 하나다. MERGE와 bulk upsert는 **추가·갱신만** 할 수 있어서, hot을 떠난 레코드가 파생 저장소에 영원히 남는다. 그래서 `DeleteProject`/`DeleteMissing`이 프로젝트 범위의 삭제를 담당하고, 다른 프로젝트를 건드리는 `Drop`/`Clear`는 전량 경로에만 쓴다.

### 3.1 두 평면을 잇는 것: provenance

`knowledge.Node.Provenance []string`과 `knowledge.Edge.Provenance []string`은 유래 episode의 **ULID**를 담는다. episode ID는 불변이고 cold로 내려가도 바뀌지 않으므로 링크가 끊기지 않는다.

`GET /v1/{ws}/{team}/{proj}/episodes/{id}`는 이 계약을 지키기 위해 2단 조회를 한다: `HotStore.GetEpisode` → `errors.Is(err, errs.ErrNotFound)`면 `ColdArchive.FetchArchivedEpisode`가 S3 월별 배치를 스캔한다.

문서도 같은 방식으로 두 평면에 걸친다 — 원본 바이트는 blob, 본문은 `document_chunk` episode들, 존재 자체는 `kind: document` knowledge 노드. 자세한 것은 [06-documents](06-documents.md).

## 4. 세 저장 계층

| 계층 | 위치 | 구현 | 수명 / 실패 시 |
|---|---|---|---|
| **hot (정본)** | `~/.local/dj-memory` (env `DJ_MEMORY_HOME`) | `hotstore.Client` — temp+rename 원자 쓰기, fsync 후 rename, 디렉터리 `0700` / 파일 `0600`, 단일 mutex가 "파일 + manifest 항목"을 한 단위로 직렬화 | 영구. 죽으면 서버가 못 쓴다 (`store == nil`이면 hot을 만지는 모든 핸들러가 503 `hot store unavailable`) |
| **derived (파생)** | 도커 컨테이너, 볼륨 없음 | `search.Client`(OpenSearch 2.x + nori), `graph.Client`(Neo4j 5) | 언제든 폐기 가능. 죽으면 쓰기는 degraded로 성공, 검색 읽기만 503 |
| **cold (아카이브)** | `s3://vms-memory-mcp` (`ap-northeast-2`, versioning on) | `cold.Client`(아카이브 포맷·키 레이아웃) 위에 패키지 내부 `objectStore` 인터페이스와 그 S3 구현 `s3Objects`(`internal/cold/s3.go`, 원시 IO) | 백스톱. 죽으면 에이징·스냅샷·문서 ingest가 막힌다 |

세 계층 모두 **exported 인터페이스 + unexported 구현체 + `New(Config) (Client, error)` 하나**의 형태다(code-standards §1). 생성자는 I/O를 하지 않으며, 연결 확인은 `Ping`(또는 `/v1/status`)이 따로 한다.

### 4.1 hot 디스크 레이아웃

```
~/.local/dj-memory/
  episodic/{ws}/{team}/{proj}.json    # []episodic.Record
  knowledge/{ws}/{team}/{proj}.json   # knowledge.Graph {nodes, edges}
  blobs/{sha256}                      # 문서 원본 캐시(평면 배치, 축출 가능)
  manifest.json                       # hotstore.Manifest
```

`hotstore.ProjectKey{Workspace, Team, Project}`의 각 세그먼트는 `^[a-z0-9._-]+$`여야 한다 — 그래야 파일 경로와 S3 키에 그대로 끼워 넣어도 안전하다. 검증은 두 경계에서 각각 한 번씩:

- HTTP 경계 `server.validateProjectKey` — 빈 값, 세그먼트 전체가 `.` 또는 `..`, 패턴 불일치를 400으로 거부.
- 저장소 경계 `hotstore.ProjectKey.Validate` — 같은 패턴에 더해 `..`를 **부분 문자열로 포함**해도 `KindInvalid`로 거부.

### 4.2 manifest.json — 파생물이 진실한지 판정하는 근거

```go
type Manifest struct {
    Files     map[string]FileState  `json:"files"`      // 키: "{plane}/{ws}/{team}/{proj}"
    Indexes   map[string]IndexState `json:"indexes"`    // 키: "opensearch" | "neo4j"
    UpdatedAt time.Time             `json:"updated_at"`
}
type FileState struct {
    SHA256      string    `json:"sha256"`
    RecordCount int       `json:"record_count"`
    IndexedAt   time.Time `json:"indexed_at"`
    Dirty       bool      `json:"dirty"`
}
type IndexState struct {
    LastHydratedSHA string `json:"last_hydrated_sha"`
}
```

`RecordCount`는 **파생 저장소의 단위 개수**다 — episodic 평면은 record 수, knowledge 평면은 **노드 수**(엣지 제외). OpenSearch doc-count / Neo4j node-count와 직접 비교하기 위해서다. `LastHydratedSHA`는 `rehydrate.PlaneStateSHA`가 계산한, 평면 내 모든 파일 해시의 결정적 다이제스트다 — 개수만 같고 내용이 바뀐 경우를 잡는다.

키 문자열을 만드는 헬퍼는 각각 하나뿐이다: 파일 키는 `hotstore.ManifestFileKey(plane, key)`, 인덱스 키는 `rehydrate.IndexKeyFor(plane)`.

### 4.3 cold S3 키 레이아웃 (`internal/cold/keys.go`)

```
{username}/episodic/{ws}/{team}/{proj}/{yyyy-mm}.json     # 월별 배치, id 기준 병합(멱등)
{username}/knowledge/{ws}/{team}/{proj}/latest.json       # consolidation마다 갱신
{username}/knowledge/{ws}/{team}/{proj}/snapshots/{ts}.json  # ts = 20060102T150405Z
{username}/blobs/{sha[:2]}/{sha}                          # content-addressed, ingest 즉시
```

`{username}`은 `DJ_MEMORY_USERNAME` 또는 OS 사용자명. 리전은 **반드시** `DJ_MEMORY_S3_REGION`(기본 `ap-northeast-2`)으로 명시 설정한다 — `AWS_PROFILE`(기본 `vms-holdings`)의 기본 리전을 상속하면 `PermanentRedirect`가 난다.

## 5. 기동 시퀀스 (`cmd/memory-mcp/main.go`)

`main`은 로거를 만들고 `run(logger)`만 호출한다. 실제 조립은 `build(cfg, logger)`에 따로 떼어 놓았는데, 이 seam이 있어야 포트를 열지 않고도 테스트가 전체 객체 그래프를 세워 볼 수 있다.

1. `slog.New(slog.NewTextHandler(os.Stderr, nil))` 로거 생성 후 `slog.SetDefault`. 이후 모든 패키지는 이 로거를 **주입받는다**(전역 참조 금지).
2. `config.Load()` — env 읽기, 기본값 적용, `Validate()`.
3. `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)` — 이 ctx가 graceful shutdown 트리거.
4. `hotstore.NewSystemClock()` → `ulid.New(ulid.Config{Clock: clock})`(프로세스당 하나, 단조 증가 보장) → `hotstore.New(hotstore.Config{Home, Clock})` → `blob.New(blob.Config{Dir: filepath.Join(cfg.Home, "blobs")})`. 어느 것도 디스크를 건드리지 않는다(지연 생성).
5. `search.New(search.Config{URL, Logger})` — 다이얼하지 않는다. 실패 시 `Warn("opensearch client init failed; episodic search degraded")`, `index` 변수는 nil `search.Client`로 남는다.
6. `graph.New(graph.Config{URL, User, Password, Logger})` — 성공 시 `release` 훅에 `client.Close(context.Background())`를 등록(종료 시점엔 서브 ctx가 이미 취소되어 있으므로 새 ctx 사용). 실패 시 `Warn("neo4j client init failed; knowledge graph degraded")`.
7. `cold.New(cold.Config{Bucket, Region, Profile, Username})` — `~/.aws/{config,credentials}`만 읽고 네트워크는 타지 않는다. 실패 시 `Warn("s3 client init failed; cold archive degraded")`.
8. 서비스 조립: `document.New(document.Config{...})`, `consolidate.New(consolidate.Config{...})`, `rehydrate.New(rehydrate.Config{...})`. 셋 다 `(Service, error)`를 반환한다.
9. `server.New(server.Config{...})` — 협력자 중 `Store`/`Index`/`Graph`/`Documents`/`Consolidator`/`Rehydrator`/`Archiver`는 nil이어도 되지만, **`Clock`/`IDs`/`Logger`는 필수**다. nil이면 `Config.validate()`가 `KindInvalid`를 돌려주고 기동이 실패한다(degrade할 수 없는 의존이라 첫 요청에서 panic이 나는 편보다 낫다). 바인드 주소가 loopback이 아니어도 여기서 거부한다.
10. **`srv.Startup(ctx)`** — `Rehydrator.CheckDrift` → 드리프트가 하나라도 있으면 `RehydrateAll(ctx, false)`. 결과는 전부 로그. **절대 fatal이 아니다**: 파생 저장소가 부팅 시점에 죽어 있어도 hot 쓰기는 동작해야 하기 때문.
11. `srv.ListenAndServe(ctx)` — `http.Server{Addr: cfg.ListenAddr, ReadHeaderTimeout: 5s}`. ctx 취소 시 10초(`shutdownGrace`) 타임아웃으로 `Shutdown`. 요청 단위 타임아웃은 일부러 없다 — 큰 문서 ingest는 정당하게 수 초가 걸린다.

기동을 중단시키는 실패는 `run`이 에러를 반환하는 경우뿐이다: `config.Load`, `build`의 어느 생성자, 그리고 `ListenAndServe`. 이때 `main`이 `Error("memory-mcp exited")`를 남기고 `os.Exit(1)`한다. `defer release()`가 먼저 돌기 때문에 bolt 드라이버 커넥션은 항상 정리된다.

**핵심**: 파생 클라이언트 생성 실패는 기동을 막지 않는다. `server.Config`의 저장소 필드는 전부 인터페이스이고 nil이 될 수 있으며, 각 핸들러가 nil을 degraded 또는 503으로 번역한다.

### 5.1 설정 (`internal/config`)

| env | 기본값 | 비고 |
|---|---|---|
| `DJ_MEMORY_HOME` | `~/.local/dj-memory` | hot 루트 |
| `DJ_MEMORY_USERNAME` | OS 사용자명 | 모든 S3 키의 프리픽스 |
| `DJ_MEMORY_S3_BUCKET` | `vms-memory-mcp` | |
| `DJ_MEMORY_S3_REGION` | `ap-northeast-2` | 프로파일 리전 상속 금지 |
| `AWS_PROFILE` | `vms-holdings` | |
| `DJ_MEMORY_OPENSEARCH_URL` | `http://127.0.0.1:9200` | |
| `DJ_MEMORY_NEO4J_URL` | `bolt://127.0.0.1:7687` | 자격증명은 env가 아니라 상수 `config.DefaultNeo4jUser`/`DefaultNeo4jPassword`(`neo4j` / `djmemory-local`) — compose의 `NEO4J_AUTH`와 짝을 이룬 로컬 고정값 |
| `DJ_MEMORY_EPISODIC_TTL_DAYS` | `30` | 양의 정수가 아니면 Load 실패 |
| `DJ_MEMORY_LISTEN_ADDR` | `127.0.0.1:8420` | `127.0.0.1:` 또는 `localhost:` 접두사가 아니면 Load 실패 |

`Config`에는 json 태그가 하나도 없다 — Neo4j 비밀번호를 들고 있어서 응답 본문이나 로그로 직렬화되면 안 되기 때문이다.

결정적 상한(패키지 상수로 고정, 전 패키지가 이 값 하나를 공유):

| 상수 | 값 | 쓰이는 곳 |
|---|---|---|
| `config.MaxProjectFileBytes` | 5 MiB | episodic 압박 에이징 |
| `config.MaxProjectRecords` | 5000 | episodic 압박 에이징 |
| `config.DocumentChunkBytes` | 2048 | 문서 청크 목표 크기 |
| `config.MaxDocumentChunks` | 500 | 청크 상한(초과분은 `truncated`로 보고) |
| `search.DefaultSearchSize` | 20 | 검색 히트 상한(`Query.Size` 미지정 시) |
| `graph.searchLimit` | 50 | knowledge fulltext 상한 |
| `graph.maxDepth` | 10 | Cypher 순회 깊이 클램프(기본 1) |
| `server.maxGraphDepth` | 10 | `depth` 쿼리 파라미터 상한 — 범위를 벗어나면 클램프가 아니라 400 |
| `server.maxJSONBodyBytes` | 1 MiB | JSON 요청 본문 |
| `server.maxUploadBytes` | 128 MiB | multipart 문서 업로드 |
| `rehydrate.debounceInterval` | 2s | 요청 진입 stat-gate 디바운스(패키지 비공개) |

## 6. 요청 흐름 end-to-end

라우터(`server.Router()`)가 마운트하는 전부:

```
GET  /healthz                          GET  /swagger/*
GET  /v1/status                        POST /v1/consolidate      POST /v1/reindex
GET  /v1/documents/{sha}               GET  /v1/documents/{sha}/chunks
POST /v1/{ws}/{team}/{proj}/episodes           GET .../episodes/search   GET .../episodes/{id}
POST .../knowledge/nodes   PATCH .../knowledge/nodes/{id}   DELETE .../knowledge/nodes/{id}
POST .../knowledge/edges   GET .../knowledge/search          GET .../knowledge/graph
POST .../documents
```

미들웨어는 의도적으로 하나도 없다 — 관측은 핸들러 안의 `slog` 호출이 담당한다.

모든 응답은 `server.Envelope{success, data, error}` 봉투다. `error`는 자유 문장이 아니라 `apierr.Error`가 직렬화된 `{code, message, details?}` 객체이고, 성공 응답에서는 생략된다(`data`는 실패 시에도 `null`로 남는다). 예외는 단 하나 — `GET /v1/documents/{sha}`는 `application/octet-stream`으로 원시 바이트를 스트리밍한다.

### 6.1 쓰기 — `POST /v1/{ws}/{team}/{proj}/episodes`

1. `validateProjectKey` (400) → `store == nil` 확인 (503) → `decodeJSON`(1 MiB 상한, 400) → `CreateEpisodeRequest.validate()` (400, `episodic.ValidKind`/`ValidActor`로 어휘 검증).
2. `Clock.Now().UTC()`로 시각을 잡고 `IDGenerator.GenerateAt(now.UnixMilli())`로 ULID 발급. `occurred_at`이 비어 있으면 now. `consolidated`는 항상 `false`로 시작 — 클라이언트가 지정할 수 없다.
3. **`HotStore.AppendEpisode`** — 실패하면 `apierr.From(err)`. 여기가 정본 경계다.
4. `index == nil`이거나 `EpisodeIndex.IndexRecords`가 실패하면 → `degraded: ["search unavailable"]` + `markDirty`. 성공하면 `markIndexed`(dirty 해제, `IndexedAt` 갱신, 평면 `LastHydratedSHA` 재계산).
5. `201` + `CreateEpisodeResponse{record, degraded?}`.

### 6.2 읽기 — `GET .../episodes/search?q=&from=&to=&kinds=`

1. 파라미터 검증(`q` 필수, `from`/`to`는 RFC3339, `kinds`는 `episodic.ValidKind`가 아는 값) → 400.
2. `index == nil`이면 **503 `search unavailable`** (쓰기와 달리 읽기는 degraded로 넘어가지 않는다).
3. **stat-gate**: `Rehydrator.StatGate(ctx, key)` — 프로젝트별 2초 디바운스 후, manifest의 dirty 플래그·파일 mtime > `IndexedAt`·미추적 파일이면(hot이 변했나) 또는 파생 저장소의 문서 수가 manifest보다 적으면(파생이 사라졌나) 해당 프로젝트만 부분 재수화. 실패해도 요청은 진행한다(경고 로그만).
4. `EpisodeIndex.Search` → `errors.Is(err, errs.ErrUnavailable)`이면 503(문구는 degraded와 동일한 `search unavailable`), 그 외 에러는 `apierr.From`.
5. `bumpRecall` — 히트한 record들의 `recall_count`/`last_recalled`를 hot에 best-effort 반영하고, 이어서 `convergeEpisodes`가 그 레코드들을 인덱스에 다시 밀어 넣는다. `recall_count`·`last_recalled`·`consolidated`는 매핑된 인덱스 필드라, hot만 고치면 파생이 즉시 낡고 드리프트가 영구히 켜진다. 실패는 로그 + dirty 마킹뿐(검색 결과 자체는 이미 옳다).
6. `200` + `[]search.Hit{record, score, excerpt}`. 발췌·메타·점수만 나가고 본문 전문을 통째로 주입하지 않는다.

### 6.3 운영 — `/v1/status`, `/v1/consolidate`, `/v1/reindex`

- **`GET /v1/status`**: `CheckDrift`(라이브 대조) + manifest의 dirty 파일 목록 + 전 프로젝트 스캔으로 얻은 `Unconsolidated`/`StaleUnconsolidated` + S3 도달 가능성 + 마지막 아카이브/스냅샷 시각. S3 프로브는 `FetchBlob(sha256(""))`이다 — HEAD가 아니라 **GET**인 이유는, S3가 없는 버킷에 대한 `HeadObject`를 "키 없음"과 구분 불가능한 404로 답하기 때문. 성공이거나 `errs.ErrNotFound`면 도달 가능으로 본다. 어느 하위 조회가 실패해도 `Degraded[]`에 적고 200을 반환한다.
- **`POST /v1/consolidate`**: `consolidate.Service.Run` — 증류 후보 제안 → entity 통계 → 에이징(§2.2 순서) → knowledge 스냅샷 → manifest 갱신. `dry_run`이면 S3 업로드·hot 삭제·인덱스 삭제·스냅샷을 전부 건너뛰고 "무엇이 움직였을지"만 센다. 성공 시 `lastArchiveAt`/`lastSnapshotAt`을 갱신(`statusMu`로 보호).
- **`POST /v1/reindex?verify=true`**: `RehydrateAll` — episodic은 `Drop`+`EnsureIndex`+벌크, knowledge는 `Clear`+MERGE 재생. `verify`면 프로젝트별 doc-count/node-count를 hot과 대조하는 감사까지 수행. 부분 실패는 `Report.Failures[]`로 전부 노출한다.

## 7. 실패의 두 얼굴 — 에러 2계층과 degraded

### 7.1 `internal/errs` → `internal/server/apierr`

핸들러 아래는 HTTP를 모른다. 대신 모든 계층이 **의미 에러** `*errs.Error`를 반환하고, 전송 계층이 그것을 **정확히 한 곳에서** 상태 코드로 번역한다 (code-standards §2).

```
hotstore / search / graph / cold / blob / episodic / knowledge
document / consolidate / rehydrate / ulid / config     ──►  *errs.Error
                                                              │ apierr.From(err)
                                                    handler ──►  *apierr.Error
```

`errs.Error`는 `Kind` · `Op`(`"hotstore.AppendEpisode"` 같은 읽히는 경로) · `Entity` · `ID` · `Msg` · `Fields`를 들고 다닌다. 비교는 항상 센티넬로 한다 — `errors.Is(err, errs.ErrNotFound)`, `errs.ErrUnavailable` 등 다섯 개. Kind 문자열 직접 비교는 금지다. `LogValue()`가 slog 그룹을 만들어 주므로 `Fields`는 로그에만 흐르고 응답 본문에는 절대 나가지 않는다.

`apierr.From`이 유일한 매핑 지점이다:

| domain `Kind` | HTTP | `code` |
|---|---|---|
| `KindInvalid` | 400 | `invalid_request` |
| `KindNotFound` | 404 | `not_found` |
| `KindConflict` | 409 | `conflict` |
| `KindUnavailable` | 503 | `unavailable` |
| `KindInternal` / 미분류 | 500 | `internal` |

- 핸들러가 직접 만드는 에러는 **전송 계층 고유의 실패**뿐이다: `badRequest` / `notFound` / `unavailable` 세 생성자(`respond.go`)가 그것이고, 전부 `apierr.New`를 감싼다.
- 응답 본문에는 `Code`·`Message`·`Details`만 나간다. `cause`는 `writeAPIError`가 `log.Error("request failed", ...)`로 한 번만 남긴다 — 도메인 원인은 이미 `LogValue`로 구조화되어 있으므로 핸들러가 중복 로깅하지 않는다.
- `apierr.From`은 이미 전송 에러인 것을 그대로 통과시킨다. 그래서 `UpdateKnowledge` 클로저 안에서 반환한 `notFound("edge endpoint node not found")` 같은 값이 hotstore를 거쳐 나와도 404를 유지한다.
- `Message`는 `publicMessage`가 체인을 **바깥에서 안으로 훑어 처음 만나는 저자 있는 메시지**로 정한다. `errs.Wrap`은 자기 메시지를 붙이지 않으므로 결과적으로 실패를 실제로 인지한 가장 안쪽 지점의 문구가 클라이언트에게 간다(정본: [08 §2.1](08-http-api.md)).

### 7.2 degraded — 실패가 아닌 상태

`internal/server/degraded.go`에 문구가 상수로 고정되어 있다.

| 상수 | 문구 | 언제 |
|---|---|---|
| `degradedSearch` | `search unavailable` | OpenSearch 미설정/도달 불가 |
| `degradedGraph` | `graph unavailable` | Neo4j 미설정/도달 불가 |
| `degradedCold` | `cold storage unavailable` | S3 미설정/도달 불가 |
| `degradedPromotion` | `provenance episodes not marked consolidated` | 노드는 써졌지만 provenance episode에 `consolidated`를 못 찍음 |

협력자 자체가 조립되지 않은 경우는 degraded가 아니라 503이다 — 정직한 부분 응답이 존재하지 않기 때문: `msgHotStoreUnavailable`, `msgDocumentsUnavailable`, `msgConsolidatorUnavailable`, `msgRehydratorUnavailable`.

규칙:

- **쓰기 + 파생 실패 = 성공.** 2xx + `data.degraded[]` + manifest dirty. 다음 재수화가 수렴시킨다.
- **읽기 + 파생 실패 = 503.** 검색·그래프 순회는 파생물 없이는 답이 없으므로 정직하게 거절한다. 이때 503 본문의 문구는 쓰기가 달았을 degraded 노트와 **글자 그대로 같다** — 클라이언트가 하나의 어휘만 읽으면 되도록.
- **예외 — 문서 ingest는 콜드 우선.** `handleIngestDocument`는 `archiver == nil`이면 **503 `cold storage unavailable`**을 낸다. blob은 S3가 정본이고 로컬 `blobs/`는 축출 가능한 캐시이므로, 콜드 사본 없는 ingest는 계약 위반이다.

## 8. 비목표

- **인증·멀티유저** — `config.Validate()`와 `server.Config.validate()`가 각각 loopback 바인딩을 강제한다. 인증 미들웨어도, 사용자 개념도 없다(`{username}`은 S3 키 프리픽스일 뿐).
- **서버 내 LLM / OCR / 임베딩 / 요약** — 문서 추출은 결정적 텍스트 레이어 추출뿐(`document.Config.Extractor`가 nil이면 내장 `textExtractor`가 선택된다). 추출 불가면 `extractable:false`로 정직하게 보고하고 끝낸다. 증류(episode → knowledge)는 에이전트가 API를 호출해서 한다.
- **MCP stdio 어댑터** — 현재 바이너리는 `cmd/memory-mcp` 하나이고 HTTP만 제공한다. 레포 루트의 `src/`(TypeScript v1)와 `package.json`은 역사적 잔존물이며 Go 서버와 연결되어 있지 않다.
- **파생 저장소 영속화** — `deploy/docker-compose.yml`의 두 컨테이너에는 볼륨이 없다. 의도된 설계이며, 이것이 재수화 경로가 상시 검증되는 이유다.
- **자동 삭제** — 미통합(unconsolidated) episode는 나이와 무관하게 hot에 남는다. `consolidate.ageEligible`은 `Consolidated == false`인 record를 압박 상황에서도 후보에 넣지 않는다. 대신 `/v1/status`가 `stale_unconsolidated`로 영원히 노출한다.

## 9. ⚠️ 설계 문서와 차이

1. **버킷 이름 표기.** `architecture-v2.md` §0 스택 행은 S3를 `vms-holdings`로 적었지만, `vms-holdings`는 **AWS 프로파일 이름**이고 버킷은 `vms-memory-mcp`다(같은 문서 §1 다이어그램·§8과는 일치). 코드 기본값이 정답: `config.DefaultS3Bucket = "vms-memory-mcp"`, `config.DefaultAWSProfile = "vms-holdings"`.
2. **env 목록 차이.** 설계 §8의 env 열거에 없는 `DJ_MEMORY_LISTEN_ADDR`가 코드에 있고 loopback 검증까지 붙어 있다. (Neo4j 자격증명이 env가 아닌 것은 차이가 아니다 — 설계 §8도 "auth 로컬 고정"이라고 못박았고, 코드는 그것을 `config.DefaultNeo4jUser`/`DefaultNeo4jPassword` 상수로 구현했다.)
3. **`search.Client` 인터페이스 시그니처.** `code-standards.md` §1의 예시 코드는 `EnsureIndex(ctx, key)` / `Upsert(ctx, key, recs)`로 적혀 있지만, 실제 인덱스는 전 프로젝트가 공유하는 단일 인덱스이므로 `EnsureIndex(ctx)`이고 upsert 메서드 이름은 `IndexRecords`다. 예시가 규약이 아니라 형태(Client/client 쌍 + 단일 `New(Config)`)가 규약이고, 그 형태는 지켜져 있다.

## 10. 코드 위치

| 개념 | 파일 |
|---|---|
| 기동·의존성 조립 | `cmd/memory-mcp/main.go` |
| 설정·상수·loopback 강제 | `internal/config/config.go` |
| 도메인 에러(Kind·센티넬·`Wrap`/`IO`/`FromContext`) | `internal/errs/errs.go` |
| 전송 에러·Kind→HTTP 매핑 | `internal/server/apierr/apierr.go` |
| 라우트 테이블·`Config`·graceful shutdown | `internal/server/server.go` |
| 서버가 요구하는 narrow interface 모음 | `internal/server/deps.go` |
| 부팅 시 드리프트 대조 → 재수화 | `internal/server/startup.go` |
| degraded 상수·manifest 마킹·stat-gate | `internal/server/degraded.go` |
| `{success,data,error}` 봉투·`badRequest`/`notFound`/`unavailable` | `internal/server/respond.go` |
| 경로/본문 검증·요청 상한 | `internal/server/validate.go` |
| episodic 핸들러(쓰기·검색·단건·recall 수렴) | `internal/server/handlers_episodic.go` |
| knowledge 핸들러(노드·엣지·순회·purge·provenance 승격) | `internal/server/handlers_knowledge.go` |
| 핸들러가 쓰는 순수 그래프 대수·엣지 검증 | `internal/server/knowledge_graph.go` |
| documents 핸들러(ingest·원본·청크) | `internal/server/handlers_documents.go` |
| status·consolidate·reindex 핸들러 | `internal/server/handlers_ops.go` |
| hot 정본 계약·`Config`·`New` | `internal/hotstore/hotstore.go` |
| 주입되는 `Clock`·`NewSystemClock` | `internal/hotstore/clock.go` |
| `ProjectKey`·`Plane`·`ManifestFileKey` | `internal/hotstore/key.go` |
| 원자 쓰기·경로·`ListProjects`·`FileInfo` | `internal/hotstore/file.go` |
| `Manifest`/`FileState`/`IndexState`·dirty 마킹 | `internal/hotstore/manifest.go` |
| episodic 파일 연산(append·조회·갱신·삭제) | `internal/hotstore/episode.go` |
| knowledge 문서 연산(read·update) | `internal/hotstore/knowledge.go` |
| episodic 도메인·어휘(`ValidKind`/`ValidActor`)·검증 | `internal/episodic/record.go` |
| knowledge 도메인·어휘(`ValidNodeKind`/`ValidState`/`ValidTrust`/`ValidRel`) | `internal/knowledge/knowledge.go` |
| knowledge 상태기계(`Transition`·`Supersede`·`Purge`) | `internal/knowledge/lifecycle.go` |
| OpenSearch 계약·`Client`/`client` | `internal/search/search.go` |
| OpenSearch 저수준 라운드트립·URL 조립 | `internal/search/transport.go` |
| 인덱스 생성·수복(`EnsureIndex`·`Drop`) | `internal/search/index.go` |
| bulk upsert·삭제 | `internal/search/bulk.go` |
| 질의 조립·발췌·doc count | `internal/search/query.go` |
| nori 인덱스 매핑·`IndexName` | `internal/search/mapping.go` |
| Neo4j 계약·스키마·`Client`/`client` | `internal/graph/graph.go` |
| bolt 드라이버 어댑터(`runner`) | `internal/graph/bolt.go` |
| Cypher(upsert·검색·순회·supersede 체인) | `internal/graph/queries.go` |
| 드라이버 값 ↔ 도메인 변환·depth 클램프 | `internal/graph/convert.go` |
| 콜드 아카이브 계약·`Client`/`client` | `internal/cold/cold.go` |
| S3 원시 IO(`objectStore` 구현) | `internal/cold/s3.go` |
| 아카이브 포맷(월별 병합·스냅샷·blob) | `internal/cold/archive.go` |
| S3 키 레이아웃 | `internal/cold/keys.go` |
| blob 로컬 캐시·`ValidSHA` | `internal/blob/blob.go` |
| 문서 파이프라인 계약·`Service`/`service` | `internal/document/document.go` |
| 결정적 텍스트 추출(PDF/markdown/text) | `internal/document/extract.go` |
| 청킹·`Truncation` | `internal/document/chunk.go` |
| ingest 파이프라인(blob→청크→노드) | `internal/document/ingest.go` |
| 원본·청크 조회(캐시 미스 시 cold) | `internal/document/read.go` |
| consolidation 계약·`Run` | `internal/consolidate/consolidate.go` |
| 에이징 규칙(`ageEligible`·압박 쿼터) | `internal/consolidate/age.go` |
| 증류 후보 클러스터링·entity 통계 | `internal/consolidate/cluster.go` |
| `Report`/`Options`·실패 문자열 어휘 | `internal/consolidate/report.go` |
| 드리프트 판정·`Service` 계약 | `internal/rehydrate/rehydrate.go` |
| 전량/부분 재수화·stat-gate | `internal/rehydrate/hydrate.go` |
| 평면 다이제스트·manifest 커밋·`IndexKeyFor` | `internal/rehydrate/manifest.go` |
| ULID(`Client`/`client`·`Valid`) | `internal/ulid/ulid.go` |
| 파생 컨테이너(볼륨 없음) | `deploy/docker-compose.yml`, `deploy/opensearch/Dockerfile` |
| 실행·검증 타깃 | `Makefile` (`make start` / `make test` / `make blackbox` / `make swagger`) |
| 블랙박스 수용 시나리오 | `test/blackbox/` |
