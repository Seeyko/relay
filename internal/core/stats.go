package core

import (
	"sort"

	"github.com/Seeyko/relay/internal/store"
)

type TopPost struct {
	ID        string           `json:"id"`
	AccountID string           `json:"account_id"`
	URL       string           `json:"url"`
	Text      string           `json:"text"`
	Value     int64            `json:"value"`
	Metrics   map[string]int64 `json:"metrics"`
}

type Summary struct {
	Posts     int                         `json:"posts"`
	Metric    string                      `json:"metric"`
	ByAccount map[string]map[string]int64 `json:"by_account"`
	Top       []TopPost                   `json:"top"`
}

// Summarize totals metrics per account and ranks posts by one metric.
func Summarize(posts []*store.Post, metric string, top int) Summary {
	if metric == "" {
		metric = "like_count"
	}
	if top <= 0 {
		top = 10
	}
	s := Summary{Posts: len(posts), Metric: metric, ByAccount: map[string]map[string]int64{}}
	var ranked []TopPost
	for _, p := range posts {
		acc := s.ByAccount[p.AccountID]
		if acc == nil {
			acc = map[string]int64{}
			s.ByAccount[p.AccountID] = acc
		}
		acc["posts"]++
		for k, v := range p.Metrics {
			acc[k] += v
		}
		text := ""
		if len(p.Parts) > 0 {
			text = p.Parts[0]
		}
		ranked = append(ranked, TopPost{ID: p.ID, AccountID: p.AccountID, URL: p.RemoteURL, Text: text, Value: p.Metrics[metric], Metrics: p.Metrics})
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].Value > ranked[j].Value })
	if len(ranked) > top {
		ranked = ranked[:top]
	}
	s.Top = ranked
	return s
}
