package pos

import "context"

// PaymentRequest 传给收银渠道扩展点的结账上下文快照。
type PaymentRequest struct {
	OrderID           int64
	SpotLabel         string
	GrossCents        int
	DiscountCents     int
	AmountCents       int
	BalancePaidCents  int
	ExternalPaidCents int
	ItemsCount        int
	Actor             string
	Method            string
	IdempotencyKey    string
	MemberID          *int64
	DiscountLabel     string
}

// PaymentResult 是收银渠道扩展点确认记账后的返回信息。
type PaymentResult struct {
	Provider  string
	Reference string
}

// PaymentGateway 是 POS 结账扩展点（编译期接入微信/支付宝/线下核销等渠道）。
// 当前事务内仅适合同步核销/现金/记账确认；若接异步扫码支付，应先创建待支付单再走回调。
type PaymentGateway interface {
	Name() string
	Settle(ctx context.Context, req PaymentRequest) (PaymentResult, error)
}

// ManualGateway 默认店员人工确认收款实现，无在线支付通道。
type ManualGateway struct{}

func (ManualGateway) Name() string { return "manual" }

func (ManualGateway) Settle(context.Context, PaymentRequest) (PaymentResult, error) {
	return PaymentResult{
		Provider: "manual",
	}, nil
}

// ReceiptLine 记录小票打印与订单事件所需的单行快照。
type ReceiptLine struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	ML        int    `json:"ml,omitempty"`
	Qty       int    `json:"qty"`
	UnitCents int    `json:"unit_cents"`
}

// Receipt 描述一次已提交结账后交给硬件打印器扩展点的票据内容。
type Receipt struct {
	PaymentID         int64
	OrderID           int64
	SpotLabel         string
	GrossCents        int
	DiscountCents     int
	AmountCents       int
	BalancePaidCents  int
	ExternalPaidCents int
	DiscountLabel     string
	MemberName        string
	PointsEarned      int
	PassCupsUsed      int
	ItemsCount        int
	Actor             string
	Lines             []ReceiptLine
}

// ReceiptPrinter 是 POS 小票打印扩展点；在结账事务提交后按 best-effort 调用。
type ReceiptPrinter interface {
	Name() string
	Print(ctx context.Context, r Receipt) error
}

// NoopPrinter 默认空实现（未接入热敏打印机）。
type NoopPrinter struct{}

func (NoopPrinter) Name() string                         { return "noop" }
func (NoopPrinter) Print(context.Context, Receipt) error { return nil }
