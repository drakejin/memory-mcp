# memory-mcp 설계 — 세 소스의 종합

| 항목 | 내용 |
|---|---|
| 목표 | 에이전트의 장기 기억을 관리하는 로컬 MCP 서버 — 기억의 저장·회상·개정·풍화·충돌을 라이프사이클로 관리 |
| 작성일 | 2026-08-24 |
| 입력 소스 | ① [codebase-memory-mcp 실측 분석](../analyze/codebase-memory-mcp.md) ② [Codebase-Memory 논문](../analyze/codebase-memory-paper-analysis.md) ③ [MemOS 논문](../analyze/memos-memory-os-analysis.md) ④ AX workspace memory lifecycle policy (Notion, 2026-08-18) |

## 1. 문제 정의

에이전트 기억은 두 종류로 갈리고, 요구되는 방법론이 정반대다.

| | 정본이 있는 기억 | 정본이 없는 기억 (주장 기억) |
|---|---|---|
| 예 | 코드 인덱스, 문서 인덱스 | 사용자 발화·선호, 결정, 교훈, 조사 결론 |
| 진실 판정 | 해시 비교 (원본과 일치하는가) | 판정 불가 — 상태·버전·신뢰도·게이트 필요 |
| 풍화 | 존재하지 않음 (신선도 문제) | 실재함 (지식이 낡고 뒤집힌다) |
| 대표 방법론 | codebase-memory-mcp | MemOS, AX policy |

memory-mcp가 다루는 것은 **주장 기억**이다. 단, 저장 구조는 "원본 파일 + 파생 인덱스"의 이중 구조로 만들어 — 원본에는 MemOS/AX의 라이프사이클 방법론을, 파생물에는 codebase-memory-mcp의 신선도 방법론을 각각 적용한다. 이 결합이 이 설계의 중심 아이디어다.

## 2. 설계 원칙 (소스 추적 가능)

1. **원본이 콘텐츠를 소유한다** — 기억의 정본은 사람이 읽고 git으로 관리할 수 있는 마크다운 파일. SQLite 인덱스·통계·FTS는 전부 파생물이며 `reindex` 한 번으로 폐기·재구성 가능. 파생물에만 존재하는 콘텐츠 금지. *(AX 원칙 1 + codebase-memory 이식 ①)*
2. **판정 없는 자동 덮어쓰기는 없다** — 충돌은 감지·기록·표면화까지가 시스템의 일이고, 판정은 호출 에이전트(및 그 뒤의 사람)가 한다. 시스템은 판정 재료(최신성 신호·중복 근거·신뢰도)를 제공할 뿐. *(AX 판정 게이트의 MCP-native 적응)*
3. **검색은 주입이 아니라 도구다** — recall은 경로+발췌+메타데이터만 반환하고 본문은 에이전트가 읽는다. 질의당 토큰을 1급 성능 지표로 취급. *(AX 원칙 3 + 논문 이식 ①)*
4. **삭제는 상태 전이의 끝이지 시작이 아니다** — 즉시 삭제 대신 archived/deprecated 완충층. purge만이 파일을 지우며 명시적 confirm을 요구. *(MemOS 이식 ①)*
5. **갱신은 비파괴다** — 개정은 supersede 체인(새 기억이 옛 기억을 대체·아카이브)으로, 유래는 링크로 남는다. *(MemOS 이식 ②)*
6. **자기 한계를 1급으로 보고한다** — 파싱 실패 파일, 인덱스 신선도, 미해결 충돌을 status가 항상 노출. "보고 없음 ≠ 완전함". *(codebase-memory 이식 ③ + 논문 이식 ④)*

## 3. 저장 모델

### 3.1 store 레이아웃

```
<store>/                      # 기본: ~/.memory-mcp/default (env MEMORY_MCP_ROOT), 도구별 store 파라미터로 교체 가능
  memories/
    <ulid>.md                 # 기억 원본 (모든 상태의 기억이 여기 있음 — 상태는 frontmatter)
  dictionary.md               # (선택) entity 사전 — 통제 어휘 {이름, 설명, 동의어}
  .derived/
    index.db                  # SQLite 파생 인덱스 — 언제든 삭제 가능
```

- store = 격리 단위. 삭제·백업·공유가 디렉토리 연산 하나 *(codebase-memory 이식 ④)*. store를 git으로 관리하면 purge의 백스톱이 git 이력이 된다 *(AX의 S3 versioning 백스톱과 동형)*.
- hot/cold를 물리 분리하지 않는다 — AX는 팀 규모·용량 때문에 S3 cold가 필요했지만, 개인 store는 상태 필드로 충분하다. 용량이 문제 되는 시점에 `archive/` 서브디렉토리 분리를 검토한다(비목표 §9).

### 3.2 기억 파일 포맷

```markdown
---
id: 01JD2K3A9FZQ4W8XVBH5T6N7RM
type: fact | preference | decision | episode | reference | knowledge
state: active | archived | deprecated
trust: user-stated | agent-inferred | imported
entities: [ads-genisys, pacing]
supersedes: [01JD...]
superseded_by: 01JD...        # 시스템이 기록
source: "2026-08-24 세션, 사용자 교정"
created: 2026-08-24T14:03:00Z
updated: 2026-08-24T14:03:00Z
review_after: 2026-11-01      # (선택) TTL이 아니라 재검토 기한
---
# 제목 한 줄

본문. 첫 문단이 recall 발췌로 쓰인다.
```

- **MemCube의 3분류 메타데이터를 파일 frontmatter로 구현** *(MemOS §MemCube)*: 서술 식별자(id/type/created/source), 거버넌스(state/trust/review_after), 행동 지표(usage heat — 단, 이것은 파생물이므로 DB에만 둔다. 원본 파일을 조회수 때문에 rewrite하지 않는다).
- `trust` 3단계: `user-stated`(사용자 직접 발화·교정 — 최상위) > `agent-inferred`(대화에서 추론) > `imported`(외부 문서 유래). 사용자 정정이 추론 기억을 자동으로 밀어내는 랭킹 근거 *(MemOS 이식 ③)*.
- `review_after`는 삭제 트리거가 아니라 **재검토 기한**이다. 기한이 지나면 status에 떠오를 뿐, 아무것도 자동 삭제되지 않는다 *(원칙 2)*.
- frontmatter는 엄격한 YAML 부분집합(스칼라·문자열 배열)만 허용. 파싱 실패 파일은 색인에서 빠지고 status에 `unparseable`로 보고된다 *(원칙 6)*.

## 4. 라이프사이클 상태기계

```
            remember
               │
               ▼
  ┌──────── active ────────┐
  │          │  ▲          │
  │ supersede│  │ 본문 수정  │ forget(archive)
  │ 되면 자동  │  │ 시 자동    │
  │          ▼  │ 복귀      ▼
  │        archived ◄──────┘
  │          │
  │ forget(deprecate — reason 필수)
  ▼          ▼
       deprecated
             │ forget(purge, confirm 필수)
             ▼
          (파일 삭제 — git 이력이 백스톱)
```

| 상태 | recall 노출 | 의미 |
|---|---|---|
| `active` | 기본 | 현재의 사실 |
| `archived` | `include_archived` 옵트인 | 대체됐거나 뜸해진 기억. 복원 가능 |
| `deprecated` | `include_deprecated` 명시 시만 | **틀린 것으로 판정된** 기억. reason과 함께 보존 — "왜 틀렸는지"도 지식이다 |

- MemOS의 5상태(Generated→Activated→Merged→Archived→Expired)에서 스케줄링용 상태(Activated/Merged)는 **파생 지표(usage heat)로 대체**하고, 원본에는 의사(意思)를 나타내는 상태만 남겼다. AX의 4상태(live/archived/deprecated/purged)와 사실상 동형이다.
- **archived → active 자동 복귀**: reconcile이 archived 기억의 *본문* 변경(사람이 파일을 직접 고침)을 감지하면 active로 승격한다 — 최신 의사 우선 *(AX 상태기계 전이 ③)*. frontmatter만의 변경(시스템이 superseded_by를 쓰는 경우)은 승격을 유발하지 않도록 body 해시를 별도로 둔다.

## 5. 갱신 — supersede 체인

- `remember(supersedes: [old_id])` 또는 `update(action: supersede)`: 새 기억이 active로 생성되고, 옛 기억은 `superseded_by` 기록 + active였다면 archived로 전이. 링크는 양방향으로 파생 인덱스에 저장.
- in-place `edit`도 허용한다(오탈자·보강 — AX의 "archive edit"처럼 상태 유지·재색인만 유발). **의미가 바뀌는 개정은 supersede, 표현만 다듬는 수정은 edit** — 이 구분은 판정이 필요하므로 호출 에이전트의 몫이고, 도구 설명에 명시한다.
- 체인 조회: `read`가 supersedes/superseded_by 체인을 함께 반환 — "왜 이렇게 기억하고 있는가"의 유래 추적 *(MemOS Provenance)*.

## 6. 충돌 게이트 — 판정의 위임

AX 판정 게이트(자동 latest-wins → LLM 판정 → 질문 PR)를 MCP 환경에 맞게 접는다. **MCP 서버 안에는 LLM이 없고, 호출자가 LLM이다.** 따라서 데몬의 LLM 판정·질문 PR 단계를 "도구 응답으로 판정 재료를 반환"으로 치환한다:

1. **감지**: `remember` 시 신규 본문으로 이웃 검색(FTS 상위 K + entity 교집합). 동일 content 해시면 기존 id 반환(멱등, 중복 생성 없음).
2. **재료 제공**: 이웃이 임계 이상이면 응답에 `possible_conflicts: [{id, title, excerpt, state, trust, age_days, latest_wins_eligible}]`를 담아 돌려준다. `latest_wins_eligible`은 유효 시각 차가 모호성 창(기본 7일)을 넘는 경우 — AX의 자동 latest-wins 조건을 **자동 실행 대신 신호로** 제공한다.
3. **판정**: 호출 에이전트가 supersede / keep-both / 자기 기억 폐기를 결정해 후속 도구 호출로 적용한다. 애매하면 에이전트가 사용자에게 물어본다 — AX의 "질문 PR" 역할을 대화가 대신한다.
4. **멱등 가드**: 표면화된 쌍은 `conflicts(pair_hash)`에 기록되어 같은 내용 쌍을 다시 들이밀지 않는다 *(AX 루프 가드 pair-sha)*. 미해결 쌍은 status가 계속 보여준다.

## 7. 풍화 — 감쇠는 랭킹으로, 정리는 제안으로

- **usage heat**: recall에 반환될 때마다 `recall_count`/`last_recalled` 갱신(파생물에만). 랭킹의 열 보정에 쓰인다 *(MemOS 행동 지표)*.
- **랭킹 공식** (결정적, 응답에 구성요소 공개): `score = text_score × trust_w × state_w × (1 + freshness + heat)` — trust_w: user-stated 1.0 / agent-inferred 0.85 / imported 0.7; state_w: active 1.0 / archived 0.5(옵트인 시) / deprecated 0.25(명시 시). 낡고 안 쓰인 기억은 **지워지는 게 아니라 가라앉는다**.
- **compaction 후보 제안**: status가 "오래됐고(기본 30일) 최근 회상되지 않은 active episode"를 후보로 나열한다. 실행은 에이전트가: 같은 주제 episode들을 읽고 `type: knowledge` 요약 기억을 만들어 그것들을 supersede — **증류를 먼저, 이동은 나중** *(AX compaction의 지식-우선 원칙)*. 서버는 어떤 기억도 스스로 아카이브하지 않는다.
- `review_after` 도래 건도 status에 노출 — 시한부 기억(예: "이번 분기까지 유효한 정책")의 재검토 장치.

## 8. 검색 — 한국어를 포함한 하이브리드

임베딩 없이 시작한다(AX와 같은 판단 — 형태소/렉시컬의 효과를 확인한 뒤 벡터를 검토). SQLite FTS5 이중 인덱스 + 질의 계획기:

| 토큰 유형 | 경로 |
|---|---|
| 라틴/숫자 | unicode61 word FTS, prefix 질의(`tok*`) — BM25 |
| CJK ≥3자 | trigram FTS 구절 질의(`"토큰"`) + word-prefix 병행 |
| CJK ≤2자 | word-prefix(`끄*` → 끄는/끄고) + LIKE 폴백 스캔 |

- 조사·어미 변형("보안을/보안이", "끄는/끄고")을 prefix·trigram 조합으로 근사한다. nori급 형태소 분석은 비목표 — 그 수준이 필요해지면 외부 분석기 연동을 검토(§9). 실측 근거: node:sqlite(SQLite 3.50) trigram은 3자 미만 토큰을 매칭하지 못하고, 구절은 따옴표가 필요함 — 계획기가 이를 흡수한다.
- 후보 병합 후 §7 랭킹 적용. `entities` 파라미터는 사전 정규화를 거쳐 교집합 필터로.
- recall 응답은 항상 **인덱스 신선도와 검색 한계**를 함께 반환한다(마지막 reconcile 시각, unparseable 수) *(원칙 6)*.

## 9. reconcile — 데몬 없는 수렴

AX의 60초 데몬 사이클을 **call-time lazy reconcile**로 접는다. MCP 서버는 세션과 함께 뜨고 지므로 상주 데몬이 부적합하고, 파생물이 임베디드 SQLite라 수렴 비용이 싸기 때문이다:

- 모든 도구 호출 진입 시: memories/ 전체 stat 스캔 → `mtime_ns+size` 불일치 파일만 sha256 → 변경분만 재파싱·upsert, 디스크에 없는 행은 삭제 *(codebase-memory 3단 증분: stat-gate → 해시 → 부분 재색인)*. 직전 reconcile 2초 이내면 skip(디바운스).
- 이벤트 워처를 두지 않는다 — 호출 시점 수렴으로 충분하고, "이벤트는 힌트일 뿐 판정은 해시"라는 원칙의 극한 적용이다.
- `reindex(verify: true)` = 전량 재해싱 감사 — AX의 주 1회 감사를 온디맨드 도구로 *(mtime 보존 복사로 생기는 감지 구멍까지 잡는 경로)*.
- 사전(dictionary.md) 버전 = 콘텐츠 해시로 추적, 바뀌면 entity 정규화 재적용 — **비파일 입력도 해시로 무효화** *(codebase-memory 가상 파일 트릭)*.

## 10. 도구 표면 (7개)

최소 표면 원칙 *(AX "도구 2개" 정신 + 논문의 목적특화 타입드 도구)*. 모든 도구는 `store` 파라미터(절대경로, 생략 시 기본 store)를 받고, 구조화 JSON을 반환한다.

| 도구 | 역할 | 응답의 핵심 |
|---|---|---|
| `memory_remember` | 기억 생성 (+supersedes) | id, 경로, `possible_conflicts`, entity 후보 |
| `memory_recall` | 하이브리드 검색 | 경로+발췌+점수 구성요소+메타, 인덱스 신선도 |
| `memory_read` | id/경로로 전문 + 체인 | 본문, supersedes 체인, 충돌 이력 |
| `memory_update` | edit / supersede / set_state / link / resolve_conflict | 적용된 전이 |
| `memory_forget` | archive / deprecate(reason 필수) / purge(confirm 필수) | 전이 결과 — 자동 삭제 없음 |
| `memory_status` | 상태·타입별 집계, 미해결 충돌, review 도래, compaction 후보, unparseable, 신선도 | 자기 한계 보고 |
| `memory_reindex` | 파생물 전체 재구성 / verify 감사 | 재구성 통계, drift 보고 |

- 보안: id 형식 검증, 경로 접근은 store 루트 realpath 컨테인먼트, purge는 confirm 플래그 *(논문 이식 ⑤)*.

## 11. 비목표 (v1)

- **프롬프트 자동 주입 RAG** — 검색은 도구다 *(AX 비목표와 동일)*
- **서버 내 LLM 호출** — 요약·판정은 호출 에이전트의 일. 서버는 결정적으로만 동작
- **임베딩 벡터 검색** — 렉시컬 효과 확인 후 (AX 일정 7과 동일하게 보류)
- **hot/cold 물리 분리(S3)** — 개인 스케일에선 상태 필드로 충분
- **형태소 분석기 내장** — prefix/trigram 근사로 시작
- **멀티 유저 권한 모델** — store 파일 권한에 위임 (MemOS 3항 권한은 공유 store 도입 시점에)

## 12. 소스 → 결정 추적표

| 결정 | 출처 |
|---|---|
| 원본 md + 파생 SQLite 이중 구조 | AX 원칙 1 · codebase-memory ① |
| store=디렉토리 격리, git 백스톱 | codebase-memory ④ · AX S3 versioning |
| frontmatter 메타데이터 3분류 | MemOS MemCube |
| 상태기계 + archived 완충 + deprecated 보존 | MemOS ① · AX 상태기계 |
| 본문 수정 시 archived→active 자동 복귀 | AX 전이 ③ (최신 의사 우선) |
| supersede 체인·비파괴 개정 | MemOS ② |
| trust×state×heat 랭킹, 가라앉는 풍화 | MemOS ③ + usage heat |
| 충돌: 감지→재료 제공→판정 위임, latest-wins는 신호로만 | AX 판정 게이트 (무데몬 적응) |
| pair-hash 멱등 가드 | AX 루프 가드 |
| stat-gate→해시→부분 재색인, call-time reconcile | codebase-memory ② · AX 데몬 사이클 |
| verify 전량 재해싱 감사 | AX 주간 감사 |
| 사전 버전 해시로 재정규화 | AX entity 사전 + codebase-memory 가상 파일 |
| 검색=경로+발췌, 토큰 경제 | AX 원칙 3 · 논문 ① |
| 커버리지·신선도 정직 보고 | codebase-memory ③ · 논문 ④ |
| compaction = 지식 증류 우선, 제안만 | AX compaction |
| 파생 연결에 근거·신뢰도 동봉 | 논문 ② (6단계 캐스케이드) |
| 해시 용도 분리(stat/sha256) | 논문 ③ |
| 경로 컨테인먼트·confirm 플래그 | 논문 ⑤ (보안 하드닝) |

## 13. 스택

TypeScript(ESM, erasable syntax — Node 22.5+에서 빌드 없이 실행 가능) · `node:sqlite`(외부 네이티브 의존성 0) · `@modelcontextprotocol/sdk` + `zod` · biome · `node --test`. 빌드는 tsc(`rewriteRelativeImportExtensions`)로 dist 산출. 런타임 의존성을 SDK와 zod 둘로 억제 — codebase-memory-mcp의 zero-dependency 철학의 현실적 타협.
