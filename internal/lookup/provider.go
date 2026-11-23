package lookup

import (
	"context"
	"errors"
	"strings"
	"unicode"

	"pourops/internal/platform"
)

var (
	ErrNotFound  = errors.New("未找到该酒款详情")
	ErrInvalidID = platform.UserErr("无效的酒款 ID")
)

type Candidate struct {
	Source     string `json:"source"`
	ID         string `json:"id"`
	Brewery    string `json:"brewery"`
	Name       string `json:"name"`
	NameEN     string `json:"name_en,omitempty"`
	Style      string `json:"style,omitempty"`
	Rating     string `json:"rating,omitempty"`
	PosterURL  string `json:"poster_url,omitempty"`
	IsDraft    bool   `json:"is_draft,omitempty"`
	HasPricing bool   `json:"has_pricing,omitempty"`
}

type Response struct {
	Items   []Candidate `json:"items"`
	Warning string      `json:"warning,omitempty"`
}

type Detail struct {
	Source      string `json:"source"`
	SourceLabel string `json:"source_label"`
	Brewery     string `json:"brewery"`
	Name        string `json:"name"`
	Style       string `json:"style"`
	Category    string `json:"category"`
	ABV         string `json:"abv"`
	IBU         string `json:"ibu"`
	UTRating    string `json:"ut_rating"`
	UTCount     string `json:"ut_count"`
	Tags        string `json:"tags"`
	PosterURL   string `json:"poster_url"`
	CapacityML  string `json:"capacity_ml"`
	Cost        string `json:"cost"`
	Price150    string `json:"price_150"`
	Price330    string `json:"price_330"`
	Price500    string `json:"price_500"`
}

// Provider 是酒名自动搜索的编译期扩展接口；新增数据源（本地库、酒核、酒商报价接口等）只需实现本接口并注册到 Service。
type Provider interface {
	Name() string
	Label() string
	Enabled() bool
	SupportsEmptyQuery() bool
	Search(ctx context.Context, q string, limit int) ([]Candidate, error)
	Detail(ctx context.Context, id string) (Detail, error)
}

func dedupKey(c Candidate) string {
	norm := func(s string) string {
		var b strings.Builder
		for _, r := range strings.ToLower(strings.TrimSpace(s)) {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	return norm(c.Brewery) + "|" + norm(c.Name)
}
