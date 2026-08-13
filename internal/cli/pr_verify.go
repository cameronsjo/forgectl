package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/cameronsjo/forgectl/internal/pr"
)

type dispatchVerificationState uint8

const (
	dispatchVerificationSkipped dispatchVerificationState = iota
	dispatchVerificationLive
	dispatchVerificationGone
	dispatchVerificationUnknown
)

type dispatchVerificationResult struct {
	State dispatchVerificationState
	Gone  []pr.Dispatch
	Cause error
}

func verifyReviewDispatches(ctx context.Context, client *pr.Client, dispatches []pr.Dispatch, noVerify bool) dispatchVerificationResult {
	if noVerify || len(dispatches) == 0 {
		return dispatchVerificationResult{State: dispatchVerificationSkipped}
	}
	gone, err := client.VerifyDispatched(ctx, dispatches)
	if err != nil {
		return dispatchVerificationResult{State: dispatchVerificationUnknown, Cause: err}
	}
	if len(gone) > 0 {
		return dispatchVerificationResult{State: dispatchVerificationGone, Gone: gone}
	}
	return dispatchVerificationResult{State: dispatchVerificationLive}
}

func dispatchVerificationError(result dispatchVerificationResult) error {
	switch result.State {
	case dispatchVerificationSkipped, dispatchVerificationLive:
		if len(result.Gone) == 0 && result.Cause == nil {
			return nil
		}
	case dispatchVerificationGone:
		if len(result.Gone) > 0 && result.Cause == nil {
			refs := make([]string, 0, len(result.Gone))
			for _, dispatch := range result.Gone {
				refs = append(refs, dispatch.Ref.String())
			}
			return fmt.Errorf("review window disappeared during startup: %s; inspect `forgectl pr list` and discard the clean room with `forgectl pr teardown <breadcrumb>` before retrying", strings.Join(refs, ", "))
		}
	case dispatchVerificationUnknown:
		if len(result.Gone) == 0 && result.Cause != nil {
			return fmt.Errorf("could not verify review window startup; dispatch state is unknown: %w", result.Cause)
		}
	}
	return fmt.Errorf("could not verify review window startup: invalid verification result")
}

func dispatchVerificationLogValue(state dispatchVerificationState) string {
	switch state {
	case dispatchVerificationSkipped:
		return "skipped"
	case dispatchVerificationLive:
		return "live"
	case dispatchVerificationGone:
		return "gone"
	default:
		return "unknown"
	}
}
