package auth

import (
	"context"
	"net/http"
)

// repeatedNotFoundGuard stops credential rotation for one request once two
// different credentials answer HTTP 404 with an error nothing else classifies.
//
// A single 404 can belong to the credential (an account without the route), so
// the first one is held back from the result recorder while the request tries
// the next credential. A second 404 from another credential shows the request
// itself points at something missing. Rotating further would put every
// credential into the 12-hour not-found cooldown, as one expired Claude thread
// did to six credentials on 2026-10-02. Both results are then recorded
// request-scoped: no cooldown, session bindings kept, and the caller receives
// the 404. Any other outcome releases the held result unchanged.
//
// A nil guard records every result directly.
type repeatedNotFoundGuard struct {
	held    *Result
	heldCtx context.Context
	record  func(context.Context, Result)
}

// observe records a failed attempt's result through record, or holds it. It
// reports true when the failure ends the request as request-scoped; the caller
// must then stop rotating and return the error wrapped by wrapRequestStopError.
func (g *repeatedNotFoundGuard) observe(ctx context.Context, result Result, record func(context.Context, Result)) bool {
	if g == nil || !isUnclassifiedNotFoundResult(result) {
		g.release()
		record(ctx, result)
		return false
	}
	if g.held == nil || g.held.AuthID == result.AuthID {
		g.release()
		g.held, g.heldCtx, g.record = &result, ctx, record
		return false
	}
	held, heldCtx, heldRecord := *g.held, g.heldCtx, g.record
	g.held, g.heldCtx, g.record = nil, nil, nil
	heldRecord(heldCtx, asRequestScopedResult(held))
	record(ctx, asRequestScopedResult(result))
	return true
}

// release records the held result unchanged. Call it before recording any other
// outcome of the request and when the request ends.
func (g *repeatedNotFoundGuard) release() {
	if g == nil || g.held == nil {
		return
	}
	held, heldCtx, heldRecord := *g.held, g.heldCtx, g.record
	g.held, g.heldCtx, g.record = nil, nil, nil
	heldRecord(heldCtx, held)
}

// isUnclassifiedNotFoundResult reports a 404 that no other rule has claimed:
// not request-scoped, not a forced cooldown, not a model-support error and not
// credential scoped.
func isUnclassifiedNotFoundResult(result Result) bool {
	resultErr := result.Error
	if result.Success || resultErr == nil || result.CredentialScope {
		return false
	}
	if statusCodeFromResult(resultErr) != http.StatusNotFound {
		return false
	}
	if resultErr.Code == ErrorCodeForceCooldown || isRequestScopedResultError(resultErr) {
		return false
	}
	return !isModelSupportResultError(resultErr)
}

func asRequestScopedResult(result Result) Result {
	result.Error = cloneError(result.Error)
	result.Error.Code = ErrorCodeRequestScoped
	return result
}
