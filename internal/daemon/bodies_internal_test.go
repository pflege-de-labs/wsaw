package daemon

import (
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Story 1.11, AC7: a pending retry keeps its first attempt's sampling decision
// across a reload, and the decision is dropped once the observation is over.
func TestTheBodyDecisionFollowsTheRetryAcrossAReload(t *testing.T) {
	t.Parallel()

	decision := &model.BodyCapture{Sampled: true, Decision: model.BodyDecisionSampled, DecidedBy: "first"}

	prev := &job{attempt: 1}
	prev.scheduleRetry(time.Now(), time.Minute, "browser crashed", decision)

	reloaded := &job{}
	reloaded.carryOver(prev)

	if reloaded.bodies != decision {
		t.Fatal("a reload dropped the decision a pending retry inherits")
	}

	d := &Daemon{}
	if run := d.markStarted(reloaded, time.Now()); run.bodies != decision || run.attempt != 2 {
		t.Errorf("the retry started as %+v, want attempt 2 with the inherited decision", run)
	}

	reloaded.resetRetries()

	if reloaded.bodies != nil {
		t.Error("the decision outlived its observation")
	}

	if run := d.markStarted(reloaded, time.Now()); run.bodies != nil {
		t.Error("a first attempt was handed a decision to inherit")
	}
}
