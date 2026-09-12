package render

import (
	"io"

	"github.com/owenrumney/go-sarif/v2/sarif"
	"github.com/pgrundev/pgbot/internal/model"
)

// docsBaseURL is where a finding's catalogue page lives, used as the SARIF
// helpUri so GitHub's Security tab links each result to its explanation.
const docsBaseURL = "https://github.com/pgrundev/pgbot/blob/main/docs/findings/"

// SARIF writes the findings as SARIF 2.1.0 — the format GitHub's code-scanning
// upload turns into Security-tab alerts. The catalogue built in B7 is exactly the
// metadata SARIF wants: finding id → ruleId, severity → level, the docs page →
// helpUri, and the finding's Object → a logical location. Suppressed findings are
// emitted with a SARIF `suppressions` entry, not dropped, so a reviewer sees what
// a config muted. Built with go-sarif so the output is valid by construction.
func SARIF(w io.Writer, c *model.Context) error {
	report, err := sarif.New(sarif.Version210)
	if err != nil {
		return err
	}
	run := sarif.NewRunWithInformationURI("pgbot", "https://pgbot.dev")

	seenRule := map[string]bool{}
	for i := range c.Findings {
		f := &c.Findings[i]
		// --fail-on-new (D3-2): a finding already in the base isn't a regression
		// this change introduced. Excluded from SARIF so the Security tab annotates
		// only what the PR added (it stays in --json). Suppressed criticals still
		// surface via their suppression below.
		if f.Preexisting {
			continue
		}
		if !seenRule[f.ID] {
			seenRule[f.ID] = true
			rule := run.AddRule(f.ID).
				WithHelpURI(docsBaseURL + f.ID + ".md").
				WithDescription(f.Title)
			rule.WithProperties(sarif.Properties{
				"dimension":         f.Impact.Dimension,
				"defaultSeverity":   f.Severity,
				"security-severity": securityScore(f),
			})
		}

		result := run.CreateResultForRule(f.ID).
			WithLevel(sarifLevel(f.Severity)).
			WithMessage(sarif.NewTextMessage(resultMessage(f)))

		// A database object isn't a source file; a logical location is the honest
		// SARIF shape and GitHub groups results by it.
		obj := f.Object
		if obj == "" {
			obj = "cluster"
		}
		loc := sarif.NewLocation().WithLogicalLocations([]*sarif.LogicalLocation{
			sarif.NewLogicalLocation().WithFullyQualifiedName(obj).WithKind("resource"),
		})
		result.AddLocation(loc)

		if f.Suppressed {
			result.WithSuppression([]*sarif.Suppression{
				sarif.NewSuppression("external").
					WithJustifcation(strOr(f.SuppressionReason, "suppressed by .pgbot.toml")),
			})
		}
	}

	report.AddRun(run)
	return report.PrettyWrite(w)
}

// sarifLevel maps pgbot severities to SARIF result levels.
func sarifLevel(sev string) string {
	switch sev {
	case model.SeverityCritical:
		return "error"
	case model.SeverityWarn:
		return "warning"
	default:
		return "note"
	}
}

// securityScore gives GitHub a 0–10 severity it sorts by (critical=9, warn=5.5,
// info=2), mapped from the finding's CLASS — Impact scores are not comparable
// across dimensions (a risk-95 and a cost-5 finding are both "info" to a
// security scanner), so severity is the only honest basis.
func securityScore(f *model.Finding) string {
	switch f.Severity {
	case model.SeverityCritical:
		return "9.0"
	case model.SeverityWarn:
		return "5.5"
	default:
		return "2.0"
	}
}

// resultMessage is the per-result text: the title, the remediation, and — for a
// destructive-action finding — its structured safety guards, so a DROP INDEX /
// VACUUM FULL warning is never lost on the way to the GitHub Security tab.
func resultMessage(f *model.Finding) string {
	msg := f.Title
	if f.Remediation != "" {
		msg += " — " + f.Remediation
	}
	if s := safetyText(f); s != "" {
		msg += " " + s
	}
	return msg
}

func strOr(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}
