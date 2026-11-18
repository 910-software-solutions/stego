// Package kafkaconsumer is the generator of the kafka-consumer component (ST-SUP-RC38). For the one declared
// subscription it emits the declaration of the subscription and the constructor of its Kafka inbox on the runtime
// library, kafkainbox of the pinned github.com/jsell-rh/stego/registry/runtime (ST-SUP-RP01-LC35 to
// ST-SUP-RP01-LC38), and the migration text of the table of the refused records. The worker mode of the service
// runtime runs the inbox. The checks, the inbox write, the offsets and the refusals live in the runtime library; the
// generated code holds what varies with the declaration (ST-STD-RUNTIME-R05). The orchestrator decided ST-SUP-Q158
// under the owner's delegation, 2026-11-17.
package kafkaconsumer

import (
	"bytes"
	"errors"
	"fmt"
	"go/format"
	"path"
	"strings"

	"github.com/jsell-rh/stego/internal/declaration"
	"github.com/jsell-rh/stego/internal/gen"
)

// Generator is the generator of the kafka-consumer component.
type Generator struct{}

// file is the one Go file that the component emits.
const file = "subscriptions.go"

// MinimumGoVersion is the Go version of the runtime library that the emitted code imports.
func (*Generator) MinimumGoVersion() string { return "1.26.11" }

// ValidateContext checks the context as Generate does.
func (*Generator) ValidateContext(ctx gen.Context) error {
	_, err := plan(ctx)
	return err
}

// subscription is what the emitted file holds of the declared subscription.
type subscription struct {
	Package, Service, Name, Source, Worker string
	Events                                 []string
}

// plan reads the one subscription and the worker that consumes it, and refuses a context that the emitted code or
// the runtime would refuse (ST-SUP-RC38-OC01): a setting of the component, no subscription or more than
// declaration.MaxSubscriptions, a service with no outbox or no worker component, and a subscription that not exactly
// one worker consumes.
func plan(ctx gen.Context) (*subscription, error) {
	if err := gen.ValidateGoPackageNamespace(ctx.OutputNamespace); err != nil {
		return nil, err
	}
	for key := range ctx.ComponentConfig {
		return nil, fmt.Errorf("kafka-consumer: the component has no setting %s", key)
	}
	if len(ctx.Subscriptions) == 0 {
		return nil, errors.New("kafka-consumer: the component reads the topic of a declared subscription, and the service declares none")
	}
	if len(ctx.Subscriptions) > declaration.MaxSubscriptions {
		return nil, fmt.Errorf("kafka-consumer: while the component is alpha a service has at most %d subscription", declaration.MaxSubscriptions)
	}
	if ctx.PeerNamespaces["outbox"] == "" || ctx.PeerNamespaces["worker"] == "" {
		return nil, errors.New("kafka-consumer: the inbox writes each record into the outbox for a consuming worker, so the service needs the outbox and the worker components (ST-SUP-RC38-OC01)")
	}
	s := ctx.Subscriptions[0]
	var consumers []string
	for _, w := range ctx.Workers {
		if destination, ok := strings.CutPrefix(w.Trigger, "consumes "); ok && destination == s.Name {
			consumers = append(consumers, w.Name)
		}
	}
	if len(consumers) != 1 {
		return nil, fmt.Errorf("kafka-consumer: the subscription %s has %d workers with the trigger consumes %s; it needs exactly one (ST-DECL-INV98)", s.Name, len(consumers), s.Name)
	}
	return &subscription{Package: path.Base(ctx.OutputNamespace), Service: ctx.ServiceName, Name: s.Name, Source: s.Source,
		Worker: consumers[0], Events: append([]string(nil), s.Events...)}, nil
}

// Generate emits the declaration of the subscription, the constructor of its inbox and the migration text of the
// refused records (ST-SUP-RC38-OC01, ST-SUP-RC38-OC02).
func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	s, err := plan(ctx)
	if err != nil {
		return nil, nil, err
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Package %s holds the declared subscription of the service and the constructor of its Kafka inbox, which the\n// worker mode runs (ST-SUP-RC38-OC01).\npackage %s\n\n", s.Package, s.Package)
	b.WriteString("import (\n\t\"github.com/jsell-rh/stego/registry/runtime/outbox/kafkainbox\"\n\t\"github.com/jsell-rh/stego/registry/runtime/service\"\n)\n\n")
	b.WriteString("// Subscription is the declared subscription: its name is the destination that the worker consumes and the stager\n// of each message, and the worker's service account is the staging principal of each message (ST-SUP-RP10-LC16).\n")
	fmt.Fprintf(&b, "var Subscription = kafkainbox.Subscription{Service: %q, Name: %q, Source: %q, Worker: %q,\n\tEvents: []string{", s.Service, s.Name, s.Source, s.Worker)
	for i, kind := range s.Events {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q", kind)
	}
	b.WriteString("}}\n\n")
	b.WriteString("// Inbox gives the Kafka inbox of the subscription with the settings of the process environment, for the worker\n// mode (ST-SUP-RP01-LC38). A setting that it refuses names the variable and no value.\n")
	b.WriteString("func Inbox(env service.InboxEnv) (*kafkainbox.Inbox, error) {\n\tsettings, err := kafkainbox.ReadSettings(Subscription.Service, Subscription.Name)\n\tif err != nil {\n\t\treturn nil, err\n\t}\n")
	b.WriteString("\treturn kafkainbox.New(kafkainbox.Config{Subscription: Subscription, Settings: settings, Queue: env.Queue, DB: env.DB,\n\t\tTenants: env.Tenants, TenantSchema: env.TenantSchema, Retention: env.Retention, Logs: env.Logs, Meter: env.Meter})\n}\n\n")
	b.WriteString("// Migration gives the migration text of the table of the refused records for the worker role (ST-SUP-RP01-LC37).\nfunc Migration(workerRole string) (string, error) { return kafkainbox.Migration(workerRole) }\n")
	source, err := format.Source(b.Bytes())
	if err != nil {
		return nil, nil, fmt.Errorf("kafka-consumer: format the subscription file: %w", err)
	}
	return []gen.File{{Path: path.Join(ctx.OutputNamespace, file), Content: source}}, &gen.Wiring{}, nil
}
