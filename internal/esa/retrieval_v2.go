package esa

import (
	"sort"
	"strings"

	"esa/internal/retrieval"
)

type RetrievalTrace struct {
	Version    string  `json:"version"`
	CorpusHash string  `json:"corpus_hash"`
	BM25Rank   int     `json:"bm25_rank"`
	SignalRank int     `json:"signal_rank"`
	RRFScore   float64 `json:"rrf_score"`
}

func MatchCasesV2(t Ticket, cases []Case) []Match {
	eligible := []Case{}
	docs := []retrieval.Document{}
	for _, c := range cases {
		if c.ProjectID != t.ProjectID || c.TicketID == t.ID || c.CreatedAt.IsZero() || c.UpdatedAt.IsZero() || c.TrustedAt.IsZero() {
			continue
		}
		eligible = append(eligible, c)
		docs = append(docs, retrieval.Document{ID: c.ID, Text: c.Subject + "\n" + c.Summary + "\n" + strings.Join(c.Keywords, " ")})
	}
	lexical, corpus := retrieval.BM25(ticketText(t), docs)
	lexIDs := []string{}
	lexRanks := map[string]int{}
	for index, hit := range lexical {
		lexIDs = append(lexIDs, hit.ID)
		lexRanks[hit.ID] = index + 1
	}
	matches := matchCases(t, eligible, 0)
	signalIDs := []string{}
	signalRanks := map[string]int{}
	for _, m := range matches {
		for _, c := range eligible {
			if c.ID == m.CaseID && len(intersects(signals(t), c.Signals)) > 0 {
				signalIDs = append(signalIDs, c.ID)
				signalRanks[c.ID] = len(signalIDs)
			}
		}
	}
	scores := retrieval.RRF(lexIDs, signalIDs)
	byID := map[string]Case{}
	for _, c := range eligible {
		byID[c.ID] = c
	}
	for index := range matches {
		m := &matches[index]
		c := byID[m.CaseID]
		m.Retrieval = &RetrievalTrace{Version: "bm25-rrf-1", CorpusHash: corpus, BM25Rank: lexRanks[c.ID], SignalRank: signalRanks[c.ID], RRFScore: scores[c.ID]}
		// Relevance never relaxes the repair gate. A generic exception is not
		// enough; require contextual terms and complete affected-version coverage.
		if len(intersects(keywords(ticketText(t)), c.Keywords)) < 4 || len(c.Environment) == 0 || len(intersects(t.Versions, c.Versions)) != len(unique(t.Versions)) || len(intersects(t.Components, c.Components)) != len(unique(t.Components)) {
			m.Applicable = false
			m.Solution = ""
			m.Reasons = append(m.Reasons, "V2 repair gate requires contextual evidence and complete scope coverage")
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Applicable != matches[j].Applicable {
			return matches[i].Applicable
		}
		if matches[i].Retrieval.RRFScore == matches[j].Retrieval.RRFScore {
			return matches[i].CaseID < matches[j].CaseID
		}
		return matches[i].Retrieval.RRFScore > matches[j].Retrieval.RRFScore
	})
	if len(matches) > 5 {
		matches = matches[:5]
	}
	return matches
}
