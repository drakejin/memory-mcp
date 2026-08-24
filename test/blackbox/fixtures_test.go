//go:build blackbox

package blackbox

// Fixtures for the §10 scenarios. Markers (agedmarkerx, stalemarkerx,
// degradedmarkerx, seoulnine) are single unique tokens so full-text hits and
// absences are unambiguous. Korean sentences exercise the nori morphology
// path; the spec's own example pair 보안을 끄고 / 보안을 끄는 is fixture 0.

// koreanEpisodeTexts are the three episodes of scenario 2. Index 0 carries
// the nori target phrase "보안을 끄고" which must be found by noriQuery.
var koreanEpisodeTexts = []string{
	"오늘 OpenSearch 보안을 끄고 단일 노드 모드로 컨테이너를 띄웠다.",
	"Neo4j 그래프 데이터베이스를 볼륨 없이 실행했고 재수화로 복구를 검증했다.",
	"한국어 형태소 분석을 위해 nori 플러그인을 오픈서치 이미지에 설치했다.",
}

// noriQuery must hit koreanEpisodeTexts[0] via morphological analysis
// (끄는 → 끄 → matches 끄고), per §10 scenario 2.
const noriQuery = "보안을 끄는"

// Scenario 3 supersede-chain fixture: fact1 is superseded by fact3.
const (
	fact1Name = "opensearch-security-policy"
	fact1Body = "로컬 개발 환경에서는 OpenSearch 보안 플러그인을 끈다"
	fact2Name = "neo4j-auth-policy"
	fact2Body = "Neo4j 로컬 인증은 고정 비밀번호 djmemory-local 을 쓴다"
	fact3Name = "opensearch-security-policy-v2"
	fact3Body = "보안 플러그인은 환경 변수 DISABLE_SECURITY_PLUGIN=true 로 끈다"
)

// pdfFixtureLines feed makeMinimalPDF for scenario 4; pdfQueryToken is the
// unique chunk-search token.
var pdfFixtureLines = []string{
	"memory-mcp blackbox acceptance fixture seoulnine",
	"This PDF carries a real extractable text layer.",
	"Korean morphology is covered by the episode fixtures.",
}

const pdfQueryToken = "seoulnine"

// Scenario 6 fixtures: both are 40 days old; agedText gets consolidated=true
// and must sink to cold, staleText stays unconsolidated and must remain hot
// forever (§3.1), surfacing as stale_unconsolidated in /status.
const (
	agedText  = "agedmarkerx 통합이 이미 완료된 40일 지난 에피소드 기록이다."
	staleText = "stalemarkerx 아직 통합되지 않은 40일 지난 에피소드는 hot에 남는다."
)

// Scenario 7 fixture: written while OpenSearch is down.
const degradedText = "degradedmarkerx 오픈서치가 내려간 동안에도 기록은 hot에 저장된다."
