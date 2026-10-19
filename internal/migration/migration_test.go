package migration_test

import (
	"testing"
	"time"

	"github.com/useshiftctl/shiftctl/internal/migration"
)

// — SanitizeName ──────────────────────────────────────────────────────────────

func TestSanitizeName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"github-to-gitlab", "github-to-gitlab"},
		{"GitHub-to-GitLab", "github-to-gitlab"},
		{"  hello world  ", "hello-world"},
		{"hello world", "hello-world"},
		{"Hello!@#$World", "helloworld"},
		{"MY_MIGRATION", "my_migration"},
		{"", ""},
		{"   ", ""},
		{"github-gitlab-2026", "github-gitlab-2026"},
		{"abc123", "abc123"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := migration.SanitizeName(tt.input)
			if got != tt.want {
				t.Errorf("SanitizeName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// — ValidateName ──────────────────────────────────────────────────────────────

func TestValidateName(t *testing.T) {
	valid := []string{
		"github-to-gitlab",
		"a",
		"abc123",
		"my_migration",
		"github-gitlab-2026",
		"GitHub GitLab", // sanitizes to "github-gitlab" — valid
	}
	for _, name := range valid {
		t.Run("valid:"+name, func(t *testing.T) {
			if err := migration.ValidateName(name); err != nil {
				t.Errorf("ValidateName(%q) returned unexpected error: %v", name, err)
			}
		})
	}

	invalid := []string{
		"",
		"   ",
		"-starts-with-hyphen",
		"_starts-with-underscore",
	}
	for _, name := range invalid {
		t.Run("invalid:"+name, func(t *testing.T) {
			if err := migration.ValidateName(name); err == nil {
				t.Errorf("ValidateName(%q) expected error, got nil", name)
			}
		})
	}
}

// — Save / Load / Exists / ListAll ────────────────────────────────────────────

func TestSaveAndLoad(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	rec := &migration.Record{
		Name:            "test-migration",
		SourcePlatform:  "github",
		DestinationName: "GitLab",
		Context:         "skip marketing repos",
		CreatedAt:       time.Now().UTC().Truncate(time.Second),
		Status:          migration.StatusSetup,
	}

	if err := migration.Save(rec); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	got, err := migration.Load("test-migration")
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if got.Name != rec.Name {
		t.Errorf("Name: got %q, want %q", got.Name, rec.Name)
	}
	if got.SourcePlatform != rec.SourcePlatform {
		t.Errorf("SourcePlatform: got %q, want %q", got.SourcePlatform, rec.SourcePlatform)
	}
	if got.DestinationName != rec.DestinationName {
		t.Errorf("DestinationName: got %q, want %q", got.DestinationName, rec.DestinationName)
	}
	if got.Context != rec.Context {
		t.Errorf("Context: got %q, want %q", got.Context, rec.Context)
	}
	if got.Status != rec.Status {
		t.Errorf("Status: got %q, want %q", got.Status, rec.Status)
	}
}

func TestExists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	exists, err := migration.Exists("nonexistent")
	if err != nil {
		t.Fatalf("Exists() error before save: %v", err)
	}
	if exists {
		t.Fatal("Exists() returned true before any save")
	}

	rec := &migration.Record{
		Name:      "exists-test",
		Status:    migration.StatusSetup,
		CreatedAt: time.Now().UTC(),
	}
	if err := migration.Save(rec); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	exists, err = migration.Exists("exists-test")
	if err != nil {
		t.Fatalf("Exists() error after save: %v", err)
	}
	if !exists {
		t.Fatal("Exists() returned false after save")
	}
}

func TestListAll(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	names, err := migration.ListAll()
	if err != nil {
		t.Fatalf("ListAll() error on empty dir: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("ListAll() expected 0 entries, got %d", len(names))
	}

	for _, name := range []string{"alpha", "beta", "gamma"} {
		if err := migration.Save(&migration.Record{
			Name:      name,
			Status:    migration.StatusSetup,
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("Save(%q) error: %v", name, err)
		}
	}

	names, err = migration.ListAll()
	if err != nil {
		t.Fatalf("ListAll() error: %v", err)
	}
	if len(names) != 3 {
		t.Fatalf("ListAll() expected 3 entries, got %d: %v", len(names), names)
	}
	// Results must be sorted alphabetically.
	want := []string{"alpha", "beta", "gamma"}
	for i, name := range names {
		if name != want[i] {
			t.Errorf("ListAll()[%d] = %q, want %q", i, name, want[i])
		}
	}
}
