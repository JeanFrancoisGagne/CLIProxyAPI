package main

import (
	"fmt"
	"sort"
	"time"
)

// Thresholds and the priority ladder. The proxy serves new sessions from the
// highest priority tier that has an available credential, so the credential
// with the largest number here takes the next new session of its provider.
const (
	fiveHourLimit    = 95.0 // at or above this 5-hour utilization a credential sits out
	sevenDayLimit    = 99.0 // at or above this weekly utilization a credential sits out
	topPriority      = 100
	priorityStep     = 10
	priorityFloor    = 20
	fallbackPriority = 10 // ineligible or unknown: still usable when nothing ranks higher
)

// Window is one usage window: the percent used and the instant it resets.
type Window struct {
	Used    float64
	HasUsed bool
	ResetAt time.Time // zero when the payload carried no reset time
}

// Usage holds the windows the ranking reads. A nil field means the usage
// payload had no such window.
type Usage struct {
	FiveHour *Window
	SevenDay *Window
	Fable    *Window // Claude's separate weekly Fable bucket
}

// Credential is one proxy credential as the ranking sees it.
type Credential struct {
	Name       string
	Provider   string
	Priority   int    // current priority; the proxy treats an unset priority as 0
	Usage      *Usage // nil when the usage fetch failed
	UsageError string // why Usage is nil
}

// Decision is the ranking's verdict for one credential.
type Decision struct {
	Name     string
	Provider string
	Old      int
	New      int
	Eligible bool
	Reason   string
}

// Changed reports whether the credential needs a PATCH.
func (d Decision) Changed() bool { return d.Old != d.New }

// Rank assigns a priority to every credential, one provider at a time.
// Eligible credentials are ordered by weekly reset, soonest first, and get
// 100, 90, 80, ... down to 20. All others get 10. Decisions come back in the
// order of creds.
func Rank(creds []Credential) []Decision {
	decisions := make([]Decision, len(creds))
	eligibleByProvider := map[string][]int{}
	for i, c := range creds {
		ok, reason := eligibility(c)
		decisions[i] = Decision{
			Name:     c.Name,
			Provider: c.Provider,
			Old:      c.Priority,
			New:      fallbackPriority,
			Eligible: ok,
			Reason:   reason,
		}
		if ok {
			eligibleByProvider[c.Provider] = append(eligibleByProvider[c.Provider], i)
		}
	}
	for _, idx := range eligibleByProvider {
		sort.SliceStable(idx, func(a, b int) bool { return rankBefore(creds[idx[a]], creds[idx[b]]) })
		for rank, i := range idx {
			decisions[i].New = max(topPriority-rank*priorityStep, priorityFloor)
			decisions[i].Reason = fmt.Sprintf("rank %d of %d by 7d reset", rank+1, len(idx))
			if weeklyReset(creds[i]).IsZero() {
				decisions[i].Reason += ", no reset time so last"
			}
		}
	}
	return decisions
}

// eligibility says whether a credential may take new sessions, and why not.
func eligibility(c Credential) (bool, string) {
	u := c.Usage
	if u == nil {
		if c.UsageError != "" {
			return false, "usage unknown: " + c.UsageError
		}
		return false, "usage unknown"
	}
	if u.FiveHour == nil && u.SevenDay == nil && u.Fable == nil {
		return false, "usage has no windows"
	}
	if w := u.FiveHour; w != nil && w.HasUsed && w.Used >= fiveHourLimit {
		return false, fmt.Sprintf("5h at %.0f%%, sits out at %.0f%%", w.Used, fiveHourLimit)
	}
	if w := u.SevenDay; w != nil && w.HasUsed {
		if w.Used >= sevenDayLimit {
			return false, fmt.Sprintf("7d at %.0f%%, sits out at %.0f%%", w.Used, sevenDayLimit)
		}
		return true, ""
	}
	// No weekly figure: the Fable bucket is the only weekly signal left.
	if f := u.Fable; f != nil && f.HasUsed && f.Used >= sevenDayLimit {
		return false, fmt.Sprintf("no 7d window and fable at %.0f%%", f.Used)
	}
	return true, ""
}

// rankBefore orders two eligible credentials: a known weekly reset before an
// unknown one, then the sooner reset, then the lower weekly use, then the name.
func rankBefore(a, b Credential) bool {
	ra, rb := weeklyReset(a), weeklyReset(b)
	if ra.IsZero() != rb.IsZero() {
		return !ra.IsZero()
	}
	if !ra.Equal(rb) {
		return ra.Before(rb)
	}
	if ua, ub := weeklyUsed(a), weeklyUsed(b); ua != ub {
		return ua < ub
	}
	return a.Name < b.Name
}

func weeklyReset(c Credential) time.Time {
	if c.Usage == nil || c.Usage.SevenDay == nil {
		return time.Time{}
	}
	return c.Usage.SevenDay.ResetAt
}

func weeklyUsed(c Credential) float64 {
	if c.Usage == nil || c.Usage.SevenDay == nil || !c.Usage.SevenDay.HasUsed {
		return 0
	}
	return c.Usage.SevenDay.Used
}
