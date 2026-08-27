package cli

import (
	"context"
	"strings"
	"time"

	"github.com/yasyf/cc-context/internal/render"
)

// mqProbeMerging through mqProbeNotReady are gt 1.8.6's own wording for
// classifyQueueProbe; version-dependent, kept as lone constants so an upgrade
// is a one-line change (precedent: lane.go's gtProbe* set).
const (
	mqProbeMerging  = "The stack is already merging"
	mqProbeMerged   = "The stack has already merged"
	mqProbeNotReady = "not ready to merge"
)

// mqProbeTimeout caps the probe. It reaches Graphite, so it can hang the way
// the reachability probe does.
var mqProbeTimeout = 20 * time.Second

// queueVerdict is what gt says about the current downstack's place in the merge
// queue. It exists because Graphite's activity comment is not always posted: a
// pull request can enter the queue and merge without one, which leaves the
// comment saying nothing and the label already consumed.
type queueVerdict string

const (
	queueMerging  queueVerdict = "merging"
	queueMerged   queueVerdict = "merged"
	queueReady    queueVerdict = "not queued"
	queueNotReady queueVerdict = "not mergeable"
	queueUnknown  queueVerdict = "unknown"
)

// statusProbe is gt's verdict on the downstack, and the sentence it gave.
type statusProbe struct {
	Verdict queueVerdict `json:"verdict"`
	Detail  string       `json:"detail,omitempty"`
}

// queueProbe asks gt whether the downstack is in the merge queue. --dry-run
// reports what would merge and terminates without merging anything, which is
// what makes a merge verb safe to run from a report.
func queueProbe(ctx context.Context, l lane) *statusProbe {
	probeCtx, cancel := context.WithTimeout(ctx, mqProbeTimeout)
	defer cancel()
	stdout, code, stderr, err := render.RunCLIProbe(probeCtx, l.dir(), "gt",
		[]string{"merge", "--dry-run", "--no-interactive"})
	if err != nil {
		return &statusProbe{Verdict: queueUnknown, Detail: "gt merge --dry-run could not run: " + err.Error()}
	}
	if probeCtx.Err() != nil {
		return &statusProbe{Verdict: queueUnknown, Detail: "gt merge --dry-run did not answer within " + mqProbeTimeout.String()}
	}
	verdict, detail := classifyQueueProbe(gtJoinStreams(stdout, stderr), code)
	return &statusProbe{Verdict: verdict, Detail: detail}
}

// classifyQueueProbe maps the probe's output to a verdict, quoting gt's own
// sentence. A nonzero exit gt gave no recognized reason for is unknown rather
// than a no: the probe informs the report, it does not gate anything.
func classifyQueueProbe(output string, code int) (queueVerdict, string) {
	switch {
	case strings.Contains(output, mqProbeMerging):
		return queueMerging, mqProbeLine(output, mqProbeMerging)
	case strings.Contains(output, mqProbeMerged):
		return queueMerged, mqProbeLine(output, mqProbeMerged)
	case strings.Contains(output, mqProbeNotReady):
		return queueNotReady, mqProbeLine(output, mqProbeNotReady)
	case code == 0:
		return queueReady, ""
	default:
		return queueUnknown, mqProbeFirstLine(output)
	}
}

// mqProbeLine returns the whole output line carrying marker, stripped of the
// emoji and box-drawing gt decorates its progress with.
func mqProbeLine(output, marker string) string {
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, marker) {
			return mqProbeClean(line)
		}
	}
	return marker
}

// mqProbeFirstLine summarizes an unrecognized failure as gt's first non-empty
// line, capped the way gtProbeFallbackNote caps one.
func mqProbeFirstLine(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if cleaned := mqProbeClean(line); cleaned != "" {
			if len(cleaned) > gtProbeNoteBudget {
				return strings.ToValidUTF8(cleaned[:gtProbeNoteBudget], "") + "…"
			}
			return cleaned
		}
	}
	return "gt merge --dry-run failed without output"
}

// mqProbeClean strips the leading emoji gt prefixes its status lines with, so a
// report's one-line segment reads as a sentence.
func mqProbeClean(line string) string {
	cleaned := strings.TrimSpace(line)
	for _, r := range cleaned {
		if r < 0x2000 {
			break
		}
		cleaned = strings.TrimSpace(strings.TrimPrefix(cleaned, string(r)))
	}
	return strings.TrimRight(cleaned, ".")
}
