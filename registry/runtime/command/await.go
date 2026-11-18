package command

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/otel"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// AwaitRevocation waits, before a command decides its permission, for the revocation of the caller's own write that
// the HTTP request names in Authz-Revocation-After, by its identity or by the path of Authz-Revocation-Location
// (ST-SUP-RP10-LC47). It waits through authz WaitFor with its waiter limits of tenant t and principal p, at most the
// wait of Prefer: wait=N when the request gives one, at most the wait cap and at most half of the time that the
// deadline of the request leaves. It sets no response header: Authz-Revocation of a command names the revocation of
// the command's own write. A revocation of another tenant or principal, and one still pending at the end of the
// wait, is no error: the command then decides as it does with no wait. A value that is not an identity, and more
// than one value, is ErrBadRequest. A request with no such header waits for nothing.
func (rt *Runtime) AwaitRevocation(r *http.Request, t tenant.Tenant, p authz.Principal) error {
	return rt.awaitRevocation(r.Context(), t, p, r.Header.Values(authz.RevocationAfterHeader), r.Header.Values("Prefer"))
}

// AwaitRevocationRPC is AwaitRevocation for a gRPC call (ST-SUP-RP10-LC47): the revocation comes from the request
// metadata authz-revocation-after and the wait preference from the request metadata prefer. It sends no header
// metadata: authz-revocation of a command names the revocation of the command's own write (ST-SUP-RP10-LC40).
func (rt *Runtime) AwaitRevocationRPC(ctx context.Context, t tenant.Tenant, p authz.Principal) error {
	md, _ := metadata.FromIncomingContext(ctx)
	return rt.awaitRevocation(ctx, t, p, md.Get(authz.RevocationAfterMetadata), md.Get(authz.PreferMetadata))
}

// awaitRevocation is the wait of AwaitRevocation and AwaitRevocationRPC for the values of the revocation and of the
// wait preference that the request gives.
func (rt *Runtime) awaitRevocation(ctx context.Context, t tenant.Tenant, p authz.Principal, values, prefer []string) error {
	if len(values) == 0 || len(values) == 1 && values[0] == "" {
		return nil
	}
	if len(values) != 1 {
		return fmt.Errorf("%w: more than one %s value", ErrBadRequest, authz.RevocationAfterHeader)
	}
	text := strings.TrimPrefix(values[0], authz.RevocationPath)
	id, err := uuid.Parse(text)
	if err != nil || id.String() != text {
		return fmt.Errorf("%w: %s is not the identity of a revocation", ErrBadRequest, authz.RevocationAfterHeader)
	}
	if rt == nil || rt.cfg.Authz == nil {
		return errors.New("command: no authorization runtime to wait for a revocation")
	}
	wait, ok := authz.PreferWait(http.Header{"Prefer": prefer})
	if !ok {
		wait = math.MaxInt64
	}
	if deadline, ok := ctx.Deadline(); ok {
		wait = min(wait, time.Until(deadline)/2)
	}
	_, _, err = rt.cfg.Authz.WaitFor(ctx, t, p, id, wait)
	if err != nil && !errors.Is(err, authz.ErrRevocationNotFound) {
		return err
	}
	return ctx.Err()
}

// awaitAnswer is the answer of an error of AwaitRevocation (ST-SUP-RP10-LC47): 400 for a bad value, 503 unavailable
// for a database that cannot serve the wait now, and 500 internal for every other error. ok is false for a client
// that went away or a deadline that passed during the wait, which gets no answer and no internal code.
func awaitAnswer(ctx context.Context, err error) (httpStatus int, code string, ok bool) {
	switch {
	case errors.Is(err, ErrBadRequest):
		return http.StatusBadRequest, "", true
	case ctx.Err() != nil:
		return 0, "", false
	case tenant.Unavailable(err):
		otel.SetErrorCode(ctx, ErrorCodeUnavailable)
		return http.StatusServiceUnavailable, ErrorCodeUnavailable, true
	}
	otel.SetErrorCode(ctx, ErrorCodeInternal)
	return http.StatusInternalServerError, ErrorCodeInternal, true
}

// awaitStatus is the gRPC status of an error of AwaitRevocationRPC: InvalidArgument bad_request, the status of the
// context for a call that its client cancelled or whose deadline passed, Unavailable unavailable or Internal internal.
func awaitStatus(ctx context.Context, err error) error {
	httpStatus, code, ok := awaitAnswer(ctx, err)
	switch {
	case !ok:
		return status.FromContextError(ctx.Err()).Err()
	case httpStatus == http.StatusBadRequest:
		return status.Error(codes.InvalidArgument, "bad_request")
	}
	return status.Error(RPCCode(httpStatus), code)
}
