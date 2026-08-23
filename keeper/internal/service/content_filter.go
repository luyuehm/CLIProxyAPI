package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"

	"cpa-usage-keeper/internal/contentfilter"
	"cpa-usage-keeper/internal/entities"
	"cpa-usage-keeper/internal/repository"
)

type ContentFilterRuleCreateRequest struct {
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Enabled        *bool    `json:"enabled"`
	Scenario       string   `json:"scenario"`
	Action         string   `json:"action"`
	SensitiveWords []string `json:"sensitive_words"`
	PIITypes       []string `json:"pii_types"`
	WhiteList      []string `json:"white_list"`
	Models         []string `json:"models"`
	Priority       int      `json:"priority"`
}

type ContentFilterRuleUpdateRequest struct {
	Name           string    `json:"name"`
	Description    string    `json:"description"`
	Enabled        *bool     `json:"enabled"`
	Scenario       string    `json:"scenario"`
	Action         string    `json:"action"`
	SensitiveWords *[]string `json:"sensitive_words"`
	PIITypes       *[]string `json:"pii_types"`
	WhiteList      *[]string `json:"white_list"`
	Models         *[]string `json:"models"`
	Priority       *int      `json:"priority"`
}

type FilterTextRequest struct {
	Text     string `json:"text"`
	Model    string `json:"model"`
	ClientIP string `json:"client_ip"`
	UserID   string `json:"user_id"`
}

type FilterTextResponse struct {
	FilteredText string                        `json:"filtered_text"`
	OriginalText string                        `json:"original_text"`
	Changed      bool                          `json:"changed"`
	Blocked      bool                          `json:"blocked"`
	BlockReason  string                        `json:"block_reason,omitempty"`
	Action       string                        `json:"action"`
	MatchCount   int                           `json:"match_count"`
	MatchedWords []string                      `json:"matched_words"`
	MatchedPII   []string                      `json:"matched_pii"`
	Details      []contentfilter.MatchedDetail `json:"details"`
	MatchedRules []string                      `json:"matched_rules"`
}

type ContentFilterProvider interface {
	ListRules(ctx context.Context) ([]entities.ContentFilterRule, error)
	GetRule(ctx context.Context, id int64) (*entities.ContentFilterRule, error)
	CreateRule(ctx context.Context, req ContentFilterRuleCreateRequest) (*entities.ContentFilterRule, error)
	UpdateRule(ctx context.Context, id int64, req ContentFilterRuleUpdateRequest) (*entities.ContentFilterRule, error)
	DeleteRule(ctx context.Context, id int64) error
	ListLogs(ctx context.Context, q repository.ContentFilterLogQuery) ([]entities.ContentFilterLog, int64, error)
	FilterText(ctx context.Context, req FilterTextRequest) (*FilterTextResponse, error)
}

type ContentFilterService struct {
	repo   *repository.ContentFilterRepository
	mu     sync.RWMutex
	engine *contentfilter.Filter
}

func NewContentFilterService(repo *repository.ContentFilterRepository) *ContentFilterService {
	s := &ContentFilterService{repo: repo}
	_ = repo.SeedDefaultRulesIfEmpty(context.Background())
	_ = s.reloadEngine(context.Background())
	return s
}

func (s *ContentFilterService) reloadEngine(ctx context.Context) error {
	rules, err := s.repo.ListRules(ctx)
	if err != nil {
		return err
	}

	var configs []contentfilter.RuleConfig
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		cfg := contentfilter.RuleConfig{
			ID:             r.ID,
			Name:           r.Name,
			Enabled:        r.Enabled,
			Scenario:       r.Scenario,
			Action:         r.Action,
			SensitiveWords: splitLinesOrCommas(r.SensitiveWords),
			PIITypes:       splitLinesOrCommas(r.PIITypes),
			WhiteList:      splitLinesOrCommas(r.WhiteList),
			Models:         splitLinesOrCommas(r.Models),
			Priority:       r.Priority,
		}
		configs = append(configs, cfg)
	}

	engine := contentfilter.NewFilter(configs)

	s.mu.Lock()
	s.engine = engine
	s.mu.Unlock()

	return nil
}

func (s *ContentFilterService) ListRules(ctx context.Context) ([]entities.ContentFilterRule, error) {
	return s.repo.ListRules(ctx)
}

func (s *ContentFilterService) GetRule(ctx context.Context, id int64) (*entities.ContentFilterRule, error) {
	return s.repo.GetRuleByID(ctx, id)
}

func (s *ContentFilterService) CreateRule(ctx context.Context, req ContentFilterRuleCreateRequest) (*entities.ContentFilterRule, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, errors.New("rule name is required")
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	action := strings.TrimSpace(req.Action)
	if action == "" {
		action = contentfilter.ActionMask
	}

	scenario := strings.TrimSpace(req.Scenario)
	if scenario == "" {
		scenario = contentfilter.ScenarioGeneral
	}

	rule := &entities.ContentFilterRule{
		Name:           name,
		Description:    strings.TrimSpace(req.Description),
		Enabled:        enabled,
		Scenario:       scenario,
		Action:         action,
		SensitiveWords: strings.Join(req.SensitiveWords, "\n"),
		PIITypes:       strings.Join(req.PIITypes, ","),
		WhiteList:      strings.Join(req.WhiteList, ","),
		Models:         strings.Join(req.Models, ","),
		Priority:       req.Priority,
	}

	if err := s.repo.CreateRule(ctx, rule); err != nil {
		return nil, err
	}

	_ = s.reloadEngine(ctx)
	return rule, nil
}

func (s *ContentFilterService) UpdateRule(ctx context.Context, id int64, req ContentFilterRuleUpdateRequest) (*entities.ContentFilterRule, error) {
	rule, err := s.repo.GetRuleByID(ctx, id)
	if err != nil {
		return nil, err
	}

	if req.Name != "" {
		rule.Name = strings.TrimSpace(req.Name)
	}
	if req.Description != "" {
		rule.Description = strings.TrimSpace(req.Description)
	}
	if req.Enabled != nil {
		rule.Enabled = *req.Enabled
	}
	if req.Scenario != "" {
		rule.Scenario = strings.TrimSpace(req.Scenario)
	}
	if req.Action != "" {
		rule.Action = strings.TrimSpace(req.Action)
	}
	if req.SensitiveWords != nil {
		rule.SensitiveWords = strings.Join(*req.SensitiveWords, "\n")
	}
	if req.PIITypes != nil {
		rule.PIITypes = strings.Join(*req.PIITypes, ",")
	}
	if req.WhiteList != nil {
		rule.WhiteList = strings.Join(*req.WhiteList, ",")
	}
	if req.Models != nil {
		rule.Models = strings.Join(*req.Models, ",")
	}
	if req.Priority != nil {
		rule.Priority = *req.Priority
	}

	if err := s.repo.UpdateRule(ctx, rule); err != nil {
		return nil, err
	}

	_ = s.reloadEngine(ctx)
	return rule, nil
}

func (s *ContentFilterService) DeleteRule(ctx context.Context, id int64) error {
	if err := s.repo.DeleteRule(ctx, id); err != nil {
		return err
	}
	_ = s.reloadEngine(ctx)
	return nil
}

func (s *ContentFilterService) ListLogs(ctx context.Context, q repository.ContentFilterLogQuery) ([]entities.ContentFilterLog, int64, error) {
	return s.repo.ListLogs(ctx, q)
}

func (s *ContentFilterService) FilterText(ctx context.Context, req FilterTextRequest) (*FilterTextResponse, error) {
	s.mu.RLock()
	engine := s.engine
	s.mu.RUnlock()

	if engine == nil {
		return &FilterTextResponse{
			FilteredText: req.Text,
			OriginalText: req.Text,
			Action:       contentfilter.ActionMask,
		}, nil
	}

	res := engine.ProcessText(req.Text, contentfilter.ProcessOptions{
		Model:    req.Model,
		ClientIP: req.ClientIP,
		UserID:   req.UserID,
	})

	resp := &FilterTextResponse{
		FilteredText: res.FilteredText,
		OriginalText: res.OriginalText,
		Changed:      res.Changed,
		Blocked:      res.Blocked,
		BlockReason:  res.BlockReason,
		Action:       res.Action,
		MatchCount:   res.MatchCount,
		MatchedWords: res.MatchedWords,
		MatchedPII:   res.MatchedPII,
		Details:      res.Details,
		MatchedRules: res.MatchedRules,
	}

	// If there were matches or blocked, record audit log asynchronously
	if res.MatchCount > 0 || res.Blocked {
		filterType := "sensitive_word"
		if len(res.MatchedPII) > 0 && len(res.MatchedWords) > 0 {
			filterType = "combined"
		} else if len(res.MatchedPII) > 0 {
			filterType = "pii"
		}

		matchesBytes, _ := json.Marshal(res.Details)

		rawPreview := req.Text
		if len([]rune(rawPreview)) > 200 {
			rawPreview = string([]rune(rawPreview)[:200]) + "..."
		}
		filteredPreview := res.FilteredText
		if len([]rune(filteredPreview)) > 200 {
			filteredPreview = string([]rune(filteredPreview)[:200]) + "..."
		}

		ruleName := "ContentFilter"
		if len(res.MatchedRules) > 0 {
			ruleName = strings.Join(res.MatchedRules, ", ")
		}

		log := &entities.ContentFilterLog{
			RuleName:        ruleName,
			FilterType:      filterType,
			MatchCount:      res.MatchCount,
			Matches:         string(matchesBytes),
			Action:          res.Action,
			Model:           req.Model,
			ClientIP:        req.ClientIP,
			UserID:          req.UserID,
			RawPreview:      rawPreview,
			FilteredPreview: filteredPreview,
		}
		_ = s.repo.CreateLog(ctx, log)
	}

	return resp, nil
}

func splitLinesOrCommas(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	raw := strings.ReplaceAll(s, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	raw = strings.ReplaceAll(raw, ",", "\n")
	parts := strings.Split(raw, "\n")
	var result []string
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
