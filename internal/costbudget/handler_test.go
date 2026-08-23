package costbudget

import (
	"net/http/httptest"
	"testing"

	"cpa-usage-keeper/internal/entities"
	"github.com/gin-gonic/gin"
)

func TestBudgetPeriodFromQuery(t *testing.T) {
	cases := []struct {
		query   string
		period  entities.BudgetPeriod
		ok      bool
	}{
		{"", entities.BudgetPeriodMonthly, true},
		{"period=monthly", entities.BudgetPeriodMonthly, true},
		{"period=quarterly", entities.BudgetPeriodQuarterly, true},
		{"period=yearly", entities.BudgetPeriodYearly, true},
		{"period=weekly", "", false},
	}
	for _, tc := range cases {
		gin.SetMode(gin.TestMode)
		request := httptest.NewRequest("GET", "/budget/usage?"+tc.query, nil)
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = request
		period, ok := budgetPeriodFromQuery(ctx)
		if ok != tc.ok {
			t.Fatalf("budgetPeriodFromQuery(%q) ok = %v, want %v", tc.query, ok, tc.ok)
		}
		if ok && period != tc.period {
			t.Fatalf("budgetPeriodFromQuery(%q) period = %q, want %q", tc.query, period, tc.period)
		}
	}
}
