# 06 — 문서: blob · 추출 · 청킹

문서 하나를 blob(원본 바이트) + knowledge `document` 노드 + N개의 episodic `document_chunk`로 분해해 세 저장 계층에 남기고, 청크 히트에서 원본 바이트까지 되짚어 오는 경로.

| 항목 | 내용 |
|---|---|
| 관련 코드 | `internal/document/` (`document.go`, `ingest.go`, `chunk.go`, `read.go`, `extract.go`) · `internal/blob/blob.go` · `internal/cold/` (`cold.go`, `archive.go`, `keys.go`, `s3.go`) · `internal/errs/errs.go` · `internal/server/handlers_documents.go` · `internal/server/apierr/apierr.go` · `cmd/memory-mcp/main.go` |
| 관련 스펙 | [01-overview](01-overview.md) · [02-storage-model](02-storage-model.md) · [03-lifecycle](03-lifecycle.md) · [04-episodic-search](04-episodic-search.md) · [05-knowledge-graph](05-knowledge-graph.md) · [07-rehydration](07-rehydration.md) · [08-http-api](08-http-api.md) · 인덱스: [README](README.md) |
| 상태 | 구현 완료. 단위 테스트 `internal/document/{document,ingest,chunk,read,extract}_test.go`, 수용 시나리오 `test/blackbox/blackbox_test.go` `TestScenario04_DocumentIngest`. 설계 문서([architecture-v2.md](../design/architecture-v2.md) §6, [code-standards.md](../design/code-standards.md))가 아니라 **현재 코드**를 기술한다 |

![문서 ingest 파이프라인과 세 산출물](assets/06-documents.svg)

## 1. "문서 하나"의 정의

문서는 단일 레코드가 아니다. 업로드 한 번이 성격이 다른 세 산출물을 만들고, 각각 다른 계층이 소유한다.

| 산출물 | 무엇 | 정본 위치 | 파생/캐시 |
|---|---|---|---|
| blob | 원본 바이트, sha256으로 주소화 | S3 `{username}/blobs/{sha[:2]}/{sha}` (cold-first) | `{Home}/blobs/{sha}` 로컬 캐시 |
| `document_chunk` × N | 추출 텍스트를 자른 episodic 레코드 | hot `episodic/{ws}/{team}/{proj}.json` | OpenSearch 색인 |
| `document` 노드 × 1 | knowledge 그래프의 문서 표제 노드 | hot `knowledge/{ws}/{team}/{proj}.json` | Neo4j |

세 산출물은 sha256으로 묶인다. 청크는 `refs.doc_sha`로, 노드는 `aliases`에 담긴 sha로 blob을 가리킨다. 이 sha가 유일한 조인 키다 — 문서 노드와 청크 사이에는 엣지가 없다.

계약은 `document.Service`다.

```go
type Service interface {
	Ingest(ctx context.Context, key hotstore.ProjectKey, filename string, data io.Reader) (IngestResult, error)
	Original(ctx context.Context, sha string) (io.ReadCloser, error)
	Chunks(ctx context.Context, sha string) ([]episodic.Record, error)
}
```

구현체는 unexported `document.service`이고, 생성 경로는 `document.New(Config) (Service, error)` 하나뿐이다. 이 패키지는 자기 외부 시스템을 가진 클라이언트가 아니라 주입된 클라이언트들 위의 파이프라인이므로 `Client`/`client`가 아니라 코드 규약 §1.1의 Service/service 형태를 쓴다(패키지 doc 주석이 그 근거를 명시한다).

협력자는 전부 **소비자 측 narrow interface**로 이 패키지가 다시 선언한다 — 제공자 패키지의 넓은 `Client`를 그대로 의존하지 않는다.

| Config 필드 | 인터페이스 | 메서드 | 필수 |
|---|---|---|---|
| `Store` | `HotStore` | `AppendEpisode` `ListEpisodes` `ListProjects` `UpdateKnowledge` `MarkDirty` | ✔ |
| `Cache` | `BlobCache` | `Put` `Get` `Has` | ✔ |
| `Clock` | `Clock` | `Now` | ✔ |
| `IDs` | `IDGenerator` | `GenerateAt` | ✔ |
| `Archiver` | `BlobArchiver` | `UploadBlob` `FetchBlob` | — (없으면 ingest 자체가 unavailable) |
| `Index` | `RecordIndexer` | `IndexRecords` | — (없으면 degraded) |
| `Graph` | `NodeUpserter` | `UpsertNodes` | — (없으면 degraded) |
| `Extractor` | `Extractor` | `Extract` | — (nil이면 내장 `textExtractor`) |
| `Logger` | `*slog.Logger` | — | — (nil이면 `slog.Default()`) |

`New`는 앞의 넷 중 하나라도 nil이면 `errs.Invalid`를 돌려주고 I/O는 하지 않는다(`TestNewValidatesConfig`가 "무엇이 필수이고 무엇이 degraded인가"를 고정한다). 배선은 `cmd/memory-mcp/main.go:155`의 `document.New(document.Config{...})` 한 곳뿐이고, 거기서 `hotstore.Client`(Store)·`blob.Client`(Cache)·`hotstore.Clock`(Clock, `hotstore.NewSystemClock()`)·`ulid.Client`(IDs)·`cold.Client`(Archiver)·`search.Client`(Index)·`graph.Client`(Graph)가 구조적 타이핑으로 위 인터페이스를 만족한다 — 어느 제공자 패키지도 이 인터페이스들을 알지 못한다.

응답 타입:

```go
type IngestResult struct {
	SHA         string      `json:"sha"`
	BlobKey     string      `json:"blob_key"`
	NodeID      string      `json:"node_id"`
	Extractable bool        `json:"extractable"`
	ChunkIDs    []string    `json:"chunk_ids"`
	Truncated   *Truncation `json:"truncated,omitempty"`  // Truncation{Total, Indexed int}
	Degraded    []string    `json:"degraded,omitempty"`   // "search unavailable" / "graph unavailable"
}
```

`ChunkIDs`는 `Ingest` 진입 즉시 `[]string{}`으로 초기화되므로 JSON에서 `null`이 아니라 `[]`로 나간다. `Truncated`는 잘림이 실제로 일어났을 때만(§5.1), `Degraded`는 파생 미러가 실제로 실패했을 때만(§7) 존재한다.

## 2. 파이프라인 5단계와 실패 경계

`service.Ingest`(`ingest.go`)는 아래 순서를 고정으로 실행한다. 어느 단계가 ingest 전체를 실패시키고 어느 단계가 degraded로 끝나는지가 이 표의 요점이다.

| 단계 | 코드 | 하는 일 | 실패하면 |
|---|---|---|---|
| ① sha256 + 로컬 캐시 | `cache.Put` → `blob.Client.Put` | 스트림을 해싱하면서 캐시에 기록, sha 반환 | ingest 실패 (`errs.Wrap("document.Ingest", …)`) |
| ② cold-first blob 업로드 | `uploadBlob` → `cold.Client.UploadBlob` | 캐시본을 다시 열어 S3로 스트리밍 | ingest 실패. `archiver == nil`이면 `errs.Unavailable` |
| ③ 결정적 추출 | `extract` → `Extractor.Extract` | 캐시본을 다시 열어 텍스트 추출 | **IO/파싱 오류만** ingest 실패. "추출 불가"는 실패가 아니다 |
| ④ 청킹 + hot append | `ensureChunks` → `chunkDocument` + `store.AppendEpisode` | 청크마다 `document_chunk` 레코드 append | hot append 실패 시 ingest 실패. 색인 실패는 degraded |
| ⑤ knowledge 문서 노드 | `ensureDocumentNode` → `store.UpdateKnowledge` | 그래프에 `kind=document` 노드 추가 | hot write 실패 시 ingest 실패. Neo4j 실패는 degraded |

- ④는 `extractable == true`일 때만 실행된다. 스캔 PDF 같은 입력은 청크가 0개인 채로 ②·⑤만 남는다.
- ①은 multipart 스트림을 한 번 소비하며 캐시에 쓰고, ②·③은 그 캐시본을 각각 다시 연다(`cache.Get`) — 원본 바이트는 총 세 번 읽힌다.
- **cold가 hot보다 먼저다.** ②가 성공하기 전에는 hot에 아무것도 쓰지 않는다. 그래서 중간 실패의 잔해는 항상 "S3와 캐시에는 blob이 있고 hot에는 참조가 없는 상태"이고, 같은 파일을 다시 올리면 §6의 멱등 경로로 수렴한다. 반대 방향(hot에 청크가 있는데 S3에 blob이 없음)은 만들어지지 않는다.
- 이 패키지를 떠나는 에러는 전부 `*errs.Error`다. 각 단계는 `errs.Wrap(opIngest, err)`로 `Op`만 붙이고 원인의 `Kind`·`Msg`를 보존하므로, 핸들러의 `apierr.From`이 상태 코드를 정확히 한 번 결정한다(§7).

## 3. blob — content-addressed, cold-first

### 3.1 로컬 캐시 (`internal/blob`)

`blob.Client`(구현체 unexported `client`, 생성자 `blob.New(Config{Dir})`)는 `{DJ_MEMORY_HOME}/blobs/` 아래에 **플랫하게** `{sha256}` 이름으로 저장한다(cold와 달리 2자 샤딩이 없다). 디렉터리 0700, 파일 0600. 뮤텍스는 없다 — 모든 쓰기가 temp 파일을 거치고 content address 위로의 rename은 정의상 멱등이기 때문이다.

`Put`의 순서가 content-addressing의 안전성을 만든다.

1. `os.MkdirAll` → 같은 디렉터리에 `os.CreateTemp(dir, ".tmp-*")`
2. `io.Copy(io.MultiWriter(hasher, tmp), data)` — 해싱과 기록이 한 번의 스트리밍
3. `Sync` → `Close` → 여기서야 sha가 확정
4. `{dir}/{sha}`가 이미 있으면 **그대로 반환**(멱등 재put, temp는 defer로 삭제)
5. 없으면 `Chmod(0600)` → `Rename`

즉 주소(sha)를 알기 전에는 최종 경로에 아무것도 나타나지 않는다. 중간에 죽어도 `.tmp-*` 파편만 남지 부분 기록이 content address 아래로 노출되지 않는다.

`Get`/`Has`/`Evict`는 공통 `guard(ctx, op, sha)`로 죽은 컨텍스트와 잘못된 주소를 IO 전에 걷어낸다. 주소 검증은 `ValidSHA` = `^[0-9a-f]{64}$`(`shaPattern`) — 경로 조작을 `{dir}/{sha}` 안에 가두는 유일한 장치다. `ValidSHA`가 exported인 이유는 HTTP 경계가 같은 정규식을 두 벌 갖지 않기 위해서다: `internal/server/validate.go`의 `validateSHA`가 이 함수를 호출한다(코드 규약 §4 중복 헬퍼 통합, `ulid.Valid` 선례).

에러 어휘는 패키지 전용 센티넬이 아니라 `internal/errs`다 — 캐시 미스는 `errs.NotFound(…, "blob", sha)`(→ `errors.Is(err, errs.ErrNotFound)`), 잘못된 sha는 `errs.Invalid`, 파일시스템 실패는 `errs.IO`(= `Internal` + `path` 필드, 경로는 로그 전용).

### 3.2 cold (`internal/cold`)

- 키 레이아웃의 단일 정본은 `cold.BlobKey(username, sha)` = `{username}/blobs/{sha[:2]}/{sha}`(`keys.go`). 블랙박스 테스트가 응답의 `blob_key`를 이 함수 결과와 직접 비교한다.
- `cold.Client.UploadBlob`(`archive.go`)은 `objects.Exists`(HeadObject)로 먼저 확인하고 있으면 업로드를 **건너뛴다**. content-addressed이므로 같은 키의 기존 객체는 정의상 같은 바이트다.
- 아카이브 포맷 계층(`client`)과 원시 객체 IO 계층(`objectStore` / `s3Objects`)이 분리돼 있다. 업로드는 `manager.NewUploader`(멀티파트 `Upload`), 다운로드는 `GetObject`. 없는 객체는 `types.NoSuchKey`/`types.NotFound`/에러코드 문자열 셋 중 무엇으로 오든 `isNotFoundErr`(`s3.go`)가 걸러 `errs.NotFound`로 정규화하고, `FetchBlob`이 이를 다시 sha 주소로 바꿔 돌려준다. 나머지 S3 전송 실패는 전부 `errs.Unavailable`이다 — degraded 판단의 근거가 되는 신호.
- 버킷·리전·프로파일·username은 전부 config에서 온다: `DJ_MEMORY_S3_BUCKET`(기본 `vms-memory-mcp`), `DJ_MEMORY_S3_REGION`(기본 `ap-northeast-2`, 프로파일 리전 상속 금지), `AWS_PROFILE`(기본 `vms-holdings`), `DJ_MEMORY_USERNAME`(기본 OS 사용자).

## 4. 결정적 추출 — 되는 것과 안 되는 것

`Extractor.Extract(ctx, filename, data)`는 `(text string, extractable bool, err error)`를 돌려준다. 이 3-튜플의 조합이 정직성 계약이다. 기본 구현은 unexported `textExtractor`이고, `Config.Extractor`가 nil일 때 `New`가 골라 넣는다(테스트는 같은 인터페이스의 fake를 주입한다 — 프로덕션 코드에 테스트 분기 없음).

| 반환 | 의미 | ingest 결과 |
|---|---|---|
| `(text, true, nil)` | 결정적 텍스트를 얻었다 | 청킹 진행 |
| `("", false, nil)` | 형식은 인식했지만 결정적 텍스트가 없다 | 청크 0개, blob·노드는 생성. **에러 아님** |
| `("", false, err)` | IO 실패 또는 파일 손상 | ingest 실패 (`errs.Internal`) |

디스패치 순서:

1. `bytes.HasPrefix(buf, "%PDF-")`(`pdfMagic`) **또는** 확장자 `.pdf` → PDF 경로. 매직 바이트가 확장자보다 우선하므로 `doc.bin`으로 이름 붙은 PDF도 텍스트 레이어를 읽는다.
2. 확장자 `.md` / `.markdown` / `.txt` / `.text` → `utf8.Valid(buf)`면 그대로 통과. 유효하지 않으면 **인코딩을 추측하지 않고** `("", false, nil)`.
3. 그 외 전부 → `("", false, nil)`. 이미지·오피스 문서·바이너리에는 결정적 추출기가 없다.

### 4.1 PDF

라이브러리는 `github.com/ledongthuc/pdf v0.0.0-20250511090121-5959a4027728`. `extractPDF`는 `pdf.NewReader(ra, size)` 후 1..`NumPage()`를 돌면서

- `page.V.IsNull()`인 페이지는 건너뛴다,
- `page.GetPlainText(nil)`이 에러면 **그 페이지를 건너뛴다** — 텍스트 레이어가 없는 페이지로 취급하고 내용을 지어내지 않는다,
- 빈 문자열도 건너뛴다,
- 나머지를 `\n`으로 이어붙이고 `TrimSpace`.

전부 합쳐 빈 문자열이면 `("", false, nil)`. **스캔 PDF의 정의가 이것이다** — 텍스트 레이어가 없으면 `extractable: false`로 정직하게 보고하고, OCR은 서버가 하지 않는다(설계 §11 비목표). blob은 이미 S3에 있으므로 에이전트가 원본을 내려받아 스스로 처리하면 된다.

`extractPDF`에는 `defer recover()`가 있다. `ledongthuc/pdf`는 일부 손상 입력에서 panic하는데, 이를 `errs.Internal`(+ `filename`·`panic` 필드)로 변환해 **손상된 업로드 하나가 서버를 내리지 못하게** 한다(코드 규약 §3: `main` 밖 panic 금지). 파싱 실패가 `Invalid`가 아니라 `Internal`인 것도 계약이다 — 우리가 못 읽은 것이지 클라이언트가 틀린 게 아니므로 400이 아니라 500이다(`TestTextExtractorExtract`의 `corrupt pdf is an error` 케이스가 이를 고정한다).

### 4.2 메모리

`Extract`는 `io.ReadAll`로 파일 전체를 메모리에 올린다 — `pdf.NewReader`가 `io.ReaderAt`을 요구하기 때문이다. 따라서 추출 순간의 피크 메모리는 파일 크기 그대로이고, 상한은 HTTP 계층의 업로드 캡(128 MiB)이다.

## 5. 청킹 — 2 KiB, 룬 경계, 500 상한

```go
func chunkDocument(text string) ([]string, Truncation)                       // 스펙 상수 적용
func chunkText(text string, chunkBytes, maxChunks int) ([]string, Truncation) // 알고리즘
```

둘 다 unexported다(`chunk.go`). 파이프라인은 언제나 `chunkDocument`만 부르므로 §6의 예산·상한을 재진술하는 호출부가 존재하지 않는다.

- **바이트 예산**이지 문자 수가 아니다. 다음 룬을 넣으면 `chunkBytes`를 넘길 때 끊는다. 한글은 UTF-8 3바이트이므로 2048바이트 ≈ 682자, ASCII는 2048자.
- **룬을 절대 쪼개지 않는다**(`utf8.DecodeRuneInString`). 예산보다 큰 룬 하나는 통째로 방출된다.
- `chunkBytes <= 0`이면 `config.DocumentChunkBytes`(2048), `maxChunks <= uncapped`(0)이면 무제한. 빈 문자열은 `(nil, Truncation{})`.
- 의미 경계(문장·문단·마크다운 헤딩)를 전혀 보지 않는다. 그래서 청크를 순서대로 이어붙이면 추출 텍스트가 바이트 단위로 복원된다 — `TestChunksOrderedBySeq`가 이걸 단언한다.

프로덕션 파라미터는 `internal/config/config.go`의 상수다.

| 상수 | 값 | 뜻 |
|---|---|---|
| `config.DocumentChunkBytes` | `2048` | 청크 목표 크기(바이트) |
| `config.MaxDocumentChunks` | `500` | 하드 상한 — 문서당 색인되는 텍스트는 최대 약 1 MiB |

`TestChunkDocumentUsesSpecLimits`가 `chunkDocument`가 실제로 이 두 상수를 쓰는지(청크 개수 = 500, 각 청크 = 정확히 2048바이트) 검사한다.

### 5.1 잘림의 정직한 보고

`Truncation{Total, Indexed}`의 `Total`은 **상한과 무관한 실제 청크 수**다. 상한에 걸려 만들어지지 않은 꼬리까지 센다(루프가 `total++`를 먼저 하고 `len(chunks) < maxChunks`일 때만 append). `Indexed`는 실제로 만들어진 수.

`IngestResult.Truncated`는 `setChunks`가 `Total > Indexed`일 때만 채운다. 잘림이 없으면 `omitempty`로 응답에서 사라진다 — "truncated 필드가 없다"가 곧 "전량 색인됐다"이다.

501 × 2048바이트 문서를 올리면 `{"total": 501, "indexed": 500}`과 500개의 `chunk_ids`가 온다(`TestIngestTruncatesAtChunkCap`). 같은 테스트가 재업로드에서도 `Total`이 501로 유지되는지 확인한다.

### 5.2 만들어지는 레코드

```go
id, err := s.ids.GenerateAt(now.UnixMilli())   // now = s.clock.Now()
rec := episodic.Record{
	ID:         id,
	Kind:       episodic.KindDocumentChunk,
	OccurredAt: now,                                  // 문서 날짜가 아니라 ingest 시각
	Actor:      episodic.ActorSystem,
	Text:       chunk,
	Entities:   []string{},
	Refs:       &episodic.Refs{DocSHA: sha, ChunkSeq: seq},
}
```

- 시계와 ID 생성기는 주입된다(`Config.Clock`, `Config.IDs`). 패키지 안에 `time.Now()` 직접 호출도, 전역 ULID 생성기도 없다(코드 규약 §1.1) — 그래서 청크 타임스탬프와 ID가 테스트에서 결정적이다.
- `Consolidated`는 zero value인 `false`. 서버는 이 값을 자동으로 켜지 않으므로(설계 §3) 청크는 에이전트가 증류하기 전까지 hot에 남는다.
- 모든 청크가 같은 밀리초로 `GenerateAt`을 호출한다. `internal/ulid`의 `client`가 같은 밀리초 안에서 랜덤부를 증가시키므로 **청크 ID는 seq 순서대로 강하게 증가**한다. 프로세스당 생성기가 하나(`main.go`에서 한 번 만들어 전 계층에 주입)이므로 이 보장이 서버 전체에 걸쳐 유지되고, hot 파일을 ID 순으로 읽으면 청크 순서가 보존된다.
- `episodic.Record.Validate`가 `kind=document_chunk ⟺ refs != nil`을 강제한다. `refs.doc_sha`는 비어 있으면 안 되고 `chunk_seq >= 0`이어야 한다. 같은 규칙이 `POST .../episodes`에도 적용되어(핸들러가 `validateSHA`로 한 번 더 검사) 에이전트가 손으로 만든 청크도 같은 모양을 갖는다.

## 6. 같은 sha 재업로드 — 멱등

동일 바이트를 다시 올리면 각 계층이 각자 멱등하게 동작한다.

| 계층 | 판정 방법 | 결과 |
|---|---|---|
| 로컬 캐시 | `os.Stat({dir}/{sha})` | rename 생략, 같은 sha 반환 |
| cold | `objects.Exists(BlobKey)` | 업로드 생략, 같은 키 반환 |
| 청크 | `chunkIDsFor(ListEpisodes(key), sha)` — 프로젝트 episodic 파일에서 `kind=document_chunk ∧ refs.doc_sha == sha` 스캔 | 기존 ID를 seq 순으로 그대로 반환, append 없음 |
| 문서 노드 | `UpdateKnowledge` 클로저 안에서 `kind=document ∧ sha ∈ aliases` 스캔 | 기존 노드 ID 반환. **`errNodeExists`로 쓰기를 중단**해 그래프 파일을 건드리지 않는다 |

문서 노드의 조회와 append는 **하나의 `UpdateKnowledge` 클로저 안**에서 일어난다 — 읽기·변환·쓰기가 스토어 뮤텍스 한 번 안에 들어가므로 같은 프로젝트에 대한 동시 ingest가 서로의 노드를 덮어쓰지 못한다. 회귀 방지는 `internal/hotstore/concurrency_test.go`와 `internal/server/handlers_knowledge_concurrency_test.go`. 재업로드가 `errNodeExists`로 되돌아 나오는 이유도 같은 결에 있다: 내용이 같은데도 파일을 다시 쓰면 mtime이 바뀌어 §5의 stat 게이트가 수렴할 것도 없는 재수화를 예약하게 된다.

`TestIngestSameShaIsIdempotent`가 두 번째 ingest 후 `uploads == 1`, 청크 1개, 노드 1개, 그리고 `NodeID`·`ChunkIDs`가 첫 번째와 동일함을 단언한다.

재업로드 시의 잘림 보고는 다시 계산된다: `chunkDocument`를 돌려 `Total`을 얻고 `Indexed = min(계산값, 기존 청크 수)`로 낮춘다. 즉 "이미 저장된 것 기준"으로 정직하게 답한다.

주의할 스코프 비대칭:

- 청크 존재 확인과 노드 조회는 **요청 경로의 프로젝트 안에서만** 한다. 같은 파일을 다른 `{ws}/{team}/{proj}`로 올리면 청크와 문서 노드가 그 프로젝트에 새로 생긴다(blob은 계정 전역이므로 하나). 이건 프로젝트별 정본 파일 모델의 자연스러운 귀결이다.
- 반면 `Chunks(sha)`와 `GET /v1/documents/{sha}`는 **전역**이다(§8).
- 재업로드 때 파일명이 달라져도 기존 노드의 `Name`은 바뀌지 않는다 — 첫 업로드의 파일명이 유지된다.
- 재업로드는 파생 미러를 다시 건드리지 않는다. 청크도 노드도 이미 있으므로 `IndexRecords`·`UpsertNodes` 호출 자체가 없고, 따라서 `degraded` 노트도 붙지 않는다. 파생 저장소가 비어 있는 상태를 되돌리는 건 재수화의 일이지 재업로드의 일이 아니다([07 · 재수화](07-rehydration.md)).

## 7. degraded와 실패의 구분

| 죽은 것 | 결과 | 코드 |
|---|---|---|
| document 서비스 자체 미배선 | **503** `document pipeline unavailable` | 세 핸들러 공통 (`handlers_documents.go:37`, `:90`, `:124`) |
| S3 (`Archiver == nil`) | **503** `cold storage unavailable` — 핸들러 사전 검사 | `handlers_documents.go:41` |
| S3 (업로드 중 실패) | **503** `service unavailable` — cold의 `errs.Unavailable`이 `apierr.From`을 통해 매핑 | `uploadBlob` → `cold.objects.Put` |
| OpenSearch | ingest **성공(201)**. `degraded: ["search unavailable"]` + `WarnContext` + `MarkDirty(key, PlaneEpisodic)` | `ensureChunks` → `indexChunks` → `reportDegraded` |
| Neo4j | ingest **성공(201)**. `degraded: ["graph unavailable"]` + `WarnContext` + `MarkDirty(key, PlaneKnowledge)` | `ensureDocumentNode` → `upsertNode` → `reportDegraded` |

- 파생 저장소가 **죽은 경우**와 **애초에 배선되지 않은 경우**(`Index == nil` / `Graph == nil`)가 같은 결과를 낸다. `indexChunks`·`upsertNode`가 nil 협력자를 `errs.Unavailable`로 바꿔 같은 `reportDegraded` 경로로 보내기 때문이다 — 클라이언트 입장에서 "OpenSearch가 다운"과 "OpenSearch를 안 붙였음"은 구분할 필요가 없는 같은 사실이다(`TestIngestDerivedFailureIsDegradedNotFatal`의 네 케이스).
- 두 미러는 독립적으로 실패하므로 한 응답이 노트 두 개를 동시에 담을 수 있다(`TestIngestAccumulatesEveryDegradedNote`). 반대로 전부 정상이면 `Degraded`는 비어 있다(`TestIngestFullySuccessfulReportsNoDegradation`) — 항상 붙는 노트는 정보가 아니다.
- 노트 문자열은 서버의 degraded 어휘와 **글자 단위로 같다**(`document.degradedSearch`/`degradedGraph` ↔ `server.degradedSearch`/`degradedGraph`). 에피소드 쓰기든 노드 쓰기든 문서 ingest든 클라이언트가 읽는 어휘가 하나다.
- 파생 저장소가 죽어도 hot 쓰기는 성공하고 manifest에 dirty가 찍힌다 — 다음 재수화가 수렴시킨다. `MarkDirty` 자체가 실패하면 로그만 남기고 요청은 계속 성공한다(`TestIngestDirtyMarkFailureStillSucceeds`).
- 청크가 0개면(추출 불가) 색인 호출이 아예 없으므로 search degraded 노트도 붙지 않는다.

상태 코드 결정은 핸들러 한 줄, `writeAPIError(w, s.log, apierr.From(err))`이 전부다. `apierr.From`이 `errs.Kind`를 고정 표로 매핑한다: `Invalid`→400, `NotFound`→404, `Conflict`→409, `Unavailable`→503, 그 외/미분류→500. 응답 본문의 `message`는 `publicMessage`가 체인을 바깥에서 안으로 훑어 **처음 만나는 authored `Msg`**다 — `errs.Wrap`은 자기 메시지를 만들지 않으므로 실패를 실제로 인지한 가장 안쪽 지점의 문구가 그대로 올라온다. 원인(`cause`)은 `slog`로만 나간다.

## 8. 회수 — 청크 히트에서 원본까지

### 8.1 `GET /v1/documents/{sha}` — 원본 바이트

경로 파라미터는 핸들러 경계에서 `validateSHA` → `blob.ValidSHA`(`^[0-9a-f]{64}$`)로 먼저 검증한다(400). 그다음 `service.Original`(`read.go`):

1. `cache.Get(sha)` — 히트면 그대로 스트리밍.
2. 실패하면 `cacheMissed`가 `cache.Has(sha)`로 **"없는 것"과 "못 읽은 것"을 가른다**. 미스가 아니면(읽기 실패거나 Has 자체가 실패) 원래 에러를 그대로 감싸 반환 — 망가진 로컬 캐시를 cold 폴백으로 덮어 감추지 않는다. 이 판정을 캐시에게 물어보므로 이 패키지는 캐시의 에러 어휘를 몰라도 된다.
3. `archiver == nil`이면 `errs.NotFound`(+ `reason` 필드) → **404**.
4. `FetchBlob`이 `errs.ErrNotFound`면 404, 그 외 실패는 그대로 전파(S3 장애는 `Unavailable` → 503). **cold 장애가 404로 둔갑하지 않는 것**이 `TestOriginalErrors`의 핵심 단언이다.
5. `FetchBlob` 성공 시 `cache.Put`으로 **재캐시**하고, 반환된 해시가 요청 sha와 다르면 `errs.Internal`(+ `sha`/`cold_sha`/`reason` 필드) — cold 객체 손상 탐지.
6. 재캐시가 실패하면 `WarnContext` 후 `fetchFromCold`로 cold에서 직접 스트리밍한다(이 경로에서는 해시 검증이 없다). 그 사이 cold까지 사라지면 nil 리더가 아니라 장애 에러가 나간다(`TestOriginalColdLostAfterRecacheFailure`).

응답은 `{success, data, error}` 봉투가 아니라 `application/octet-stream` 원시 바이트다 — API 전체에서 유일한 예외. 헤더 전송 후 스트림이 끊기면 `log.Warn`만 남긴다(상태 코드를 되돌릴 수 없으므로).

### 8.2 `GET /v1/documents/{sha}/chunks` — 청크 목록

`service.Chunks`는 `store.ListProjects()`로 **모든 프로젝트**를 순회하며 각 episodic 파일을 읽고 `isChunkOf(rec, sha)`(= `kind=document_chunk ∧ refs.doc_sha == sha`)를 모은다. 정렬은 `chunk_seq` 우선, 동률이면 ID(`strings.Compare`). 없는 sha는 404가 아니라 빈 배열 `[]`이다(`TestChunksUnknownShaIsEmptyNotAnError`).

비용은 hot episodic 레코드 총량에 비례한다(O(전체 레코드)). 상한 500청크 문서 하나를 위해 모든 프로젝트 파일을 읽는 구조라, 프로젝트가 많아지면 여기가 먼저 아프다.

### 8.3 검색으로 들어오는 경로

일반적인 회수는 검색에서 시작한다.

```
GET /v1/{ws}/{team}/{proj}/episodes/search?q=...&kinds=document_chunk
  → Hit.Record.Refs.DocSHA
  → GET /v1/documents/{sha}          (원본 바이트)
  → GET /v1/documents/{sha}/chunks   (전후 청크)
```

검색 응답은 발췌 + 메타 + 점수만 담는다(설계 §7) — 청크 본문 전량을 주입하지 않는다. 자세한 질의 구성은 [04 · Episodic 검색](04-episodic-search.md).

청크는 평범한 episode이므로 [03 · 생명주기](03-lifecycle.md)의 에이징 규칙을 똑같이 받는다. 에이전트가 `consolidated=true`로 만든 청크는 30일 후 S3 `{yyyy-mm}.json` 배치로 내려가고 hot과 OpenSearch에서 사라진다. 그러면 `Chunks(sha)`(hot만 읽는다)의 결과에서도 빠진다 — blob은 남아 있으므로 원본 다운로드는 계속 된다.

## 9. 경계값

| 항목 | 값 | 위치 |
|---|---|---|
| 업로드 본문 상한 | 128 MiB (`maxUploadBytes`, `http.MaxBytesReader`) | `internal/server/validate.go:25` |
| multipart 인메모리 임계 | 32 MiB (`multipartMemoryBytes`) | `internal/server/validate.go:27` |
| multipart 필드명 | `file` (`formFieldFile`, 고정). 없으면 400 | `handlers_documents.go:52` |
| 파일명 | 비어 있으면 400 | `handlers_documents.go:58` |
| 청크 크기 / 개수 | 2048 B / 500 | `internal/config/config.go:53,55` |
| 문서당 색인 텍스트 | 최대 약 1 MiB (500 × 2048) | 위 상수의 곱 |
| 추출 피크 메모리 | 파일 크기 전체 (`io.ReadAll`) | `internal/document/extract.go:54` |
| 성공 응답 | `201 Created` + `IngestResult` | `handlers_documents.go:68` |

요청 단위 타임아웃은 없다. 대용량 문서 ingest가 수 초 걸리는 것이 정상이라 `readHeaderTimeout`(5초, 느린 헤더 방어)만 두고 전체 요청 시간은 제한하지 않는다(`server.go:31`).

## 10. ⚠️ 설계 문서와 차이

architecture-v2.md §6·code-standards.md와 코드가 아직 어긋나는 지점.

1. **`handleIngestDocument`는 `markIndexed`를 호출하지 않는다.** 다른 쓰기 핸들러(`handlers_episodic.go:137`·`:280`, `handlers_knowledge.go:268`·`:584`)는 파생 upsert 성공 후 manifest의 `indexed_at`과 plane 하이드레이션 sha를 갱신한다. 문서 ingest는 하지 않으므로, 아무 문제 없이 성공한 ingest 뒤에도 `CheckDrift`가 "episodic hot content changed since last hydration"을 보고할 수 있다(불필요한 재수화를 유발하되 데이터는 안전). 파생 미러 성공/실패를 `document` 패키지가 이미 알고 있으므로(§7의 `reportDegraded` 경로) 반대편 신호를 핸들러로 돌려주면 닫히는 간극이다.
2. **`blob.Client.Evict`에 프로덕션 호출자가 없다.** 설계 §1·§3은 로컬 blob 캐시를 "축출 가능"이라고 규정하지만, 축출을 실제로 실행하는 코드는 없다(`main.go`는 `blob.New`만 부르고, `document`가 선언한 `BlobCache`에도 `Evict`가 없다). 즉 캐시는 자동으로 줄지 않고 무한히 자란다. 축출은 현재 수동(`rm`)이며, S3가 백스톱이므로 안전하기는 하다. 코드 규약 §4(데드코드) 기준으로는 `Evict`가 정리 대상이거나 호출부가 생겨야 한다. — 같은 항목에 있던 `Has`는 해소됐다: `service.Original`이 캐시 미스와 읽기 실패를 가르는 데 쓴다(§8.1).
3. **문서 ingest는 `KindUnavailable`을 503으로 바꾸는 유일한 쓰기 경로다.** 코드 규약 §2.2는 "`KindUnavailable`을 503으로 바꾸는 건 읽기 검색 경로뿐"이라고 못박지만, S3가 없거나 blob 업로드가 실패하면 이 쓰기 엔드포인트가 503을 낸다. 이는 architecture-v2.md §6 step 2(cold-first)가 S3를 계약의 전제로 만들었기 때문에 의도된 예외다 — hot 쓰기가 성공한 적이 없으므로 degraded로 보고할 것도 없다. 두 설계 문서 중 어느 쪽 문장을 고칠지가 남아 있다.

## 11. 코드 위치

| 개념 | 파일 | 심볼 |
|---|---|---|
| 서비스 계약·의존성·생성 | `internal/document/document.go` | `Service`, `Config`, `New`, `service`, `HotStore`, `BlobCache`, `BlobArchiver`, `RecordIndexer`, `NodeUpserter`, `Clock`, `IDGenerator` |
| 응답 타입·잘림 보고 | `internal/document/document.go` | `IngestResult`, `Truncation`, `degradedSearch`, `degradedGraph` |
| 파이프라인 5단계 | `internal/document/ingest.go` | `Ingest`, `uploadBlob`, `extract`, `ensureChunks`, `ensureDocumentNode` |
| degraded 보고 | `internal/document/ingest.go` | `mirror`, `indexChunks`, `upsertNode`, `reportDegraded`, `setChunks` |
| 멱등 재업로드 판정 | `internal/document/ingest.go` | `chunkIDsFor`, `isChunkOf`, `errNodeExists` |
| 청킹 알고리즘 | `internal/document/chunk.go` | `chunkDocument`, `chunkText`, `uncapped` |
| 회수 | `internal/document/read.go` | `Original`, `cacheMissed`, `fetchFromCold`, `Chunks` |
| 추출 디스패치 | `internal/document/extract.go` | `Extractor`, `textExtractor.Extract`, `pdfMagic` |
| PDF 텍스트 레이어 | `internal/document/extract.go` | `extractPDF` (+ panic recover) |
| 로컬 blob 캐시 | `internal/blob/blob.go` | `Client`, `client`, `New`, `Config`, `ValidSHA`, `shaPattern`, `guard` |
| 아카이브 포맷 | `internal/cold/archive.go` | `client.UploadBlob`, `client.FetchBlob` |
| S3 원시 IO | `internal/cold/s3.go` | `s3Objects`, `isNotFoundErr` |
| cold 계약·생성 | `internal/cold/cold.go` | `Client`, `Config`, `New`, `objectStore` |
| 키 레이아웃 | `internal/cold/keys.go` | `BlobKey` |
| 도메인 에러 어휘 | `internal/errs/errs.go` | `Error`, `Kind`, `ErrNotFound`, `ErrUnavailable`, `Invalid`, `NotFound`, `Unavailable`, `Internal`, `IO`, `Wrap` |
| 상태 코드 매핑 | `internal/server/apierr/apierr.go` | `From`, `mappingFor`, `publicMessage` |
| 청크 레코드 스키마 | `internal/episodic/record.go` | `KindDocumentChunk`, `Refs`, `Record.Validate`, `ValidKind` |
| 문서 노드 스키마 | `internal/knowledge/knowledge.go` | `KindDocument`, `StateActive`, `TrustImported`, `Node` |
| 상수 | `internal/config/config.go` | `DocumentChunkBytes`, `MaxDocumentChunks` |
| HTTP 핸들러 | `internal/server/handlers_documents.go` | `handleIngestDocument`, `handleGetDocument`, `handleGetDocumentChunks` |
| 요청 검증·상한 | `internal/server/validate.go` | `validateSHA`, `maxUploadBytes`, `multipartMemoryBytes`, `formFieldFile` |
| degraded 어휘 | `internal/server/degraded.go` | `degradedCold`, `msgDocumentsUnavailable`, `markIndexed` |
| 라우트 | `internal/server/server.go` | `Router` — `/v1/documents/{sha}`, `/v1/documents/{sha}/chunks`, `/v1/{ws}/{team}/{proj}/documents` |
| 서버 측 계약 | `internal/server/deps.go` | `DocumentIngestor` |
| 배선 | `cmd/memory-mcp/main.go` | `blob.New`, `cold.New`, `document.New` (`:155`) |
| 생성·추출기 선택 테스트 | `internal/document/document_test.go` | `TestNewValidatesConfig`, `TestNewDefaultsExtractorAndLogger`, `TestIngestInjectedExtractor`, `TestIngestExtractorFailure` |
| 파이프라인 테스트 | `internal/document/ingest_test.go` | `TestIngestTextDocument`, `TestIngestSameShaIsIdempotent`, `TestIngestDerivedFailureIsDegradedNotFatal`, `TestIngestAccumulatesEveryDegradedNote`, `TestIngestColdFirstFailures`, `TestIngestUnextractable`, `TestIngestTruncatesAtChunkCap` |
| 청킹 테스트 | `internal/document/chunk_test.go` | `TestChunkText`, `TestChunkDocumentUsesSpecLimits` |
| 회수 테스트 | `internal/document/read_test.go` | `TestOriginal`, `TestOriginalErrors`, `TestOriginalStreamsFromColdWhenRecacheFails`, `TestChunksOrderedBySeq` |
| 추출 테스트 | `internal/document/extract_test.go` | `TestTextExtractorExtract`, `TestTextExtractorReadFailure` (`textPDF`, `noTextPDF`) |
| 테스트 fake | `internal/document/fakes_test.go` | `rig`, `newRig`, `fakeCache`, `fakeArchiver`, `fakeIndexer`, `fakeGraph`, `fakeStore`, `fixedClock`, `fakeIDs`, `stubExtractor` |
| 수용 시나리오 | `test/blackbox/blackbox_test.go` | `TestScenario04_DocumentIngest` (+ `test/fixtures/pdf` `Minimal`) |
