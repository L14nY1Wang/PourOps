package platform

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestToneOf(t *testing.T) {
	cases := map[string]string{
		"IPA - New England": "hop",
		"Pilsner - German":  "hop",
		"波西米亚比尔森":           "hop",
		"淡色拉格":              "hop",
		"德式拉格":              "fresh",
		"Imperial Stout":    "dark",
		"柏林酸小麦":             "sour",
		"比利时三料":             "strong",
		"未知风格":              "fresh",
	}
	for style, want := range cases {
		if got := ToneOf(style); got != want {
			t.Errorf("ToneOf(%q) = %q, want %q", style, got, want)
		}
	}
}

func TestMoneyCentsAndStrictBeerValidation(t *testing.T) {
	good := map[string]int{
		"":      0,
		"45":    4500,
		"¥45.5": 4550,
		"1,200": 120000,
		"1,350": 135000,
	}
	for in, want := range good {
		got, err := MoneyCents(in)
		if err != nil || got != want {
			t.Errorf("MoneyCents(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"-1", "abc", ",", "30000000"} {
		if _, err := MoneyCents(bad); err == nil {
			t.Errorf("MoneyCents(%q) should fail", bad)
		}
	}

	form := url.Values{"name": {"测试酒"}, "abv": {"120"}}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_ = req.ParseForm()
	if _, err := ParseBeerForm(req); err == nil {
		t.Fatal("ParseBeerForm should reject ABV > 100")
	}

	if n, err := ParseSortOrder("-2147483647"); err != nil || n != -math.MaxInt32 {
		t.Fatalf("ParseSortOrder = %d, %v", n, err)
	}
}

func TestPriceCentsAndText(t *testing.T) {
	cases := map[string]int{"45": 4500, "45.5": 4550, "¥ 45": 4500, "￥168": 16800, "": 0, "abc": 0}
	for in, want := range cases {
		if got := PriceCents(in); got != want {
			t.Errorf("PriceCents(%q) = %d, want %d", in, got, want)
		}
	}
	if got := PriceText(4500); got != "¥ 45" {
		t.Errorf("PriceText(4500) = %q", got)
	}
	if got := PriceText(4550); got != "¥ 45.50" {
		t.Errorf("PriceText(4550) = %q", got)
	}
}

func TestSplitList(t *testing.T) {
	got := SplitList("美式IPA、热带水果, 苦爽；")
	if len(got) != 3 || got[0] != "美式IPA" || got[2] != "苦爽" {
		t.Fatalf("SplitList = %#v", got)
	}
	if got := SplitList("https://assets.untappd.com/x.jpeg"); len(got) != 1 {
		t.Fatalf("URL 被拆分: %#v", got)
	}
}

func TestURLMsgRoundTrip(t *testing.T) {
	for _, msg := range []string{"已删除", "删除失败: 外键 & 约束 #1?", "a+b c", "100% 保证金", ""} {
		u, err := url.Parse("/admin/beers?msg=" + URLMsg(msg))
		if err != nil {
			t.Fatalf("URLMsg(%q) 生成的 URL 非法: %v", msg, err)
		}
		if got := u.Query().Get("msg"); got != msg {
			t.Errorf("URLMsg 往返 %q → %q", msg, got)
		}
	}
}

func TestOptFloatRejectsNonFinite(t *testing.T) {
	for _, in := range []string{"NaN", "Inf", "+Inf", "-Inf", "1e999", "abc"} {
		if got := OptFloat(in); got != nil {
			t.Errorf("OptFloat(%q) = %v，应拒绝", in, *got)
		}
	}
	if got := OptFloat(" 5.2 "); got == nil || *got != 5.2 {
		t.Errorf("OptFloat(5.2) = %v", got)
	}
}

func TestStrictBeerValidationAndUserErrorSanitization(t *testing.T) {
	if _, ok := UserErrorMessage(errors.New("pq: integer out of range")); ok {
		t.Fatal("普通系统/DB 错误不应被当成用户可展示错误")
	}
	if msg, ok := UserErrorMessage(fmt.Errorf("第 2 行：%w", UserErr("金额过大"))); !ok || msg != "第 2 行：金额过大" {
		t.Fatalf("包装后的 UserError 应保留完整中文上下文，实际 ok=%v msg=%q", ok, msg)
	}

	badForms := []struct {
		name  string
		field string
		val   string
		want  string
	}{
		{"空酒名", "name", "  ", "酒款名称不能为空"},
		{"ABV超100", "abv", "105", "ABV"},
		{"ABV非数字", "abv", "abc", "ABV"},
		{"IBU为负", "ibu", "-1", "IBU"},
		{"IBU超int4", "ibu", "2147483648", "IBU"},
		{"UT超5", "ut_rating", "5.5", "UT"},
		{"评分人数超int4", "ut_count", "2147483648", "评分人数"},
		{"未知分类色", "category", "rainbow", "分类色"},
		{"危险酒标", "poster_url", "javascript:alert(1)", "酒标"},
		{"杯价为负", "price_330", "-5", "330ml"},
		{"杯价溢出", "price_500", "30000000", "500ml"},
	}
	for _, tc := range badForms {
		t.Run(tc.name, func(t *testing.T) {
			form := url.Values{
				"name":      {"测试酒"},
				"price_330": {"45"},
			}
			form.Set(tc.field, tc.val)
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			_ = req.ParseForm()
			_, err := ParseBeerForm(req)
			if err == nil {
				t.Fatalf("%s=%q 应返回错误", tc.field, tc.val)
			}
			msg, ok := UserErrorMessage(err)
			if !ok || !strings.Contains(msg, tc.want) {
				t.Fatalf("%s=%q 错误不对: ok=%v err=%v", tc.field, tc.val, ok, err)
			}
		})
	}

	for _, badSort := range []string{"abc", "2147483648", "-2147483648"} {
		if _, err := ParseSortOrder(badSort); err == nil {
			t.Errorf("ParseSortOrder(%q) 应报错", badSort)
		}
	}
}

func TestBusinessDayBoundaries(t *testing.T) {
	ts := func(year, month, day, hour, min, sec int) time.Time {
		return time.Date(year, time.Month(month), day, hour, min, sec, 0, ReportZone)
	}
	cases := []struct {
		name    string
		in      time.Time
		wantKey string
		wantAt  time.Time
	}{
		{"次日凌晨2点半属前一天", ts(2026, 1, 12, 2, 30, 0), "2026-01-11", ts(2026, 1, 11, 7, 0, 0)},
		{"早06:59:59仍属前一天", ts(2026, 1, 12, 6, 59, 59), "2026-01-11", ts(2026, 1, 11, 7, 0, 0)},
		{"早07:00:00切换当天", ts(2026, 1, 12, 7, 0, 0), "2026-01-12", ts(2026, 1, 12, 7, 0, 0)},
		{"跨月1号凌晨属上月末", ts(2024, 3, 1, 5, 0, 0), "2024-02-29", ts(2024, 2, 29, 7, 0, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotStart := BusinessDayStart(tc.in)
			if !gotStart.Equal(tc.wantAt) {
				t.Fatalf("BusinessDayStart(%v) = %v, want %v", tc.in, gotStart, tc.wantAt)
			}
			if gotKey := BusinessDayKey(tc.in); gotKey != tc.wantKey {
				t.Fatalf("BusinessDayKey(%v) = %q, want %q", tc.in, gotKey, tc.wantKey)
			}
		})
	}
	dateStart := BusinessDayStartOfDate(time.Date(2026, 1, 11, 0, 0, 0, 0, ReportZone))
	if !dateStart.Equal(ts(2026, 1, 11, 7, 0, 0)) {
		t.Fatalf("BusinessDayStartOfDate = %v, want 2026-01-11 07:00", dateStart)
	}
}
