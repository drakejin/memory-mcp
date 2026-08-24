# memory-mcp

에이전트의 장기 기억을 관리하는 로컬 MCP 서버.
기억을 "저장했다가 꺼내는 것"이 아니라 **태어나고, 회상되고, 개정되고, 판정받고, 가라앉는 라이프사이클**로 다룬다.

## 핵심 아이디어

에이전트 기억은 두 종류다. 정본(ground truth)이 있는 기억(코드 인덱스류)은 해시 동기화면 충분하지만, **정본이 없는 주장 기억**(사용자 발화·결정·교훈)은 상태·버전·신뢰도·판정 게이트가 필요하다. memory-mcp는 이 둘을 이중 구조로 결합한다:

- **원본** — 사람이 읽고 git으로 관리하는 마크다운 파일 (`<store>/memories/*.md`, frontmatter 메타데이터). 라이프사이클 상태·supersede 체인·신뢰도가 여기 산다.
- **파생물** — SQLite 인덱스 (FTS·랭킹·충돌 대장). 언제든 폐기하고 `memory_reindex` 한 번으로 원본에서 전부 재구성한다.

세 소스를 분석해 설계했다 (근거 추적은 [docs/design/architecture.md](docs/design/architecture.md) §12):

| 소스 | 가져온 것 |
|---|---|
| [codebase-memory-mcp 실측 분석](docs/analyze/codebase-memory-mcp.md) | 원본–파생물 분리, stat-gate→해시→부분 재색인, 커버리지 정직성, store=디렉토리 격리 |
| [Codebase-Memory 논문 (arXiv:2603.27277)](docs/analyze/codebase-memory-paper-analysis.md) | 토큰 경제(경로+발췌만 반환), 파생 연결에 신뢰도 동봉, 경로 컨테인먼트 |
| [MemOS 논문 (arXiv:2507.03724)](docs/analyze/memos-memory-os-analysis.md) | 상태기계 + archived 완충층, 비파괴 supersede 버전 체인, trust×state×heat 랭킹 |
| AX workspace memory lifecycle policy | 판정 게이트(자동 덮어쓰기 금지), pair-hash 멱등, latest-wins 신호, 증류 우선 compaction, entity 사전 |

## 동작 원리

```
remember ──► active ◄─────────── 본문 수정 시 자동 복귀
               │ supersede 되면 자동
               ▼
            archived ──forget(deprecate, reason 필수)──► deprecated
               │                                            │
               └────────── forget(purge, confirm 필수) ──────┘
                            (파일 삭제 — git 이력이 백스톱)
```

- **충돌 게이트**: `memory_remember`가 이웃 기억을 감지하면 판정 재료(공유 entity, 시각 차, `latest_wins_eligible`)를 응답에 담아 돌려준다. 판정은 호출 에이전트(와 사용자)의 몫 — **판정 없는 자동 덮어쓰기는 없다.** 표면화된 쌍은 pair-hash로 기록되어 두 번 들이밀지 않는다.
- **풍화**: 낡고 안 쓰인 기억은 지워지지 않고 랭킹에서 **가라앉는다** — `score = text × trust × state × (1 + freshness + heat)`. `memory_status`가 오래된 episode를 증류(compaction) 후보로 제안하면, 에이전트가 `knowledge` 기억으로 요약하고 원본들을 supersede한다.
- **reconcile**: 데몬 없음. 모든 도구 호출 진입 시 stat-gate(mtime+size) → sha256 → 변경분만 재색인. 손으로 파일을 고쳐도 다음 호출에 수렴한다. archived 기억의 본문을 직접 고치면 active로 자동 복귀(최신 의사 우선).
- **한국어 검색**: 임베딩 없이 word FTS(prefix로 조사 변형 흡수) + trigram FTS(구절) + 2자 이하 LIKE 폴백을 질의 계획기가 조합한다. "보안을 끄는" ↔ "보안을 끄고" 매칭.
- **정직성**: recall/status가 인덱스 신선도·unparseable 파일·미해결 충돌을 항상 보고한다. 보고 없음 ≠ 완전함.

## 도구 (7개)

| 도구 | 역할 |
|---|---|
| `memory_remember` | 기억 생성(+supersedes). 중복은 멱등, 충돌은 판정 재료로 반환 |
| `memory_recall` | 하이브리드 검색 — 경로+발췌+점수 구성요소만 (본문 주입 없음) |
| `memory_read` | id/경로로 전문 + supersede 체인 + 충돌 이력 |
| `memory_update` | edit / supersede / set_state / link / resolve_conflict |
| `memory_forget` | archive / deprecate(reason 필수) / purge(confirm 필수) — 자동 삭제 없음 |
| `memory_status` | 집계·미해결 충돌·review 도래·증류 후보·unparseable·신선도 |
| `memory_reindex` | 파생물 전체 재구성(재해 복구) / verify 전량 재해싱 감사 |

## 설치

Node ≥ 22.5 (node:sqlite 내장 — 외부 네이티브 의존성 없음).

```bash
pnpm install && pnpm build
```

Claude Code 등록:

```bash
claude mcp add memory-mcp -- node /path/to/memory-mcp/dist/main.js
```

또는 `.mcp.json`:

```json
{
  "mcpServers": {
    "memory-mcp": {
      "type": "stdio",
      "command": "node",
      "args": ["/path/to/memory-mcp/dist/main.js"],
      "env": { "MEMORY_MCP_ROOT": "/path/to/your/store" }
    }
  }
}
```

기본 store는 `~/.memory-mcp/default` (env `MEMORY_MCP_ROOT`로 교체, 도구별 `store` 파라미터로 호출 단위 교체). **store를 git으로 관리하면 purge의 백스톱이 git 이력이 된다.**

## 기억 파일 포맷

```markdown
---
id: 01JD2K3A9FZQ4W8XVBH5T6N7RM
type: fact | preference | decision | episode | reference | knowledge
state: active | archived | deprecated
trust: user-stated | agent-inferred | imported
entities: [ads-genisys, pacing]
source: "2026-08-24 세션, 사용자 교정"
created: 2026-08-24T14:03:00Z
updated: 2026-08-24T14:03:00Z
review_after: 2026-11-01
---
# 제목 한 줄

본문. 첫 문단이 recall 발췌로 쓰인다.
```

`<store>/dictionary.md`를 두면 entity 표기가 정규화된다 (`- 정식명: 설명 (aka: 동의어, 약어)`).

## 개발

```bash
pnpm typecheck   # tsc --noEmit
pnpm test        # node --test (29 tests)
pnpm lint        # biome
pnpm build       # dist/
```

## 문서

- 설계: [docs/design/architecture.md](docs/design/architecture.md) — 원칙·상태기계·소스→결정 추적표
- 분석: [codebase-memory-mcp 실측](docs/analyze/codebase-memory-mcp.md) · [Codebase-Memory 논문](docs/analyze/codebase-memory-paper-analysis.md) · [MemOS 논문](docs/analyze/memos-memory-os-analysis.md)

## License

MIT
