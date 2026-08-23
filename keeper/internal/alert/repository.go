package alert

import (
	"errors"

	"gorm.io/gorm"
)

// Repository 提供告警通道、规则和事件的本机落盘。
type Repository struct {
	db *gorm.DB
}

func NewRepository(db *gorm.DB) *Repository {
	return &Repository{db: db}
}

func (r *Repository) ListChannels() ([]AlertChannel, error) {
	var channels []AlertChannel
	if err := r.db.Order("id DESC").Find(&channels).Error; err != nil {
		return nil, err
	}
	return channels, nil
}

func (r *Repository) GetChannel(id int64) (*AlertChannel, error) {
	var channel AlertChannel
	if err := r.db.First(&channel, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrChannelNotFound
		}
		return nil, err
	}
	return &channel, nil
}

func (r *Repository) CreateChannel(req ChannelCreateRequest) (*AlertChannel, error) {
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	channel := &AlertChannel{
		Name:       req.Name,
		Platform:   req.Platform,
		WebhookURL: req.WebhookURL,
		Secret:     req.Secret,
		Enabled:    enabled,
	}
	if err := r.db.Create(channel).Error; err != nil {
		return nil, err
	}
	return channel, nil
}

func (r *Repository) UpdateChannel(id int64, req ChannelUpdateRequest) (*AlertChannel, error) {
	if _, err := r.GetChannel(id); err != nil {
		return nil, err
	}
	updates := map[string]interface{}{}
	if req.Name != nil {
		updates["name"] = *req.Name
	}
	if req.Platform != nil {
		updates["platform"] = *req.Platform
	}
	if req.WebhookURL != nil {
		updates["webhook_url"] = *req.WebhookURL
	}
	if req.Secret != nil {
		updates["secret"] = *req.Secret
	}
	if req.Enabled != nil {
		updates["enabled"] = *req.Enabled
	}
	if len(updates) == 0 {
		return r.GetChannel(id)
	}
	if err := r.db.Model(&AlertChannel{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		return nil, err
	}
	return r.GetChannel(id)
}

func (r *Repository) DeleteChannel(id int64) error {
	result := r.db.Where("id = ?", id).Delete(&AlertChannel{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrChannelNotFound
	}
	return nil
}

func (r *Repository) ListRules() ([]AlertRule, error) {
	var rules []AlertRule
	if err := r.db.Order("id DESC").Find(&rules).Error; err != nil {
		return nil, err
	}
	return rules, nil
}

func (r *Repository) GetRule(id int64) (*AlertRule, error) {
	var rule AlertRule
	if err := r.db.First(&rule, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrRuleNotFound
		}
		return nil, err
	}
	return &rule, nil
}

func (r *Repository) CreateRule(req RuleCreateRequest) (*AlertRule, error) {
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	rule := &AlertRule{
		Name:         req.Name,
		MetricType:   req.MetricType,
		ConditionOp:  req.ConditionOp,
		ConditionVal: req.ConditionVal,
		ChannelID:    req.ChannelID,
		Enabled:      enabled,
	}
	if err := r.db.Create(rule).Error; err != nil {
		return nil, err
	}
	return rule, nil
}

func (r *Repository) UpdateRule(id int64, req RuleUpdateRequest) (*AlertRule, error) {
	if _, err := r.GetRule(id); err != nil {
		return nil, err
	}
	updates := map[string]interface{}{}
	if req.Name != nil {
		updates["name"] = *req.Name
	}
	if req.MetricType != nil {
		updates["metric_type"] = *req.MetricType
	}
	if req.ConditionOp != nil {
		updates["condition_op"] = *req.ConditionOp
	}
	if req.ConditionVal != nil {
		updates["condition_val"] = *req.ConditionVal
	}
	if req.ChannelID != nil {
		updates["channel_id"] = *req.ChannelID
	}
	if req.Enabled != nil {
		updates["enabled"] = *req.Enabled
	}
	if len(updates) == 0 {
		return r.GetRule(id)
	}
	if err := r.db.Model(&AlertRule{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		return nil, err
	}
	return r.GetRule(id)
}

func (r *Repository) DeleteRule(id int64) error {
	result := r.db.Where("id = ?", id).Delete(&AlertRule{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrRuleNotFound
	}
	return nil
}

func (r *Repository) CreateEvent(event *AlertEvent) error {
	return r.db.Create(event).Error
}

func (r *Repository) ListEvents(limit int) ([]AlertEvent, error) {
	if limit <= 0 {
		limit = 20
	}
	var events []AlertEvent
	query := r.db.Order("id DESC")
	if limit > 0 {
		query = query.Limit(limit)
	}
	if err := query.Find(&events).Error; err != nil {
		return nil, err
	}
	return events, nil
}

func (r *Repository) MarkEventSent(id int64) error {
	return r.db.Model(&AlertEvent{}).Where("id = ?", id).Updates(map[string]interface{}{
		"status":        string(EventStatusSent),
		"attempt_count": gorm.Expr("attempt_count + 1"),
		"last_error":    "",
	}).Error
}

func (r *Repository) MarkEventFailed(id int64, errMsg string) error {
	return r.db.Model(&AlertEvent{}).Where("id = ?", id).Updates(map[string]interface{}{
		"status":        string(EventStatusFailed),
		"attempt_count": gorm.Expr("attempt_count + 1"),
		"last_error":    errMsg,
	}).Error
}