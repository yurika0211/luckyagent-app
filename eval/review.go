package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Review is a human override of one case's latest automatic verdict.
// The automatic verdict stays on the attempt. Reports show both.
type Review struct {
	CaseID    string    `json:"case_id"`
	Verdict   Verdict   `json:"verdict"`
	Note      string    `json:"note"`
	Reviewer  string    `json:"reviewer,omitempty"`
	Attempt   int       `json:"attempt"`
	Original  Verdict   `json:"original_verdict"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (s *Store) reviewsPath(runID string) string {
	return filepath.Join(s.runDir(runID), "reviews.json")
}

func (s *Store) LoadReviews(runID string) (map[string]Review, error) {
	path := s.reviewsPath(runID)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]Review{}, nil
		}
		return nil, fmt.Errorf("read reviews: %w", err)
	}
	var list []Review
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("decode reviews: %w", err)
	}
	out := make(map[string]Review, len(list))
	for _, item := range list {
		out[item.CaseID] = item
	}
	return out, nil
}

// SaveReview records a human verdict without rewriting attempts.jsonl.
// The original automatic verdict is copied onto the review so a later report
// can still show what the validators decided.
func (s *Store) SaveReview(runID string, review Review) (Review, error) {
	switch review.Verdict {
	case VerdictPass, VerdictFail, VerdictSkip, VerdictUnknown:
	default:
		return Review{}, fmt.Errorf("review verdict must be pass, fail, skip or unknown")
	}
	if strings.TrimSpace(review.Note) == "" {
		return Review{}, fmt.Errorf("review note is required")
	}
	manifest, err := s.LoadManifest(runID)
	if err != nil {
		return Review{}, err
	}
	found := false
	for _, c := range manifest.Dataset.Cases {
		if c.ID == review.CaseID {
			found = true
			break
		}
	}
	if !found {
		return Review{}, fmt.Errorf("case %q is not in the frozen manifest", review.CaseID)
	}
	attempts, err := s.Attempts(runID)
	if err != nil {
		return Review{}, err
	}
	latest := LatestAttempts(attempts)
	auto, ok := latest[review.CaseID]
	if !ok {
		return Review{}, fmt.Errorf("case %q has no attempt to review", review.CaseID)
	}
	review.Attempt = auto.Attempt
	review.Original = auto.Verdict
	review.Note = strings.TrimSpace(review.Note)
	review.Reviewer = strings.TrimSpace(review.Reviewer)
	review.UpdatedAt = s.now()

	all, err := s.LoadReviews(runID)
	if err != nil {
		return Review{}, err
	}
	all[review.CaseID] = review
	list := make([]Review, 0, len(all))
	for _, item := range all {
		list = append(list, item)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CaseID < list[j].CaseID })
	if err := writeJSON(s.reviewsPath(runID), list); err != nil {
		return Review{}, err
	}
	return review, nil
}
