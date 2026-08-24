package infrastructure

import (
	"context"
	"errors"

	"lidradar/backend/internal/ai/domain"
)

var errStubHasNoJobs = errors.New("stub AI cloud has no jobs")

// StubCloud keeps the AI agent runtime active without transmitting data to an
// external cloud service. Replace it at the composition root when Cloud Core is available.
type StubCloud struct{}

func (StubCloud) Heartbeat(context.Context) error { return nil }
func (StubCloud) Claim(context.Context) (domain.Job, bool, error) {
	return domain.Job{}, false, nil
}
func (StubCloud) Started(context.Context, string) (string, error) { return "", errStubHasNoJobs }
func (StubCloud) Complete(context.Context, string, string, string, int64, string) error {
	return errStubHasNoJobs
}
func (StubCloud) Failed(context.Context, string, string, string) error { return errStubHasNoJobs }
