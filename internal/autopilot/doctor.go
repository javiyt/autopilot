package autopilot

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

func Doctor(parent context.Context, cfg Config) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	var lines []string
	for _, tool := range []string{"git", "gh", "pi"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			return "", err
		}
		lines = append(lines, tool+": "+path)
	}
	out, err := command(ctx, "", "gh", "auth", "status")
	if err != nil {
		return "", fmt.Errorf("gh authentication: %w", err)
	}
	lines = append(lines, "GitHub: authenticated")
	_ = out
	models, err := command(ctx, "", "pi", "--list-models")
	if err != nil {
		return "", fmt.Errorf("Pi model inventory: %w", err)
	}
	for _, role := range []string{"investigate", "plan", "implement", "review", "chat"} {
		name := cfg.Models[role].ID
		if !strings.Contains(models, name) {
			lines = append(lines, "WARNING: model "+name+" not found in pi --list-models (verify the exact ID).")
		} else {
			lines = append(lines, role+" model: "+name)
		}
	}
	return strings.Join(lines, "\n"), nil
}
