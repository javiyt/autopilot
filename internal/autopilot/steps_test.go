package autopilot

import "testing"

func TestInvestigateStatusParser(t *testing.T) {
	cases := []struct {
		name   string
		report string
		want   string
		wantOK bool
	}{
		{"exact first line", "STATUS: READY\nrest", "READY", true},
		{"heading decoration", "## STATUS: READY\ndetails", "READY", true},
		{"bold decoration", "**STATUS: READY**\ndetails", "READY", true},
		{"preamble before marker", "Now I have enough context to compile a report:\n\nSTATUS: READY\n\n# Report", "READY", true},
		{"cr line endings", "intro\r\nSTATUS: READY\r\ndetails", "READY", true},
		{"blocked wins when first", "Preamble\nSTATUS: BLOCKED\nlater I found STATUS: READY", "BLOCKED", true},
		{"mixed case heading as in issue 157 report", "Based on my investigation...\n\n---\n\n## INVESTIGATION REPORT: Issue #157\n\n### Status: BLOCKED\n\nThe issue is ...", "BLOCKED", true},
		{"lowercase with trailing period", "intro\nstatus: ready.\ndetails", "READY", true},
		{"discussion of marker is not a marker", "The issue says to print STATUS: READY when done.\nMore analysis", "", false},
		{"no marker", "Just a plain report", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := investigateStatus(tc.report)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("investigateStatus = (%q, %t), want (%q, %t)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestReviewDecisionParser(t *testing.T) {
	cases := []struct {
		name   string
		report string
		want   string
	}{
		{"exact", "DECISION: APPROVE", "DECISION: APPROVE"},
		{"preamble and final verdict", "analysis...\n\nfiles checked\n\nDECISION: REQUEST_CHANGES", "DECISION: REQUEST_CHANGES"},
		{"bold and heading", "text\n**DECISION: APPROVE**", "DECISION: APPROVE"},
		{"last wins over mid mention", "A DECISION: REQUEST_CHANGES would be needed if X.\n\nfinal check done\nDECISION: APPROVE", "DECISION: APPROVE"},
		{"none", "Since I cannot execute commands, let me compile my review.\n## Summary\n...", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reviewDecision(tc.report); got != tc.want {
				t.Fatalf("reviewDecision = %q, want %q", got, tc.want)
			}
		})
	}
}
