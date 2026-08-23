package alert

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// Service 提供告警通道/规则的 CRUD 以及事件发送与重试。
type Service struct {
	repo   *Repository
	sender *WebhookSender
}

func NewService(db *gorm.DB, timeout time.Duration) *Service {
	return &Service{
		repo:   NewRepository(db),
		sender: NewWebhookSender(timeout),
	}
}

func (s *Service) ListChannels() ([]AlertChannel, error) {
	return s.repo.ListChannels()
}

func (s *Service) GetChannel(id int64) (*AlertChannel, error) {
	return s.repo.GetChannel(id)
}

func (s *Service) CreateChannel(req ChannelCreateRequest) (*AlertChannel, error) {
	return s.repo.CreateChannel(req)
}

func (s *Service) UpdateChannel(id int64, req ChannelUpdateRequest) (*AlertChannel, error) {
	return s.repo.UpdateChannel(id, req)
}

func (s *Service) DeleteChannel(id int64) error {
	return s.repo.DeleteChannel(id)
}

func (s *Service) ListRules() ([]AlertRule, error) {
	return s.repo.ListRules()
}

func (s *Service) GetRule(id int64) (*AlertRule, error) {
	return s.repo.GetRule(id)
}

func (s *Service) CreateRule(req RuleCreateRequest) (*AlertRule, error) {
	if _, err := s.repo.GetChannel(req.ChannelID); err != nil {
		return nil, fmt.Errorf("validate rule channel: %w", err)
	}
	return s.repo.CreateRule(req)
}

func (s *Service) UpdateRule(id int64, req RuleUpdateRequest) (*AlertRule, error) {
	if req.ChannelID != nil {
		if _, err := s.repo.GetChannel(*req.ChannelID); err != nil {
			return nil, fmt.Errorf("validate rule channel: %w", err)
		}
	}
	return s.repo.UpdateRule(id, req)
}

func (s *Service) DeleteRule(id int64) error {
	return s.repo.DeleteRule(id)
}

func (s *Service) ListEvents(limit int) ([]AlertEvent, error) {
	return s.repo.ListEvents(limit)
}

// SendEvent 为指定规则创建事件并立即尝试发送。
func (s *Service) SendEvent(ctx context.Context, ruleID int64, message string) (*AlertEvent, error) {
	rule, err := s.repo.GetRule(ruleID)
	if err != nil {
		return nil, err
	}
	if !rule.Enabled {
		return nil, fmt.Errorf("rule %d is disabled", ruleID)
	}
	channel, err := s.repo.GetChannel(rule.ChannelID)
	if err != nil {
		return nil, err
	}
	if !channel.Enabled {
		return nil, fmt.Errorf("channel %d is disabled", channel.ID)
	}

	event := &AlertEvent{
		RuleID:    rule.ID,
		ChannelID: channel.ID,
		Status:    string(EventStatusPending),
		Message:   message,
	}
	if err := s.repo.CreateEvent(event); err != nil {
		return nil, err
	}

	if _, sendErr := s.sender.Send(ctx, *channel, message); sendErr != nil {
		_ = s.repo.MarkEventFailed(event.ID, sendErr.Error())
		return event, sendErr
	}
	_ = s.repo.MarkEventSent(event.ID)
	return event, nil
}

// RetryEvent 重试一个已失败的告警事件。
func (s *Service) RetryEvent(ctx context.Context, eventID int64) (*AlertEvent, error) {
	event, err := s.getEvent(eventID)
	if err != nil {
		return nil, err
	}
	channel, err := s.repo.GetChannel(event.ChannelID)
	if err != nil {
		return nil, err
	}

	if _, sendErr := s.sender.Send(ctx, *channel, event.Message); sendErr != nil {
		_ = s.repo.MarkEventFailed(event.ID, sendErr.Error())
		return event, sendErr
	}
	_ = s.repo.MarkEventSent(event.ID)
	return event, nil
}

func (s *Service) getEvent(id int64) (*AlertEvent, error) {
	events, err := s.repo.ListEvents(1000)
	if err != nil {
		return nil, err
	}
	for i := range events {
		if events[i].ID == id {
			return &events[i], nil
		}
	}
	return nil, ErrEventNotFound
}