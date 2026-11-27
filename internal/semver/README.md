# Semantic Version Calculator

A Go library for automatic SemVer version calculation based on Conventional Commits.

## Features

- **Conventional Commits Parsing**: Recognizes standard commit types (feat, fix, chore, etc.)
- **Semantic Versioning**: Calculates next version based on commit history
- **Breaking Change Detection**: Identifies breaking changes marked with `!` or `BREAKING CHANGE:`
- **Safe Tag Creation**: Handles concurrent pushes and prevents duplicate tags
- **Release Notes Generation**: Builds formatted release notes from commits

## Package Structure

```
internal/semver/
├── README.md
├── types.go          # Core types and ConventionalCommit parsing
├── version.go        # Version calculation and tag management
├── semver.go         # SemanticVersionManager implementation
└── semver_test.go    # Comprehensive test coverage
```

## Usage

```go
import "github.com/javimartin/pi-autopilot/internal/semver"

// Initialize the version manager
vm, err := semver.New(".")
if err != nil {
    log.Fatal(err)
}
defer vm.ReleaseLock()

// Calculate the next version
newVersion, err := vm.BumpVersion()
if err != nil {
    log.Fatal(err)
}

fmt.Printf("New version: %s\n", newVersion.String())

// Create a tag
if err := vm.CreateTag(newVersion); err != nil {
    log.Fatal(err)
}

// Generate release notes
notes, err := vm.BuildReleaseNotes(previousVersion, newVersion)
if err != nil {
    log.Fatal(err)
}
```

## Supported Commit Types

| Type | Bump Type | Example |
|------|-----------|---------|
| `feat` | Minor | `feat: add new feature` |
| `fix` | Patch | `fix: resolve memory leak` |
| `perf` | Patch | `perf: optimize database queries` |
| `refactor` | Patch | `refactor: clean up code` |
| `test` | Patch | `test: add unit tests` |
| `docs` | Patch | `docs: update README` |
| `chore` | Patch | `chore: update dependencies` |
| `build` | Patch | `build: update build config` |
| `ci` | Patch | `ci: update GitHub Actions` |
| `style` | Patch | `style: format code` |
| `revert` | Patch | `revert: undo last change` |
| `bump` | Patch | `bump: update package versions` |
| `dependencies` | Patch | `dependencies: update deps` |
| `security` | Patch | `security: fix vulnerability` |

## Breaking Changes

Breaking changes are automatically detected when a commit message contains:
- `!` after the type (e.g., `feat!:`)
- `BREAKING CHANGE:` or `BREAKING-CHANGE:` footer

Example:
```
feat!: remove deprecated API

BREAKING CHANGE: The deprecated API has been removed
```

## Version Bump Logic

- **Major**: At least one breaking change since last release
- **Minor**: At least one feature since last release, no breaking changes
- **Patch**: Any other valid commits since last release

## Concurrency Safety

The `SemanticVersionManager` uses mutex locks to ensure:
- Atomic version calculations
- No duplicate tags
- Safe concurrent access
- Automatic release of locks

## Running Tests

```bash
go test -v ./internal/semver/...
```

## Command-Line Tool

Use the `bump-version` binary to calculate and display the new version:

```bash
bump-version
```

## License

MIT

## GitHub Actions Integration

A GitHub Actions workflow is available at `.github/workflows/release.yml` that automates the release process.

### Features

- **Triggered on**: Pushes to the `main` branch with Go code changes
- **Version Calculation**: Uses the `bump-version` tool to calculate the next SemVer version
- **Git Tag Creation**: Automatically creates and pushes version tags (e.g., `v1.0.0`)
- **Release Notes Generation**: Creates markdown release notes
- **GitHub Release Creation**: Publishes a draft release on GitHub

### Requirements

To enable this workflow, you need to:

1. Have a GitHub repository
2. The workflow will use the automatic `GITHUB_TOKEN` for creating tags and drafts
3. For fully published releases (not drafts), configure a personal access token as `GITHUB_TOKEN` secret

### Workflow Steps

1. **Checkout Code**: Fetches full git history
2. **Setup Go**: Installs the required Go version
3. **Build Tool**: Compiles the `bump-version` binary
4. **Calculate Version**: Runs version calculation
5. **Create Tag**: Pushes Git tag to remote
6. **Generate Release Notes**: Creates markdown release notes
7. **Create Release**: Publishes draft release on GitHub

### Manual Override

To run the workflow manually or override version calculation:

```bash
# Run bump-version manually
bump-version

# Create tag manually
git tag v1.0.0
git push origin v1.0.0
```

### Customization

To customize the release workflow:

1. Edit `.github/workflows/release.yml`
2. Change trigger conditions in `on:` section
3. Modify release note templates
4. Adjust GitHub CLI permissions as needed
