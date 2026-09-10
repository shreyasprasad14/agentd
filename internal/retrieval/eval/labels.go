// Package eval measures retrieval quality against a hand-labeled query set:
// recall@k and MRR for each retrieval mode, with thresholds that turn a
// regression into a nonzero exit (spec §12; the M5 harness folds this in).
package eval

import (
	"fmt"
	"io"

	"go.yaml.in/yaml/v3"
)

// Labels is the checked-in label file.
type Labels struct {
	// Thresholds maps mode key (vector, bm25, hybrid, hybrid_rerank) to
	// metric key (recall_at_8, recall_at_20, recall_at_50, mrr) to the
	// minimum acceptable value.
	Thresholds map[string]map[string]float64 `yaml:"thresholds"`
	Cases      []Case                        `yaml:"cases"`
}

// Case is one labeled query.
type Case struct {
	Query    string     `yaml:"query"`
	Relevant []Relevant `yaml:"relevant"`
}

// Relevant is one known-relevant item. Without Ordinals, any chunk of the
// document counts (document-level label); with them, only those chunks do.
type Relevant struct {
	SourceID string `yaml:"source_id"`
	Ordinals []int  `yaml:"ordinals,omitempty"`
}

// Matches reports whether a hit satisfies this label.
func (r Relevant) Matches(sourceID string, ordinal int) bool {
	if sourceID != r.SourceID {
		return false
	}
	if len(r.Ordinals) == 0 {
		return true
	}
	for _, o := range r.Ordinals {
		if o == ordinal {
			return true
		}
	}
	return false
}

// ReadLabels parses and validates a label file.
func ReadLabels(r io.Reader) (*Labels, error) {
	var l Labels
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&l); err != nil {
		return nil, err
	}
	if len(l.Cases) == 0 {
		return nil, fmt.Errorf("label file has no cases")
	}
	for i, c := range l.Cases {
		if c.Query == "" {
			return nil, fmt.Errorf("case %d: empty query", i)
		}
		if len(c.Relevant) == 0 {
			return nil, fmt.Errorf("case %d (%q): no relevant items", i, c.Query)
		}
		for j, rel := range c.Relevant {
			if rel.SourceID == "" {
				return nil, fmt.Errorf("case %d relevant %d: missing source_id", i, j)
			}
		}
	}
	return &l, nil
}
