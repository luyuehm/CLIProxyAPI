package costbudget

import (
	"fmt"
	"sort"
	"time"

	"cpa-usage-keeper/internal/entities"
	"cpa-usage-keeper/internal/timeutil"
	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

const (
	// AlertCurrency 固定按 USD 展示；KEEPER 本地预算的计价口径与 Usage 页一致。
	alertCurrency = "USD"
	// defaultAlertThreshold 是未显式配置时的默认告警阈值（80%）。
	defaultAlertThreshold = 80.0
	// alertResetGraceDays 是周期结束后仍视为当前周期告警的缓冲天数。
	alertResetGraceDays = 2
)

// Service 是预算模块的业务服务。
type Service struct {
	db *gorm.DB
}

func NewService(db *gorm.DB) *Service {
	return &Service{db: db}
}

// GetConfig 返回指定周期的预算配置；未配置时返回默认空配置。
func (s *Service) GetConfig(period entities.BudgetPeriod) (BudgetConfigView, error) {
	if !validPeriod(period) {
		return BudgetConfigView{}, fmt.Errorf("invalid budget period %q", period)
	}
	config, err := s.loadOrCreate(period, time.Now())
	if err != nil {
		return BudgetConfigView{}, err
	}
	return toConfigView(config), nil
}

// UpdateConfig 更新预算配置，并在写入后重新评估本周期告警状态。
func (s *Service) UpdateConfig(input BudgetUpdateInput) (BudgetConfigView, error) {
	if !validPeriod(input.Period) {
		return BudgetConfigView{}, fmt.Errorf("invalid budget period %q", input.Period)
	}
	if input.Amount <= 0 {
		return BudgetConfigView{}, fmt.Errorf("budget amount must be greater than zero")
	}
	if input.AlertThreshold < 0 || input.AlertThreshold > 100 {
		return BudgetConfigView{}, fmt.Errorf("alert threshold must be between 0 and 100")
	}

	now := time.Now()
	config, err := s.loadOrCreate(input.Period, now)
	if err != nil {
		return BudgetConfigView{}, err
	}
	config.Amount = input.Amount
	config.AlertThreshold = input.AlertThreshold
	config.AlertEnabled = input.AlertEnabled
	config.PeriodStart, config.PeriodEnd = periodRange(input.Period, now)
	// 修改配置后重置告警状态，由下一次 Usage 读取重新评估。
	config.AlertFired = false
	saved, err := upsertBudgetConfig(s.db, config)
	if err != nil {
		return BudgetConfigView{}, err
	}
	return toConfigView(saved), nil
}

// Usage 返回预算使用进度，并顺带检查是否需要触发超预算或阈值告警。
func (s *Service) Usage(period entities.BudgetPeriod) (BudgetUsageView, error) {
	if !validPeriod(period) {
		return BudgetUsageView{}, fmt.Errorf("invalid budget period %q", period)
	}
	now := time.Now()
	config, err := s.loadOrCreate(period, now)
	if err != nil {
		return BudgetUsageView{}, err
	}
	start, end := periodRange(period, now)
	spent, costAvailable, _, err := periodUsageCost(s.db, start, end)
	if err != nil {
		return BudgetUsageView{}, err
	}

	s.reevaluateAlert(config, spent, costAvailable, now)

	view := BudgetUsageView{
		Period:         config.Period,
		Amount:         config.Amount,
		Currency:       alertCurrency,
		Spent:          spent,
		AlertThreshold: config.AlertThreshold,
		AlertEnabled:   config.AlertEnabled,
		AlertFired:     config.AlertFired,
		PeriodStart:    start,
		PeriodEnd:      end,
		CostAvailable:  costAvailable,
	}
	view.Remaining = config.Amount - spent
	if view.Remaining < 0 {
		view.Remaining = 0
	}
	if config.Amount > 0 {
		percent := spent / config.Amount * 100
		if percent > 100 {
			percent = 100
		}
		view.UsagePercent = percent
	}
	view.Exceeded = spent > config.Amount
	return view, nil
}

// Report 返回预算周期内按模型拆分的费用报表。
func (s *Service) Report(period entities.BudgetPeriod) (BudgetReportView, error) {
	if !validPeriod(period) {
		return BudgetReportView{}, fmt.Errorf("invalid budget period %q", period)
	}
	now := time.Now()
	config, err := s.loadOrCreate(period, now)
	if err != nil {
		return BudgetReportView{}, err
	}
	start, end := periodRange(period, now)
	spent, _, items, err := periodUsageCost(s.db, start, end)
	if err != nil {
		return BudgetReportView{}, err
	}

	total := spent
	for index := range items {
		if total > 0 {
			items[index].CostShare = items[index].Cost / total * 100
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Cost != items[j].Cost {
			return items[i].Cost > items[j].Cost
		}
		return items[i].Model < items[j].Model
	})

	view := BudgetReportView{
		Period:      period,
		Amount:      config.Amount,
		Currency:    alertCurrency,
		Spent:       spent,
		PeriodStart: start,
		PeriodEnd:   end,
		Items:       items,
	}
	if config.Amount > 0 {
		view.UsagePercent = spent / config.Amount * 100
		if view.UsagePercent > 100 {
			view.UsagePercent = 100
		}
	}
	return view, nil
}

// reevaluateAlert 在读取进度时检查阈值/超限，满足条件且启用告警则落盘告警状态并输出本地日志。
// "本机落盘" 是预算告警的持久化语义：状态写入 budget_configs.alert_fired，事件通过日志保留。
func (s *Service) reevaluateAlert(config *entities.BudgetConfig, spent float64, costAvailable bool, now time.Time) {
	if config == nil || !config.AlertEnabled || !costAvailable {
		return
	}
	if config.PeriodStart.IsZero() || now.Before(config.PeriodStart) {
		return
	}
	if now.After(config.PeriodEnd.AddDate(0, 0, alertResetGraceDays)) {
		// 周期早已结束且未重置时，视为过期状态，交给下次加载重置。
		return
	}
	if config.AlertFired {
		return
	}
	exceeded := spent > config.Amount
	thresholdReached := config.Amount > 0 && spent >= config.Amount*config.AlertThreshold/100
	if !exceeded && !thresholdReached {
		return
	}

	config.AlertFired = true
	if _, err := upsertBudgetConfig(s.db, config); err != nil {
		logrus.WithError(err).Error("budget alert state persist failed")
		return
	}
	if exceeded {
		logrus.Warnf("budget alert fired: %s budget exceeded, spent %.4f USD, limit %.4f USD", config.Period, spent, config.Amount)
	} else {
		logrus.Warnf("budget alert fired: %s budget reached %.0f%% threshold, spent %.4f USD, limit %.4f USD", config.Period, spent/config.Amount*100, spent, config.Amount)
	}
}

// loadOrCreate 读取指定周期配置；不存在时创建默认配置并初始化当前周期范围。
// 周期滚动时自动推进 period_start/period_end 并重置告警状态。
func (s *Service) loadOrCreate(period entities.BudgetPeriod, now time.Time) (*entities.BudgetConfig, error) {
	config, err := getBudgetConfig(s.db, period)
	if err != nil {
		return nil, err
	}
	start, end := periodRange(period, now)
	if config == nil {
		config = &entities.BudgetConfig{
			Period:         period,
			Amount:         0,
			AlertThreshold: defaultAlertThreshold,
			AlertEnabled:   true,
			PeriodStart:    start,
			PeriodEnd:      end,
		}
		saved, err := upsertBudgetConfig(s.db, config)
		if err != nil {
			return nil, err
		}
		return saved, nil
	}
	if config.PeriodStart.IsZero() || now.Before(config.PeriodStart) || now.After(config.PeriodEnd) {
		config.PeriodStart = start
		config.PeriodEnd = end
		config.AlertFired = false
		saved, err := upsertBudgetConfig(s.db, config)
		if err != nil {
			return nil, err
		}
		return saved, nil
	}
	return config, nil
}

func validPeriod(period entities.BudgetPeriod) bool {
	switch period {
	case entities.BudgetPeriodMonthly, entities.BudgetPeriodQuarterly, entities.BudgetPeriodYearly:
		return true
	default:
		return false
	}
}

// periodRange 返回本地时区下当前周期的闭区间。
func periodRange(period entities.BudgetPeriod, now time.Time) (time.Time, time.Time) {
	now = timeutil.NormalizeStorageTime(now)
	year, month, _ := now.Date()
	location := now.Location()
	switch period {
	case entities.BudgetPeriodQuarterly:
		quarterStartMonth := time.Month((int(month)-1)/3*3 + 1)
		start := time.Date(year, quarterStartMonth, 1, 0, 0, 0, 0, location)
		return start, start.AddDate(0, 3, 0).Add(-time.Nanosecond)
	case entities.BudgetPeriodYearly:
		start := time.Date(year, time.January, 1, 0, 0, 0, 0, location)
		return start, start.AddDate(1, 0, 0).Add(-time.Nanosecond)
	default:
		start := time.Date(year, month, 1, 0, 0, 0, 0, location)
		return start, start.AddDate(0, 1, 0).Add(-time.Nanosecond)
	}
}

func toConfigView(config *entities.BudgetConfig) BudgetConfigView {
	return BudgetConfigView{
		Period:         config.Period,
		Amount:         config.Amount,
		Currency:       alertCurrency,
		AlertThreshold: config.AlertThreshold,
		AlertEnabled:   config.AlertEnabled,
		AlertFired:     config.AlertFired,
		PeriodStart:    config.PeriodStart,
		PeriodEnd:      config.PeriodEnd,
		UpdatedAt:      config.UpdatedAt,
	}
}
