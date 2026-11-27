package authz

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/authzed/spicedb/pkg/proto/core/v1"
	"github.com/authzed/spicedb/pkg/schemadsl/compiler"
	"github.com/authzed/spicedb/pkg/schemadsl/input"
	"google.golang.org/protobuf/proto"
)

// digestBase is a schema with each construct that the canonical form must keep: arrows with .any() and .all(),
// subject relations, a wildcard, a caveat with its expression, an expiration and the floor marker.
const digestBase = `use expiration

caveat only_on(day int, allowed list<int>) {
	day in allowed && day > 0
}

definition user {}

definition group {
	relation member: user | group#member
}

definition org {
	relation owner: user
	relation admin: user with only_on
	relation guest: user:* | user with expiration
	relation member: user | group#member
	relation parent: org
	permission catalog_edit = owner + admin
	permission up_any = parent.any(catalog_edit)
	permission up_all = parent.all(catalog_edit)
	permission excl = (owner + member) - guest
	permission inter = owner & member
}

definition stego_floor {
	relation marker: stego_floor
}`

func mustDigest(t *testing.T, text string) string {
	t.Helper()
	d, err := SchemaDigest(text)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// stripped compiles text and gives its definitions with no metadata and no source position, in name order.
func stripped(t *testing.T, text string) []proto.Message {
	t.Helper()
	compiled, err := compiler.Compile(compiler.InputSchema{Source: input.Source("t"), SchemaString: text}, compiler.AllowUnprefixedObjectType())
	if err != nil {
		t.Fatal(err)
	}
	var out []proto.Message
	for _, c := range compiled.CaveatDefinitions {
		c.Metadata, c.SourcePosition = nil, nil
		out = append(out, c)
	}
	for _, o := range compiled.ObjectDefinitions {
		o.Metadata, o.SourcePosition = nil, nil
		for _, r := range o.Relation {
			r.Metadata, r.SourcePosition = nil, nil
			clearPositions(r.GetUsersetRewrite())
			for _, a := range r.GetTypeInformation().GetAllowedDirectRelations() {
				a.SourcePosition = nil
			}
		}
		out = append(out, o)
	}
	return out
}

func clearPositions(rewrite *core.UsersetRewrite) {
	if rewrite == nil {
		return
	}
	rewrite.SourcePosition = nil
	for _, op := range []*core.SetOperation{rewrite.GetUnion(), rewrite.GetIntersection(), rewrite.GetExclusion()} {
		for _, child := range op.GetChild() {
			child.SourcePosition = nil
			clearPositions(child.GetUsersetRewrite())
		}
	}
}

func TestSchemaDigest(t *testing.T) {
	t.Run("ST-SUP-RP10-LC59-T13 The canonical form is the same for another order and other comments, and differs for a caveat, an expiration or a wildcard", func(t *testing.T) {
		reordered := `use expiration
// The schema again.
definition stego_floor {
	relation marker: stego_floor
}
/* the org */
definition org {
	permission inter = owner & member
	permission excl = (owner + member) - guest
	permission up_all = parent.all(catalog_edit)
	permission up_any = parent.any(catalog_edit)
	/** who edits */
	permission catalog_edit = owner + admin
	relation parent: org
	relation member: user | group#member
	relation guest: user:* | user with expiration
	relation admin: user with only_on
	relation owner: user // the owner
}
definition group {
	relation member: user | group#member
}
definition user {}
caveat only_on(day int, allowed list<int>) {
	day in allowed && day > 0
}`
		base := mustDigest(t, digestBase)
		if got := mustDigest(t, reordered); got != base {
			t.Fatal("the schema in another order and with other comments has another digest")
		}
		for name, changed := range map[string]string{
			"a caveat of an allowed type": strings.Replace(digestBase, "relation owner: user\n", "relation owner: user with only_on\n", 1),
			"an expiration":               strings.Replace(digestBase, "relation owner: user\n", "relation owner: user with expiration\n", 1),
			"a wildcard":                  strings.Replace(digestBase, "relation owner: user\n", "relation owner: user | user:*\n", 1),
			"a subject relation":          strings.Replace(digestBase, "relation member: user | group#member\n\trelation parent", "relation member: user | group\n\trelation parent", 1),
			"an arrow function":           strings.Replace(digestBase, "parent.any(catalog_edit)", "parent.all(catalog_edit)", 1),
			"a caveat expression":         strings.Replace(digestBase, "day > 0", "day > 1", 1),
			"an operator":                 strings.Replace(digestBase, "(owner + member) - guest", "(owner + member) & guest", 1),
		} {
			if changed == digestBase {
				t.Fatalf("%s: the test schema did not change", name)
			}
			if mustDigest(t, changed) == base {
				t.Fatalf("a schema that differs only in %s has the digest of the base", name)
			}
		}
		if _, err := SchemaDigest("definition {"); err == nil || strings.Contains(err.Error(), "definition {") {
			t.Fatalf("a schema that does not compile gave %v, want an error with no schema text", err)
		}
	})

	t.Run("ST-SUP-RP10-LC59-T13 The canonical form compiles to the same definitions as the schema, for .any() and .all() arrows, subject relations and caveat expressions", func(t *testing.T) {
		canonical, err := CanonicalSchema(digestBase)
		if err != nil {
			t.Fatal(err)
		}
		want, got := stripped(t, digestBase), stripped(t, canonical)
		if len(want) != len(got) {
			t.Fatalf("the canonical form has %d definitions, want %d:\n%s", len(got), len(want), canonical)
		}
		index := map[string]proto.Message{}
		for _, m := range got {
			index[m.(interface{ GetName() string }).GetName()] = m
		}
		for _, m := range want {
			name := m.(interface{ GetName() string }).GetName()
			other, ok := index[name]
			if !ok {
				t.Fatalf("the canonical form drops %s", name)
			}
			// The relations of a definition are sorted in the canonical form; compare them by name.
			if a, ok := m.(*core.NamespaceDefinition); ok {
				b := other.(*core.NamespaceDefinition)
				rel := map[string]*core.Relation{}
				for _, r := range b.Relation {
					rel[r.Name] = r
				}
				for _, r := range a.Relation {
					if !proto.Equal(r, rel[r.Name]) {
						t.Fatalf("the canonical form changes %s#%s:\n%s", name, r.Name, canonical)
					}
				}
				continue
			}
			if !proto.Equal(m, other) {
				t.Fatalf("the canonical form changes the caveat %s:\n%s", name, canonical)
			}
		}
		for _, construct := range []string{"parent.any(catalog_edit)", "parent.all(catalog_edit)", "group#member", "user:*", "with only_on", "with expiration", "use expiration", "day in allowed"} {
			if !strings.Contains(canonical, construct) {
				t.Fatalf("the canonical form has no %s:\n%s", construct, canonical)
			}
		}
		if again, err := CanonicalSchema(canonical); err != nil || again != canonical {
			t.Fatalf("the canonical form of the canonical form is another text (%v)", err)
		}
	})

	t.Run("ST-SUP-RP10-LC59-T13 A failed shared read of the served schema sends every waiter to the provider, and a reading lasts the bound only", func(t *testing.T) {
		reader := &blockingReader{release: make(chan struct{}), err: errors.New("unavailable")}
		gate, err := NewSchemaGate(digestBase, reader, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if gate.limit != SchemaReadLimit {
			t.Fatalf("the read limit is %v, want min(1 s, the decision limit)", gate.limit)
		}
		if short, _ := NewSchemaGate(digestBase, reader, 300*time.Millisecond); short.limit != 300*time.Millisecond {
			t.Fatalf("the read limit with a decision limit of 300 ms is %v", short.limit)
		}
		var wg sync.WaitGroup
		results := make(chan bool, 4)
		for range 4 {
			wg.Add(1)
			go func() { defer wg.Done(); results <- gate.Holds(context.Background()) }()
		}
		time.Sleep(100 * time.Millisecond)
		close(reader.release)
		wg.Wait()
		close(results)
		for holds := range results {
			if holds {
				t.Fatal("a waiter of a failed shared read holds")
			}
		}
		if gate.Reads() != 1 {
			t.Fatalf("four waiters made %d reads, want one", gate.Reads())
		}
		// A good reading holds for the bound on the gate's clock, and the next decision after it reads again.
		now := time.Unix(100, 0)
		gate.now = func() time.Time { return now }
		reader.err, reader.text = nil, digestBase
		if !gate.Holds(context.Background()) || !gate.Holds(context.Background()) || gate.Reads() != 2 {
			t.Fatalf("a good reading does not hold or is not reused (%d reads)", gate.Reads())
		}
		reader.text = strings.Replace(digestBase, "day > 0", "day > 1", 1)
		now = now.Add(SchemaDigestBound)
		if gate.Holds(context.Background()) || gate.Reads() != 3 {
			t.Fatalf("a reading past the bound is used (%d reads)", gate.Reads())
		}
		// A failed read after the bound of a good reading does not keep it.
		reader.text = digestBase
		now = now.Add(SchemaDigestBound)
		if !gate.Holds(context.Background()) {
			t.Fatal("a good reading does not hold")
		}
		reader.err = errors.New("unavailable")
		now = now.Add(SchemaDigestBound)
		if gate.Holds(context.Background()) {
			t.Fatal("a failed read keeps the reading of before")
		}
		reader.err = nil
		// A cancelled first caller ends the shared read with no reading.
		reader.text = digestBase
		now = now.Add(SchemaDigestBound)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		reader.hang = true
		if gate.Holds(ctx) {
			t.Fatal("a read of a cancelled caller holds")
		}
		var nilGate *SchemaGate
		if nilGate.Holds(context.Background()) {
			t.Fatal("a nil gate holds")
		}
	})
}

// blockingReader serves text or err after release closes, or waits for its context while hang is set.
type blockingReader struct {
	release chan struct{}
	text    string
	err     error
	hang    bool
}

func (r *blockingReader) ReadSchema(ctx context.Context) (string, error) {
	<-r.release
	if r.hang {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return r.text, r.err
}
