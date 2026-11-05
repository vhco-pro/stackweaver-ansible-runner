// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package ansibleexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/michielvha/logger"
	"github.com/michielvha/stackweaver/backend/internal/executor"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

// Event is one non-callback row in a job's output, such as the Galaxy install progress or the
// playbook source banner.
type Event struct {
	Event  string
	Task   string
	Stdout string
	Stderr string
	Failed bool
}

// EventSink records a job event for the job being prepared. The executor posts it to the API;
// the queue runner, until it is retired, writes the row itself.
type EventSink interface {
	Emit(ctx context.Context, ev Event) error
}

// EmitEvent records ev and logs a failure instead of returning it: an event that cannot be stored
// must never fail the job it describes.
func EmitEvent(ctx context.Context, sink EventSink, ev Event) {
	if err := sink.Emit(ctx, ev); err != nil {
		logger.Warnf("Failed to record %s job event: %v", ev.Event, err)
	}
}

// maxEventBody is what one request to the events endpoint may carry: the API caps the route's
// body at 1 MiB, and a little room is left for the envelope.
const maxEventBody = 1<<20 - 4096

// eventRequest renders ev as the events endpoint's request body. Output too large for the
// endpoint is shortened from the front, because a request over the cap is rejected whole and the
// event, with the reason an install failed, would be lost. The field budget is halved until the
// encoded body fits, since JSON escaping can inflate it.
func eventRequest(ev Event) (runnercontract.JobEventRequest, error) {
	for budget := maxEventBody / 2; ; budget /= 2 {
		req := runnercontract.JobEventRequest{
			Event:  ev.Event,
			Task:   ev.Task,
			Stdout: keepOutputTail(ev.Stdout, budget),
			Stderr: keepOutputTail(ev.Stderr, budget),
			Failed: ev.Failed,
		}
		body, err := json.Marshal(req)
		if err != nil {
			return req, fmt.Errorf("encoding the %s job event: %w", ev.Event, err)
		}
		if len(body) <= maxEventBody || budget == 0 {
			return req, nil
		}
	}
}

// keepOutputTail returns s unchanged when it fits in limit bytes. Otherwise it returns the end of
// s, cut at a character boundary, behind a line saying how much was dropped: the end of a tool's
// output is where it says what went wrong.
func keepOutputTail(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	start := len(s) - limit
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return fmt.Sprintf("[... %d bytes of earlier output omitted ...]\n", start) + s[start:]
}

// apiSink is the executor's EventSink. An executor has no database, so it records a job event by
// posting it to the control plane, which stores it with the job's next counter. One synchronous
// call per event keeps the events in order with each other.
type apiSink struct {
	client *executor.Client
	jobID  string
}

// Emit posts ev to the job's events endpoint. An answer other than 2xx is logged and not returned:
// like the queue runner on a failed event insert, the job carries on. An error is returned only
// when the request could not be made at all.
func (s apiSink) Emit(ctx context.Context, ev Event) error {
	req, err := eventRequest(ev)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.client.Event(ctx, s.jobID, req); err != nil {
		var se *executor.StatusError
		if errors.As(err, &se) {
			logger.Warnf("Server rejected %s job event for job %s: %v", ev.Event, s.jobID, err)
			return nil
		}
		return fmt.Errorf("posting the %s job event: %w", ev.Event, err)
	}
	return nil
}
