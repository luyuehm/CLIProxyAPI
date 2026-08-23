package costallocation

import (
	"testing"
	"time"

	"cpa-usage-keeper/internal/entities"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:?_busy_timeout=5000&_foreign_keys=on"), &gorm.Config{})
	if err != nil {
		t.Skipf("sqlite driver unavailable in this environment (cgo disabled): %v", err)
	}
	if err := db.AutoMigrate(&entities.CostAllocationRule{}, &entities.UsageEvent{}); err != nil {
		t.Fatalf("migrate test db: %v", err)
	}
	return db
}

func TestNewService(t *testing.T) {
	db := openTestDB(t)
	s := NewService(db)
	if s == nil {
		t.Fatal("NewService returned nil")
	}
}

func TestCreateRule(t *testing.T) {
	db := openTestDB(t)
	s := NewService(db)

	view, err := s.CreateRule(CostAllocationRuleCreateInput{
		Name:        "Engineering",
		Dimension:   entities.CostAllocationDimensionDepartment,
		MatchType:   entities.CostAllocationMatchAPIKey,
		MatchValues: []string{"key-eng-01", "key-eng-02"},
		Enabled:     true,
		Priority:    10,
		Note:        "Engineering dept",
	})
	if err != nil {
		t.Fatalf("CreateRule failed: %v", err)
	}
	if view.ID == 0 {
		t.Fatal("CreateRule: expected non-zero ID")
	}
	if view.Name != "Engineering" {
		t.Fatalf("CreateRule: name = %q, want %q", view.Name, "Engineering")
	}
	if view.Dimension != entities.CostAllocationDimensionDepartment {
		t.Fatalf("CreateRule: dimension = %q", view.Dimension)
	}
	if len(view.MatchValues) != 2 {
		t.Fatalf("CreateRule: match_values count = %d, want 2", len(view.MatchValues))
	}
}

func TestCreateRuleValidation(t *testing.T) {
	db := openTestDB(t)
	s := NewService(db)

	cases := []struct {
		name  string
		input CostAllocationRuleCreateInput
	}{
		{"empty name", CostAllocationRuleCreateInput{Name: "", Dimension: entities.CostAllocationDimensionDepartment, MatchType: entities.CostAllocationMatchAPIKey, MatchValues: []string{"v1"}}},
		{"invalid dimension", CostAllocationRuleCreateInput{Name: "Test", Dimension: "invalid", MatchType: entities.CostAllocationMatchAPIKey, MatchValues: []string{"v1"}}},
		{"invalid match type", CostAllocationRuleCreateInput{Name: "Test", Dimension: entities.CostAllocationDimensionDepartment, MatchType: "invalid", MatchValues: []string{"v1"}}},
		{"empty match values", CostAllocationRuleCreateInput{Name: "Test", Dimension: entities.CostAllocationDimensionDepartment, MatchType: entities.CostAllocationMatchAPIKey, MatchValues: []string{}}},
	}
	for _, tc := range cases {
		_, err := s.CreateRule(tc.input)
		if err == nil {
			t.Fatalf("CreateRule %q: expected error, got nil", tc.name)
		}
	}
}

func TestListRules(t *testing.T) {
	db := openTestDB(t)
	s := NewService(db)

	rules, err := s.ListRules()
	if err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("ListRules: expected 0, got %d", len(rules))
	}

	_, _ = s.CreateRule(CostAllocationRuleCreateInput{
		Name: "DeptA", Dimension: entities.CostAllocationDimensionDepartment, MatchType: entities.CostAllocationMatchAPIKey, MatchValues: []string{"key-a"},
	})
	_, _ = s.CreateRule(CostAllocationRuleCreateInput{
		Name: "DeptB", Dimension: entities.CostAllocationDimensionDepartment, MatchType: entities.CostAllocationMatchAPIKey, MatchValues: []string{"key-b"},
	})

	rules, err = s.ListRules()
	if err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("ListRules: expected 2, got %d", len(rules))
	}
}

func TestUpdateRule(t *testing.T) {
	db := openTestDB(t)
	s := NewService(db)

	view, _ := s.CreateRule(CostAllocationRuleCreateInput{
		Name: "Dept", Dimension: entities.CostAllocationDimensionDepartment, MatchType: entities.CostAllocationMatchAPIKey, MatchValues: []string{"key-01"},
	})

	updated, err := s.UpdateRule(view.ID, CostAllocationRuleUpdateInput{
		Name: strPtr("Updated Dept"),
	})
	if err != nil {
		t.Fatalf("UpdateRule: %v", err)
	}
	if updated.Name != "Updated Dept" {
		t.Fatalf("UpdateRule: name = %q", updated.Name)
	}

	enabled := false
	updated2, err := s.UpdateRule(view.ID, CostAllocationRuleUpdateInput{
		Enabled: &enabled,
	})
	if err != nil {
		t.Fatalf("UpdateRule: %v", err)
	}
	if updated2.Enabled {
		t.Fatal("UpdateRule: expected enabled=false")
	}

	// Update non-existent rule
	_, err = s.UpdateRule(99999, CostAllocationRuleUpdateInput{Name: strPtr("Nope")})
	if err != ErrRuleNotFound {
		t.Fatalf("UpdateRule non-existent: err = %v, want ErrRuleNotFound", err)
	}
}

func TestDeleteRule(t *testing.T) {
	db := openTestDB(t)
	s := NewService(db)

	view, _ := s.CreateRule(CostAllocationRuleCreateInput{
		Name: "Dept", Dimension: entities.CostAllocationDimensionDepartment, MatchType: entities.CostAllocationMatchAPIKey, MatchValues: []string{"key-01"},
	})

	if err := s.DeleteRule(view.ID); err != nil {
		t.Fatalf("DeleteRule: %v", err)
	}

	// Delete non-existent
	if err := s.DeleteRule(99999); err != ErrRuleNotFound {
		t.Fatalf("DeleteRule non-existent: err = %v, want ErrRuleNotFound", err)
	}

	rules, _ := s.ListRules()
	if len(rules) != 0 {
		t.Fatalf("DeleteRule: expected 0 rules, got %d", len(rules))
	}
}

func TestNormalizeRange(t *testing.T) {
	start, end := normalizeRange(time.Time{}, time.Time{})
	if start.IsZero() || end.IsZero() {
		t.Fatal("normalizeRange: got zero time for empty input")
	}
}

func TestValidateDimension(t *testing.T) {
	if err := validateDimension(entities.CostAllocationDimensionDepartment); err != nil {
		t.Fatalf("department: %v", err)
	}
	if err := validateDimension(entities.CostAllocationDimensionTeam); err != nil {
		t.Fatalf("team: %v", err)
	}
	if err := validateDimension(entities.CostAllocationDimensionProject); err != nil {
		t.Fatalf("project: %v", err)
	}
	if err := validateDimension("invalid"); err == nil {
		t.Fatal("validateDimension: expected error for invalid")
	}
}

func TestSplitMatchValues(t *testing.T) {
	cases := []struct {
		input string
		want  int
	}{
		{"a,b,c", 3},
		{"a\nb\nc", 3},
		{"a, b, c", 3},
		{"", 0},
		{"   ", 0},
		{"a,", 1},
	}
	for _, tc := range cases {
		got := splitMatchValues(tc.input)
		if len(got) != tc.want {
			t.Fatalf("splitMatchValues(%q): len=%d, want %d", tc.input, len(got), tc.want)
		}
	}
}

func TestMatchValuesToEntity(t *testing.T) {
	got := matchValuesToEntity([]string{"key-a", "key-b"})
	if got != "key-a,key-b" {
		t.Fatalf("matchValuesToEntity: %q", got)
	}
	got = matchValuesToEntity([]string{"  key-a  ", "\nkey-b\n"})
	if got != "key-a,key-b" {
		t.Fatalf("matchValuesToEntity with whitespace: %q", got)
	}
}

func strPtr(s string) *string {
	return &s
}
