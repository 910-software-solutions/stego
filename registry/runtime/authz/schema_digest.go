package authz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	core "github.com/authzed/spicedb/pkg/proto/core/v1"
	"github.com/authzed/spicedb/pkg/schemadsl/compiler"
	"github.com/authzed/spicedb/pkg/schemadsl/generator"
	"github.com/authzed/spicedb/pkg/schemadsl/input"
	"google.golang.org/grpc"

	"github.com/910-software-solutions/stego/registry/runtime/internal/authzclock"
	"github.com/910-software-solutions/stego/registry/runtime/otel"
)

// The schema digest of ST-SUP-RP10-LC59: a proof that checking made from the schema of a release holds only while
// SpiceDB serves a schema of the same meaning. The canonical form of a schema is the SpiceDB generator form of every
// definition and caveat, with each comment and doc comment stripped, the definitions and the caveats sorted by name
// and the relations and permissions inside each definition sorted by name, after the use directives of the schema in
// name order. It keeps each allowed type with its traits, each expression and each use directive, so only the order
// and the comments of the text do not change it.

// CanonicalSchema gives the canonical form of the schema text, compiled with the schema compiler of the pinned
// SpiceDB library as checking compiles it. A schema that does not compile is an error that holds no schema text.
func CanonicalSchema(text string) (string, error) {
	compiled, err := compiler.Compile(compiler.InputSchema{Source: input.Source("schema"), SchemaString: text},
		compiler.AllowUnprefixedObjectType(), compiler.DisallowImportFlag())
	if err != nil {
		return "", errors.New("authz: the schema does not compile")
	}
	var definitions []compiler.SchemaDefinition
	caveats := slices.Clone(compiled.CaveatDefinitions)
	slices.SortFunc(caveats, func(a, b *core.CaveatDefinition) int { return strings.Compare(a.GetName(), b.GetName()) })
	for _, c := range caveats {
		c.Metadata, c.SourcePosition = nil, nil
		definitions = append(definitions, c)
	}
	objects := slices.Clone(compiled.ObjectDefinitions)
	slices.SortFunc(objects, func(a, b *core.NamespaceDefinition) int { return strings.Compare(a.GetName(), b.GetName()) })
	for _, o := range objects {
		o.Metadata, o.SourcePosition = nil, nil
		slices.SortFunc(o.Relation, func(a, b *core.Relation) int { return strings.Compare(a.GetName(), b.GetName()) })
		for _, r := range o.Relation {
			r.Metadata, r.SourcePosition = nil, nil
		}
		definitions = append(definitions, o)
	}
	generated, _, err := generator.GenerateSchema(context.Background(), definitions)
	if err != nil {
		return "", errors.New("authz: the schema cannot be put in its canonical form")
	}
	// The generator writes the use directive of each trait that it emits, such as expiration; each directive of the
	// text is kept too, in name order, so a directive that changes the meaning of the schema is part of the form.
	var body []string
	flags := useDirectives(text)
	for _, line := range strings.Split(generated, "\n") {
		if flag, ok := strings.CutPrefix(line, "use "); ok && !strings.ContainsAny(flag, " \t{}") {
			flags = append(flags, flag)
			continue
		}
		body = append(body, line)
	}
	slices.Sort(flags)
	flags = slices.Compact(flags)
	var b strings.Builder
	for _, flag := range flags {
		b.WriteString("use " + flag + "\n")
	}
	if len(flags) > 0 {
		b.WriteString("\n")
	}
	b.WriteString(strings.TrimLeft(strings.Join(body, "\n"), "\n"))
	return b.String(), nil
}

// useDirectives gives the flag of each use directive of the text: a line, outside a block comment, that is use and
// one name.
func useDirectives(text string) []string {
	var flags []string
	inComment := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if inComment {
			end := strings.Index(line, "*/")
			if end < 0 {
				continue
			}
			inComment, line = false, strings.TrimSpace(line[end+2:])
		}
		if start := strings.Index(line, "/*"); start >= 0 {
			if !strings.Contains(line[start:], "*/") {
				inComment = true
			}
			line = strings.TrimSpace(line[:start])
		}
		if flag, ok := strings.CutPrefix(line, "use "); ok {
			if flag = strings.TrimSpace(flag); flag != "" && !strings.ContainsAny(flag, " \t{}/") {
				flags = append(flags, flag)
			}
		}
	}
	return flags
}

// SchemaDigest gives the SHA-256 of the canonical form of the schema text, in hexadecimal.
func SchemaDigest(text string) (string, error) {
	canonical, err := CanonicalSchema(text)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:]), nil
}

// SchemaReader reads the schema that the provider serves now. The SpiceDB provider reads it with ReadSchema.
type SchemaReader interface {
	ReadSchema(ctx context.Context) (string, error)
}

// ReadSchema reads the schema text that SpiceDB serves, with one ReadSchema call. Its error holds no provider text.
func (s *SpiceDB) ReadSchema(ctx context.Context) (string, error) {
	response, err := s.schema.ReadSchema(ctx, &v1.ReadSchemaRequest{})
	if err != nil {
		return "", spiceError("read the schema", err)
	}
	return response.GetSchemaText(), nil
}

// ReadSchemaText reads the schema text that SpiceDB serves on conn, for the read-back of the migration job.
func ReadSchemaText(ctx context.Context, conn grpc.ClientConnInterface) (string, error) {
	if conn == nil {
		return "", errors.New("authz: ReadSchemaText needs the SpiceDB connection")
	}
	s, err := NewSpiceDB(conn)
	if err != nil {
		return "", err
	}
	return s.ReadSchema(ctx)
}

// The bounds of the schema gate (ST-SUP-RP10-LC59): one reading of the served schema is used for at most
// SchemaDigestBound after it returned, and each ReadSchema call runs within SchemaReadLimit, never more than the
// decision time limit.
const (
	SchemaDigestBound = 5 * time.Second
	SchemaReadLimit   = time.Second
)

// SchemaGate tells whether SpiceDB serves the schema of the release of this pod now (ST-SUP-RP10-LC59). Its expected
// digest is the canonical digest of the schema compiled into the release, computed once when it is made; nothing is
// looked up by a release name. Holds reads the served schema with ReadSchema, at most once in each SchemaDigestBound
// on the monotonic clock, and the concurrent re-reads of one gate collapse into one call. A read error, a time-out
// and a digest that is not the own one make Holds false, so the runtime decides at the provider (fail closed).
type SchemaGate struct {
	own    string
	reader SchemaReader
	limit  time.Duration
	// now is the clock of the bound: time.Now, whose reading holds the monotonic clock, unless a test sets it.
	now func() time.Time

	mu sync.Mutex
	// reading is the last reading that returned, and read the time when it returned; holds tells whether its digest
	// is the own one.
	reading  bool
	readAt   time.Time
	holds    bool
	inFlight *schemaRead
	// reads counts the ReadSchema calls, for the tests.
	reads int
}

// schemaRead is one ReadSchema call that the waiters of a gate share. done closes when it returned; holds is its
// answer, false for an error.
type schemaRead struct {
	done  chan struct{}
	holds bool
}

// NewSchemaGate makes the gate of the schema text own, the text that the migration job of the release writes, over
// reader. decisionLimit is the decision time limit of the runtime, or zero for DefaultDecisionTimeout: each read runs
// within the smaller of it and SchemaReadLimit. It refuses a schema that does not compile.
func NewSchemaGate(own string, reader SchemaReader, decisionLimit time.Duration) (*SchemaGate, error) {
	if reader == nil {
		return nil, errors.New("authz: the schema gate needs the reader of the served schema")
	}
	digest, err := SchemaDigest(own)
	if err != nil {
		return nil, err
	}
	if decisionLimit <= 0 {
		decisionLimit = DefaultDecisionTimeout
	}
	return &SchemaGate{own: digest, reader: reader, limit: min(SchemaReadLimit, decisionLimit), now: time.Now}, nil
}

// The authz package gives the tests of the runtime module the clock of a schema gate, through the internal package
// authzclock, so the code that a service generates cannot change the bound of a reading.
func init() {
	authzclock.RegisterGate(func(gate any, now func() time.Time) bool {
		g, ok := gate.(*SchemaGate)
		if ok && now != nil {
			g.mu.Lock()
			g.now = now
			g.mu.Unlock()
		}
		return ok
	})
}

// Digest is the expected digest of the gate: the canonical digest of the schema of the release.
func (g *SchemaGate) Digest() string { return g.own }

// Holds tells whether the schema that SpiceDB serves has the digest of the release, by a reading of at most
// SchemaDigestBound before. A nil gate never holds.
func (g *SchemaGate) Holds(ctx context.Context) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	if g.reading && g.now().Sub(g.readAt) < SchemaDigestBound {
		holds := g.holds
		g.mu.Unlock()
		return holds
	}
	if call := g.inFlight; call != nil {
		g.mu.Unlock()
		select {
		case <-call.done:
			return call.holds
		case <-ctx.Done():
			return false
		}
	}
	call := &schemaRead{done: make(chan struct{})}
	g.inFlight = call
	g.reads++
	g.mu.Unlock()

	holds, ok := g.read(ctx)
	g.mu.Lock()
	if ok {
		g.reading, g.readAt, g.holds = true, g.now(), holds
	} else {
		// A failed read leaves no reading: the next decision reads again, and no waiter keeps the reading of before.
		g.reading = false
	}
	call.holds = ok && holds
	g.inFlight = nil
	g.mu.Unlock()
	close(call.done)
	return call.holds
}

// read reads the served schema within the limit and compares its digest; ok is false for an error or a time-out.
func (g *SchemaGate) read(ctx context.Context) (holds, ok bool) {
	ctx, cancel := context.WithTimeout(ctx, g.limit)
	defer cancel()
	text, err := g.reader.ReadSchema(ctx)
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		if ok, held := schemaGateLog.Allow(Cause(err)); ok {
			slog.WarnContext(ctx, "authz: the served schema could not be read; a reference is decided at the provider", otel.SlogEventKey, "authz.schema.unread",
				"cause", Cause(err), "held_back", held)
		}
		return false, false
	}
	digest, err := SchemaDigest(text)
	if err != nil {
		return false, true
	}
	return digest == g.own, true
}

// Reads gives the count of the ReadSchema calls of the gate.
func (g *SchemaGate) Reads() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reads
}

// schemaGateLog holds back the warning of a failed read of the served schema to one line for each cause in each
// LogEvery.
var schemaGateLog = NewCauseLog(nil)
