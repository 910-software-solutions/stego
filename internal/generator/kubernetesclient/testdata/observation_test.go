package kubernetes

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// noNetwork replaces net.DefaultResolver, http.DefaultTransport and the transport of http.DefaultClient with ones that
// count each call and refuse it, for the time of the test, and gives the count. Go has no default dialer of the
// process, so a dial of its own or a resolver of its own is not seen here; staticNoNetwork refuses those. No test of
// this package may call t.Parallel while the swap holds.
func noNetwork(t *testing.T) *atomic.Int64 {
	t.Helper()
	var calls atomic.Int64
	refuse := func(context.Context, string, string) (net.Conn, error) {
		calls.Add(1)
		return nil, errors.New("the observation helper made a network call")
	}
	resolver, transport, client := net.DefaultResolver, http.DefaultTransport, http.DefaultClient.Transport
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: refuse}
	http.DefaultTransport = roundTrip(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("the observation helper sent an HTTP request")
	})
	http.DefaultClient.Transport = http.DefaultTransport
	t.Cleanup(func() {
		net.DefaultResolver, http.DefaultTransport, http.DefaultClient.Transport = resolver, transport, client
	})
	return &calls
}

// networkPackages are the packages whose functions, methods, types or values can open a connection, resolve a name
// or start a process that does. The observation helpers may use none of them.
var networkPackages = map[string]bool{"net": true, "net/http": true, "net/http/httptrace": true, "net/rpc": true, "net/smtp": true,
	"crypto/tls": true, "os/exec": true, "syscall": true, "plugin": true, "unsafe": true, "reflect": true}

// pureNetwork names the objects of networkPackages that only parse or hold a value and make no call: the parse of an
// IP address and the key pair of a TLS Secret. A field or method of the types named here is allowed too.
var pureNetwork = map[string]bool{"net.ParseIP": true, "net.IP": true, "crypto/tls.X509KeyPair": true, "crypto/tls.Certificate": true}

// pure tells whether the object of a network package is one of pureNetwork, or a field or method of a type of it.
func pure(object types.Object) bool {
	if pureNetwork[object.Pkg().Path()+"."+object.Name()] {
		return true
	}
	var receiver types.Type
	switch o := object.(type) {
	case *types.Func:
		if signature, ok := o.Type().(*types.Signature); ok && signature.Recv() != nil {
			receiver = signature.Recv().Type()
		}
	case *types.Var:
		if o.IsField() {
			return false
		}
	}
	if pointer, ok := receiver.(*types.Pointer); ok {
		receiver = pointer.Elem()
	}
	named, ok := receiver.(*types.Named)
	return ok && pureNetwork[named.Obj().Pkg().Path()+"."+named.Obj().Name()]
}

// generated holds the generated package of this test, type-checked once from the export data of the build.
var generated struct {
	once  sync.Once
	err   error
	pkg   *types.Package
	info  *types.Info
	decls map[types.Object]*ast.FuncDecl
	// values maps each package variable to the declaration that gives its value.
	values map[types.Object]ast.Node
}

// loadGenerated type-checks the files of this package that are not tests, with the export data that go list -export
// gives for each import, and maps each function and method that the package declares to its declaration.
func loadGenerated() error {
	generated.once.Do(func() {
		out, err := exec.Command("go", "list", "-export", "-deps", "-f", "{{if .Export}}{{.ImportPath}}={{.Export}}{{end}}", ".").Output()
		if err != nil {
			generated.err = fmt.Errorf("go list -export: %w", err)
			return
		}
		exports := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if path, file, ok := strings.Cut(line, "="); ok {
				exports[path] = file
			}
		}
		fset := token.NewFileSet()
		lookup := func(path string) (io.ReadCloser, error) {
			file, ok := exports[path]
			if !ok {
				return nil, fmt.Errorf("no export data of %s", path)
			}
			return os.Open(file)
		}
		names, err := filepath.Glob("*.go")
		if err != nil {
			generated.err = err
			return
		}
		var files []*ast.File
		for _, name := range names {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fset, name, nil, 0)
			if err != nil {
				generated.err = err
				return
			}
			files = append(files, file)
		}
		info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}, Types: map[ast.Expr]types.TypeAndValue{}}
		pkg, err := (&types.Config{Importer: importer.ForCompiler(fset, "gc", lookup)}).Check("generated", fset, files, info)
		if err != nil {
			generated.err = err
			return
		}
		decls, values := map[types.Object]*ast.FuncDecl{}, map[types.Object]ast.Node{}
		for _, file := range files {
			for _, declaration := range file.Decls {
				switch d := declaration.(type) {
				case *ast.FuncDecl:
					if d.Body != nil {
						decls[info.Defs[d.Name]] = d
					}
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						if value, ok := spec.(*ast.ValueSpec); ok {
							for _, name := range value.Names {
								values[info.Defs[name]] = value
							}
						}
					}
				}
			}
		}
		generated.pkg, generated.info, generated.decls, generated.values = pkg, info, decls, values
	})
	return generated.err
}

// staticNoNetwork walks the static call graph of the package from the named functions: each function or method of
// the package that a reachable body names, as a call or as a value, and the declaration of each package variable or
// constant that it reads. It fails the test on a use of an object of
// networkPackages, of a package of the module outside this one, a call of an interface method, or a go statement
// in a reachable body, and on a root that the package does not declare.
func staticNoNetwork(t *testing.T, roots ...string) {
	t.Helper()
	if err := loadGenerated(); err != nil {
		t.Fatalf("the generated package does not type-check: %v", err)
	}
	pkg, info := generated.pkg, generated.info
	var faults []string
	seen := map[types.Object]bool{}
	var walk func(types.Object)
	walk = func(object types.Object) {
		if seen[object] {
			return
		}
		seen[object] = true
		// A function or method gives its body; a package variable or constant gives its declaration, so that a
		// variable that holds a network function is judged by its value.
		var body ast.Node
		if function := generated.decls[object]; function != nil {
			body = function.Body
		} else if value := generated.values[object]; value != nil {
			body = value
		} else {
			faults = append(faults, object.Name()+": no body in the package")
			return
		}
		ast.Inspect(body, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.GoStmt:
				faults = append(faults, object.Name()+": a go statement")
			case *ast.SelectorExpr:
				if selection, ok := info.Selections[n]; ok {
					// A field of a type of a network package, such as Certificate.Leaf, is judged by its struct type.
					if field, isField := selection.Obj().(*types.Var); isField && field.Pkg() != nil && networkPackages[field.Pkg().Path()] {
						if named, ok := derefNamed(selection.Recv()); !ok || !pureNetwork[named.Obj().Pkg().Path()+"."+named.Obj().Name()] {
							faults = append(faults, object.Name()+": reads the field "+field.Pkg().Path()+"."+field.Name())
						}
					}
					if _, isInterface := selection.Recv().Underlying().(*types.Interface); isInterface && selection.Kind() == types.MethodVal {
						if !allowedInterfaceMethod(selection.Obj()) {
							faults = append(faults, object.Name()+": calls the interface method "+selection.Obj().Name())
						}
					}
				}
			case *ast.Ident:
				used := info.Uses[n]
				if used == nil || used.Pkg() == nil {
					return true
				}
				if used.Pkg() != pkg {
					if (networkPackages[used.Pkg().Path()] && !pure(used)) || !isStandard(used.Pkg().Path()) {
						faults = append(faults, object.Name()+": uses "+used.Pkg().Path()+"."+used.Name())
					}
					return true
				}
				switch u := used.(type) {
				case *types.Func:
					walk(u.Origin())
				case *types.Var, *types.Const:
					if u.Parent() == pkg.Scope() {
						walk(u)
					}
				}
			}
			return true
		})
	}
	for _, root := range roots {
		object := pkg.Scope().Lookup(root)
		if _, ok := object.(*types.Func); !ok {
			t.Fatalf("the generated package declares no function %s", root)
		}
		walk(object)
	}
	if len(faults) > 0 {
		t.Fatalf("the code that %v reaches can make a network call: %v", roots, faults)
	}
}

// derefNamed gives the named type of a type or of a pointer to it.
func derefNamed(typ types.Type) (*types.Named, bool) {
	if pointer, ok := typ.(*types.Pointer); ok {
		typ = pointer.Elem()
	}
	named, ok := typ.(*types.Named)
	return named, ok
}

// allowedInterfaceMethod tells whether a call of the interface method is one that the helpers may make: the method
// Error of error, which reads a value and makes no call.
func allowedInterfaceMethod(method types.Object) bool {
	return method.Name() == "Error" && method.Pkg() == nil
}

// isStandard tells whether the import path is of the standard library: its first element has no dot.
func isStandard(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// revocableSecret gives the root and a TLS Secret whose leaf names an OCSP server and a CRL distribution point, so
// that a check that fetches either makes a network call.
func revocableSecret(t *testing.T, host string, usage x509.ExtKeyUsage) (*x509.CertPool, Object) {
	t.Helper()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "observation issuer"}, IsCA: true, BasicConstraintsValid: true,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{host}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
		OCSPServer: []string{"http://ocsp.observation.invalid/"}, CRLDistributionPoints: []string{"http://crl.observation.invalid/root.crl"}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, root, &key.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}))
	secret := Object{"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/tls",
		"metadata": Object{"name": "server-tls", "namespace": "widget", "uid": "secret-one", "resourceVersion": "4", "labels": Object{"test.example/owner": "widget-one"}},
		"data": Object{"tls.crt": base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})),
			"tls.key": base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))}}
	return roots, secret
}

func TestObservationHelpersMakeNoNetworkCall(t *testing.T) {
	t.Run("ST-SUP-RC14-OC24-T01 The availability check makes no network call", func(t *testing.T) {
		staticNoNetwork(t, "DeploymentAvailable")
		calls := noNetwork(t)
		owner := Owner{"example.com/owner": "tenant-1"}
		available := availableDeployment(t)
		unavailable := availableDeployment(t)
		delete(unavailable["status"].(map[string]any), "availableReplicas")
		if ready, err := DeploymentAvailable(available, owner, 1); err != nil || !ready {
			t.Fatalf("the available Deployment: %v %v", ready, err)
		}
		if ready, err := DeploymentAvailable(unavailable, owner, 1); err != nil || ready {
			t.Fatalf("the unavailable Deployment: %v %v", ready, err)
		}
		if _, err := DeploymentAvailable(Object{"kind": "Deployment", "status": "broken"}, owner, 1); err == nil {
			t.Fatal("the malformed Deployment was accepted")
		}
		if n := calls.Load(); n != 0 {
			t.Fatalf("the availability check made %d network calls", n)
		}
	})
	t.Run("ST-SUP-RC14-OC24-T02 The Route admission check makes no network call", func(t *testing.T) {
		staticNoNetwork(t, "PassthroughRouteAdmitted")
		calls := noNetwork(t)
		owner := Owner{"example.com/owner": "tenant-1"}
		target := PassthroughRouteTarget{Host: "service.example.test", Service: "service", Port: "grpc", Router: "default"}
		admitted := admittedRoute(t)
		if ok, err := PassthroughRouteAdmitted(admitted, owner, target); err != nil || !ok {
			t.Fatalf("the admitted Route: %v %v", ok, err)
		}
		pending := admittedRoute(t)
		routeCondition(pending)["status"] = "False"
		if ok, _ := PassthroughRouteAdmitted(pending, owner, target); ok {
			t.Fatal("the Route that is not admitted was taken as admitted")
		}
		if n := calls.Load(); n != 0 {
			t.Fatalf("the Route admission check made %d network calls", n)
		}
	})
	t.Run("ST-SUP-RC14-OC24-T03 The TLS Secret checks make no network call", func(t *testing.T) {
		staticNoNetwork(t, "VerifyServerTLSSecret", "VerifyClientTLSSecret")
		calls := noNetwork(t)
		const host = "widget.example.test"
		owner := Owner{"test.example/owner": "widget-one"}
		for _, check := range []struct {
			usage  x509.ExtKeyUsage
			verify func(Object, Owner, TLSSecretTarget) ([]byte, error)
		}{{x509.ExtKeyUsageServerAuth, VerifyServerTLSSecret}, {x509.ExtKeyUsageClientAuth, VerifyClientTLSSecret}} {
			roots, secret := revocableSecret(t, host, check.usage)
			target := ServerTLSSecretTarget{Namespace: "widget", Name: "server-tls", DNSName: host, Roots: roots}
			if _, err := check.verify(secret, owner, target); err != nil {
				t.Fatalf("the valid Secret was refused: %v", err)
			}
			secret["data"].(Object)["tls.crt"] = base64.StdEncoding.EncodeToString([]byte("not a certificate"))
			if _, err := check.verify(secret, owner, target); err == nil {
				t.Fatal("the invalid Secret was accepted")
			}
		}
		if n := calls.Load(); n != 0 {
			t.Fatalf("the TLS Secret checks made %d network calls, also to the OCSP or CRL address", n)
		}
	})
}
