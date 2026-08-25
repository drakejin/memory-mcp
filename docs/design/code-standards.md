# memory-mcp Go 코드 규약

| 항목 | 내용 |
|---|---|
| 목적 | 패키지마다 다른 스타일로 구현된 코드를 하나의 관용구로 수렴시킨다 |
| 작성일 | 2026-08-25 |
| 구속력 | [architecture-v2.md](architecture-v2.md)가 *무엇을* 만드는지의 정본이라면, 이 문서는 *어떻게* 쓰는지의 정본이다. 충돌 시 이 문서가 코드 형태를 결정한다 |
| 적용 | 신규 코드는 물론 기존 코드 전부 이 규약으로 리팩터링한다 |

## 1. 의존성 구성 — Interface / struct 쌍

외부 세계(OpenSearch, Neo4j, S3, 파일시스템, 시계)에 닿는 모든 컴포넌트는 **exported 인터페이스 + unexported 구현체** 쌍으로 만든다.

```go
package search

// Client is the episodic search index. All methods are context-first and
// return semantic errors from internal/errs.
type Client interface {
	EnsureIndex(ctx context.Context, key hotstore.ProjectKey) error
	Upsert(ctx context.Context, key hotstore.ProjectKey, recs []episodic.Record) error
	Search(ctx context.Context, q Query) ([]Hit, error)
	Ping(ctx context.Context) error
}

type client struct {
	http *http.Client
	base string
	log  *slog.Logger
}

// New returns a Client bound to cfg. It does not dial; use Ping to verify.
func New(cfg Config) (Client, error) {
	// validate cfg, build client
	return &client{...}, nil
}
```

규칙:

1. 이름은 **`Client` / `client`** 로 통일한다. 패키지명이 이미 도메인을 말하므로 `SearchClient` 같은 중복 접두사 금지 (`search.Client`).
2. 생성자는 `New(cfg Config) (Client, error)` 하나. 여러 생성 경로가 필요하면 `Config` 필드로 흡수하고, 생성자를 늘리지 않는다.
3. 구현체 struct는 **unexported**. 외부에서 필드에 손대는 경로를 만들지 않는다.
4. 생성자는 I/O를 하지 않는다. 연결 확인은 `Ping`.
5. 테스트 fake는 같은 인터페이스를 구현한다. 프로덕션 코드에 테스트용 분기를 넣지 않는다.

### 1.1 소비자 측 narrow interface (duck typing)

인터페이스는 **쓰는 쪽이 필요한 만큼만** 다시 선언한다. 제공자 패키지의 넓은 `Client`를 그대로 의존하지 않는다 — Go의 구조적 타이핑이 자동으로 만족시켜 준다.

```go
package consolidate

// 이 패키지가 실제로 쓰는 것만 요구한다. search.Client가 우연히 이걸 만족한다.
type EpisodeIndexer interface {
	Delete(ctx context.Context, key hotstore.ProjectKey, ids []string) error
}

type ColdArchiver interface {
	PutEpisodeBatch(ctx context.Context, key hotstore.ProjectKey, month string, recs []episodic.Record) (string, error)
}

type Service struct {
	hot   HotStore
	index EpisodeIndexer
	cold  ColdArchiver
	clock Clock
}
```

- 의존은 **전부 생성자 주입**. 패키지 전역 변수·싱글턴·`init()` 부작용 금지.
- `time.Now()` 직접 호출 금지. `Clock` 인터페이스(`Now() time.Time`)를 주입해 에이징·TTL 로직을 테스트 가능하게 유지한다.

## 2. 에러 — 계층별 의미 객체

**서비스 이하(도메인)와 핸들러(전송)는 서로 다른 에러 타입을 쓴다.** 경계는 단 한 곳, 핸들러의 변환 함수다.

```
hotstore / search / graph / cold / consolidate  ──►  *errs.Error   (의미)
                                                        │ apierr.From(err)
                                              handler ──►  *apierr.Error (HTTP)
```

### 2.1 도메인 에러 — `internal/errs`

```go
type Kind string

const (
	KindInvalid     Kind = "invalid"      // 입력이 규칙 위반
	KindNotFound    Kind = "not_found"
	KindConflict    Kind = "conflict"     // 상태기계 위반, 중복, supersede 충돌
	KindUnavailable Kind = "unavailable"  // 파생 저장소 다운 → degraded 모드 판단 근거
	KindInternal    Kind = "internal"
)

// Error is the semantic error carried by every layer below the handler.
type Error struct {
	Kind   Kind
	Op     string         // "hotstore.AppendEpisode" — 호출 스택 대신 읽히는 경로
	Entity string         // "episode", "knowledge_node", "blob"
	ID     string         // 대상 식별자 (있으면)
	Msg    string         // 사람이 읽는 설명. 비밀·경로 전체를 담지 않는다
	Fields map[string]any // 구조화 상세 (slog로 그대로 흘린다)
	err    error          // 원인
}

func (e *Error) Error() string
func (e *Error) Unwrap() error
func (e *Error) Is(target error) bool // 센티넬(ErrNotFound 등)과 Kind로 매칭
```

- 센티넬: `errs.ErrInvalid`, `ErrNotFound`, `ErrConflict`, `ErrUnavailable`, `ErrInternal` — 비교는 항상 `errors.Is(err, errs.ErrNotFound)`. Kind 문자열 직접 비교 금지.
- 생성자: `errs.Invalid(op, entity, msg)`, `errs.NotFound(op, entity, id)`, `errs.Conflict(...)`, `errs.Unavailable(op, cause)`, `errs.Internal(op, cause)`, 그리고 래핑용 `errs.Wrap(op, err)`.
- 하위 계층은 **절대 HTTP를 모른다** — status code, `http.Error`, 헤더가 서비스 이하에 등장하면 규약 위반.
- `fmt.Errorf("%w")`로 감싸는 것은 허용하되, 패키지 경계를 넘길 때는 `errs.Wrap`으로 `Op`를 붙인다.
- 에러 문자열은 소문자 시작, 마침표 없음 (Go 관용).

### 2.2 핸들러 에러 — `internal/server/apierr`

```go
// Error is the transport-layer error. It owns HTTP status and the public code
// exposed in the {success,data,error} envelope.
type Error struct {
	Status  int
	Code    string         // "not_found", "invalid_request", "search_unavailable"
	Message string         // 클라이언트에게 보이는 문구
	Details map[string]any
	cause   error          // 로깅 전용 — 응답에 나가지 않는다
}

// From maps a domain error to its transport representation. This is the ONLY
// place where errs.Kind becomes an HTTP status.
func From(err error) *Error
```

매핑 표(고정):

| domain Kind | HTTP | code |
|---|---|---|
| `KindInvalid` | 400 | `invalid_request` |
| `KindNotFound` | 404 | `not_found` |
| `KindConflict` | 409 | `conflict` |
| `KindUnavailable` | 503 | `unavailable` |
| `KindInternal` / 미분류 | 500 | `internal` |

- 핸들러는 `errs` 생성자를 직접 부르지 않는다. 요청 파싱·경로 파라미터 검증처럼 **전송 계층 고유의 실패**만 `apierr.New(...)`로 만든다.
- 내부 원인(`cause`)은 `slog`로만 남기고 응답 본문에 넣지 않는다. 단, `Details`에 담긴 필드는 의도적으로 공개하는 값이다.
- degraded 모드: 파생 저장소가 죽어도 hot 쓰기가 성공한 경우는 **에러가 아니다**. 200 + `data.degraded: [...]`로 보고한다 (스펙 §5). `KindUnavailable`을 503으로 바꾸는 건 읽기 검색 경로뿐.

## 3. 균일한 코드 패턴

| 항목 | 규칙 |
|---|---|
| 컨텍스트 | 모든 I/O 메서드의 첫 인자는 `ctx context.Context`. 구조체 필드로 보관 금지 |
| 로깅 | `log/slog`만. `fmt.Print*`·`log.*` 금지. 로거는 주입, 전역 금지. 메시지는 소문자, 값은 구조화 필드로 |
| 패닉 | `main` 밖에서 금지. 라이브러리 코드는 에러를 반환한다 |
| 네이밍 | 생성자 `New`, 변환 `From`/`To`, 검증 `Validate`. 약어는 대문자 유지(`ID`, `URL`, `SHA`) |
| 반환값 | named return 금지(짧은 defer 조작 제외). 3개 초과 반환은 struct로 |
| 상수 | 매직 넘버·문자열 금지. 패키지 상단 `const` 블록에 의미 있는 이름으로 |
| 시간 | 저장·비교는 RFC3339 UTC 문자열, 계산은 `time.Time`. 경계에서만 변환 |
| JSON | 도메인 struct에 json 태그 명시, `omitempty`는 스펙이 생략을 허용한 필드만 |
| 동시성 | 공유 상태는 뮤텍스로 보호하고 주석으로 무엇을 지키는지 명시. 고루틴 누수 금지(`ctx` 전파) |
| 파일 크기 | 800줄 상한, 400줄 넘으면 분할 검토. 한 파일 = 한 관심사 |
| 테스트 | table-driven + `t.Run`. 외부 의존은 fake. `testify` 금지(stdlib만). 커버리지 80%+ |

## 4. 데드코드 정책

파생물처럼 코드도 **쓰이지 않으면 지운다.** 유지 근거가 없는 코드는 리뷰 부채다.

- 판정 도구: `go vet ./...`, `staticcheck ./...`(U1000 unused), `go build` 후 수동 교차 확인. 도구가 못 잡는 것(exported이지만 아무도 안 쓰는 API)은 **스펙 §7 API 표면·§9 패키지 계약에 없으면 삭제**한다.
- 삭제 대상: 미사용 exported 함수/타입/상수, 죽은 분기, 스텁만 남은 not-implemented 함수, 중복 헬퍼, 쓰이지 않는 struct 필드, 주석 처리된 코드.
- 예외로 남기려면 코드에 **왜 지금 안 쓰이는지** 한 줄 주석을 남긴다. 근거 없는 "나중에 쓸지도"는 삭제 사유다(YAGNI).
- 중복 헬퍼는 삭제가 아니라 **통합**한다 — 같은 일을 하는 함수가 두 패키지에 있으면 하나로 모으고 호출부를 고친다.

## 5. 리뷰 체크리스트

- [ ] 외부 의존 컴포넌트가 `Client`/`client` 쌍인가, 생성자가 `New(Config) (Client, error)` 하나인가
- [ ] 소비자가 넓은 인터페이스 대신 필요한 만큼의 narrow interface를 선언했는가
- [ ] 서비스 이하에 HTTP 개념이 없는가, 핸들러가 `errs`를 직접 만들지 않는가
- [ ] 모든 에러가 `*errs.Error`로 의미를 갖는가 (맨 `errors.New`/`fmt.Errorf` 유출 없음)
- [ ] `errors.Is/As`로 비교하는가 (문자열 비교·Kind 직접 비교 없음)
- [ ] degraded와 실패를 혼동하지 않는가 (쓰기 성공+파생 실패 = 200 + degraded)
- [ ] `time.Now()`/전역 상태/`init()` 부작용이 없는가
- [ ] 데드코드·미사용 export·스텁이 남아있지 않은가
- [ ] 테이블 주도 테스트, fake 주입, 커버리지 80%+
- [ ] 스펙(architecture-v2.md)의 정직성 원칙이 코드에 있는가 — 잘림·드리프트·미통합을 응답에 보고
