package semver

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func setupTestRepo(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "semver-test-*")
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	if _, err := exec.Command("git", "init").CombinedOutput(); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.Command("git", "config", "user.email", "test@test.com").CombinedOutput(); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.Command("git", "config", "user.name", "Test User").CombinedOutput(); err != nil {
		t.Fatal(err)
	}

	return dir
}

func TestParseSemVer(t *testing.T) {
	tests := []struct {
		version string
		valid   bool
	}{
		{"1.2.3", true},
		{"0.0.1", true},
		{"10.20.30", true},
		{"1.0.0", true},
		{"v1.2.3", true},
		{"1.2", false},
		{"a.b.c", false},
		{"1.2.3.4", false},
	}

	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			v, err := Parse(tt.version)
			if tt.valid && err != nil {
				t.Errorf("Parse(%q) error = %v, want nil", tt.version, err)
			}
			if !tt.valid && err == nil {
				t.Errorf("Parse(%q) = %v, want error", tt.version, v)
			}
		})
	}
}

func TestSemVerArithmetic(t *testing.T) {
	v := SemVer{1, 2, 3}

	tests := []struct {
		name     string
		op       func(SemVer) SemVer
		expected SemVer
	}{
		{"NextMajor", v.NextMajor, SemVer{2, 0, 0}},
		{"NextMinor", v.NextMinor, SemVer{1, 3, 0}},
		{"NextPatch", v.NextPatch, SemVer{1, 2, 4}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.op(v)
			if result != tt.expected {
				t.Errorf("%s() = %v, want %v", tt.name, result, tt.expected)
			}
		})
	}
}

func TestConventionalCommits(t *testing.T) {
	tests := []struct {
		name     string
		message  string
		wantType string
		wantB    bool
	}{
		{"feat without scope", "feat: add new feature", "feat", false},
		{"feat with scope", "feat(auth): add OAuth support", "feat", false},
		{"fix breaking", "fix(core): critical bug! (BREAKING CHANGE: changes API)", "fix", true},
		{"feat breaking", "feat!: new breaking feature (BREAKING CHANGE: API changed)", "feat", true},
		{"chore", "chore: update dependencies", "chore", false},
		{"docs", "docs: update README", "docs", false},
		{"test", "test: add unit tests", "test", false},
		{"refactor", "refactor: clean up code", "refactor", false},
		{"revert", "revert: undo last change", "revert", false},
		{"merge commit", "Merge branch 'feature' into main", "other", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			commit := ParseMessage(tt.message)

			if commit == nil {
				t.Fatal("ParseMessage returned nil")
			}

			if !commit.ConventionalCommits() {
				t.Error("Not recognized as conventional commit")
			}

			if commit.Type != tt.wantType {
				t.Errorf("Type = %q, want %q", commit.Type, tt.wantType)
			}

			if commit.Breaking != tt.wantB {
				t.Errorf("Breaking = %v, want %v", commit.Breaking, tt.wantB)
			}
		})
	}
}
