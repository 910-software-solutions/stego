package command

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/910-software-solutions/stego/registry/runtime/authz"
	"github.com/910-software-solutions/stego/registry/runtime/otel"
	"github.com/910-software-solutions/stego/registry/runtime/tenant"
)

// AwaitRevocation waits, before a command decides its permission, for the revocation of a write of the caller's
// tenant that the HTTP request names in Authz-Revocation-After, by its identity or by the path of
// Authz-Revocation-Location (ST-SUP-RP10-LC47): the caller's own write, or the write of another principal of the
// tenant that gave the caller its revocation (decision ST-SUP-Q206 (a), recommended). It waits through authz
// WaitInTenant with its waiter limits of tenant t and principal p, at most the wait of Prefer: wait=N when the request
// gives one, at most the wait cap and at most half of the time that the deadline of the request leaves. It sets no
// response header: Authz-Revocation of a command names the revocation of the command's own write. A revocation of
// another tenant, and one still pending at the end of the wait, is no error: the command then decides as it does
// with no wait. A value that is not an identity, and more
// than one value, is ErrBadRequest. A request with no such header waits for nothing.
func (rt *Runtime) AwaitRevocation(r *http.Request, t tenant.Tenant, p authz.Principal) error {
	_, err := rt.awaitRevocation(r.Context(), t, p, r.Header.Values(authz.RevocationAfterHeader), r.Header.Values("Prefer"))
	return err
}

// AwaitRevocationOn is AwaitRevocation that also answers the preference of the wait on w: when the request gives
// Prefer: wait=N and the command waited for the revocation, it sets Preference-Applied to wait= and the whole seconds
// that the wait allowed, as the status route of a revocation does (RFC 7240; walkthrough 23, small items). It sets no
// other header, and none for a request with no Prefer: wait, for a revocation of another tenant and for a failed wait.
func (rt *Runtime) AwaitRevocationOn(w http.ResponseWriter, r *http.Request, t tenant.Tenant, p authz.Principal) error {
	allowed, err := rt.awaitRevocation(r.Context(), t, p, r.Header.Values(authz.RevocationAfterHeader), r.Header.Values("Prefer"))
	if err == nil && allowed > 0 && w != nil {
		if _, asked := authz.PreferWait(r.Header); asked {
			w.Header().Set("Preference-Applied", "wait="+strconv.Itoa(int(allowed/time.Second)))
		}
	}
	return err
}

// AwaitRevocationRPC is AwaitRevocation for a gRPC call (ST-SUP-RP10-LC47): the revocation comes from the request
// metadata authz-revocation-after and the wait preference from the request metadata prefer. It sends no header
// metadata: authz-revocation of a command names the revocation of the command's own write (ST-SUP-RP10-LC40).
func (rt *Runtime) AwaitRevocationRPC(ctx context.Context, t tenant.Tenant, p authz.Principal) error {
	md, _ := metadata.FromIncomingContext(ctx)
	_, err := rt.awaitRevocation(ctx, t, p, md.Get(authz.RevocationAfterMetadata), md.Get(authz.PreferMetadata))
	return err
}

// awaitRevocation is the wait of AwaitRevocation and AwaitRevocationRPC for the values of the revocation and of the
// wait preference that the request gives. It gives the wait that it allowed, zero when it did not wait.
func (rt *Runtime) awaitRevocation(ctx context.Context, t tenant.Tenant, p authz.Principal, values, prefer []string) (time.Duration, error) {
	if len(values) == 0 || len(values) == 1 && values[0] == "" {
		return 0, nil
	}
	if len(values) != 1 {
		return 0, fmt.Errorf("%w: more than one %s value", ErrBadRequest, authz.RevocationAfterHeader)
	}
	text := strings.TrimPrefix(values[0], authz.RevocationPath)
	id, err := uuid.Parse(text)
	if err != nil || id.String() != text {
		return 0, fmt.Errorf("%w: %s is not the identity of a revocation", ErrBadRequest, authz.RevocationAfterHeader)
	}
	if rt == nil || rt.cfg.Authz == nil {
		return 0, errors.New("command: no authorization runtime to wait for a revocation")
	}
	wait, ok := authz.PreferWait(http.Header{"Prefer": prefer})
	if !ok {
		wait = math.MaxInt64
	}
	if deadline, ok := ctx.Deadline(); ok {
		wait = min(wait, time.Until(deadline)/2)
	}
	// Any principal of the tenant that holds the revocation, such as a second user to whom the maker of a row gave
	// it, waits for it; the wait gives the command nothing but time (decision ST-SUP-Q206 (a), recommended;
	// walkthrough 22, P3-A).
	_, _, allowed, err := rt.cfg.Authz.WaitInTenant(ctx, t, p, id, wait)
	if errors.Is(err, authz.ErrRevocationNotFound) {
		return 0, ctx.Err()
	}
	if err != nil {
		return 0, err
	}
	return allowed, ctx.Err()
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
