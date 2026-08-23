package repository

import (
	"context"
	"errors"
	"strings"
	"time"

	"cpa-usage-keeper/internal/contentfilter"
	"cpa-usage-keeper/internal/entities"
	"gorm.io/gorm"
)

var (
	ErrContentFilterRuleNotFound = errors.New("content filter rule not found")
)

type ContentFilterLogQuery struct {
	RuleID     *int64
	FilterType string
	Action     string
	Model      string
	Limit      int
	Offset     int
}

type ContentFilterRepository struct {
	db *gorm.DB
}

func NewContentFilterRepository(db *gorm.DB) *ContentFilterRepository {
	return &ContentFilterRepository{db: db}
}

func (r *ContentFilterRepository) ListRules(ctx context.Context) ([]entities.ContentFilterRule, error) {
	var rules []entities.ContentFilterRule
	err := r.db.WithContext(ctx).
		Order("priority desc, id asc").
		Find(&rules).Error
	return rules, err
}

func (r *ContentFilterRepository) GetRuleByID(ctx context.Context, id int64) (*entities.ContentFilterRule, error) {
	var rule entities.ContentFilterRule
	err := r.db.WithContext(ctx).First(&rule, "id = ?", id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrContentFilterRuleNotFound
		}
		return nil, err
	}
	return &rule, nil
}

func (r *ContentFilterRepository) CreateRule(ctx context.Context, rule *entities.ContentFilterRule) error {
	now := time.Now()
	rule.CreatedAt = now
	rule.UpdatedAt = now
	return r.db.WithContext(ctx).Create(rule).Error
}

func (r *ContentFilterRepository) UpdateRule(ctx context.Context, rule *entities.ContentFilterRule) error {
	rule.UpdatedAt = time.Now()
	res := r.db.WithContext(ctx).Model(&entities.ContentFilterRule{}).
		Where("id = ?", rule.ID).
		Updates(map[string]any{
			"name":            rule.Name,
			"description":     rule.Description,
			"enabled":         rule.Enabled,
			"scenario":        rule.Scenario,
			"action":          rule.Action,
			"sensitive_words": rule.SensitiveWords,
			"pii_types":       rule.PIITypes,
			"white_list":      rule.WhiteList,
			"models":          rule.Models,
			"priority":        rule.Priority,
			"updated_at":      rule.UpdatedAt,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrContentFilterRuleNotFound
	}
	return nil
}

func (r *ContentFilterRepository) DeleteRule(ctx context.Context, id int64) error {
	res := r.db.WithContext(ctx).Delete(&entities.ContentFilterRule{}, "id = ?", id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrContentFilterRuleNotFound
	}
	return nil
}

func (r *ContentFilterRepository) ListLogs(ctx context.Context, q ContentFilterLogQuery) ([]entities.ContentFilterLog, int64, error) {
	db := r.db.WithContext(ctx).Model(&entities.ContentFilterLog{})
	if q.RuleID != nil {
		db = db.Where("rule_id = ?", *q.RuleID)
	}
	if q.FilterType != "" {
		db = db.Where("filter_type = ?", q.FilterType)
	}
	if q.Action != "" {
		db = db.Where("action = ?", q.Action)
	}
	if q.Model != "" {
		db = db.Where("model = ?", q.Model)
	}

	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	var logs []entities.ContentFilterLog
	err := db.Order("id desc").
		Offset(q.Offset).
		Limit(limit).
		Find(&logs).Error
	return logs, total, err
}

func (r *ContentFilterRepository) CreateLog(ctx context.Context, log *entities.ContentFilterLog) error {
	if log.CreatedAt.IsZero() {
		log.CreatedAt = time.Now()
	}
	return r.db.WithContext(ctx).Create(log).Error
}

func (r *ContentFilterRepository) SeedDefaultRulesIfEmpty(ctx context.Context) error {
	var count int64
	if err := r.db.WithContext(ctx).Model(&entities.ContentFilterRule{}).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	defaultRules := []entities.ContentFilterRule{
		{
			Name:           "通用个人隐私(PII)自动脱敏",
			Description:    "自动检测并掩码手机号、身份证、邮箱、银行卡等个人隐私信息",
			Enabled:        true,
			Scenario:       contentfilter.ScenarioGeneral,
			Action:         contentfilter.ActionMask,
			PIITypes:       "phone,id_card,email,bank_card,passport",
			SensitiveWords: strings.Join(contentfilter.DefaultGeneralSensitiveWords, "\n"),
			WhiteList:      "127.0.0.1,admin@example.com",
			Models:         "*",
			Priority:       10,
		},
		{
			Name:           "金融合规防数据泄漏规则",
			Description:    "针对银行卡、支付密码、证券与资金安全关键词进行脱敏和防护",
			Enabled:        true,
			Scenario:       contentfilter.ScenarioFinance,
			Action:         contentfilter.ActionMask,
			PIITypes:       "bank_card,phone,id_card",
			SensitiveWords: strings.Join(contentfilter.DefaultFinanceSensitiveWords, "\n"),
			WhiteList:      "",
			Models:         "*",
			Priority:       20,
		},
		{
			Name:           "医疗健康隐私数据保护规则",
			Description:    "保护患者就诊卡、处方、医保编号及重大疾病等敏感健康隐私数据",
			Enabled:        true,
			Scenario:       contentfilter.ScenarioMedical,
			Action:         contentfilter.ActionMask,
			PIITypes:       "medical_record,phone,id_card",
			SensitiveWords: strings.Join(contentfilter.DefaultMedicalSensitiveWords, "\n"),
			WhiteList:      "",
			Models:         "*",
			Priority:       20,
		},
	}

	for _, rule := range defaultRules {
		if err := r.CreateRule(ctx, &rule); err != nil {
			return err
		}
	}
	return nil
}
