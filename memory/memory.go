// Package memory defines workspace-scoped long-term memory contracts.
package memory

import (
	"context"
	"time"
)

const SearchToolName = "search_memory"

type Category string

const (
	CategoryPreference   Category = "preference"
	CategoryCorrection   Category = "correction"
	CategoryConvention   Category = "convention"
	CategoryDecision     Category = "decision"
	CategoryVerifiedFact Category = "verified_fact"
)

// Item is one durable, workspace-scoped memory. Source fields are empty for
// memories explicitly added by the host.
type Item struct {
	ID                string    `json:"id"`
	Category          Category  `json:"category"`
	Content           string    `json:"content"`
	Importance        int       `json:"importance"`
	SourceSessionID   string    `json:"source_session_id,omitempty"`
	SourceExecutionID string    `json:"source_execution_id,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// Input is used by hosts to create or replace a memory.
type Input struct {
	Category   Category `json:"category"`
	Content    string   `json:"content"`
	Importance int      `json:"importance,omitempty"`
}

// Candidate is a model-proposed memory. Basis must quote supporting text from
// the current accepted task; it is checked before the candidate is persisted.
type Candidate struct {
	Category   Category `json:"category"`
	Content    string   `json:"content"`
	Importance int      `json:"importance"`
	Basis      string   `json:"basis"`
}

type SearchResult struct {
	Item  Item    `json:"item"`
	Score float64 `json:"score,omitempty"`
}

// Embedder converts texts to vectors for optional semantic retrieval. ID must
// remain stable for one embedding model and vector format.
type Embedder interface {
	ID() string
	Embed(context.Context, []string) ([][]float32, error)
}
