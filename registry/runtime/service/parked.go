package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/command"
	"github.com/jsell-rh/stego/registry/runtime/modes"
	"github.com/jsell-rh/stego/registry/runtime/otel"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
	"github.com/jsell-rh/stego/registry/runtime/query"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// The paths of the parked-message routes after the base path of the service (ST-SUP-RP14-LC09,
// ST-SUP-RP14-LC10). The last path segment of the resolve route is <id>:resolve; the router matches the whole
// segment as the path value action, and the handler reads the ID from it.
const (
	ParkedPath  = "/stego/outbox/{destination}/parked"
	ResolvePath = ParkedPath + "/{action}"
)

// ParkedRoutes gives the routes of the parked-message list and resolve of a service whose base path is basePath:
// GET <base path>/stego/outbox/{destination}/parked and POST <base path>/stego/outbox/{destination}/parked/{action},
// where action is <id>:resolve. The generated code registers them in the api mode only, behind authentication.
func ParkedRoutes(basePath string) (list, resolve string) {
	base := strings.TrimSuffix(basePath, "/")
	return "GET " + base + ParkedPath, "POST " + base + ResolvePath
}

// ResolveAction is the audit action of the resolution of a parked message through the resolve route.
const ResolveAction = "resolve-parked-message"

// ParkedResourceType is the resource type of the audit row of a resolution: one outbox message.
const ParkedResourceType = "outbox_message"

// resolveSuffix ends the last path segment of the resolve route.
const resolveSuffix = ":resolve"

// maxResolveBody is the largest body of a resolve request that the handler reads.
const maxResolveBody = 1024

// parkedQuery is the query name that binds the cursor of the list of one destination, so a cursor of one
// destination is refused for another (ST-SUP-RP11-LC15).
const parkedQuery = "stego.outbox.parked:"

// parked serves the parked-message routes of the api mode (ST-SUP-RP14-LC09, ST-SUP-RP14-LC10): the resolver with
// the decision port of the authorization runtime, the audit writer of the service, the declared rules of the
// destinations of the service and the state hook of the relay's destination, on the pool of the runtime role.
type parked struct {
	svc      *Service
	resolver *outbox.Resolver
}

func newParked(svc *Service) (*parked, error) {
	auditor := &resolveAuditor{writer: svc.writer, settings: svc.parts.AuditSettings, service: svc.parts.Service,
		version: svc.version, declaration: svc.parts.DeclarationVersion}
	hooks := map[string]outbox.StateHook{authz.DestinationName: svc.authz.StateHook(svc.telemetry.LoggerProvider())}
	declared := append([]outbox.Rules{authz.Rules()}, svc.parts.Destinations...)
	resolver, err := outbox.NewDeclaredResolver(svc.queue, svc.authz, auditor, declared, hooks)
	if err != nil {
		return nil, err
	}
	return &parked{svc: svc, resolver: resolver}, nil
}

// RegisterParked registers the parked-message list and resolve on mux at the routes of ParkedRoutes for the base
// path basePath, and only when selection runs the api mode (ST-SUP-RP14-LC09, ST-SUP-RP14-LC10). The generated code
// serves mux behind its authentication middleware, so each request carries a verified identity. A process of the
// worker or controller mode alone registers nothing.
func (svc *Service) RegisterParked(mux *http.ServeMux, basePath string, selection modes.Selection) {
	if !selection.Has(modes.API) {
		return
	}
	list, resolve := ParkedRoutes(basePath)
	mux.HandleFunc(list, svc.parked.list)
	mux.HandleFunc(resolve, svc.parked.resolve)
}

// parkedItem is one parked message in the list response: the fixed fields only.
type parkedItem struct {
	ID          string    `json:"id"`
	Destination string    `json:"destination"`
	FailureCode string    `json:"failure_code"`
	Attempts    int64     `json:"attempts"`
	CreatedAt   time.Time `json:"created_at"`
	ParkedAt    time.Time `json:"parked_at"`
}

// list serves GET <base path>/stego/outbox/{destination}/parked (ST-SUP-RP14-LC09). It reads the limit and the
// cursor with the rules of the declared lists (ST-SUP-RP11-LC17), then decides the permission resolve on
// outbox_destination:<destination> in the caller's tenant before it reads a row, so a deny is 403 for every
// destination state; for an allow it lists the parked messages of the destination in the caller's tenant.
func (p *parked) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	b, err := command.BindIdentity(ctx, p.svc.parts.SubjectType)
	if err != nil {
		p.problem(ctx, w, http.StatusUnauthorized, "")
		return
	}
	destination := r.PathValue("destination")
	q, err := query.NewQuery(parkedQuery + destination)
	if err != nil {
		p.problem(ctx, w, http.StatusInternalServerError, command.ErrorCodeInternal)
		return
	}
	limit, err := query.Limit(r)
	if err != nil {
		p.problem(ctx, w, http.StatusBadRequest, command.ErrorCodeInvalid)
		return
	}
	if limit == 0 {
		limit = defaultParkedPage
	}
	cursor, err := p.svc.queries.ParseCursor(q, b.Tenant, b.Principal, r.URL.Query().Get(query.CursorParameter))
	after, ok := cursor.Key()
	var position int64
	if err == nil && ok && after != "" {
		position, err = strconv.ParseInt(after, 10, 64)
	}
	if err != nil || !ok || position < 0 {
		p.problem(ctx, w, http.StatusBadRequest, command.ErrorCodeInvalid)
		return
	}
	allowed, err := p.resolver.Allowed(ctx, b.Tenant, b.Principal, destination)
	if err != nil {
		p.problem(ctx, w, http.StatusInternalServerError, command.ErrorCodeInternal)
		return
	}
	if !allowed {
		p.problem(ctx, w, http.StatusForbidden, command.ErrorCodeDenied)
		return
	}
	page, more, err := p.page(ctx, b.Tenant, destination, position, limit)
	if err != nil {
		p.problem(ctx, w, http.StatusInternalServerError, command.ErrorCodeInternal)
		return
	}
	next := ""
	if more && len(page) > 0 {
		if next, err = p.svc.queries.CursorText(q, b.Tenant, b.Principal,
			query.KeyCursor(strconv.FormatInt(page[len(page)-1].Position, 10))); err != nil {
			p.problem(ctx, w, http.StatusInternalServerError, command.ErrorCodeInternal)
			return
		}
	}
	items := make([]parkedItem, len(page))
	for i, m := range page {
		items[i] = parkedItem{ID: m.ID.String(), Destination: m.Destination, FailureCode: m.FailureCode,
			Attempts: m.Attempts, CreatedAt: m.CreatedAt.UTC(), ParkedAt: m.ParkedAt.UTC()}
	}
	body, err := query.EncodePage(items, next)
	if err != nil {
		p.problem(ctx, w, http.StatusInternalServerError, command.ErrorCodeInternal)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// defaultParkedPage is the page of a list request with no limit, as for the declared lists.
const defaultParkedPage = 100

// page reads one page of the parked messages in a read-only transaction of the caller's tenant.
func (p *parked) page(ctx context.Context, t tenant.Tenant, destination string, after int64, limit int) ([]outbox.ParkedMessage, bool, error) {
	tx, err := tenant.Begin(ctx, p.svc.pool, t, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, false, err
	}
	page, more, err := p.svc.queue.Parked(ctx, tx, destination, after, limit)
	if err != nil {
		_ = tx.Rollback()
		return nil, false, err
	}
	if err := tenant.Commit(ctx, tx, t); err != nil {
		return nil, false, err
	}
	return page, more, nil
}

// resolveBody is the body of a resolve request.
type resolveBody struct {
	Resolution *string `json:"resolution"`
}

// resolve serves POST <base path>/stego/outbox/{destination}/parked/{id}:resolve (ST-SUP-RP14-LC10). It calls
// Resolve with the caller's principal in a transaction of the caller's tenant, so the permission is decided per
// destination before the message is read and the audit row is written in the same transaction as the state change.
// It answers 204 for a resolution, 403 for a deny, the same for every message, 404 for a message that the tenant
// does not hold under the destination, 409 for a message that is no longer parked or an outcome that the
// destination refuses, and 400 for another body; no answer holds a payload or an error text.
func (p *parked) resolve(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	b, err := command.BindIdentity(ctx, p.svc.parts.SubjectType)
	if err != nil {
		p.problem(ctx, w, http.StatusUnauthorized, "")
		return
	}
	text, ok := strings.CutSuffix(r.PathValue("action"), resolveSuffix)
	if !ok {
		p.problem(ctx, w, http.StatusNotFound, "")
		return
	}
	resolution, ok := readResolution(w, r)
	if !ok {
		p.problem(ctx, w, http.StatusBadRequest, command.ErrorCodeInvalid)
		return
	}
	destination := r.PathValue("destination")
	id, err := uuid.Parse(text)
	if err != nil || id == uuid.Nil || id.String() != text {
		// No message has this ID; the answer still follows the decision, so a deny is the same for every message.
		p.refusedID(ctx, w, b.Tenant, b.Principal, destination)
		return
	}
	tx, err := tenant.Begin(ctx, p.svc.pool, b.Tenant, nil)
	if err != nil {
		p.problem(ctx, w, http.StatusInternalServerError, command.ErrorCodeInternal)
		return
	}
	err = p.resolver.Resolve(ctx, tx, outbox.ResolveRequest{ID: id, Destination: destination, Principal: b.Principal,
		Resolution: resolution})
	if err != nil {
		_ = tx.Rollback()
		switch {
		case errors.Is(err, outbox.ErrNotAllowed):
			p.problem(ctx, w, http.StatusForbidden, command.ErrorCodeDenied)
		case errors.Is(err, outbox.ErrNotFound):
			p.problem(ctx, w, http.StatusNotFound, errorCodeNotFound)
		case errors.Is(err, outbox.ErrNotParked), errors.Is(err, outbox.ErrRefusedOutcome):
			p.problem(ctx, w, http.StatusConflict, audit.ErrorCodeConflict)
		default:
			// A destination name that is not a label is refused before the decision: it is a deny, as no
			// destination of that name can be granted.
			if allowed, aerr := p.resolver.Allowed(ctx, b.Tenant, b.Principal, destination); aerr == nil && !allowed {
				p.problem(ctx, w, http.StatusForbidden, command.ErrorCodeDenied)
				return
			}
			p.problem(ctx, w, http.StatusInternalServerError, command.ErrorCodeInternal)
		}
		return
	}
	if err := tenant.Commit(ctx, tx, b.Tenant); err != nil {
		p.problem(ctx, w, http.StatusInternalServerError, command.ErrorCodeInternal)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// errorCodeNotFound is the fixed code of a message that the tenant does not hold under the destination.
const errorCodeNotFound = "not_found"

// refusedID answers a resolve request whose ID is not the text form of a message ID: 403 for a deny, as for every
// message, and 404 for an allow.
func (p *parked) refusedID(ctx context.Context, w http.ResponseWriter, t tenant.Tenant, principal authz.Principal, destination string) {
	allowed, err := p.resolver.Allowed(ctx, t, principal, destination)
	switch {
	case err != nil:
		p.problem(ctx, w, http.StatusInternalServerError, command.ErrorCodeInternal)
	case !allowed:
		p.problem(ctx, w, http.StatusForbidden, command.ErrorCodeDenied)
	default:
		p.problem(ctx, w, http.StatusNotFound, errorCodeNotFound)
	}
}

// readResolution reads the body {"resolution": "acted" | "not_acted" | "discarded"}: one JSON object with that one
// member, at most maxResolveBody bytes.
func readResolution(w http.ResponseWriter, r *http.Request) (outbox.Resolution, bool) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxResolveBody))
	decoder.DisallowUnknownFields()
	var body resolveBody
	if err := decoder.Decode(&body); err != nil || body.Resolution == nil {
		return "", false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return "", false
	}
	switch resolution := outbox.Resolution(*body.Resolution); resolution {
	case outbox.Acted, outbox.NotActed, outbox.Discard:
		return resolution, true
	}
	return "", false
}

// problem writes the problem response of a refused request with the fixed code only, and gives the code to the
// completion log of the call (ST-SUP-RP03-LC12).
func (p *parked) problem(ctx context.Context, w http.ResponseWriter, status int, code string) {
	if code != "" {
		otel.SetErrorCode(ctx, code)
	}
	writeTenantProblem(w, status, p.svc.parts.ProblemType, code)
}

// resolveAuditor writes the audit row of a resolution through the audit writer of the service, in the caller's
// transaction: the action resolve-parked-message on the resource outbox_message:<id>, with the principal as the
// actor and, at the change level, the destination and the state before and after with the resolution.
type resolveAuditor struct {
	writer      *audit.Writer
	settings    audit.Settings
	service     string
	version     string
	declaration string
}

func (a *resolveAuditor) Record(ctx context.Context, tx *sql.Tx, entry outbox.AuditEntry) error {
	kind := audit.ActorUser
	if strings.HasPrefix(entry.Subject, authz.ServiceAccountType+":") {
		kind = audit.ActorService
	}
	after := map[outbox.Resolution]outbox.State{outbox.Acted: outbox.Done, outbox.NotActed: outbox.Pending,
		outbox.Discard: outbox.Discarded}[entry.Resolution]
	e := audit.Event{TenantID: entry.Tenant.ID(), ActorID: entry.Subject, ActorKind: kind, Action: ResolveAction,
		Operation: ResolveAction, ResourceType: ParkedResourceType, ResourceID: entry.MessageID.String(),
		Outcome: audit.OutcomeOK, OccurredAt: entry.At, Service: a.service, ServiceVersion: a.version,
		DeclarationVersion: a.declaration}
	if span := trace.SpanContextFromContext(ctx); span.IsValid() {
		e.TraceID, e.SpanID = span.TraceID().String(), span.SpanID().String()
	}
	if err := e.SetStates(a.settings, nil,
		map[string]any{"destination": entry.Destination, "state": string(outbox.Parked)},
		map[string]any{"destination": entry.Destination, "state": string(after), "resolution": string(entry.Resolution)}); err != nil {
		return err
	}
	_, err := a.writer.Record(ctx, tx, entry.Tenant, []audit.Event{e})
	return err
}
