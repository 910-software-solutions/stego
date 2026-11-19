// Package httpclient renders the common HTTPS client for service and CLI code.
package httpclient

import (
	"bytes"
	_ "embed"
	"fmt"
	"github.com/jsell-rh/stego/internal/gen"
	"go/format"
	"path"
	"text/template"
)

//go:embed client.go.tmpl
var source string

//go:embed local.go.tmpl
var localSource string

// LinkPolicy says whether the emitted client may read a credential or CA file
// through a leaf symbolic link.
type LinkPolicy bool

// FollowLinks keeps the established behavior. A Kubernetes projected Secret
// mount is a symbolic link into "..data", so a service must follow the link.
const FollowLinks LinkPolicy = false

// RefuseLinks makes the emitted client refuse a leaf symbolic link. A command
// line program that reads a caller-supplied path asks for this.
const RefuseLinks LinkPolicy = true

func Render(namespace, tracing string, links LinkPolicy) (gen.File, error) {
	return render(namespace, tracing, 0, links, false, false)
}

// RenderProxy renders the client of an API proxy, which sends the Idempotency-Key header that the browser gives for
// a command. The other clients refuse that header, except the clients of RenderCLI and RenderSDK: they do not retry,
// so they cannot keep its promise. A proxy does not retry either; the browser that gave the key retries with it. A port that is not
// 0 adds the client of an application in the same network namespace, as RenderLocalApplication does.
func RenderProxy(namespace, tracing string, port int, links LinkPolicy) (gen.File, error) {
	if port != 0 && (port < 1024 || port > 65535) {
		return gen.File{}, fmt.Errorf("local application port must be 1024 through 65535")
	}
	return render(namespace, tracing, port, links, true, false)
}

// RenderCLI renders the client of the generated CLI, which sends the Idempotency-Key header that the CLI makes for
// each write. The CLI sends a write again only with the same key (ST-SUP-RC04-OC31), so it keeps the promise of the
// key. The client refuses the key on a request that is not a POST or a PATCH, as the service ignores it there
// (security review of PR 447, I-3). The CLI reads its credential and CA files from paths that its user gives, so the
// client refuses a leaf symbolic link.
func RenderCLI(namespace, tracing string) (gen.File, error) {
	return render(namespace, tracing, 0, RefuseLinks, true, true)
}

// RenderSDK renders the client of the generated Go SDK, which sends the Idempotency-Key header that the caller gives
// as a parameter of a command or of a create: the route requires it (ST-SUP-RC20-OC53, ST-SUP-RC30-OC01), and the
// caller, not the SDK, sends a write again with the same key, as the browser does through a proxy. The SDK itself
// sends no request again (ST-SUP-RC06-OC02). As the client of the CLI, it refuses the key on a request that is not a
// POST or a PATCH. It keeps the link behavior of Render.
func RenderSDK(namespace, tracing string) (gen.File, error) {
	return render(namespace, tracing, 0, FollowLinks, true, true)
}

// RenderLocalApplication adds a client for an application in the same network
// namespace. The compiler fixes its unprivileged port. The application must
// listen only on 127.0.0.1. New still requires verified HTTPS.
func RenderLocalApplication(namespace, tracing string, port int, links LinkPolicy) (gen.File, error) {
	if port < 1024 || port > 65535 {
		return gen.File{}, fmt.Errorf("local application port must be 1024 through 65535")
	}
	return render(namespace, tracing, port, links, false, false)
}

// keyHeader lets the client send the Idempotency-Key header, and keyWritesOnly lets it send the header only on a POST
// and a PATCH.
func render(namespace, tracing string, port int, links LinkPolicy, keyHeader, keyWritesOnly bool) (gen.File, error) {
	if err := gen.ValidatePath(namespace); err != nil {
		return gen.File{}, err
	}
	if tracing != "" {
		if err := gen.ValidateGoImportNamespace(tracing); err != nil {
			return gen.File{}, err
		}
	}
	input := source
	if port != 0 {
		input += localSource
	}
	t, err := template.New("client").Parse(input)
	if err != nil {
		return gen.File{}, err
	}
	var output bytes.Buffer
	data := struct {
		Package, Tracing, LocalAddress        string
		RefuseLinks, KeyHeader, KeyWritesOnly bool
	}{path.Base(namespace), tracing, fmt.Sprintf("127.0.0.1:%d", port), bool(links), keyHeader, keyWritesOnly}
	if err = t.Execute(&output, data); err != nil {
		return gen.File{}, err
	}
	code, err := format.Source(output.Bytes())
	return gen.File{Path: path.Join(namespace, "client.go"), Content: code}, err
}
