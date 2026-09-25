// Package billing holds MailX's plan table (Free/Plus/Pro) and the Paystack
// integration (Initialize Transaction + webhook signature verification).
// It deliberately imports nothing from internal/database so the database
// layer can use the plan table in its own enforcement queries.
package billing

// Unlimited is the sentinel for "no cap" in a Plan's numeric limits. It
// follows internal/ratelimit.Policy's convention that a zero count means
// the control is off (see Policy.Validate's count helper).
const Unlimited = 0

// Plan is one billing tier's limits. A zero numeric limit means Unlimited.
type Plan struct {
	ID            string
	PriceUSDCents int64 // 0 for free
	DailySends    int
	Domains       int
	Members       int
	RetentionDays int
	Broadcasts    bool
	Webhooks      bool
}

const (
	PlanFree = "free"
	PlanPlus = "plus"
	PlanPro  = "pro"
)

// Plans is the confirmed v0.47 phase 2 plan table.
var Plans = map[string]Plan{
	PlanFree: {ID: PlanFree, PriceUSDCents: 0, DailySends: 500, Domains: 5, Members: 1, RetentionDays: 7, Broadcasts: false, Webhooks: false},
	PlanPlus: {ID: PlanPlus, PriceUSDCents: 600, DailySends: 10_000, Domains: 15, Members: 5, RetentionDays: 30, Broadcasts: true, Webhooks: true},
	PlanPro:  {ID: PlanPro, PriceUSDCents: 2400, DailySends: 100_000, Domains: Unlimited, Members: Unlimited, RetentionDays: 90, Broadcasts: true, Webhooks: true},
}

// PlanFor returns the plan with this id; unknown or empty ids are Free.
func PlanFor(id string) Plan {
	if p, ok := Plans[id]; ok {
		return p
	}
	return Plans[PlanFree]
}

// IsPaid reports whether id names a purchasable plan.
func IsPaid(id string) bool {
	p, ok := Plans[id]
	return ok && p.PriceUSDCents > 0
}

// Within reports whether a current count leaves room for one more under
// limit (Unlimited always does).
func Within(current, limit int) bool {
	return limit == Unlimited || current < limit
}
