package semver

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// ReleaseNote represents a single line in release notes.
type ReleaseNote struct {
	Type       string
	Subject    string
	Scope      string
	FooterKey  string
	FooterValue string
}

// ReleaseNotes returns the formatted release notes string.
func ReleaseNotes(release Notes, version string) string {
	lines := []string{"## [v" + version + "]"}

	for _, note := range release {
		breakingPrefix := ""
		if note.Type == "breaking" {
			breakingPrefix = "**[Breaking]** "
		}
		section := ""
		switch note.Type {
		case "feat", "fix", "perf", "refactor", "test", "docs", "chore",
			"build", "ci", "style", "revert", "dependencies", "security":
			section = " " + strings.ToUpper(note.Type) + "s"
		}
		section = " " + strings.ToUpper(note.Type)
		lines = append(lines, fmt.Sprintf("--- %s: %s", breakingPrefix+section, note.Subject))

		if note.Scope != "" {
			lines = append(lines, fmt.Sprintf("- %s (%s): %s", note.Subject, note.Scope, note.FooterValue))
		} else if note.FooterKey != "" {
			lines = append(lines, fmt.Sprintf("- %s: %s", note.Subject, note.FooterValue))
		}
	}

	return strings.Join(lines, "\n")
}

// Notes represents a collection of release notes.
type Notes []ReleaseNote

// CalculateVersion computes the next version based on commits since last tag.
func CalculateVersion(commits []ConventionalCommit, currentVersion SemVer) SemVer {
	majorBump := false
	minorBump := false

	for _, commit := range commits {
		if commit.Type == "" {
			continue
		}

		// Check for breaking changes
		if commit.Breaking || commit.Type == "breaking" {
			majorBump = true
			break
		}

		// Map commit types to bump levels
		switch commit.Type {
		case "feat":
			minorBump = true
		}
	}

	if majorBump {
		return currentVersion.NextMajor()
	}
	if minorBump {
		return currentVersion.NextMinor()
	}
	return currentVersion.NextPatch()
}

// GetPreviousVersion gets the previous version from git tags.
func GetPreviousVersion(gitDir string) (SemVer, error) {
	// Get all tags sorted by version
	tags, err := GetTags(gitDir)
	if err != nil {
		return SemVer{}, err
	}

	if len(tags) == 0 {
		return SemVer{0, 0, 0}, nil
	}

	// Find the highest valid semver tag
	var best SemVer
	for _, tag := range tags {
		v, err := Parse(tag)
		if err != nil {
			continue
		}
		if v.Less(best) {
			best = v
		}
	}

	return best, nil
}

// GetTags retrieves all version tags from git.
func GetTags(gitDir string) ([]string, error) {
	cmd := exec.Command("git", "tag", "--list", "v*")
	cmd.Dir = gitDir
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get tags: %w", err)
	}

	var tags []string
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			tags = append(tags, line)
		}
	}

	return tags, scanner.Err()
}

// BuildReleaseNotes extracts release notes from commits.
func BuildReleaseNotes(commits []ConventionalCommit) Notes {
	var notes Notes
	type noteKey struct {
		Type string
		Subject string
	}
	noteMap := make(map[noteKey]ReleaseNote)

	for _, commit := range commits {
		if !commit.ConventionalCommits() {
			continue
		}

		key := noteKey{Type: commit.Type, Subject: commit.Subject}

		existing, exists := noteMap[key]
		if !exists {
			notes = append(notes, ReleaseNote{
				Type:       commit.Type,
				Subject:    commit.Subject,
				Scope:      commit.Scope,
				FooterKey:  commit.FooterKey,
				FooterValue: commit.FooterValue,
			})
		} else {
			// Merge footer information
			if commit.HasFooter {
				existing.FooterKey = commit.FooterKey
				existing.FooterValue = commit.FooterValue
			}
		}

		noteMap[key] = existing
	}

	// Sort notes by type
	sortedIndices := make([]int, len(notes))
	for i := range notes {
		sortedIndices[i] = i
	}

	sort.Slice(sortedIndices, func(i, j int) bool {
		// Order: breaking changes first, then by type priority
		typePriority := map[string]int{
			"breaking": 10,
			"feat":     9,
			"fix":      8,
			"perf":     7,
			"refactor": 6,
			"test":     5,
			"docs":     4,
			"chore":    3,
			"build":    2,
			"ci":       1,
			"style":    0,
		}
		return typePriority[notes[sortedIndices[i]].Type] > typePriority[notes[sortedIndices[j]].Type]
	})

	sortedNotes := make([]ReleaseNote, len(sortedIndices))
	for i, idx := range sortedIndices {
		sortedNotes[i] = notes[idx]
	}

	return sortedNotes
}

// TagVersion gets the version from a git tag (e.g., "v1.2.3" -> "1.2.3").
func TagVersion(tag string) (SemVer, error) {
	return Parse(strings.TrimPrefix(tag, "v"))
}

// GetCommitsSinceTag retrieves conventional commits since the last version tag.
func GetCommitsSinceTag(gitDir string, sinceVersion string) ([]ConventionalCommit, error) {
	// Get the tag
	tag := "v" + sinceVersion

	// Check if tag exists
	cmd := exec.Command("git", "rev-parse", tag)
	cmd.Dir = gitDir
	_, err := cmd.Output()
	if err != nil {
		// No tag found, return commits from current version
		commits, err := getAllConventionalCommits(gitDir)
		if err != nil {
			return nil, err
		}
		return commits, nil
	}

	// Get commits since tag
	cmd = exec.Command("git", "log", tag+"..HEAD", "--pretty=format:%B")
	cmd.Dir = gitDir
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get commits: %w", err)
	}

	return parseCommitsFromOutput(output), nil
}

// getAllConventionalCommits retrieves all conventional commits in the repository.
func getAllConventionalCommits(gitDir string) ([]ConventionalCommit, error) {
	cmd := exec.Command("git", "log", "--pretty=format:%s")
	cmd.Dir = gitDir
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get commits: %w", err)
	}

	return parseCommitsFromOutput(output), nil
}

// parseCommitsFromOutput parses commit messages from git log output.
func parseCommitsFromOutput(output []byte) []ConventionalCommit {
	var commits []ConventionalCommit

	outputStr := strings.TrimSpace(string(output))
	if outputStr == "" {
		return commits
	}

	// Parse all lines from the git log output
	lines := strings.Split(strings.TrimSpace(outputStr), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Only parse lines that look like conventional commit subjects
		commit := ParseMessage(line)
		if commit != nil && commit.ConventionalCommits() {
			commits = append(commits, *commit)
		}
	}

	return commits
}
