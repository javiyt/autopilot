package semver

import (
	"regexp"
	"strconv"
	"strings"
)

// SemVer represents a semantic version.
type SemVer struct {
	Major int
	Minor int
	Patch int
}

// String returns the canonical SemVer string.
func (v SemVer) String() string {
	return strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) + "." + strconv.Itoa(v.Patch)
}

// Parse parses a SemVer string.
func Parse(s string) (SemVer, error) {
	var v SemVer
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return v, &InvalidVersionError{s}
	}

	var err error
	v.Major, err = strconv.Atoi(parts[0])
	if err != nil {
		return v, &InvalidVersionError{s}
	}
	v.Minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return v, &InvalidVersionError{s}
	}
	v.Patch, err = strconv.Atoi(parts[2])
	if err != nil {
		return v, &InvalidVersionError{s}
	}

	return v, nil
}

// NextMajor returns the next major version.
func (v SemVer) NextMajor() SemVer {
	return SemVer{v.Major + 1, 0, 0}
}

// NextMinor returns the next minor version.
func (v SemVer) NextMinor() SemVer {
	return SemVer{v.Major, v.Minor + 1, 0}
}

// NextPatch returns the next patch version.
func (v SemVer) NextPatch() SemVer {
	return SemVer{v.Major, v.Minor, v.Patch + 1}
}

// Less returns true if v is less than other.
func (v SemVer) Less(other SemVer) bool {
	if v.Major != other.Major {
		return v.Major < other.Major
	}
	if v.Minor != other.Minor {
		return v.Minor < other.Minor
	}
	return v.Patch < other.Patch
}

// Equal returns true if v equals other.
func (v SemVer) Equal(other SemVer) bool {
	return v.Major == other.Major && v.Minor == other.Minor && v.Patch == other.Patch
}

// InvalidVersionError represents a version parsing error.
type InvalidVersionError struct {
	Original string
}

func (e *InvalidVersionError) Error() string {
	return "invalid version: " + e.Original
}

// ConventionalCommit represents a parsed conventional commit.
type ConventionalCommit struct {
	Type       string
	Subject    string
	Scope      string
	Breaking   bool
	HasFooter  bool
	FooterKey  string
	FooterValue string
	Prefix     string
}

// ConventionalCommits returns true if this is a valid conventional commit.
func (c *ConventionalCommit) ConventionalCommits() bool {
	return c.Type != "" && c.Subject != ""
}

// ParseMessage parses a commit message into a ConventionalCommit.
func ParseMessage(message string) *ConventionalCommit {
	msg := strings.TrimSpace(message)
	if msg == "" {
		return nil
	}

	commit := &ConventionalCommit{}

	// Check for merge commits
	if strings.HasPrefix(msg, "Merge ") || strings.HasPrefix(msg, "merged ") {
		return nil
	}

	// Pattern: type(scope): subject or type: subject
	pattern := regexp.MustCompile(`^[a-zA-Z]+(?:\([^)]+\))?:\s*(.*)$`)
	parts := pattern.FindStringSubmatch(msg)
	
	if len(parts) >= 2 {
		commit.Type = strings.ToLower(parts[1])
		subjectParts := strings.SplitN(msg, ":", 2)
		commit.Subject = strings.TrimSpace(subjectParts[1])
	} else {
		commit.Type = "other"
		commit.Subject = strings.TrimSpace(msg)
	}

	// Check for breaking change indicators
	if strings.Contains(msg, "!") || strings.Contains(msg, "BREAKING CHANGE:") || strings.Contains(msg, "BREAKING-CHANGE:") {
		commit.Breaking = true
	}

	// Check for footer
	footerPattern := regexp.MustCompile(`^([a-zA-Z]+)\s*:\s*(.*)$`)
	for _, line := range strings.Split(msg, "\n") {
		footerParts := footerPattern.FindStringSubmatch(line)
		if len(footerParts) >= 3 {
			commit.HasFooter = true
			commit.FooterKey = strings.TrimSpace(footerParts[1])
			commit.FooterValue = strings.TrimSpace(footerParts[2])
			break
		}
	}

	return commit
}

// ClassifyCommitType classifies a commit type for version bumping.
func ClassifyCommitType(t string) (string, bool) {
	conventionalTypes := map[string]bool{
		"feat":            true,
		"fix":             true,
		"perf":            true,
		"refactor":        true,
		"test":            true,
		"docs":            true,
		"chore":           true,
		"build":           true,
		"ci":              true,
		"style":           true,
		"revert":          true,
		"bump":            true,
		"dependencies":    true,
		"security":        true,
	}

	if conventionalTypes[t] {
		return t, true
	}
	return t, false
}
