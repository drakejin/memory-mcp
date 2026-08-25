# 09 — 코드 구조: 패키지 경계·인터페이스·에러 계층

`internal/` 15개 패키지가 어떤 단일 책임을 지고, 어느 방향으로만 의존하며, 에러가 어느 한 지점에서 도메인 의미(`errs.Kind`)에서 HTTP 전송(`apierr.Error`)으로 바뀌는지.

| 항목 | 내용 |
|---|---|
| 관련 코드 | `cmd/memory-mcp/main.go` · `internal/{blob,cold,config,consolidate,document,episodic,errs,graph,hotstore,knowledge,rehydrate,search,server,server/apierr,ulid}` |
| 관련 스펙 | [01-overview](01-overview.md) · [02-storage-model](02-storage-model.md) · [07-rehydration](07-rehydration.md) · [08-http-api](08-http-api.md) · [10-operations](10-operations.md) · [11-testing](11-testing.md) · 인덱스: [README](README.md) |
| 상태 | **구현됨.** 계층·`Client`/`client` 쌍·단일 `New(Config)`·narrow interface·주입·2계층 에러(`internal/errs` / `internal/server/apierr`) 전부 코드에 있다 |

![패키지 계층과 에러 타입 경계](assets/09-code-structure.svg)

---

## 1. 패키지 지도 — 단일 책임과 의존 방향

`go.mod` 모듈 경로는 `github.com/drakejin/memory-mcp`, Go 1.25.9. 아래 "internal 의존"은 테스트 파일을 제외한 실제 import 목록이다(`go list -f '{{.Imports}}'` 기준).

| 계층 | 패키지 | 단일 책임 | internal 의존 |
|---|---|---|---|
| L6 조립 | `cmd/memory-mcp` | 구현체 선택·배선·시그널 처리. 유일한 composition root | blob, cold, config, consolidate, document, graph, hotstore, rehydrate, search, server, ulid, docs |
| L5 전송 | `internal/server` | chi 라우팅, 요청 검증, HTTP status, `{success,data,error}` 봉투, degraded 표기 | blob, consolidate, document, episodic, errs, hotstore, knowledge, rehydrate, search, server/apierr, ulid |
| L5 전송 | `internal/server/apierr` | 전송 에러 타입 — `Status`/`Code`/`Message`/`Details`, `Kind`→HTTP 고정 매핑 | errs |
| L4 서비스 | `internal/consolidate` | §4 파이프라인 — 증류 후보 클러스터링, entity 통계, 에이징, 스냅샷 | cold, config, episodic, errs, hotstore, knowledge |
| L4 서비스 | `internal/document` | §6 인제스트 — sha256, blob cold-first 업로드, 결정적 추출, 청킹, 문서 노드 | config, episodic, errs, hotstore, knowledge |
| L4 서비스 | `internal/rehydrate` | §5 드리프트 판정, 전체/부분 재수화, stat-gate 디바운스 | episodic, errs, hotstore, knowledge |
| L3 어댑터 | `internal/search` | OpenSearch — nori 질의, bulk 색인, doc count, 인덱스 drop | episodic, errs, hotstore |
| L3 어댑터 | `internal/graph` | Neo4j — MERGE upsert, 이웃 순회, supersede 체인, node count | errs, hotstore, knowledge |
| L3 어댑터 | `internal/cold` | S3 — 오브젝트 IO + 아카이브 포맷·키 레이아웃 (`Client` 하나) | episodic, errs, hotstore, knowledge |
| L3 어댑터 | `internal/blob` | 로컬 content-addressed 캐시 `{home}/blobs/{sha256}` | errs |
| L2 정본 | `internal/hotstore` | 정본 JSON 저장소 — 원자 쓰기, manifest, `ProjectKey` 검증, `Clock` 선언 | episodic, errs, knowledge |
| L1 도메인 | `internal/episodic` | episodic record 타입·어휘(`ValidKind`/`ValidActor`)·검증 | errs, ulid |
| L1 도메인 | `internal/knowledge` | 노드·엣지 타입, 어휘 4종, 상태기계(`Transition`), `Supersede`, `Purge` | errs, ulid |
| L0 기반 | `internal/ulid` | Crockford base32 ULID 생성(`Client`)·검증(`Valid`) | errs |
| L0 기반 | `internal/config` | env 로딩·검증 + 스펙 상수(`MaxDocumentChunks`, `MaxProjectRecords`, …) | errs |
| L0 기반 | `internal/errs` | 도메인 에러 어휘 — `Kind`, `*Error`, 센티널 5종, 생성자 | **없음** |

### 1.1 의존 규칙

1. **아래로만 흐른다.** L(n)은 L(n-1) 이하만 import한다. 위 표에 계층 역행이 없다 = 순환이 없다.
2. **`errs`가 최하층이다.** internal import가 0인 유일한 패키지이고, 그 대신 나머지 14개 패키지 전부가 이것을 import한다. 에러 어휘를 공유하기 때문에 계층 역행 없이 모든 층이 같은 `Kind`로 말할 수 있다.
3. **어댑터끼리는 서로 모른다.** `search`↔`graph`↔`cold`↔`blob` 사이에 import가 없다. 두 어댑터를 함께 쓰는 조합은 L4 서비스나 L5에서만 일어난다.
4. **서비스끼리도 서로 모른다.** `consolidate`↔`document`↔`rehydrate` 사이에 import가 없다.
5. **서비스는 어댑터의 *클라이언트*를 import하지 않는다.** narrow interface(§3) 덕분에 `document`·`rehydrate`는 `blob`/`cold`/`search`/`graph`를 아예 import하지 않는다. 남은 하나는 `consolidate` → `cold` 뿐이고, 그것도 `cold.Client`가 아니라 순수 키 헬퍼 `cold.ArchiveMonth`를 쓰기 위한 것이다 (`internal/consolidate/consolidate.go:236`).
6. **`hotstore`가 주소 허브다.** L3 이상 전부(`blob` 제외)가 `hotstore.ProjectKey`를 공유하고, L4 이상은 `Plane`·`Manifest`까지 쓴다. `search`·`graph`·`cold`가 hotstore를 import하는 이유는 **오직 `ProjectKey` 하나**다. `blob`은 sha256으로만 주소를 잡으므로 프로젝트 키를 몰라도 된다 — internal 의존이 `errs` 하나뿐인 이유다. 시계는 다르다: `hotstore.Clock`은 *선언*만 여기 있고 `Config.Clock` 필드를 가진 패키지는 6개뿐이며(§4.1) 그중 어댑터는 없다.
7. **`server`는 `config`를 import하지 않는다.** 전송 계층은 자기 `server.Config`를 갖고, `config.Config` → `server.Config` 매핑은 조립 루트(`main.go:193-207`)에서만 일어난다.

검증 방법:

```bash
go list -deps ./internal/... >/dev/null   # 순환이면 컴파일 자체가 실패한다
go vet ./...
```

Go 컴파일러가 패키지 순환을 금지하므로 `make build`가 통과하는 한 §1.1-1은 자동 보장된다. 계층 역행(예: `hotstore`가 `search`를 import)은 컴파일은 되지만 규약 위반이므로 리뷰에서 잡는다.

---

## 2. Interface / 구현체 규약 — 코드의 실제 모습

[code-standards.md §1](../design/code-standards.md)이 요구하는 **exported 인터페이스 + unexported 구현체 + 단일 `New(cfg Config)`** 형태가 전 패키지에 적용돼 있다. 외부 세계(OpenSearch·Neo4j·S3·파일시스템·시계·엔트로피)에 닿는 패키지는 `Client`, 그 위에서 조합만 하는 파이프라인은 `Service`를 쓴다.

| 패키지 | 인터페이스 | 구현체 | 생성자 | 계약 체크 |
|---|---|---|---|---|
| `hotstore` | `Client` (12 메서드) | `client` | `New(Config) (Client, error)` :136 | `var _ Client = (*client)(nil)` :132 |
| `hotstore` | `Clock` | `systemClock` | `NewSystemClock() Clock` (clock.go:19) | — |
| `search` | `Client` (8) | `client` | `New(Config) (Client, error)` :135 | :132 |
| `graph` | `Client` (11) | `client` | `New(Config) (Client, error)` :120 | :117 |
| `cold` | `Client` (5) | `client` | `New(Config) (Client, error)` :113 | :108 |
| `blob` | `Client` (4) | `client` | `New(Config) (Client, error)` :95 | :92 |
| `ulid` | `Client` (2) | `client` | `New(Config) (Client, error)` :97 | :94 |
| `document` | `Service` (3) | `service` | `New(Config) (Service, error)` :187 | :184 |
| `document` | `Extractor` | `textExtractor` (값 타입) | — (`Config.Extractor` nil이면 기본값) | `var _ Extractor = textExtractor{}` extract.go:50 |
| `consolidate` | `Service` (1) | `service` | `New(Config) (Service, error)` :126 | :123 |
| `rehydrate` | `Service` (4) | `service` | `New(Config) (Service, error)` :194 | :190 |
| `server` | — | `*Server` | `New(Config) (*Server, error)` :139 | — |

지켜지고 있는 부분:

- **생성자가 단 하나이고 인자는 `Config` struct 하나다.** 위치 인자·다중 생성 경로가 남아 있는 곳이 없다. 인덱스 이름 같은 테스트 전용 노브는 `Config`를 넓히지 않고 패키지 내부 `newClient(cfg, index)` seam으로 처리한다 (`internal/search/search.go:145`, `internal/cold/cold.go:134`).
- **생성자는 네트워크 I/O를 하지 않는다.** `search.New`/`graph.New`는 다이얼하지 않고 클라이언트 객체만 만들며, 연결 확인은 `Ping(ctx)`다. `hotstore.New`/`blob.New`는 디스크를 건드리지 않고 디렉터리는 첫 쓰기에 지연 생성된다.
- **구현체가 unexported라서 외부에서 필드를 만질 경로가 없다.** `graph.Client.Close(ctx)`처럼 수명 관리 메서드도 인터페이스에 포함시켜(`internal/graph/graph.go:57`) `main.go`가 구현체를 알 필요를 없앴다.
- **`Config.Validate()`가 생성 실패를 `errs.Invalid`로 반환한다.** `hotstore`(:110), `blob`(:77), `ulid`(:73), `consolidate`(:98), `rehydrate`(:164), `config`(:159)는 exported `Validate`, `cold`(:139)·`server`(:88)는 unexported `validate`. 어느 쪽도 패닉하지 않는다.
- **테스트 fake가 같은 인터페이스를 구현한다.** 프로덕션 코드에 테스트 분기가 없다 (`internal/server/fakes_test.go`, `internal/document/fakes_test.go`).
- **파생 저장소가 nil이어도 부팅된다.** `main.go`는 `search.New`/`graph.New`/`cold.New` 실패를 `logger.Warn`으로만 처리하고 인터페이스 타입 변수를 nil로 남긴다 (`cmd/memory-mcp/main.go:112-153`). degraded 판단은 핸들러·`rehydrate`·`consolidate`·`document`가 각자 nil 체크로 수행한다.

### 2.1 ⚠️ 설계 문서와 차이

리팩터 이후 규약과 코드가 아직 어긋나 있는 지점은 아래 셋뿐이다. 앞의 둘은 **규약의 예시 코드가 실제 스펙보다 좁게 쓰인** 경우이고, 마지막 하나만 코드가 규칙에서 의도적으로 벗어난 경우다.

| 규약 문구 | 실제 코드 | 판단 |
|---|---|---|
| §1 rule 1: 이름은 `Client`/`client`로 통일 | 오케스트레이션 3종은 `Service`/`service` (`document`, `consolidate`, `rehydrate`) | §1.1의 규약 자체 예시가 `consolidate`를 `type Service struct`로 쓰므로 규약 내부에서 이미 갈린다. 코드는 "외부 시스템에 닿으면 `Client`, 조합만 하면 `Service`"로 일관되게 갈랐다 |
| §1 예시 `EnsureIndex(ctx, key)`·`Upsert`, §1.1 예시 `EpisodeIndexer.Delete`·`ColdArchiver.PutEpisodeBatch` | `search.Client.EnsureIndex(ctx)`(인자 없음)·`IndexRecords`, `consolidate.EpisodeIndexer.DeleteRecords`, `ColdArchiver.ArchiveEpisodes`/`SnapshotKnowledge` | 규약의 코드 블록은 형태를 보여주는 스케치이지 시그니처 정본이 아니다. 특히 `EnsureIndex`: 인덱스는 `IndexName = "dj-memory-episodic"` 하나이고 프로젝트 스코프는 `workspace`/`team`/`project` keyword 필드로 잡으므로(`internal/search/mapping.go`) 프로젝트별 인덱스 전제 자체가 스펙과 다르다 |
| §1 rule 4: 생성자는 I/O를 하지 않는다 | `cold.New`가 `awsconfig.LoadDefaultConfig`로 `~/.aws/{config,credentials}`를 읽는다 (`internal/cold/cold.go:123`) | 네트워크는 건드리지 않는다. 프로필 부재를 배선 시점에 `errs.Unavailable`로 알리기 위한 의도적 선택이고 주석에 근거가 있다 |

`hotstore`에 `New`와 `NewSystemClock` 두 생성자가 있는 것은 규약 위반이 아니다 — 서로 다른 타입(`Client`와 `Clock`)의 생성자이고, `Clock`은 상태 없는 값 타입이라 `Config`가 필요 없다.

---

## 3. 소비자 측 narrow interface (duck typing)

인터페이스는 **쓰는 쪽이 필요한 만큼만** 다시 선언한다. 제공자 패키지의 넓은 `Client`/`Service`를 그대로 의존하는 곳은 없다. 소비자가 다시 선언한 narrow interface는 **25개**다 — `consolidate` 4 · `document` 7 · `rehydrate` 4 · `server` 9 · `ulid` 1(`Clock`). 실측:

| 소비자 | 선언 위치 | 제공자 넓은 인터페이스 | 소비자가 요구하는 메서드 |
|---|---|---|---|
| `consolidate.HotStore` | consolidate.go:40 | `hotstore.Client` 12 | **7** |
| `consolidate.EpisodeIndexer` | consolidate.go:52 | `search.Client` 8 | **1** (`DeleteRecords`) |
| `consolidate.ColdArchiver` | consolidate.go:58 | `cold.Client` 5 | **2** |
| `document.HotStore` | document.go:93 | `hotstore.Client` 12 | **5** |
| `document.BlobCache` | document.go:102 | `blob.Client` 4 | **3** |
| `document.BlobArchiver` | document.go:115 | `cold.Client` 5 | **2** |
| `document.RecordIndexer` | document.go:122 | `search.Client` 8 | **1** (`IndexRecords`) |
| `document.NodeUpserter` | document.go:128 | `graph.Client` 11 | **1** (`UpsertNodes`) |
| `document.IDGenerator` | document.go:141 | `ulid.Client` 2 | **1** (`GenerateAt`) |
| `rehydrate.HotStore` | rehydrate.go:57 | `hotstore.Client` 12 | **6** |
| `rehydrate.EpisodeIndex` | rehydrate.go:69 | `search.Client` 8 | **6** |
| `rehydrate.KnowledgeGraph` | rehydrate.go:81 | `graph.Client` 11 | **6** |
| `server.HotStore` | deps.go:39 | `hotstore.Client` 12 | **10** |
| `server.EpisodeIndex` | deps.go:54 | `search.Client` 8 | **2** |
| `server.KnowledgeGraph` | deps.go:60 | `graph.Client` 11 | **5** |
| `server.ColdArchive` | deps.go:92 | `cold.Client` 5 | **2** |
| `server.IDGenerator` | deps.go:33 | `ulid.Client` 2 | **1** |
| `server.DocumentIngestor` | deps.go:70 | `document.Service` 3 | **3** |
| `server.Consolidator` | deps.go:77 | `consolidate.Service` 1 | **1** |
| `server.Rehydrator` | deps.go:83 | `rehydrate.Service` 4 | **3** (`RehydrateProject` 제외 — `StatGate`가 내부에서 부른다, `hydrate.go:263`) |
| `*.Clock` (5곳) | consolidate:65 · document:134 · rehydrate:92 · server:26 · ulid:49 | `hotstore.Clock` 1 | **1** |

정본 예시는 `internal/document/document.go`다 — 7개의 좁은 인터페이스를 직접 선언하고 `Config`로 주입받는다.

```go
// internal/document/document.go:101-130 (Put/Get 메서드 주석만 생략)
// BlobCache is the local content-addressed cache of original bytes.
type BlobCache interface {
	Put(ctx context.Context, data io.Reader) (string, int64, error)
	Get(ctx context.Context, sha string) (io.ReadCloser, error)
	// Has reports whether the blob is cached. It is what tells a cache miss
	// (fall back to cold) apart from a real read failure, without this package
	// having to know the cache's error vocabulary.
	Has(ctx context.Context, sha string) (bool, error)
}

// BlobArchiver is the slice of the cold archive this pipeline consumes.
type BlobArchiver interface {
	UploadBlob(ctx context.Context, sha string, r io.Reader) (string, error)
	FetchBlob(ctx context.Context, sha string) (io.ReadCloser, error)
}

// RecordIndexer mirrors chunk episodes into the episodic search index. It is
// best-effort: a failure marks the manifest dirty, it never fails an ingest.
type RecordIndexer interface {
	IndexRecords(ctx context.Context, key hotstore.ProjectKey, recs []episodic.Record) error
}

// NodeUpserter mirrors the auto-created document node into the knowledge graph,
// also best-effort.
type NodeUpserter interface {
	UpsertNodes(ctx context.Context, key hotstore.ProjectKey, nodes []knowledge.Node) error
}
```

효과가 두 군데에서 실측된다:

- **import 그래프가 좁아졌다.** `document`는 `blob`·`cold`·`search`·`graph`·`ulid`를 하나도 import하지 않고, `rehydrate`는 `search`·`graph`를 import하지 않는다(§1 표). 구현체는 `main.go`가 **넓은 값을 그냥 꽂는다** — Go의 구조적 타이핑이 자동으로 만족시킨다. 어댑터 코드도, 캐스팅도 없다 (`cmd/memory-mcp/main.go:155-191`).
- **테스트 fake가 작아졌다.** `internal/document/fakes_test.go`의 blob fake는 4개가 아니라 3개, index fake는 8개가 아니라 1개만 구현한다.

`server`가 상대적으로 넓은 것(`HotStore` 10/12)은 전송 계층이 라우팅하는 엔드포인트 전부를 커버해야 하기 때문이고, 그래도 `RemoveEpisodes`·`FileInfo`는 빠져 있다 — 에이징은 핸들러의 관심사가 아니다.

`server`가 제공자 패키지를 import하는 것은 남아 있지만 전부 **클라이언트가 아니라 타입/값/순수 함수** 때문이다. 전체 목록:

| 심볼 | 쓰임 |
|---|---|
| `search.Query` · `search.Hit` | 질의 DTO |
| `blob.ValidSHA` · `ulid.Valid` | 도메인 어휘(§6.1) |
| `document.IngestResult` · `consolidate.Options`/`Report` · `rehydrate.Report` | 응답 페이로드 |
| `rehydrate.DriftReport`/`Drift` | `/v1/status` 드리프트 블록 |
| `rehydrate.IndexKeyFor` · `rehydrate.PlaneStateSHA` | manifest 키·sha 계산 순수 함수. `markIndexed`가 쓴다 (`degraded.go:56`) |

---

## 4. 주입 — Clock, Logger, IDGenerator, 전역 금지

### 4.1 Clock

`Clock`의 정본 선언은 프로젝트 전체에 **하나**다 (`internal/hotstore/clock.go:8`). 소비자는 §3 표대로 각자 같은 모양의 1-메서드 인터페이스를 다시 선언하므로 `hotstore`를 import하지 않는 패키지(`ulid`)도 같은 값을 받을 수 있다.

```go
// internal/hotstore/clock.go
type Clock interface {
	Now() time.Time
}

// systemClock is the production Clock — the only place time.Now() is called.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// NewSystemClock returns the production Clock.
func NewSystemClock() Clock { return systemClock{} }
```

주입 지점: `hotstore.Config.Clock`, `ulid.Config.Clock`, `consolidate.Config.Clock`, `rehydrate.Config.Clock`, `document.Config.Clock`, `server.Config.Clock`. `main.go`가 `clock := hotstore.NewSystemClock()` 하나를 만들어 전부에 같은 값을 넘긴다 (`cmd/memory-mcp/main.go:92`).

**`time.Now()` 직접 호출은 전 프로덕션 코드베이스에 딱 한 군데**다 — `internal/hotstore/clock.go:16`의 `systemClock.Now()` 본체. ULID도 더는 예외가 아니다: `ulid.Client`가 `Clock`을 주입받고 `Generate()`가 `c.clock.Now().UnixMilli()`를 쓴다 (`internal/ulid/ulid.go:110`).

### 4.2 IDGenerator

ULID 생성이 패키지 함수에서 **주입되는 클라이언트**로 바뀌었다. 프로세스당 하나를 배선해 모노토닉 보장을 전역으로 유지한다.

```go
// cmd/memory-mcp/main.go:92-95
clock := hotstore.NewSystemClock()
// One generator per process: its ids are strictly increasing, so records
// minted anywhere in the server sort by id alone.
ids, err := ulid.New(ulid.Config{Clock: clock})
```

핸들러는 `s.clock.Now().UTC()`로 시각을 얻고 그 밀리초를 `s.ids.GenerateAt(now.UnixMilli())`에 넘긴다 (`internal/server/handlers_episodic.go:103-104`) — 즉 **테스트에서 fake clock을 넣으면 ULID까지 결정적**이다. `internal/server/fakes_test.go:31`의 `fixedNow = 2026-08-25T02:00:00Z`가 그 전제를 쓴다. 검증 함수 `ulid.Valid(value)`는 순수 함수라 클라이언트가 필요 없다 (`internal/ulid/ulid.go:147`).

### 4.3 Logger

로거는 전부 주입된다. **패키지 전역 `slog.Warn`/`Error`/`Info`/`Debug` 호출은 0건**이다.

| 패키지 | 주입 필드 | nil일 때 |
|---|---|---|
| `server` | `Config.Logger` (필수) | `New`가 `errs.Invalid`로 거부 (`server.go:103-105`) |
| `search` | `Config.Logger` | `slog.Default()` (`search.go:155`) |
| `graph` | `Config.Logger` | `slog.Default()` (`graph.go:130`) |
| `document` | `Config.Logger` | `slog.Default()` (`document.go:205`) |
| `consolidate` | `Config.Logger` | `slog.New(slog.DiscardHandler)` — Report가 이미 모든 실패를 담는다 (`consolidate.go:132`) |
| `rehydrate` | `Config.Logger` | 동일 (`rehydrate.go:200`) |
| `blob`·`cold`·`hotstore`·`ulid`·`config`·도메인 | 없음 | 로그를 남기지 않는다. 실패는 전부 `*errs.Error`로 반환 |

`respond.go`의 `writeJSON`/`writeAPIError`는 `*slog.Logger`를 인자로 받고, 직접 호출에 대비한 마지막 보루로만 `orDefaultLogger`가 `slog.Default()`를 쓴다 (`internal/server/respond.go:66`). `main.go`가 `slog.SetDefault(logger)`를 하는 것은 폴백 경로까지 같은 핸들러로 보내기 위한 것이고 주석이 그 사실을 명시한다 (`cmd/memory-mcp/main.go:44-47`).

`fmt.Print*`/`log.*` 사용은 0건, `log/slog`만 쓴다.

### 4.4 전역 상태

**프로덕션 코드에 패키지 레벨 mutable 상태가 0건**이다. 이전에 있던 두 곳이 모두 사라졌다:

- ULID 모노토닉 카운터(`mu`/`lastMillis`/`lastRandom`)는 `ulid.client` 인스턴스 필드로 이동했다 (`internal/ulid/ulid.go:85-91`). 뮤텍스가 무엇을 지키는지 주석에 명시돼 있다.
- `handlers_knowledge.go`의 함수 변수 테스트 seam(`supersedeGraph`/`transitionNode`)은 삭제됐다. 핸들러가 `knowledge.Transition`/`knowledge.Supersede`/`knowledge.Purge`를 직접 부르고, 테스트는 fake store를 주입한다.

남은 패키지 레벨 `var`는 전부 초기화 후 다시 쓰이지 않는 값이다 — 컴파일된 정규식 3종(`hotstore.segmentPattern` key.go:36, `server.keySegmentPattern` validate.go:58, `blob.shaPattern` blob.go:104), 비교 전용 `[]byte`인 `document.pdfMagic`(extract.go:31), 패키지 내부 신호 에러 2종(`hotstore.errImmutableID`, `document.errNodeExists` — §5.1), `errs`의 센티널 5종, 그리고 계약 체크 `var _ Iface = ...` 10개. 닫힌 값 집합은 맵이 아니라 **switch 함수**로 바뀌어서(`episodic.ValidKind`, `knowledge.ValidNodeKind` 등) 런타임에 변조될 수 없다.

프로덕션 `func init()`는 0건이다. 유일한 `init()`은 swag가 생성한 `docs/docs.go:1803`이고, 이 파일은 `make swagger`의 산출물이다.

---

## 5. 에러 — 2계층 설계

핸들러 아래 계층은 **`*errs.Error`(의미)** 만 말하고, 전송 계층은 **`*apierr.Error`(HTTP)** 만 말한다. 경계는 `apierr.From(err)` 한 함수다.

```
hotstore / search / graph / cold / blob / ulid / config
episodic / knowledge / consolidate / document / rehydrate  ──►  *errs.Error   (Kind)
                                                                   │ apierr.From(err)
                                                         handler ──►  *apierr.Error (Status/Code)
```

### 5.1 도메인 에러 — `internal/errs`

`internal/errs/errs.go` 219줄, internal import 0, 커버리지 100%.

```go
// internal/errs/errs.go:22-30
type Kind string

const (
	KindInvalid     Kind = "invalid"     // input violates a rule
	KindNotFound    Kind = "not_found"   // the addressed entity does not exist
	KindConflict    Kind = "conflict"    // state machine violation, duplicate, supersede clash
	KindUnavailable Kind = "unavailable" // a derived store is down — the degraded-mode signal
	KindInternal    Kind = "internal"    // anything we failed to classify
)

// internal/errs/errs.go:68-76
type Error struct {
	Kind   Kind           // what went wrong, in transport-independent terms
	Op     string         // "hotstore.AppendEpisode" — a readable path, not a stack
	Entity string         // "episode", "knowledge_node", "blob"
	ID     string         // the target identifier, when there is one
	Msg    string         // human-readable, client-safe: no secrets, no full paths
	Fields map[string]any // structured detail; flows to slog, never to the body
	err    error          // cause
}
```

**센티널과 `Is`.** 센티널 5종(`ErrInvalid`/`ErrNotFound`/`ErrConflict`/`ErrUnavailable`/`ErrInternal`, :59-65)은 `Kind`가 아니라 별도 타입 `sentinel Kind`(:53)로 선언돼 있다. `(*Error).Is`(:106)가 target을 `sentinel`로 타입 단언해 **Kind로 매칭**하므로 `errors.Is(err, errs.ErrNotFound)`는 성립하고, 같은 Kind를 가진 서로 다른 `*Error` 두 개는 여전히 구별된다. Kind 문자열 직접 비교는 코드에 0건이다.

**생성자 8종.**

| 생성자 | 위치 | 용도 |
|---|---|---|
| `Invalid(op, entity, msg)` | :155 | 입력 규칙 위반. `msg`가 클라이언트에 그대로 나간다 |
| `NotFound(op, entity, id)` | :160 | `msg`를 `"{entity} not found"`로 자동 조립 |
| `Conflict(op, entity, id, msg)` | :170 | 상태기계 위반·중복·supersede 충돌 |
| `Unavailable(op, cause)` | :177 | 파생 저장소 도달 실패. degraded 판단의 근거 신호 |
| `Internal(op, cause)` | :183 | 분류 실패. cause는 체인에만 남는다 |
| `IO(op, path, cause)` | :190 | 파일시스템 실패. **path는 `Fields`로만 흐르고 응답 본문에 절대 안 나간다** |
| `FromContext(ctx, op)` | :198 | ctx 취소/만료를 도메인 에러로. 살아 있으면 nil |
| `Wrap(op, err)` | :209 | 패키지 경계 통과 시 `Op` 부착. 기존 Kind 보존, 외부 에러는 `KindInternal` |

`Wrap`은 `*Error`가 아니라 `error`를 반환한다 — `Wrap(op, nil)`이 진짜 nil이 되어 타입 있는 nil 함정을 막는다(근거 주석 :207-208, 가드 :210-212). 그리고 **`Wrap`은 자기 `Msg`를 붙이지 않는다**: 메시지 권한은 실패를 실제로 인지한 가장 안쪽 에러가 갖고, `apierr.publicMessage`가 그걸 찾아 올린다.

**부가 메서드.** `WithField(k,v)`(:113)는 수신자를 변경하지 않고 복사본을 돌려주므로 공유된 에러 값이 뒤에서 필드가 붙는 일이 없다. `LogValue()`(:126)는 `slog.GroupValue`로 kind/op/entity/id/msg + 정렬된 `Fields` + 중첩 cause를 그대로 로그에 흘린다.

**패키지별 사용 실측** (프로덕션 코드, 호출 횟수):

| 패키지 | Invalid | NotFound | Conflict | Unavailable | Internal | IO | Wrap | FromContext |
|---|---|---|---|---|---|---|---|---|
| `hotstore` | 5 | 3 | 1 | — | 1 | 18 | 4 | 4 |
| `knowledge` | 14 | 3 | 5 | — | — | — | — | — |
| `document` | 4 | 3 | — | 3 | 4 | — | 17 | — |
| `cold` | 3 | 3 | — | 2 | 3 | — | 9 | — |
| `graph` | 1 | 3 | — | 2 | 2 | — | 2 | — |
| `blob` | 2 | 1 | — | — | — | 10 | 2 | 2 |
| `config` | 6 | — | — | — | 2 | — | 1 | — |
| `rehydrate` | 2 | — | — | 1 | 1 | — | 7 | — |
| `search` | 2 | — | — | 2 | 3 | — | — | — |
| `consolidate` | 3 | — | — | 1 | — | — | 3 | — |
| `ulid` | 2 | — | — | — | 1 | — | 1 | — |
| `episodic` | 1 (헬퍼 경유) | — | — | — | — | — | — | — |
| `server` | 5 (`Config.validate`만) | — | — | — | — | — | — | — |

`server`의 5건은 전부 `server.Config.validate`(`server.go:88-107`)에서 나오고 부팅 실패로만 쓰인다 — **핸들러가 `errs` 생성자를 부르는 곳은 0건**이다(규약 §2.2).

지켜진 규약:

- **하위 계층에 HTTP status 개념이 0건이다.** `internal/{blob,cold,config,consolidate,document,episodic,errs,graph,hotstore,knowledge,rehydrate,ulid}` 12개 패키지 어디에도 `http.Status*`·`http.Error`·헤더 조작이 없고, `net/http`를 import하지도 않는다. 유일하게 `search`가 `net/http`와 `http.StatusNotFound` 등을 쓰는데, 이건 **OpenSearch가 돌려준 응답 코드를 읽는** 것이지 우리 응답 상태를 정하는 게 아니다 (`internal/search/{transport,index,bulk,query}.go` 4개 파일).
- **`errors.Is`/`errors.As`로만 비교한다.** 문자열 비교는 없다. `cold.isNotFoundErr`는 `errors.As`로 `*types.NoSuchKey`/`*types.NotFound`를 잡고, 실패하면 smithy `ErrorCode()` 인터페이스를 다시 `errors.As`로 추출한다.
- **에러 문자열은 소문자 시작, 마침표 없음**이고 `Op`가 `"hotstore.AppendEpisode"`처럼 패키지 접두사를 갖는다.
- **남의 패키지 센티널을 반환하지 않는다.** 예전에 `consolidate`가 `search.ErrUnavailable`을 반환하던 결합은 공용 `errs.Unavailable(opDeleteIndexed, nil)`로 대체됐다 (`internal/consolidate/age.go:92`).

패키지 소유 unexported 에러는 3개만 남아 있고 전부 패키지 내부 신호다: `hotstore.errImmutableID`(episode.go:23), `document.errNodeExists`(ingest.go:34), `rehydrate`가 실패 줄을 합쳐 cause로 넣는 익명 `errors.New`(rehydrate.go:331).

### 5.2 전송 에러 — `internal/server/apierr`

`internal/server/apierr/apierr.go` 160줄, internal import는 `errs` 하나, 커버리지 100%.

```go
// internal/server/apierr/apierr.go:68-74
type Error struct {
	Status  int            `json:"-"`
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
	cause   error          // logging only — never rendered into a response
}
```

`Status`는 `json:"-"` — 상태 줄에 속하지 본문에 속하지 않는다. `cause`는 unexported라 직렬화 자체가 불가능하다. `WithCause`(:92)/`WithDetail`(:101)도 `errs.WithField`와 같이 복사본을 돌려준다.

공개 코드 5종은 상수로 고정돼 있다 (:23-29): `CodeInvalidRequest`, `CodeNotFound`, `CodeConflict`, `CodeUnavailable`, `CodeInternal`.

`Envelope`이 이제 에러를 구조체로 담는다 (`internal/server/respond.go:19-23`):

```go
type Envelope struct {
	Success bool          `json:"success"`
	Data    any           `json:"data"`
	Error   *apierr.Error `json:"error,omitempty"`
}
```

즉 실패 응답은 평문 문장이 아니라 `{"success":false,"data":null,"error":{"code":"not_found","message":"episode not found"}}` 형태다. `Data`에 `omitempty`가 없으므로 실패 본문에도 항상 `"data": null`이 들어간다.

### 5.3 변환 경계 — `apierr.From` 한 지점

**불변식: `internal/server` 밖에서 `apierr`를 import하는 코드는 0건이다.** 확인:

```bash
grep -rn "server/apierr" --include="*.go" . | grep -v _test.go
```

히트는 `internal/server/{respond,validate,knowledge_graph,handlers_documents,handlers_episodic,handlers_knowledge,handlers_ops}.go` 7개 파일과, `internal/errs/errs.go:5`의 **주석 한 줄**뿐이다(코드 참조가 아니다). 테스트까지 포함해도 `internal/server` 밖 import는 `test/blackbox/harness_test.go` 하나이고, 이건 블랙박스 스위트가 응답 봉투의 `error` 필드를 디코드하기 위한 것이다.

`apierr.From` 호출은 프로덕션 코드에 **18곳**이고 전부 핸들러 안이다(문자열 `apierr.From`이 등장하는 주석 5건은 제외 — `respond.go:38,75,89` · `server.go:6` · `handlers_knowledge.go:312`):

| 파일:줄 | 변환되는 도메인 실패 |
|---|---|
| `handlers_episodic.go:106` | `ids.GenerateAt` |
| `handlers_episodic.go:124` | `store.AppendEpisode` (중복 id면 409) |
| `handlers_episodic.go:203` | `index.Search` — Unavailable 이외 |
| `handlers_episodic.go:320` | `store.GetEpisode` — NotFound 이외 |
| `handlers_episodic.go:330` | `archiver.FetchArchivedEpisode` |
| `handlers_knowledge.go:136` | `ids.GenerateAt` |
| `handlers_knowledge.go:184` | 노드 생성 `store.UpdateKnowledge` (`Supersede` 충돌 포함) |
| `handlers_knowledge.go:320` | 엣지 생성 `store.UpdateKnowledge` |
| `handlers_knowledge.go:366` | `graph.Search` — Unavailable 이외 |
| `handlers_knowledge.go:418` | `graph.Neighborhood` — Unavailable 이외 |
| `handlers_knowledge.go:488` | PATCH `store.UpdateKnowledge` (`Transition` 위반 → 409) |
| `handlers_knowledge.go:571` | DELETE `store.UpdateKnowledge` (`Purge` 게이트 → 409) |
| `handlers_documents.go:65` | `documents.Ingest` |
| `handlers_documents.go:96` | `documents.Original` |
| `handlers_documents.go:130` | `documents.Chunks` |
| `handlers_ops.go:92` | `store.Manifest` |
| `handlers_ops.go:230` | `consolidator.Run` |
| `handlers_ops.go:268` | `rehydrator.RehydrateAll` |

`From`의 동작 순서(:125-147):

1. `nil`이면 `nil`.
2. `errors.As`로 체인에 `*apierr.Error`가 있으면 **그대로 통과**시킨다. 이 덕분에 핸들러가 도메인 클로저 안에서 만든 전송 에러가 `errs.Wrap`에 감싸여 돌아와도 원형을 되찾는다 — 예: `handlers_knowledge.go:476`의 `return g, notFound("node not found")`가 `hotstore.UpdateKnowledge`의 `errs.Wrap`(knowledge.go:43)을 거쳐 :488에서 다시 404로 복원된다.
3. `errors.As`로 `*errs.Error`가 없으면 500 `internal`, cause는 `Message`에 넣지 않는다.
4. 있으면 `mappingFor(domain.Kind)`(:51)로 status/code를 정하고, `publicMessage`(:153)가 체인을 **바깥에서 안으로 훑어 처음 만나는 작성된 `Msg`** 를 올린다. `Wrap`이 `Msg`를 만들지 않으므로 실제로 올라오는 것은 실패를 인지한 가장 안쪽 지점의 문구이지만, 알고리즘 자체는 outermost-first다. 전부 순수 `Wrap`뿐이면 Kind 기본 문구로 폴백한다([08 §2.1](08-http-api.md)이 이 규칙의 정본).

전송 계층 고유 실패(본문 파싱, 경로/쿼리 파라미터)는 `From`을 거치지 않고 `respond.go`의 생성자 3종으로 만든다: `badRequest`(선언 :78, 호출 36회 — `validate.go` 12 · `handlers_knowledge` 10 · `handlers_episodic` 8 · `handlers_documents` 3 · `knowledge_graph` 3), `notFound`(:83, 3회), `unavailable`(:92, 19회). 모두 `apierr.New`(:114) 위에 얹혀 있고, `New`는 HTTP 범위를 벗어난 status를 500으로 정규화한다.

**요청 실패 로깅은 `writeAPIError` 한 곳**이다 (`respond.go:46-61`). cause가 있으면 `log.Error("request failed", "status", "code", "cause")`를 남기고 본문에는 넣지 않는다. 도메인 cause는 `*errs.Error`라 `LogValue`가 op/entity/id/Fields를 이미 실어 나르므로 핸들러가 다시 로그를 찍지 않는다. 핸들러가 직접 로깅하는 것은 **요청을 실패시키지 않는 best-effort 작업**뿐이다 — degraded upsert, manifest 기록, recall 수렴.

**내부 원인은 절대 응답에 나가지 않는다.** 의도적으로 공개하는 값은 둘뿐이다: (1) 요청 검증 실패 문구(클라이언트가 고쳐야 할 내용), (2) 도메인 `Msg`(상태기계 규칙 자체 — `"cannot transition active -> active"` 같은 문구는 공개해도 된다). `errs.IO`의 `path`처럼 절대 나가면 안 되는 값은 `Msg`가 아니라 `Fields`에 들어가 있어 구조적으로 차단된다.

### 5.4 Kind → HTTP 매핑

`mappingFor`(apierr.go:51)가 규약 §2.2 표를 그대로 구현한다. **차이가 없다.**

| domain Kind | HTTP | code | 기본 message | 실제 생산자 |
|---|---|---|---|---|
| `KindInvalid` | 400 | `invalid_request` | `invalid request` | `knowledge` 14 · `config` 6 · `hotstore` 5 · `document` 4 · … (전 계층) |
| `KindNotFound` | 404 | `not_found` | `not found` | `hotstore.GetEpisode`/`UpdateEpisodes`/`FileInfo` · `cold` 오브젝트/blob/아카이브 · `blob.Get` · `document.Original` 3분기 · `knowledge.FindNode`/`Purge`/`Supersede` · `graph.Neighborhood`/`SupersedeChain` |
| `KindConflict` | 409 | `conflict` | `conflict` | `hotstore.AppendEpisode`(중복 id) · `knowledge.Transition` 3분기 · `knowledge.Purge`(state가 archived/deprecated가 아님 — `CanPurge` 게이트) · `knowledge.Supersede`(중복 id) |
| `KindUnavailable` | 503 | `unavailable` | `service unavailable` | `search.transport`/`index 부재` · `graph.bolt`/`Ping` · `cold.New`/S3 · `consolidate.deleteFromIndex` · `document.Ingest`(cold/index/graph 미구성) · `rehydrate.failures` |
| `KindInternal` / 미분류 | 500 | `internal` | `internal error` | `errs.IO` 28건(hotstore 18, blob 10) · 각 패키지 `Internal` |

핸들러가 `From`을 우회해 status를 직접 정하는 경우는 **degraded 어휘를 재사용하기 위해서**다. 읽기 검색이 `errs.ErrUnavailable`을 만나면 `unavailable(degradedSearch).WithCause(err)`로 답해서(`handlers_episodic.go:196-200`, `handlers_knowledge.go:361-363, 413-415`) 503 본문 문구가 쓰기 경로의 degraded 노트와 **한 글자까지 같아진다**.

**핸들러의** `errs` 센티널 비교는 5곳뿐이고 전부 이 목적이다:

| 위치 | 비교 대상 | 이유 |
|---|---|---|
| `handlers_episodic.go:196` | `errs.ErrUnavailable` | 503 본문을 `degradedSearch`로 |
| `handlers_episodic.go:319` | `!errs.ErrNotFound` | hot miss면 cold 폴백, 그 외는 진짜 실패 |
| `handlers_knowledge.go:361` | `errs.ErrUnavailable` | 503 본문을 `degradedGraph`로 |
| `handlers_knowledge.go:413` | `errs.ErrUnavailable` | 동일 |
| `handlers_ops.go:159` | `errs.ErrNotFound` | S3 프로브가 404면 버킷은 살아 있다는 뜻 |

프로덕션 전체로 넓히면 `errs` 센티널 비교는 **9곳**이다. 핸들러 밖 4곳은 전부 `ErrNotFound`를 폴백 분기로 읽는 서비스·어댑터 내부 판단이고, 상태 코드와 무관하다: `document/read.go:31,70`(blob 캐시 미스 → cold 폴백), `cold/archive.go:67,154`(월별 아카이브·blob 부재를 상위 NotFound로 재분류).

나머지 `errors.Is`는 도메인 어휘가 아니라 표준 라이브러리 센티널을 본다 — `fs.ErrNotExist` 7곳(`hotstore/{episode,manifest,file,knowledge}.go` 각 1, `blob/blob.go` 3), `http.ErrServerClosed`(`server.go:214`, 서버 수명), `context.DeadlineExceeded`(`graph/bolt.go:86`), 그리고 패키지 내부 신호 `errNodeExists`(`document/ingest.go:224`).

degraded 문구는 상수로 고정돼 있다 (`internal/server/degraded.go:12-20`):

```go
const (
	degradedSearch = "search unavailable"
	degradedGraph  = "graph unavailable"
	degradedCold   = "cold storage unavailable"
	// degradedPromotion reports that the node was written but its provenance
	// episodes could not be marked consolidated, so those records stay
	// ineligible for cold aging until the next promotion (§3.1).
	degradedPromotion = "provenance episodes not marked consolidated"
)
```

같은 문자열이 두 역할을 겸한다 — 읽기 검색 실패 시엔 503 본문, 쓰기 성공 시엔 `data.degraded[]` 원소. `document` 패키지도 `degradedSearch`/`degradedGraph`를 같은 문구로 별도 선언해(`internal/document/document.go:44-47`) 인제스트 응답이 같은 어휘를 쓴다.

협력자 자체를 만들지 못한 경우는 degraded가 아니라 503이다 (`degraded.go:25-30`): `msgHotStoreUnavailable`, `msgDocumentsUnavailable`, `msgConsolidatorUnavailable`, `msgRehydratorUnavailable` — 정본 저장소나 문서 파이프라인이 없으면 정직한 부분 답이 존재하지 않기 때문이다.

### 5.5 degraded는 에러가 아니다 (규약 §2.2 마지막 항목)

hot 쓰기가 성공했는데 파생 저장소 upsert가 실패하면 **에러가 아니다.** 201/200 + `data.degraded: [...]`로 보고하고, manifest에 dirty를 찍어 다음 재수화가 수렴시킨다.

```go
// internal/server/handlers_episodic.go:128-139 — hot append 성공 이후
resp := CreateEpisodeResponse{Record: rec}
if s.index == nil {
	resp.Degraded = append(resp.Degraded, degradedSearch)
	s.markDirty(r.Context(), key, hotstore.PlaneEpisodic)
} else if err := s.index.IndexRecords(r.Context(), key, []episodic.Record{rec}); err != nil {
	s.log.Warn("episode index upsert failed; degraded", "project", key.String(), "error", err)
	resp.Degraded = append(resp.Degraded, degradedSearch)
	s.markDirty(r.Context(), key, hotstore.PlaneEpisodic)
} else {
	s.markIndexed(r.Context(), key, hotstore.PlaneEpisodic)
}
writeJSON(w, s.log, http.StatusCreated, resp)   // ← 503이 아니라 201
```

`markIndexed`/`markDirty`/`statGate`는 전부 **실패해도 요청을 실패시키지 않는다** — 로그만 남긴다 (`internal/server/degraded.go:36-81`, 파일 전체가 81줄이다). 같은 규칙이 knowledge 쓰기(`mirrorKnowledge`, `handlers_knowledge.go:251`), recall 카운터 갱신(`bumpRecall` `handlers_episodic.go:215` / `convergeEpisodes` :249), provenance 승격(`promoteProvenance`, `handlers_knowledge.go:212`)에도 적용된다. 자세한 동작은 [03-lifecycle](03-lifecycle.md)·[07-rehydration](07-rehydration.md) 참조.

`KindUnavailable`이 503이 되는 경로는 **읽기뿐**이다: `GET .../episodes/search`, `GET .../knowledge/search`, `GET .../knowledge/graph`, 문서 조회, 그리고 의존 자체가 nil인 경우.

---

## 6. 규약 준수 현황 요약

code-standards.md **§5 리뷰 체크리스트 10항목(#1-10)** 과 **§3 균일 코드 패턴 표에서 기계적으로 확인 가능한 4항목(#11-14)** 을 코드에 대조한 결과.

| # | 체크 항목 | 상태 | 근거 |
|---|---|---|---|
| 1 | 외부 의존이 인터페이스/구현체 쌍, 생성자가 `New(Config)` 하나 | ✅ | 인터페이스/unexported 구현체 쌍 10개, 컴파일 타임 계약 체크도 정확히 10개. 그중 9개가 단일 `New(Config)`를 갖는다(`document.Extractor`만 `document.New`가 고르는 기본값이라 생성자가 없다) (§2). 남은 어긋남 3건은 §2.1 |
| 2 | 소비자가 narrow interface 선언 | ✅ | 소비자 측 인터페이스 25개(consolidate 4 · document 7 · rehydrate 4 · server 9 · ulid 1). 어댑터를 보는 것은 대부분 제공자의 절반 이하로 좁다 (§3). `document`·`rehydrate`는 어댑터 패키지를 import조차 하지 않는다. 가장 넓은 `server.HotStore`(10/12)는 라우팅하는 엔드포인트 전부를 커버해야 해서다 |
| 3 | 서비스 이하에 HTTP 개념 없음 | ✅ | L4 이하 11개 패키지에 `http.Status`·`http.Error` 0건. `search`의 `net/http`는 OpenSearch 응답 코드 판독용 (§5.1) |
| 4 | 모든 에러가 `*errs.Error`로 의미를 가짐 | ✅ | 패키지별 센티널 전부 삭제, 남은 unexported 헬퍼 3개는 패키지 내부 신호 (§5.1) |
| 5 | `errors.Is/As`로 비교 | ✅ | Kind 문자열·에러 문자열 직접 비교 0건. 핸들러의 센티널 비교는 5곳, 프로덕션 전체로도 9곳뿐 (§5.4) |
| 6 | degraded와 실패를 구분 | ✅ | §5.5 |
| 7 | `time.Now()`/전역 상태/`init()` 부작용 없음 | ✅ | `time.Now()` 1곳(`systemClock.Now` 본체), 프로덕션 전역 mutable 0건, 프로덕션 `init()` 0건 (§4) |
| 8 | 데드코드·미사용 export·스텁 없음 | ⚠️ | 스텁(`notImplemented`)·`episodic.NewRecord`·`ulid.IsULID` 삭제됨. 프로덕션 호출 0건인 exported API 6종이 남아 있고, 그중 `blob.Client.Evict`·`ulid.Client.Generate` 2종은 규약 §4가 요구하는 유지 근거 주석이 없다 — §6.1 |
| 9 | 테이블 주도 테스트 + fake 주입, 커버리지 80%+ | ⚠️ | `internal/` 15개 패키지 전부 86.2%~100% (`errs`·`apierr`·`ulid`·`episodic` 100%, 최저 `blob` 86.2%). **`cmd/memory-mcp`만 72.9%로 미달** — 배선 코드라 실패 경로 상당수가 실제 협력자 없이는 재현되지 않는다. [11-testing](11-testing.md) |
| 10 | 정직성 원칙이 코드에 있음 (잘림·드리프트·미통합 보고) | ✅ | `IngestResult.Truncated`/`.Degraded`(`document.go:69-75`), `StatusReport.Drift`/`.Degraded`(`handlers_ops.go:34-50`), 응답 `data.degraded[]` (§5.5) |
| 11 | 파일 크기 800줄 이하 | ⚠️ | 프로덕션 최대 `internal/server/handlers_knowledge.go` 587줄로 여유. **테스트 2개가 상한 초과** — `handlers_knowledge_test.go` 937줄, `rehydrate_test.go` 899줄 |
| 12 | `fmt.Print*`/`log.*` 금지, 로거 주입 | ✅ | 0건. 전역 `slog.Warn/Error/...` 호출도 0건 (§4.3) |
| 13 | 컨텍스트가 첫 인자, struct 필드 보관 금지 | ✅ | 모든 I/O 메서드가 `ctx context.Context` 우선. `ctx`를 필드로 든 struct 없음 |
| 14 | `main` 밖 패닉 금지 | ✅ | 프로덕션 코드에 `panic(` 0건 |

### 6.1 남은 검증 이중화 (code-standards.md §4)

정적 도구 없이 교차 확인한 결과, 프로덕션 호출이 0인 exported API는 **여섯 개**다. 셋은 **데드코드가 아니라 검증 로직 이중화**이고 코드가 이 절을 근거로 인용하며 남겨 두고 있다:

| 심볼 | 위치 | 상태 |
|---|---|---|
| `episodic.Record.Validate()` | `internal/episodic/record.go:117` | 프로덕션 호출 0건. 전송 계층이 `CreateEpisodeRequest.validate()`(`handlers_episodic.go:37`)로 같은 불변식을 따로 검사한다 |
| `knowledge.Node.Validate()` | `internal/knowledge/knowledge.go:173` | 프로덕션 호출 0건. `CreateNodeRequest.validate()`(`handlers_knowledge.go:69`)가 kind/name/trust/review_after를 따로 본다 |
| `knowledge.Edge.Validate()` | `internal/knowledge/knowledge.go:208` | 프로덕션 호출 0건. `validateEdge`(`knowledge_graph.go:69`)가 ULID/rel/confidence를 따로 본다 |

나머지 셋은 **`Client` 인터페이스에 선언돼 있지만 프로덕션 호출부가 없는 메서드**다. 어느 소비자의 narrow interface(§3)에도 들어 있지 않다:

| 심볼 | 위치 | 유일한 호출자 | 유지 근거 주석 |
|---|---|---|---|
| `graph.Client.SupersedeChain` | `internal/graph/graph.go:50` | `test/blackbox/blackbox_test.go:171,370` · `graph/{client,live}_test.go` | ✅ 있음 — doc comment가 "(blackbox scenario 3)"으로 소비자를 명시한다 |
| `blob.Client.Evict` | `internal/blob/blob.go:67` | `blob/blob_test.go`만 | ❌ 없음 — "왜 지금 안 쓰이는지" 한 줄이 규약 §4가 요구하는 예외 조건인데 그게 없다 |
| `ulid.Client.Generate` | `internal/ulid/ulid.go:57` | `ulid/ulid_test.go`만 | ❌ 없음 — 핸들러는 결정성을 위해 전부 `GenerateAt(now.UnixMilli())`를 쓴다 (§4.2) |

`Evict`·`Generate` 둘은 규약 §4 기준으로는 삭제 후보이거나 근거 주석을 붙여야 하는 상태다. 판단이 갈리는 이유는 둘 다 **인터페이스 계약의 완결성** 쪽 논거가 있기 때문이다 — 캐시에 `Put`/`Get`/`Has`만 있고 제거가 없는 것, ULID 생성기에 "지금 시각으로" 경로가 없는 것은 그 자체로 어색하다. 어느 쪽으로 가든 §4가 요구하는 것은 **결정을 코드에 한 줄로 남기는 것**이다.

> ```go
> // internal/knowledge/knowledge.go:170-172
> // Kept alongside the transport-layer checks in internal/server on purpose: the
> // spec (docs/spec/09-code-structure.md §6.1) records the two as duplicated
> // validation to be merged handler-side, not as dead code to drop.
> ```

드리프트 위험은 실재한다 — 예를 들어 `knowledge.Node.Validate()`는 "active 노드는 `superseded_by`를 가질 수 없다"(:189)를 검사하지만 핸들러는 이를 검사하지 않는다. 통합 방향은 핸들러가 도메인 `Validate()`를 호출하고 그 `*errs.Error`(전부 `KindInvalid`)를 `apierr.From`으로 400에 매핑하는 것이다 (§4 "중복 헬퍼는 삭제가 아니라 통합"). 이제 두 계층이 같은 에러 타입을 쓰므로 통합 비용은 리팩터 전보다 낮다.

**어휘 자체는 이미 통합됐다.** 서버가 갖고 있던 사본(`isNodeKind`/`isTrust`/`isNodeState`/`isRel`)은 삭제됐고, 전송 계층이 도메인 패키지의 어휘를 직접 호출한다: `episodic.ValidKind`/`ValidActor`, `knowledge.ValidNodeKind`/`ValidState`/`ValidTrust`/`ValidRel`, `blob.ValidSHA`, `ulid.Valid`.

`hotstore.ProjectKey.Validate()`는 반대로 정상이다 — 전송 계층의 `validateProjectKey`(경로 파라미터 방어, `validate.go:61`)와 hotstore의 `guard`가 부르는 `key.Validate()`(파일 경로 방어, `hotstore.go:149`)는 **서로 다른 두 신뢰 경계**를 지키므로 중복이 아니다. `internal/server/validate.go:55-58` 주석이 그 근거를 명시한다.

---

## 7. 코드 위치

| 개념 | 파일 | 앵커 |
|---|---|---|
| 조립 루트 (모든 배선) | `cmd/memory-mcp/main.go` | `main()` (42), `run` (58), `build` (91) |
| 서버 설정 + 협력자 | `internal/server/server.go` | `type Config` (64), `validate` (88), `type Server` (112), `func New` (139) |
| 서버 narrow interface 9종 | `internal/server/deps.go` | `Clock` (26), `IDGenerator` (33), `HotStore` (39), `EpisodeIndex` (54), `KnowledgeGraph` (60), `DocumentIngestor` (70), `Consolidator` (77), `Rehydrator` (83), `ColdArchive` (92) |
| 라우팅 테이블 | `internal/server/server.go` | `func (*Server) Router` (163) |
| 응답 봉투 · 전송 에러 생성자 | `internal/server/respond.go` | `Envelope` (19), `writeJSON` (26), `writeAPIError` (46), `badRequest` (78), `notFound` (83), `unavailable` (92) |
| degraded 상수 · manifest 기록 | `internal/server/degraded.go` | `degradedSearch/Graph/Cold/Promotion` (12), 503 문구 (25), `markIndexed` (36), `markDirty` (66), `statGate` (74) |
| 전송 계층 입력 검증 | `internal/server/validate.go` | `validateProjectKey` (61), `validateSHA` (79), `validateULID` (87), `decodeJSON` (127) |
| 그래프 순수 알고리즘 + 엣지 검증 | `internal/server/knowledge_graph.go` | `findGraphNode` (21), `affectedNodes` (32), `validateEdge` (69) |
| 부팅 드리프트 점검 | `internal/server/startup.go` | `func (*Server) Startup` (10) |
| **도메인 에러 어휘** | `internal/errs/errs.go` | `Kind` (22), 센티널 (59), `type Error` (68), `Is` (106), `WithField` (113), `LogValue` (126), 생성자 (155-219) |
| **전송 에러 + Kind→HTTP** | `internal/server/apierr/apierr.go` | 코드 상수 (23), `mappingFor` (51), `type Error` (68), `New` (114), `From` (125), `publicMessage` (153) |
| 에러 변환 경계 (18지점) | `internal/server/handlers_*.go` | `apierr.From` 호출부 — §5.3 표 |
| 상태기계 위반 → 409 | `internal/server/handlers_knowledge.go` | 488 (PATCH), 571 (DELETE) |
| 정본 저장소 계약 | `internal/hotstore/hotstore.go` | `type Client` (54), `type Config` (101), `New` (136), `guard` (145) |
| 공용 시계 | `internal/hotstore/clock.go` | `type Clock` (8), `NewSystemClock` (19) |
| 프로젝트 키 · 평면 | `internal/hotstore/key.go` | `Plane` (12), `ProjectKey` (27), `segmentPattern` (36), `Validate` (41) |
| episodic 인덱스 계약 | `internal/search/search.go` | `type Client` (73), `Config` (107), `New` (135) |
| knowledge 그래프 계약 | `internal/graph/graph.go` | `type Client` (27), `Config` (93), `New` (120) |
| 콜드 아카이브 계약 | `internal/cold/cold.go` | `type Client` (53), `objectStore` (76), `Config` (88), `New` (113) |
| 로컬 blob 캐시 | `internal/blob/blob.go` | `type Client` (57), `Config` (71), `New` (95), `ValidSHA` (113) |
| ULID 생성기 | `internal/ulid/ulid.go` | `Clock` (49), `type Client` (55), `New` (97), `GenerateAt` (114), `Valid` (147) |
| narrow interface 정본 예시 | `internal/document/document.go` | `Service` (79), `HotStore` (93), `BlobCache` (102), `BlobArchiver` (115), `RecordIndexer` (122), `NodeUpserter` (128), `Clock` (134), `IDGenerator` (141), `Config` (146) |
| 문서 추출 seam | `internal/document/extract.go` | `type Extractor` (37), `textExtractor` (47), 계약 체크 (50) |
| 통합 파이프라인 계약 | `internal/consolidate/consolidate.go` | `HotStore` (40), `EpisodeIndexer` (52), `ColdArchiver` (58), `Service` (70), `Config` (78), `New` (126) |
| 재수화 계약 | `internal/rehydrate/rehydrate.go` | `HotStore` (57), `EpisodeIndex` (69), `KnowledgeGraph` (81), `Service` (128), `Config` (145), `New` (194) |
| episodic 어휘·검증 | `internal/episodic/record.go` | `ValidKind` (54), `ValidActor` (63), `Record.Validate` (117) |
| knowledge 어휘·검증 | `internal/knowledge/knowledge.go` | `Node` (77), `Edge` (102), `ValidNodeKind` (130), `Node.Validate` (173), `Edge.Validate` (208) |
| 상태 전이·supersede·purge | `internal/knowledge/lifecycle.go` | `Transition` (54), `Purge` (97), `Supersede` (153), `FindNode` (215) |
| 결정적 상수 · env 로딩 | `internal/config/config.go` | env 이름 (31), 스펙 상수 (45), 기본값 (59), `Load` (125), `Validate` (159) |
| 빌드·검증 타깃 | `Makefile` | `build`, `vet`, `vet-blackbox`, `test`, `live`, `blackbox` |
