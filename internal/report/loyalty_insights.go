package report

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"pourops/internal/platform"
)

// LoyaltyInsights 汇总统计周期内的优惠让利、储值消耗与充值/售卡预收资金，以及当前在效会员/余额快照。
type LoyaltyInsights struct {
	DiscountTotal         int64
	PassCupsRedeemed      int64
	PassSavedCents        int64
	BalanceConsumedCents  int64
	ExternalReceivedCents int64
	RechargeCashCents     int64
	RechargeBonusCents    int64
	PassSalesCashCents    int64
	TotalMembers          int64
	ActiveMembers         int64
	TotalBalanceLiability int64
	ActivePasses          int64
}

func (l LoyaltyInsights) TotalCashInflowCents() int64 {
	return l.ExternalReceivedCents + l.RechargeCashCents + l.PassSalesCashCents
}

func (l LoyaltyInsights) DiscountTotalText() string { return platform.PriceText(int(l.DiscountTotal)) }
func (l LoyaltyInsights) PassSavedText() string     { return platform.PriceText(int(l.PassSavedCents)) }
func (l LoyaltyInsights) BalanceConsumedText() string {
	return platform.PriceText(int(l.BalanceConsumedCents))
}
func (l LoyaltyInsights) ExternalReceivedText() string {
	return platform.PriceText(int(l.ExternalReceivedCents))
}
func (l LoyaltyInsights) RechargeCashText() string {
	return platform.PriceText(int(l.RechargeCashCents))
}
func (l LoyaltyInsights) RechargeBonusText() string {
	return platform.PriceText(int(l.RechargeBonusCents))
}
func (l LoyaltyInsights) PassSalesCashText() string {
	return platform.PriceText(int(l.PassSalesCashCents))
}
func (l LoyaltyInsights) TotalCashInflowText() string {
	return platform.MoneyText64(l.TotalCashInflowCents())
}
func (l LoyaltyInsights) TotalBalanceLiabilityText() string {
	return platform.MoneyText64(l.TotalBalanceLiability)
}

// LoadLoyaltyInsights 在调用方只读 RepeatableRead 事务快照内聚合会员与优惠资金指标。
func LoadLoyaltyInsights(ctx context.Context, tx pgx.Tx, start, end, now time.Time) (LoyaltyInsights, error) {
	var out LoyaltyInsights
	err := tx.QueryRow(ctx, `
		select coalesce(sum(discount_cents), 0)::bigint,
		       coalesce(sum(balance_paid_cents), 0)::bigint,
		       coalesce(sum(coalesce(external_paid_cents, amount_cents)), 0)::bigint
		from payments
		where created_at >= $1 and created_at < $2`, start, end).
		Scan(&out.DiscountTotal, &out.BalanceConsumedCents, &out.ExternalReceivedCents)
	if err != nil {
		return out, err
	}

	if err := tx.QueryRow(ctx, `
		select coalesce(sum(cups), 0)::bigint,
		       coalesce(sum(saved_cents), 0)::bigint
		from member_pass_usages
		where created_at >= $1 and created_at < $2`, start, end).
		Scan(&out.PassCupsRedeemed, &out.PassSavedCents); err != nil {
		return out, err
	}

	if err := tx.QueryRow(ctx, `
		select coalesce(sum(delta_cents) filter (where kind = 'recharge'), 0)::bigint,
		       coalesce(sum(delta_cents) filter (where kind = 'recharge_bonus'), 0)::bigint
		from member_balance_txns
		where created_at >= $1 and created_at < $2`, start, end).
		Scan(&out.RechargeCashCents, &out.RechargeBonusCents); err != nil {
		return out, err
	}

	if err := tx.QueryRow(ctx, `
		select coalesce(sum(price_cents), 0)::bigint
		from member_passes
		where created_at >= $1 and created_at < $2 and pay_method <> 'balance'`, start, end).
		Scan(&out.PassSalesCashCents); err != nil {
		return out, err
	}

	if err := tx.QueryRow(ctx, `
		select count(*)::bigint,
		       count(*) filter (where active)::bigint,
		       coalesce(sum(balance_cents), 0)::bigint,
		       (select count(*)::bigint from member_passes where status='active' and starts_at <= $1 and expires_at > $1)
		from members`, now).
		Scan(&out.TotalMembers, &out.ActiveMembers, &out.TotalBalanceLiability, &out.ActivePasses); err != nil {
		return out, err
	}
	return out, nil
}
