package platform

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
)

type actorKey struct{}

// WithActor 让请求链把已认证用户名带进 SQL/事件层，写流水与发布事件时保留操作者快照。
func WithActor(ctx context.Context, username string) context.Context {
	return context.WithValue(ctx, actorKey{}, username)
}

// OperationActor 返回当前操作用户名；后台任务或测试未注入时返回空串。
func OperationActor(ctx context.Context) string {
	name, _ := ctx.Value(actorKey{}).(string)
	return name
}

// 流水类型，和记录页筛选项一一对应。
const (
	OpPurchase   = "purchase"
	OpTap        = "tap"
	OpUntap      = "untap"
	OpManualSale = "manual_sale"
	OpSaleVoid   = "sale_void"
	OpStock      = "stock"
	OpRetailOn   = "retail_on"
	OpRetailOff  = "retail_off"
	OpScreenOn   = "screen_on"
	OpScreenOff  = "screen_off"
)

// LogOperation 在业务事务里写入一条营运流水：业务回滚，流水同步回滚。
func LogOperation(ctx context.Context, tx pgx.Tx, kind, subject, detail string) error {
	_, err := tx.Exec(ctx, `insert into operation_events (kind, subject, detail, actor) values ($1,$2,$3,$4)`,
		kind, strings.TrimSpace(subject), strings.TrimSpace(detail), OperationActor(ctx))
	return err
}

func BeerSubject(brewery, name string) string {
	if strings.TrimSpace(brewery) == "" {
		return name
	}
	return brewery + " · " + name
}

func RetailSubject(name string) string { return "零售 · " + name }
