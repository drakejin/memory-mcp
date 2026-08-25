package episodemem

// IndexName is the single episodic index. All projects share it; scoping
// happens through the workspace/team/project keyword fields so one bulk
// rehydration pass can rebuild everything (architecture-v2.md §5).
const IndexName = "dj-memory-episodic"

// koreanAnalyzer is the custom nori analyzer declared in indexBody. Its
// presence in a live index's settings is the probe used to tell a properly
// mapped index from one OpenSearch auto-created with a dynamic mapping.
const koreanAnalyzer = "korean"

// indexBody is the index creation payload: single-shard (local single-node
// container, §8) with a nori-based Korean analyzer for the text field (§2).
//
// Analyzer notes:
//   - nori_tokenizer with decompound_mode=mixed keeps both compound tokens
//     and their parts, improving recall on compounds.
//   - nori_part_of_speech drops particles/endings (J, E, ...) so "보안을
//     끄고" and "보안을 끄는" both reduce to the stems 보안 + 끄 (§10-2).
//   - nori_readingform normalizes hanja to hangul; lowercase covers latin.
//
// Field notes: occurred_at is a date (RFC3339 via the default
// strict_date_optional_time format); last_recalled stays a keyword because
// the domain uses "" for "never recalled", which a date field would reject.
const indexBody = `{
  "settings": {
    "index": {
      "number_of_shards": 1,
      "number_of_replicas": 0
    },
    "analysis": {
      "tokenizer": {
        "korean_nori": {
          "type": "nori_tokenizer",
          "decompound_mode": "mixed",
          "discard_punctuation": "true"
        }
      },
      "filter": {
        "korean_pos": {
          "type": "nori_part_of_speech",
          "stoptags": [
            "E", "IC", "J", "MAG", "MAJ", "MM",
            "SP", "SSC", "SSO", "SC", "SE",
            "XPN", "XSA", "XSN", "XSV", "UNA", "NA", "VSV"
          ]
        }
      },
      "analyzer": {
        "korean": {
          "type": "custom",
          "tokenizer": "korean_nori",
          "filter": ["korean_pos", "nori_readingform", "lowercase"]
        }
      }
    }
  },
  "mappings": {
    "dynamic": "strict",
    "properties": {
      "workspace":    { "type": "keyword" },
      "team":         { "type": "keyword" },
      "project":      { "type": "keyword" },
      "id":           { "type": "keyword" },
      "kind":         { "type": "keyword" },
      "occurred_at":  { "type": "date" },
      "actor":        { "type": "keyword" },
      "text":         { "type": "text", "analyzer": "korean" },
      "entities":     { "type": "keyword" },
      "refs": {
        "properties": {
          "doc_sha":   { "type": "keyword" },
          "chunk_seq": { "type": "integer" }
        }
      },
      "consolidated":  { "type": "boolean" },
      "recall_count":  { "type": "integer" },
      "last_recalled": { "type": "keyword" }
    }
  }
}`
