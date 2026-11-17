package authz

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"sort"
	"strconv"
	"sync"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

// spiceDBName is the provider name in the page tokens of the SpiceDB provider.
const spiceDBName = "spicedb"

// SpiceDB is the SpiceDB provider of the decision and relationships ports (ST-SUP-RP06-LC05). It speaks gRPC over
// the connection that the service's own code opens, with its TLS and its preshared key, and opens no other
// transport. It maps Fresh to fully_consistent and AtLeast to at_least_as_fresh, writes with TOUCH and DELETE only,
// and wraps the after_result_cursor of a lookup, with the first page's consistency, in the opaque page token. It
// has no exact-snapshot path, and it uses the ZedTokens of every datastore as they are, without parsing them.
type SpiceDB struct {
	permissions v1.PermissionsServiceClient
	schema      v1.SchemaServiceClient
}

// NewSpiceDB returns the SpiceDB provider on conn.
func NewSpiceDB(conn grpc.ClientConnInterface) (*SpiceDB, error) {
	if conn == nil {
		return nil, errors.New("authz: no SpiceDB connection")
	}
	return &SpiceDB{permissions: v1.NewPermissionsServiceClient(conn), schema: v1.NewSchemaServiceClient(conn)}, nil
}

// RelationsOf gives the names of the relations of the definition that the schema SpiceDB serves declares, at full
// consistency, in name order, through one ReflectSchema call. A definition that the schema does not declare is an
// error that names it, so a reader never takes an unknown type for one with no relation.
func (s *SpiceDB) RelationsOf(ctx context.Context, definition string) ([]string, error) {
	if err := validType("resource", definition); err != nil {
		return nil, err
	}
	response, err := s.schema.ReflectSchema(ctx, &v1.ReflectSchemaRequest{
		Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
	})
	if err != nil {
		return nil, spiceError("read the schema", err)
	}
	for _, d := range response.GetDefinitions() {
		if d.GetName() != definition {
			continue
		}
		names := make([]string, 0, len(d.GetRelations()))
		for _, r := range d.GetRelations() {
			names = append(names, r.GetName())
		}
		sort.Strings(names)
		return names, nil
	}
	return nil, fmt.Errorf("authz: the schema that SpiceDB serves declares no definition %s: %w", definition, ErrNoDefinition)
}

// ErrNoDefinition is the error of RelationsOf for a definition that the served schema does not declare, such as the
// definition tenant of a service whose schema has no worker relation: a caller takes it as no relation.
var ErrNoDefinition = errors.New("authz: the served schema declares no such definition")

// Exists tells whether SpiceDB holds a relationship that the filter matches, at full consistency, through one
// ReadRelationships call with a limit of one. The filter must name one resource type, one resource ID and one
// relation, so a read never scans the relationships of other resources or of another tenant.
func (s *SpiceDB) Exists(ctx context.Context, filter Filter) (bool, error) {
	if filter.ResourceType == "" || filter.ResourceID == "" || filter.Relation == "" {
		return false, Refused(errors.New("authz: a read of relationships must name one resource type, one resource ID and one relation"))
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := s.permissions.ReadRelationships(ctx, &v1.ReadRelationshipsRequest{
		Consistency:        &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
		RelationshipFilter: spiceFilter(filter),
		OptionalLimit:      1,
	})
	if err != nil {
		return false, spiceError("read the relationships", err)
	}
	if _, err := stream.Recv(); err != nil {
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		return false, spiceError("read the relationships", err)
	}
	return true, nil
}

// Searches reports that SpiceDB offers a resource search, through LookupResources.
func (s *SpiceDB) Searches() bool { return true }

// Evaluate decides one request with CheckPermission. A conditional permission is a ContextError, not a deny
// (ST-SUP-RP06-LC08).
func (s *SpiceDB) Evaluate(ctx context.Context, request EvaluationRequest) (EvaluationResult, error) {
	consistency, err := spiceConsistency(request.Consistency)
	if err != nil {
		return EvaluationResult{}, err
	}
	caveat, err := spiceContext(request.Context)
	if err != nil {
		return EvaluationResult{}, err
	}
	response, err := s.permissions.CheckPermission(ctx, &v1.CheckPermissionRequest{
		Consistency: consistency,
		Resource:    spiceObject(request.Resource.Type, request.Resource.ID),
		Permission:  request.Action.Name,
		Subject:     spiceSubject(request.Subject, ""),
		Context:     caveat,
	})
	if err != nil {
		return EvaluationResult{}, spiceError("check the permission", err)
	}
	token, err := tokenOf(response.GetCheckedAt())
	if err != nil {
		return EvaluationResult{}, err
	}
	return resultOf(response.GetPermissionship(), response.GetPartialCaveatInfo(), token)
}

// EvaluateBatch decides a batch with one CheckBulkPermissions call, at the consistency of the first request, which
// Port and Handle give every request of a batch. ExecuteAll returns a result for every request;
// DenyOnFirstDeny and PermitOnFirstPermit return the results up to and with the first deny or allow.
func (s *SpiceDB) EvaluateBatch(ctx context.Context, request BatchRequest) ([]EvaluationResult, error) {
	if len(request.Evaluations) == 0 {
		return nil, nil
	}
	consistency, err := spiceConsistency(request.Evaluations[0].Consistency)
	if err != nil {
		return nil, err
	}
	items := make([]*v1.CheckBulkPermissionsRequestItem, len(request.Evaluations))
	for i, evaluation := range request.Evaluations {
		caveat, err := spiceContext(evaluation.Context)
		if err != nil {
			return nil, err
		}
		items[i] = &v1.CheckBulkPermissionsRequestItem{
			Resource:   spiceObject(evaluation.Resource.Type, evaluation.Resource.ID),
			Permission: evaluation.Action.Name,
			Subject:    spiceSubject(evaluation.Subject, ""),
			Context:    caveat,
		}
	}
	response, err := s.permissions.CheckBulkPermissions(ctx, &v1.CheckBulkPermissionsRequest{
		Consistency: consistency,
		Items:       items,
	})
	if err != nil {
		return nil, spiceError("check the batch", err)
	}
	if len(response.GetPairs()) != len(items) {
		return nil, errors.New("authz: SpiceDB did not answer every check of the batch")
	}
	token, err := tokenOf(response.GetCheckedAt())
	if err != nil {
		return nil, err
	}
	results := make([]EvaluationResult, 0, len(items))
	for _, pair := range response.GetPairs() {
		if pairError := pair.GetError(); pairError != nil {
			return nil, spiceError("check the batch", status.ErrorProto(pairError))
		}
		item := pair.GetItem()
		result, err := resultOf(item.GetPermissionship(), item.GetPartialCaveatInfo(), token)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
		if (request.Semantic == DenyOnFirstDeny && !result.Allowed) || (request.Semantic == PermitOnFirstPermit && result.Allowed) {
			break
		}
	}
	return results, nil
}

// Search returns one page of a resource search with LookupResources. The page token carries the
// after_result_cursor of the last resource of the page and the consistency of the first page, which each later page
// sends again unchanged, because SpiceDB binds the consistency into the cursor. The results of a later page are
// then filtered with one CheckBulkPermissions call at the request's consistency, which the handle sets at the
// floor of that page, so a later page is in force at the current floor (ST-SUP-RP06-LC05).
func (s *SpiceDB) Search(ctx context.Context, request SearchRequest) (SearchResult, error) {
	lookupConsistency := request.Consistency
	cursor, first, later := request.Page.Token.CursorFor(spiceDBName)
	if later {
		lookupConsistency = first
	} else if !request.Page.Token.IsZero() {
		return SearchResult{}, errors.New("authz: the page token is not a SpiceDB page token")
	}
	consistency, err := spiceConsistency(lookupConsistency)
	if err != nil {
		return SearchResult{}, err
	}
	caveat, err := spiceContext(request.Context)
	if err != nil {
		return SearchResult{}, err
	}
	lookup := &v1.LookupResourcesRequest{
		Consistency:        consistency,
		ResourceObjectType: request.ResourceType,
		Permission:         request.Action.Name,
		Subject:            spiceSubject(request.Subject, ""),
		Context:            caveat,
	}
	if request.Page.Limit > 0 {
		lookup.OptionalLimit = uint32(request.Page.Limit)
	}
	if later {
		lookup.OptionalCursor = &v1.Cursor{Token: cursor}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := s.permissions.LookupResources(ctx, lookup)
	if err != nil {
		return SearchResult{}, spiceError("search the resources", err)
	}
	var result SearchResult
	var last string
	found := 0
	for {
		response, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return SearchResult{}, spiceError("search the resources", err)
		}
		found++
		last = response.GetAfterResultCursor().GetToken()
		if response.GetPermissionship() == v1.LookupPermissionship_LOOKUP_PERMISSIONSHIP_CONDITIONAL_PERMISSION {
			return SearchResult{}, contextError(response.GetPartialCaveatInfo())
		}
		if response.GetPermissionship() != v1.LookupPermissionship_LOOKUP_PERMISSIONSHIP_HAS_PERMISSION {
			continue
		}
		result.Resources = append(result.Resources, Resource{Type: request.ResourceType, ID: response.GetResourceObjectId()})
		if response.GetLookedUpAt() != nil {
			if result.Token, err = tokenOf(response.GetLookedUpAt()); err != nil {
				return SearchResult{}, err
			}
		}
	}
	// A full page may have more after it; a short page is the end.
	if request.Page.Limit > 0 && found == request.Page.Limit && last != "" {
		result.Next = NewPageToken(spiceDBName, last, lookupConsistency)
	}
	if later && len(result.Resources) > 0 {
		evaluations := make([]EvaluationRequest, len(result.Resources))
		for i, resource := range result.Resources {
			evaluations[i] = EvaluationRequest{Subject: request.Subject, Action: request.Action, Resource: resource,
				Context: request.Context, Consistency: request.Consistency}
		}
		checked, err := s.EvaluateBatch(ctx, BatchRequest{Evaluations: evaluations, Semantic: ExecuteAll})
		if err != nil {
			return SearchResult{}, err
		}
		var allowed []Resource
		for i, check := range checked {
			if check.Allowed {
				allowed = append(allowed, result.Resources[i])
			}
			result.Token = check.Token
		}
		result.Resources = allowed
	}
	return result, nil
}

// Touch writes the tuples with OPERATION_TOUCH in one WriteRelationships request, which is idempotent on retry.
func (s *SpiceDB) Touch(ctx context.Context, tuples []Tuple, preconditions []Precondition) (Token, error) {
	if len(tuples) == 0 {
		return Token{}, errors.New("authz: Touch needs at least one tuple")
	}
	updates := make([]*v1.RelationshipUpdate, len(tuples))
	for i, tuple := range tuples {
		if err := tuple.valid(); err != nil {
			return Token{}, Refused(err)
		}
		updates[i] = &v1.RelationshipUpdate{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: spiceObject(tuple.Resource.Type, tuple.Resource.ID),
				Relation: tuple.Relation,
				Subject:  spiceSubject(tuple.Subject, tuple.SubjectRelation),
			},
		}
	}
	return s.write(ctx, updates, preconditions)
}

// Delete removes the relationships of the filters, which is idempotent on retry. Exact filters go in one
// WriteRelationships request with OPERATION_DELETE; one filter that is not exact goes to DeleteRelationships, and
// more than one is refused, because two calls would not be atomic. A filter that is not exact must name exactly one
// resource type, one resource ID and one relation, and no subject (Filter.OfRelation): one with an empty resource ID
// or relation would delete the tuples of other resources, so it is refused (ST-SUP-Q118).
func (s *SpiceDB) Delete(ctx context.Context, filters []Filter, preconditions []Precondition) (Token, error) {
	if len(filters) == 0 {
		return Token{}, errors.New("authz: Delete needs at least one filter")
	}
	exact := true
	for _, filter := range filters {
		if filter.ResourceType == "" {
			return Token{}, Refused(errors.New("authz: a filter needs a resource type"))
		}
		exact = exact && filter.Exact()
	}
	if exact {
		updates := make([]*v1.RelationshipUpdate, len(filters))
		for i, filter := range filters {
			updates[i] = &v1.RelationshipUpdate{
				Operation: v1.RelationshipUpdate_OPERATION_DELETE,
				Relationship: &v1.Relationship{
					Resource: spiceObject(filter.ResourceType, filter.ResourceID),
					Relation: filter.Relation,
					Subject: spiceSubject(Subject{Type: filter.SubjectType, ID: filter.SubjectID},
						filter.SubjectRelation),
				},
			}
		}
		return s.write(ctx, updates, preconditions)
	}
	if len(filters) > 1 {
		return Token{}, Refused(errors.New("authz: Delete takes more than one filter only when each names one relationship"))
	}
	if !filters[0].OfRelation() {
		return Token{}, Refused(errors.New("authz: a filter that does not name one relationship must name one resource type, one resource ID and one relation, and no subject"))
	}
	response, err := s.permissions.DeleteRelationships(ctx, &v1.DeleteRelationshipsRequest{
		RelationshipFilter:    spiceFilter(filters[0]),
		OptionalPreconditions: spicePreconditions(preconditions),
	})
	if err != nil {
		return Token{}, spiceError("delete the relationships", err)
	}
	return tokenOf(response.GetDeletedAt())
}

// Write applies the touches and the deletes of exact filters in one WriteRelationships request, with
// OPERATION_TOUCH and OPERATION_DELETE only, and returns its one written_at token.
func (s *SpiceDB) Write(ctx context.Context, touches []Tuple, deletes []Filter, preconditions []Precondition) (Token, error) {
	if len(touches)+len(deletes) == 0 {
		return Token{}, errors.New("authz: Write needs at least one update")
	}
	updates := make([]*v1.RelationshipUpdate, 0, len(touches)+len(deletes))
	for _, tuple := range touches {
		if err := tuple.valid(); err != nil {
			return Token{}, Refused(err)
		}
		updates = append(updates, &v1.RelationshipUpdate{Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{Resource: spiceObject(tuple.Resource.Type, tuple.Resource.ID),
				Relation: tuple.Relation, Subject: spiceSubject(tuple.Subject, tuple.SubjectRelation)}})
	}
	for _, filter := range deletes {
		if !filter.Exact() {
			return Token{}, Refused(errors.New("authz: Write deletes only filters that each name one relationship"))
		}
		updates = append(updates, &v1.RelationshipUpdate{Operation: v1.RelationshipUpdate_OPERATION_DELETE,
			Relationship: &v1.Relationship{Resource: spiceObject(filter.ResourceType, filter.ResourceID),
				Relation: filter.Relation,
				Subject:  spiceSubject(Subject{Type: filter.SubjectType, ID: filter.SubjectID}, filter.SubjectRelation)}})
	}
	return s.write(ctx, updates, preconditions)
}

func (s *SpiceDB) write(ctx context.Context, updates []*v1.RelationshipUpdate, preconditions []Precondition) (Token, error) {
	response, err := s.permissions.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates:               updates,
		OptionalPreconditions: spicePreconditions(preconditions),
	})
	if err != nil {
		return Token{}, spiceError("write the relationships", err)
	}
	return tokenOf(response.GetWrittenAt())
}

func spiceObject(objectType, id string) *v1.ObjectReference {
	return &v1.ObjectReference{ObjectType: objectType, ObjectId: id}
}

func spiceSubject(subject Subject, relation string) *v1.SubjectReference {
	return &v1.SubjectReference{Object: spiceObject(subject.Type, subject.ID), OptionalRelation: relation}
}

func spiceFilter(filter Filter) *v1.RelationshipFilter {
	out := &v1.RelationshipFilter{
		ResourceType:       filter.ResourceType,
		OptionalResourceId: filter.ResourceID,
		OptionalRelation:   filter.Relation,
	}
	if filter.SubjectType != "" {
		out.OptionalSubjectFilter = &v1.SubjectFilter{SubjectType: filter.SubjectType, OptionalSubjectId: filter.SubjectID}
		if filter.SubjectRelation != "" {
			out.OptionalSubjectFilter.OptionalRelation = &v1.SubjectFilter_RelationFilter{Relation: filter.SubjectRelation}
		}
	}
	return out
}

func spicePreconditions(preconditions []Precondition) []*v1.Precondition {
	if len(preconditions) == 0 {
		return nil
	}
	out := make([]*v1.Precondition, len(preconditions))
	for i, precondition := range preconditions {
		operation := v1.Precondition_OPERATION_MUST_NOT_MATCH
		if precondition.MustExist {
			operation = v1.Precondition_OPERATION_MUST_MATCH
		}
		out[i] = &v1.Precondition{Operation: operation, Filter: spiceFilter(precondition.Filter)}
	}
	return out
}

// spiceConsistency maps Fresh to fully_consistent and AtLeast to at_least_as_fresh. Every other value is refused:
// SpiceDB's minimize_latency and at_exact_snapshot have no Consistency value (ST-SUP-RP06-LC03).
func spiceConsistency(consistency Consistency) (*v1.Consistency, error) {
	if consistency.IsFresh() {
		return &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}}, nil
	}
	if token, ok := consistency.Token(); ok {
		return &v1.Consistency{Requirement: &v1.Consistency_AtLeastAsFresh{
			AtLeastAsFresh: &v1.ZedToken{Token: token.Value()},
		}}, nil
	}
	return nil, ErrNoConsistency
}

func spiceContext(context Context) (*structpb.Struct, error) {
	if len(context) == 0 {
		return nil, nil
	}
	caveat, err := structpb.NewStruct(context)
	if err != nil {
		return nil, errors.New("authz: the caveat context holds a value that is not JSON")
	}
	return caveat, nil
}

func resultOf(permissionship v1.CheckPermissionResponse_Permissionship, caveat *v1.PartialCaveatInfo, token Token) (EvaluationResult, error) {
	switch permissionship {
	case v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION:
		return EvaluationResult{Allowed: true, Token: token, Reason: "has_permission"}, nil
	case v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION:
		return EvaluationResult{Allowed: false, Token: token, Reason: "no_permission"}, nil
	case v1.CheckPermissionResponse_PERMISSIONSHIP_CONDITIONAL_PERMISSION:
		return EvaluationResult{}, contextError(caveat)
	default:
		return EvaluationResult{}, errors.New("authz: SpiceDB answered an unknown permissionship")
	}
}

func contextError(caveat *v1.PartialCaveatInfo) error {
	missing := append([]string(nil), caveat.GetMissingRequiredContext()...)
	sort.Strings(missing)
	return &ContextError{Missing: missing}
}

// ErrProvider matches every error of a provider call: errors.Is(err, ErrProvider). IsRefused tells a refusal, which
// a retry does not change, from an error that a retry may pass; a ContextError is a third kind.
var ErrProvider = errors.New("authz: the provider call failed")

// providerError is the error of a provider call. Its text is fixed for the call and the gRPC status code, and it
// wraps no error of the provider, so no provider message, subject ID or caveat value reaches it (ST-SUP-RP06-LC20).
type providerError struct {
	what  string
	code  codes.Code
	cause error
	// notServed is true when SpiceDB does not serve yet a definition, a relation or a permission of the request:
	// for some seconds after a schema write, a read at a quantized revision does not know the new definitions.
	notServed bool
}

func (e *providerError) Error() string {
	return "authz: SpiceDB: " + e.what + ": " + e.code.String()
}

func (e *providerError) Is(target error) bool { return target == ErrProvider }

// Unwrap gives the context error of a call that its context ended, so the kind stays readable.
func (e *providerError) Unwrap() error { return e.cause }

// spiceError maps a gRPC error of SpiceDB to a fixed text for what and its status code. InvalidArgument,
// FailedPrecondition, NotFound and OutOfRange are refusals that a retry does not change; any other error may pass on
// a retry.
func spiceError(what string, err error) error {
	code := status.Code(err)
	fixed := &providerError{what: what, code: code, cause: contextCause(err), notServed: notServed(err)}
	switch code {
	case codes.Canceled:
		fixed.cause = context.Canceled
	case codes.DeadlineExceeded:
		fixed.cause = context.DeadlineExceeded
	}
	switch code {
	case codes.InvalidArgument, codes.FailedPrecondition, codes.NotFound, codes.OutOfRange:
		return Refused(fixed)
	default:
		return fixed
	}
}

// tokenOf makes the Token of a ZedToken. The ZedToken is kept byte for byte and never parsed: the revision of the
// PostgreSQL datastore is a snapshot, and only SpiceDB orders its tokens (ST-SUP-RP06-LC04).
func tokenOf(token *v1.ZedToken) (Token, error) {
	if token.GetToken() == "" {
		return Token{}, errors.New("authz: SpiceDB answered with no ZedToken")
	}
	return NewToken(token.GetToken())
}

// notServed reports whether a SpiceDB error says, by the reason of its error details, that the schema SpiceDB reads
// at the revision of the request has no such definition, relation or permission. The reason is a fixed enum value,
// so no message text is read.
func notServed(err error) bool {
	for _, detail := range status.Convert(err).Details() {
		info, ok := detail.(*errdetails.ErrorInfo)
		if !ok {
			continue
		}
		switch info.GetReason() {
		case v1.ErrorReason_ERROR_REASON_UNKNOWN_DEFINITION.String(), v1.ErrorReason_ERROR_REASON_UNKNOWN_RELATION_OR_PERMISSION.String():
			return true
		}
	}
	return false
}

// The causes that Cause gives: fixed names that a log line may hold, because they hold no text of the provider.
const (
	// CauseSchemaNotServed is a provider that does not serve yet a definition, a relation or a permission of the
	// request, such as SpiceDB for some seconds after a schema write.
	CauseSchemaNotServed = "authz_schema_not_served"
	// CauseUnavailable is a provider that does not answer, or that has no capacity for the call now.
	CauseUnavailable = "authz_provider_unavailable"
	// CauseDeadline is a provider call that ran out of time.
	CauseDeadline = "authz_provider_deadline"
	// CauseCanceled is a provider call whose context was canceled.
	CauseCanceled = "authz_provider_canceled"
	// CauseRefused is a provider that refuses the request, which a retry does not change.
	CauseRefused = "authz_provider_refused"
	// CauseConditional is a conditional permission whose caveat context is missing (ContextError).
	CauseConditional = "authz_conditional_permission"
	// CauseProvider is every other failure of a provider call.
	CauseProvider = "authz_provider_failed"
	// CauseOther is an error that no provider call gave.
	CauseOther = "authz_failed"
)

// Cause gives the fixed name of the cause of an error of the decision port, for the log of an undecided read or
// write: it never holds a provider message, a subject or a resource ID (ST-SUP-RP06-LC20).
func Cause(err error) string {
	var provider *providerError
	var conditional *ContextError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &provider):
		switch {
		case provider.notServed:
			return CauseSchemaNotServed
		case provider.code == codes.Unavailable || provider.code == codes.ResourceExhausted || provider.code == codes.Aborted:
			return CauseUnavailable
		case provider.code == codes.DeadlineExceeded:
			return CauseDeadline
		case provider.code == codes.Canceled:
			return CauseCanceled
		case IsRefused(err):
			return CauseRefused
		default:
			return CauseProvider
		}
	case errors.As(err, &conditional):
		return CauseConditional
	case errors.Is(err, context.DeadlineExceeded):
		return CauseDeadline
	case errors.Is(err, context.Canceled):
		return CauseCanceled
	default:
		return CauseOther
	}
}

// Transient reports whether err is a failure of the provider that a retry of the same request may pass, so that the
// request is answered as unavailable for now (HTTP 503 with Retry-After), not as an internal error: a provider that
// does not serve yet a definition of the schema the release wrote, one that does not answer or has no capacity, and
// a provider call that ran out of time.
func Transient(err error) bool {
	switch Cause(err) {
	case CauseSchemaNotServed, CauseUnavailable, CauseDeadline:
		return true
	default:
		return false
	}
}

// The bound of AwaitSchema: at most SchemaPolls probe rounds, SchemaPause apart, each probe within probeTimeout, so
// the wait ends after about 30 s when SpiceDB answers.
// A check at least as fresh as a token older than a schema write reads at the revision that the quantization window
// of SpiceDB (5 s by default) and its staleness allowance keep behind the head, so a new definition is served to it
// after about 5 to 10 s.
const (
	SchemaPolls = 120
	SchemaPause = 250 * time.Millisecond
	// SchemaStaleness is the longest time for which SpiceDB can still choose a revision older than one it has
	// already served. It must be at least the quantization interval of SpiceDB times its max staleness percent
	// (the flags --datastore-revision-quantization-interval, 5 s by default, and
	// --datastore-revision-quantization-max-staleness-percent, 0.1 by default, so 0.5 s): the optimized revision of
	// SpiceDB v1.57 picks a candidate at a time up to that allowance in the past, at random, so for that time after
	// a round that serves the new schema, a check can still read a revision before the write.
	SchemaStaleness = time.Second
	// schemaRounds is the number of rounds in a row in which every definition must be served: enough rounds,
	// SchemaPause apart, to span SchemaStaleness, so that no candidate revision from before the write is left when
	// the wait returns; and at least two, so a SpiceDB of more than one node behind a balancer has shown the schema
	// on more than one answer.
	schemaRounds = max(int((SchemaStaleness+SchemaPause-1)/SchemaPause)+1, 2)
)

// ErrSchemaNotServed is the error of AwaitSchema when SpiceDB does not serve every definition of the written schema
// within its bound: the migration job then fails, and the release does not start (ST-SUP-Q140).
var ErrSchemaNotServed = errors.New("authz: SpiceDB does not serve every definition of the written schema to a check of an older floor within the bound")

// probeTimeout is the deadline of one probe call of AwaitSchema; a test of the package shortens it.
var probeTimeout = 2 * time.Second

// schemaPause waits between two probe rounds of AwaitSchema; a test of the package replaces it to count the rounds.
var schemaPause = func(ctx context.Context) error {
	timer := time.NewTimer(SchemaPause)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// SchemaToken reads the token of the schema that SpiceDB holds now, for AwaitSchema; the migration job reads it
// before it writes the schema. It is the zero Token when SpiceDB holds no schema yet: then no floor of the service is
// older than the write.
func SchemaToken(ctx context.Context, conn grpc.ClientConnInterface) (Token, error) {
	if conn == nil {
		return Token{}, errors.New("authz: SchemaToken needs the SpiceDB connection")
	}
	response, err := v1.NewSchemaServiceClient(conn).ReadSchema(ctx, &v1.ReadSchemaRequest{})
	if status.Code(err) == codes.NotFound {
		return Token{}, nil
	}
	if err != nil {
		return Token{}, spiceError("read the schema", err)
	}
	return tokenOf(response.GetReadAt())
}

// AwaitSchema waits until SpiceDB serves each of definitions to a read at least as fresh as since, a token from
// before the schema write (SchemaToken), for schemaRounds probe rounds in a row, which span SchemaStaleness; with a
// zero since it probes fresh.
// A check of the service at least as fresh as a floor older than the write reads at a revision no newer than the
// probe's, so it does not know a new definition before the probe does. A probe is one ReadRelationships call with a
// limit of 1 on the resource type; SpiceDB checks the type against the schema it reads at the revision of the call,
// and answers ERROR_REASON_UNKNOWN_DEFINITION while that revision is older than the write. The migration job runs it
// after it writes the schema, so the release does not start while a read of a new resource type would fail
// (walkthrough 3, G4; ST-SUP-Q140). It fails closed: after SchemaPolls rounds it gives ErrSchemaNotServed, and a
// refusal that is not about the schema, at once. Its errors hold no provider text.
func AwaitSchema(ctx context.Context, conn grpc.ClientConnInterface, definitions []string, since Token) error {
	if conn == nil || len(definitions) == 0 {
		return errors.New("authz: AwaitSchema needs the SpiceDB connection and the definitions of the schema")
	}
	consistency := Fresh()
	if !since.IsZero() {
		consistency = AtLeast(since)
	}
	return awaitSchema(ctx, v1.NewPermissionsServiceClient(conn), definitions, consistency, SchemaPolls)
}

func awaitSchema(ctx context.Context, permissions v1.PermissionsServiceClient, definitions []string, consistency Consistency, polls int) error {
	at, err := spiceConsistency(consistency)
	if err != nil {
		return err
	}
	served := 0
	for poll := 1; ; poll++ {
		all := true
		for _, definition := range definitions {
			ok, err := serves(ctx, permissions, definition, at)
			if err != nil {
				return err
			}
			if !ok {
				all = false
				break
			}
		}
		if all {
			served++
		} else {
			served = 0
		}
		if served == schemaRounds {
			return nil
		}
		if poll >= polls {
			return ErrSchemaNotServed
		}
		if err := schemaPause(ctx); err != nil {
			return fmt.Errorf("%w: %w", ErrSchemaNotServed, err)
		}
	}
}

// serves probes one definition at the consistency at: true when SpiceDB serves it, false when it does not yet, or
// does not answer now. Any other error is a refusal that a wait does not change.
func serves(parent context.Context, permissions v1.PermissionsServiceClient, definition string, at *v1.Consistency) (bool, error) {
	// Each probe has its own deadline, so a call that hangs costs one round and the bound of the wait holds (security
	// review of PR #354, I2).
	ctx, cancel := context.WithTimeout(parent, probeTimeout)
	defer cancel()
	stream, err := permissions.ReadRelationships(ctx, &v1.ReadRelationshipsRequest{
		Consistency:        at,
		RelationshipFilter: &v1.RelationshipFilter{ResourceType: definition},
		OptionalLimit:      1,
	})
	for err == nil {
		_, err = stream.Recv()
	}
	if errors.Is(err, io.EOF) {
		return true, nil
	}
	failure := spiceError("read the relationships of a definition", err)
	if (Transient(failure) || errors.Is(ctx.Err(), context.DeadlineExceeded)) && parent.Err() == nil {
		return false, nil
	}
	return false, failure
}

// LogEvery is the shortest time between two log lines of one cause of a CauseLog: a provider outage logs one line
// for each cause in each LogEvery, with the count of the lines it held back (security review of PR #354, L3).
const LogEvery = 10 * time.Second

// CauseLog lets one log line of each cause through in each LogEvery and counts the others. Its zero value is ready;
// it is safe for concurrent use.
type CauseLog struct {
	mu   sync.Mutex
	now  func() time.Time
	last map[string]time.Time
	held map[string]int
}

// NewCauseLog gives a CauseLog that reads the time from now; nil is time.Now.
func NewCauseLog(now func() time.Time) *CauseLog { return &CauseLog{now: now} }

// Allow reports whether a line of cause may be logged now, and how many lines of cause it held back since the last
// one it let through.
func (l *CauseLog) Allow(cause string) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.now != nil {
		now = l.now()
	}
	if l.last == nil {
		l.last, l.held = map[string]time.Time{}, map[string]int{}
	}
	if last, ok := l.last[cause]; ok && now.Sub(last) < LogEvery {
		l.held[cause]++
		return false, 0
	}
	if _, ok := l.last[cause]; !ok && len(l.last) >= MaxCauses {
		// The log keeps at most MaxCauses causes: a new cause evicts the cause with the oldest line, so a stream of
		// new causes cannot grow the maps (security review of PR #416, L2).
		oldest, first := "", true
		for c, t := range l.last {
			if first || t.Before(l.last[oldest]) {
				oldest, first = c, false
			}
		}
		delete(l.last, oldest)
		delete(l.held, oldest)
	}
	held := l.held[cause]
	l.last[cause], l.held[cause] = now, 0
	return true, held
}

// MaxCauses is the most causes that one CauseLog keeps.
const MaxCauses = 1024

// RetryAfter gives the Retry-After value of a 503 answer to a decision that the provider could not make now: a whole
// number of seconds from 1 to 3, at random, so the clients of an outage do not retry in step (L3).
func RetryAfter() string { return strconv.Itoa(1 + rand.IntN(3)) }
