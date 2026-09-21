package cli

import (
	"fmt"
	"strings"
)

// statusNamedChecks caps how many checks the notable line names.
const statusNamedChecks = 8

// statusRuleRequiredChecks is the ruleset rule that makes a status check
// required, as it is typed on the wire.
const statusRuleRequiredChecks = "REQUIRED_STATUS_CHECKS"

// statusClass is what one check state does to a merge. Neutral and skipped are
// classes of their own rather than kinds of pass: a neutral run holds the merge
// while every other check is green, and a skipped one reported without running,
// which is how a cached step lands a success nobody computed.
type statusClass int

const (
	statusPassed statusClass = iota
	statusFailed
	statusNeutral
	statusSkipped
	statusRunning
)

// statusClassify reads one check state. A state added to the API since this was
// written classifies as failed and is named verbatim, because a state filed
// with the passes is how a red pull request reports as ready to land.
func statusClassify(state string) statusClass {
	switch state {
	case "SUCCESS":
		return statusPassed
	case "SKIPPED":
		return statusSkipped
	case "NEUTRAL":
		return statusNeutral
	case "QUEUED", "IN_PROGRESS", "PENDING", "WAITING", "REQUESTED", "EXPECTED":
		return statusRunning
	default:
		return statusFailed
	}
}

// statusCheckBlockers names what the checks on the head do to the merge. The
// rollup over them is not consulted here: it reports success over a neutral
// hold, over a required check that skipped without running, and over a required
// check that never reported at all.
func statusCheckBlockers(pr *statusPR) []string {
	var out []string
	reported := make(map[string]bool, len(pr.Checks))
	for _, c := range pr.Checks {
		reported[c.Name] = true
		switch statusClassify(c.State) {
		case statusFailed:
			if c.Required {
				out = append(out, "required check "+c.Name+" is "+strings.ToLower(c.State))
			}
		case statusNeutral:
			out = append(out, "check "+c.Name+" is neutral — a neutral run holds the merge, it does not clear it")
		case statusSkipped:
			if c.Required {
				out = append(out, "required check "+c.Name+" was skipped — it reported without running")
			}
		}
	}
	for _, name := range pr.Required {
		if !reported[name] {
			out = append(out, "required check "+name+" has not reported on "+shortSHA(pr.Head))
		}
	}
	return append(out, statusRollupBlocker(pr)...)
}

// statusRollupBlocker names a rollup that disagrees with everything under it.
// A cancelled build leaves the commit status non-success with no failing check
// to point at, and nothing else in the report would show it.
func statusRollupBlocker(pr *statusPR) []string {
	switch pr.ChecksState {
	case "", "SUCCESS", "PENDING", "EXPECTED":
		return nil
	}
	for _, c := range pr.Checks {
		switch statusClassify(c.State) {
		case statusFailed, statusNeutral, statusRunning:
			return nil
		}
	}
	return []string{"the check rollup on the head is " + strings.ToLower(pr.ChecksState) +
		" with nothing under it failing — a cancelled build owns the commit status that way"}
}

// statusChecksValue counts the checks on the head by class. It counts rather
// than lists them because a pull request carrying two hundred would otherwise
// spend the whole report on one line; statusNotableValue names the few to look
// at.
func statusChecksValue(pr statusPR) string {
	counts := map[statusClass]int{}
	for _, c := range pr.Checks {
		counts[statusClassify(c.State)]++
	}
	segs := []string{
		fmt.Sprintf("%d passed", counts[statusPassed]),
		fmt.Sprintf("%d failed", counts[statusFailed]),
		fmt.Sprintf("%d neutral", counts[statusNeutral]),
		fmt.Sprintf("%d skipped", counts[statusSkipped]),
		fmt.Sprintf("%d running", counts[statusRunning]),
	}
	if pr.ChecksState != "" {
		segs = append(segs, "rollup "+strings.ToLower(pr.ChecksState))
	}
	return strings.Join(segs, shipSep)
}

// statusNotableValue names every check that is not a plain pass, worst first
// and capped. A skipped check is notable only where a rule requires it: a
// pipeline skips most of itself on most pull requests.
func statusNotableValue(checks []statusCheck) string {
	var segs []string
	for _, class := range []statusClass{statusFailed, statusNeutral, statusRunning, statusSkipped} {
		for _, c := range checks {
			if statusClassify(c.State) != class || (class == statusSkipped && !c.Required) {
				continue
			}
			seg := c.Name + " " + strings.ToLower(c.State)
			if c.Required {
				seg += " (required)"
			}
			segs = append(segs, seg)
		}
	}
	if len(segs) == 0 {
		return ""
	}
	rest := ""
	if len(segs) > statusNamedChecks {
		rest = fmt.Sprintf("%s+%d more", shipSep, len(segs)-statusNamedChecks)
		segs = segs[:statusNamedChecks]
	}
	return strings.Join(segs, shipSep) + rest
}

// statusUnverifiedValue names the green checks whose greenness this report
// cannot see inside. A build system reports one aggregate status upstream, so a
// shard that soft-failed inside it arrives here as a success and no field on
// the pull request carries the shard verdicts.
func statusUnverifiedValue(checks []statusCheck) string {
	var names []string
	for _, c := range checks {
		if c.External && statusClassify(c.State) == statusPassed {
			names = append(names, c.Name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	return strings.Join(names, ", ") + shipSep +
		"a build system rollup — a shard that soft-failed inside " + plural(len(names), "it", "one") +
		" still reports success here; open it to read the shards"
}

// statusRulesetChecks reads the status checks the ruleset covering the base
// branch makes required. A repository ruleset is where that requirement lives
// now, and branchProtectionRules answers with an empty required list for a base
// a ruleset governs — which reads exactly like a base requiring nothing.
func statusRulesetChecks(ref *statusBaseRef) []string {
	if ref == nil {
		return nil
	}
	var out []string
	for _, rule := range ref.Rules.Nodes {
		if rule.Type != statusRuleRequiredChecks || rule.Parameters == nil {
			continue
		}
		for _, check := range rule.Parameters.RequiredStatusChecks {
			out = append(out, check.Context)
		}
	}
	return out
}
