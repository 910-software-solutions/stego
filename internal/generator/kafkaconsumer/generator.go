// Package kafkaconsumer is the generator of the kafka-consumer component (ST-SUP-RC38). For each declared
// subscription, in the order of the declaration, it emits the declaration of the subscription and the constructor of
// its Kafka inbox on the runtime library, kafkainbox of the pinned github.com/910-software-solutions/stego/registry/runtime
// (ST-SUP-RP01-LC35 to ST-SUP-RP01-LC38), and once the migration text of the table of the refused records. The worker
// mode of the service runtime runs each inbox. The checks, the inbox write, the offsets and the refusals live in the
// runtime library; the generated code holds what varies with the declaration (ST-STD-RUNTIME-R05). The orchestrator
// decided ST-SUP-Q158 under the owner's delegation, 2026-11-17, and ST-SUP-Q204 (a consumer group and the settings
// of each subscription by its name), 2026-11-21.
package kafkaconsumer

import (
	"bytes"
	"errors"
	"fmt"
	"go/format"
	"path"
	"strings"

	"github.com/910-software-solutions/stego/internal/declaration"
	"github.com/910-software-solutions/stego/internal/gen"
)

// Generator is the generator of the kafka-consumer component.
type Generator struct{}

// file is the one Go file that the component emits.
const file = "subscriptions.go"

// MinimumGoVersion is the Go version of the runtime library that the emitted code imports.
func (*Generator) MinimumGoVersion() string { return "1.26.11" }

// subscription is what the emitted file holds of one declared subscription.
type subscription struct {
	Name, Source, Worker string
	Events               []string
}

// plan is what the emitted file holds: the package, the service and each declared subscription.
type plan struct {
	Package, Service string
	Subscriptions    []subscription
}

// readPlan reads each subscription and the worker that consumes it, and refuses a context that the emitted code or
// the runtime would refuse (ST-SUP-RC38-OC01): a setting of the component, no subscription or more than
// declaration.MaxSubscriptions, a service with no outbox or no worker component, and a subscription that not exactly
// one worker consumes.
func readPlan(ctx gen.Context) (*plan, error) {
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
		return nil, fmt.Errorf("kafka-consumer: a service has at most %d subscriptions, and the declaration has %d", declaration.MaxSubscriptions, len(ctx.Subscriptions))
	}
	if ctx.PeerNamespaces["outbox"] == "" || ctx.PeerNamespaces["worker"] == "" {
		return nil, errors.New("kafka-consumer: the inbox writes each record into the outbox for a consuming worker, so the service needs the outbox and the worker components (ST-SUP-RC38-OC01)")
	}
	p := &plan{Package: path.Base(ctx.OutputNamespace), Service: ctx.ServiceName}
	for _, s := range ctx.Subscriptions {
		var consumers []string
		for _, w := range ctx.Workers {
			if destination, ok := strings.CutPrefix(w.Trigger, "consumes "); ok && destination == s.Name {
				consumers = append(consumers, w.Name)
			}
		}
		if len(consumers) != 1 {
			return nil, fmt.Errorf("kafka-consumer: the subscription %s has %d workers with the trigger consumes %s; it needs exactly one (ST-DECL-INV98)", s.Name, len(consumers), s.Name)
		}
		p.Subscriptions = append(p.Subscriptions, subscription{Name: s.Name, Source: s.Source, Worker: consumers[0], Events: append([]string(nil), s.Events...)})
	}
	return p, nil
}

// ValidateContext checks the context as Generate does.
func (*Generator) ValidateContext(ctx gen.Context) error {
	_, err := readPlan(ctx)
	return err
}

// Generate emits the declaration of each subscription, the constructor of its inbox and the migration text of the
// refused records (ST-SUP-RC38-OC01, ST-SUP-RC38-OC02).
func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	p, err := readPlan(ctx)
	if err != nil {
		return nil, nil, err
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Package %s holds the declared subscriptions of the service and the constructor of the Kafka inbox of each, which\n// the worker mode runs (ST-SUP-RC38-OC01).\npackage %s\n\n", p.Package, p.Package)
	b.WriteString("import (\n\t\"errors\"\n\n\t\"github.com/910-software-solutions/stego/registry/runtime/outbox/kafkainbox\"\n\t\"github.com/910-software-solutions/stego/registry/runtime/service\"\n)\n\n")
	b.WriteString("// Subscriptions are the declared subscriptions, in the order of the declaration: the name of each is the destination\n// that its worker consumes and the stager of each message, and the worker's service account is the staging principal\n// of each message (ST-SUP-RP10-LC16). Each reads its topic in a consumer group of its own, with the settings of its\n// name (ST-SUP-RP01-LC38).\n")
	b.WriteString("var Subscriptions = []kafkainbox.Subscription{\n")
	for _, s := range p.Subscriptions {
		fmt.Fprintf(&b, "\t{Service: %q, Name: %q, Source: %q, Worker: %q,\n\t\tEvents: []string{", p.Service, s.Name, s.Source, s.Worker)
		for i, kind := range s.Events {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%q", kind)
		}
		b.WriteString("}},\n")
	}
	b.WriteString("}\n\n")
	b.WriteString("// names are the names of the subscriptions, which name their settings.\nfunc names() []string {\n\tout := make([]string, len(Subscriptions))\n\tfor i, s := range Subscriptions {\n\t\tout[i] = s.Name\n\t}\n\treturn out\n}\n\n")
	b.WriteString("// CheckSettings gives the setting and the problem when the process environment gives no topic of a subscription,\n// one consumer group to two subscriptions or a setting of kafka-consumer before 0.5.0, for the start of the worker\n// mode, and \"\" when it is sound (ST-SUP-RP14-LC08). It holds no value.\n")
	b.WriteString("func CheckSettings() (variable, problem string) {\n\tvar setting *kafkainbox.SettingError\n\tif errors.As(kafkainbox.CheckSettings(Subscriptions[0].Service, names()), &setting) {\n\t\treturn setting.Variable, setting.Problem\n\t}\n\treturn \"\", \"\"\n}\n\n")
	b.WriteString("// Inboxes gives the Kafka inbox of each subscription with the settings of the process environment, for the worker\n// mode (ST-SUP-RP01-LC38). A setting that it refuses names the variable and no value.\n")
	b.WriteString("func Inboxes(env service.InboxEnv) ([]*kafkainbox.Inbox, error) {\n\tinboxes := make([]*kafkainbox.Inbox, 0, len(Subscriptions))\n\tfor _, s := range Subscriptions {\n\t\tsettings, err := kafkainbox.ReadSettings(s.Service, s.Name)\n\t\tif err != nil {\n\t\t\treturn nil, err\n\t\t}\n")
	b.WriteString("\t\tinbox, err := kafkainbox.New(kafkainbox.Config{Subscription: s, Settings: settings, Queue: env.Queue, DB: env.DB,\n\t\t\tTenants: env.Tenants, TenantSchema: env.TenantSchema, Retention: env.Retention, Logs: env.Logs, Meter: env.Meter})\n\t\tif err != nil {\n\t\t\treturn nil, err\n\t\t}\n\t\tinboxes = append(inboxes, inbox)\n\t}\n\treturn inboxes, nil\n}\n\n")
	b.WriteString("// Migration gives the migration text of the table of the refused records for the worker role (ST-SUP-RP01-LC37).\nfunc Migration(workerRole string) (string, error) { return kafkainbox.Migration(workerRole) }\n")
	source, err := format.Source(b.Bytes())
	if err != nil {
		return nil, nil, fmt.Errorf("kafka-consumer: format the subscription file: %w", err)
	}
	return []gen.File{{Path: path.Join(ctx.OutputNamespace, file), Content: source}}, &gen.Wiring{}, nil
}
