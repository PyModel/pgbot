package config

import (
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

// Apply enforces the config over already-computed findings, in the precedence
// the spec fixes: [thresholds] were already applied before Compute (they change
// whether a finding is produced at all); here we remap [severity], then apply
// [[ignore]] matches. A suppressed finding is NEVER deleted — the renderer and
// exit-code logic read the Suppressed flag and decide visibility.
//
// Aggregate findings (Objects populated) suppress PER ROW: an object-scoped rule
// drops only the matching entries and the finding survives on the rest; the whole
// finding is suppressed only if a rule with no object matches, or every row is.
// This is what makes muting one sequence not swallow a new one. Rules that fire
// this run are recorded for the dead-rule detector (B2-3).
//
// now is used to skip expired ignore rules (B2-3).
func (c *Config) Apply(fs []model.Finding, now time.Time) []model.Finding {
	c.matched = map[string]bool{}
	for i := range fs {
		f := &fs[i]
		if sv, ok := c.Severity[f.ID]; ok && sv != f.Severity {
			f.SeverityRemapped = f.Severity
			f.Severity = sv
		}
		c.applyIgnore(f, now)
	}
	return fs
}

// applyIgnore marks/filters one finding by the ignore rules.
func (c *Config) applyIgnore(f *model.Finding, now time.Time) {
	if len(f.Objects) > 0 {
		c.suppressRows(f, now)
		return
	}
	if rule, ok := c.matchIgnore(f.ID, f.Object, now); ok {
		c.matched[rule.String()] = true
		f.Suppressed = true
		f.SuppressionReason = rule.Reason
		f.SuppressionRule = rule.String()
	}
}

// suppressRows applies ignore rules to an aggregate finding at the row level.
func (c *Config) suppressRows(f *model.Finding, now time.Time) {
	// A rule with no object mutes the whole aggregate (the "all of them" case).
	for _, r := range c.Ignore {
		if r.Finding == f.ID && r.Object == "" && !expired(r, now) {
			c.matched[r.String()] = true
			f.Suppressed = true
			f.SuppressionReason = r.Reason
			f.SuppressionRule = r.String()
			return
		}
	}
	// Otherwise drop only the rows an object-scoped rule matches.
	var keepEv, keepObj, cut []string
	var lastRule, lastReason string
	for i, obj := range f.Objects {
		rule, ok := c.matchIgnore(f.ID, obj, now)
		if ok {
			c.matched[rule.String()] = true
			cut = append(cut, obj)
			lastRule = rule.String()
			if rule.Reason != "" {
				lastReason = rule.Reason
			}
			continue
		}
		keepObj = append(keepObj, obj)
		if i < len(f.Evidence) {
			keepEv = append(keepEv, f.Evidence[i])
		}
	}
	switch {
	case len(cut) == 0:
		return // nothing matched
	case len(keepObj) == 0:
		// Every row matched → suppress the whole finding.
		f.Suppressed = true
		f.SuppressionReason = lastReason
		f.SuppressionRule = lastRule
	default:
		// Partial: keep the survivors, correct the leading count, and note what
		// was dropped so the suppression is never silent.
		f.Evidence, f.Objects = keepEv, keepObj
		f.Title = decrementLeadingCount(f.Title, len(keepObj))
		f.Caveats = append(f.Caveats, fmt.Sprintf("%d entr%s suppressed by config (%s): %s",
			len(cut), plural(len(cut), "y", "ies"), lastRule, strings.Join(cut, ", ")))
	}
}

// decrementLeadingCount rewrites the leading integer of an aggregate title to n
// (titles are built as "%d thing(s) …"). If the title doesn't start with a digit
// it is returned unchanged. Secondary stats in the title (worst %, total bytes)
// are not recomputed and reflect the pre-suppression set — the survivor list is
// authoritative.
func decrementLeadingCount(title string, n int) string {
	i := 0
	for i < len(title) && title[i] >= '0' && title[i] <= '9' {
		i++
	}
	if i == 0 {
		return title
	}
	return fmt.Sprintf("%d%s", n, title[i:])
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// matchIgnore returns the most specific active ignore rule for (id, object).
// Specificity: an exact object beats a glob beats an omitted object; ties break
// to the earliest rule in the file (stable).
func (c *Config) matchIgnore(id, object string, now time.Time) (IgnoreRule, bool) {
	best := -1
	bestSpec := 0
	for i, r := range c.Ignore {
		if r.Finding != id || expired(r, now) {
			continue
		}
		spec, ok := matchObject(r.Object, object)
		if ok && spec > bestSpec {
			bestSpec, best = spec, i
		}
	}
	if best < 0 {
		return IgnoreRule{}, false
	}
	return c.Ignore[best], true
}

// matchObject scores how specifically pattern matches object:
// exact=3, glob=2, omitted (match every object of the finding)=1, no match=0.
func matchObject(pattern, object string) (int, bool) {
	switch {
	case pattern == "":
		return 1, true
	case pattern == object:
		return 3, true
	}
	if ok, _ := path.Match(pattern, object); ok {
		return 2, true
	}
	return 0, false
}

// expired reports whether an ignore rule's expiry date has passed. The rule is
// active through the whole of its `expires` day (UTC); expired the next day.
func expired(r IgnoreRule, now time.Time) bool {
	if r.Expires == "" {
		return false
	}
	d, err := time.Parse(expiryLayout, r.Expires)
	if err != nil {
		return false // malformed → already warned at load; treat as no expiry
	}
	n := now.UTC()
	nowDay := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
	return nowDay.After(d)
}

// AddInlineIgnores appends one-off ignore rules from repeated --ignore flags,
// each "finding[:object]" (B2-4). They carry a fixed reason so they're
// distinguishable from file rules in the report and in --json.
func (c *Config) AddInlineIgnores(specs []string) {
	for _, s := range specs {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		finding, object := s, ""
		if i := strings.IndexByte(s, ':'); i >= 0 {
			finding, object = s[:i], s[i+1:]
		}
		if finding == "" {
			continue
		}
		c.Ignore = append(c.Ignore, IgnoreRule{Finding: finding, Object: object, Reason: "--ignore flag"})
	}
}

// ExpiredFindings returns an info finding for each ignore rule whose expiry has
// passed (B2-3). The rule has stopped applying, so the finding it muted will
// resurface — by design. The note prompts the user to renew or delete it.
func (c *Config) ExpiredFindings(now time.Time) []model.Finding {
	var out []model.Finding
	for _, r := range c.Ignore {
		if r.Expires == "" || !expired(r, now) {
			continue
		}
		obj := r.Object
		if obj != "" {
			obj = " (" + obj + ")"
		}
		// Object is the rule's full identity INCLUDING its expiry: more than one
		// rule can expire in the same run, and every suppression_expired finding
		// becomes its own Prometheus series keyed by (id, object, …) — identical
		// Objects would produce duplicate label sets and an INVALID exposition
		// (bug_report.md N5).
		out = append(out, model.Finding{
			ID: "suppression_expired", Severity: model.SeverityInfo,
			Object:      fmt.Sprintf("%s%s expires=%s", r.Finding, obj, r.Expires),
			Title:       fmt.Sprintf("suppression for %s%s expired on %s", r.Finding, obj, r.Expires),
			Detail:      "This [[ignore]] rule's expires date has passed, so it no longer suppresses anything and the finding it muted will surface again. Suppressions are meant to be revisited, not left forever.",
			Remediation: "Confirm the underlying issue is resolved and delete the rule, or renew its `expires` date if it still applies.",
			Impact:      model.Impact{Score: 5, Basis: "ignore rule past its expires date"},
			Confidence:  1.0,
		})
	}
	return out
}

// UnusedFindings builds a single info finding naming ignore rules that have
// matched nothing across the last several runs (the store decides which — B2-3).
func UnusedFindings(ruleStrings []string) []model.Finding {
	if len(ruleStrings) == 0 {
		return nil
	}
	return []model.Finding{{
		ID: "suppression_unused", Severity: model.SeverityInfo,
		Title:       fmt.Sprintf("%d suppression rule(s) haven't matched anything recently", len(ruleStrings)),
		Detail:      "These [[ignore]] rules have suppressed nothing across the last several runs — the finding they mute may be gone (the index was dropped, the setting fixed) or its identity changed (a queryid shifts across a major-version upgrade). A stale ignore list becomes a second, invisible config nobody audits.",
		Evidence:    ruleStrings,
		Remediation: "Remove the rules you no longer need; if one should still apply, check the object name still matches (major upgrades reset queryids).",
		Impact:      model.Impact{Score: 5, Basis: "no match across recent snapshots"},
		Confidence:  0.7,
	}}
}
