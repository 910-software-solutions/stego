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

// allowedPackages are the standard packages whose objects the observation helpers may use: the packages that they
// use today, none of which can open a connection, resolve a name or start a process. A use of an object of any other
// package fails the walk, so a new import is refused until a review adds it here (security review of PR #509).
var allowedPackages = map[string]bool{"bytes": true, "crypto/x509": true, "encoding/base64": true, "encoding/json": true, "encoding/pem": true,
	"errors": true, "strings": true}

// pureNetwork names the objects of the network packages net and crypto/tls that only parse or hold a value and make
// no call: the parse of an IP address and the key pair of a TLS Secret. A field or method of the types named here is
// allowed too.
var pureNetwork = map[string]bool{"net.ParseIP": true, "net.IP": true, "crypto/tls.X509KeyPair": true, "crypto/tls.Certificate": true}

// pure tells whether the object of a package that is not allowed is one of pureNetwork, or a field or method of a
// type of it.
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

// allowedObject tells whether a use of the object of another package is allowed: an object of an allowed package,
// or one of pureNetwork.
func allowedObject(object types.Object) bool {
	return allowedPackages[object.Pkg().Path()] || pure(object)
}

// checked is one package that the walk reads, type-checked from the export data of the build.
type checked struct {
	pkg   *types.Package
	info  *types.Info
	decls map[types.Object]*ast.FuncDecl
	// values maps each package variable to the declaration that gives its value.
	values map[types.Object]*ast.ValueSpec
	// inits are the init functions, which run when the package loads, so the walk starts at each of them too.
	inits []*ast.FuncDecl
	// bodies are the bodies of each function and method of the package, init functions included, with the object of
	// the function (nil for an init function), so that the walk finds the writers of a package variable.
	bodies []body
}

// body is the body of one function or method of the package.
type body struct {
	object types.Object
	node   *ast.BlockStmt
}

// generated is the generated package of this test, type-checked once.
var generated struct {
	once sync.Once
	err  error
	pkg  *checked
}

// loadGenerated type-checks the generated package of this test once.
func loadGenerated() (*checked, error) {
	generated.once.Do(func() { generated.pkg, generated.err = loadPackage(nil) })
	return generated.pkg, generated.err
}

// loadPackage type-checks the files of this package that are not tests, and each planted file of extra (name to
// source), with the export data that go list -export gives for each import, and maps each function, method and
// package variable that the package declares to its declaration.
func loadPackage(extra map[string]string) (*checked, error) {
	fset := token.NewFileSet()
	names, err := filepath.Glob("*.go")
	if err != nil {
		return nil, err
	}
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	listed := []string{"."}
	for name, source := range extra {
		file, err := parser.ParseFile(fset, name, source, 0)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
		for _, spec := range file.Imports {
			listed = append(listed, strings.Trim(spec.Path.Value, `"`))
		}
	}
	out, err := exec.Command("go", append([]string{"list", "-export", "-deps", "-f", "{{if .Export}}{{.ImportPath}}={{.Export}}{{end}}"}, listed...)...).Output()
	if err != nil {
		return nil, fmt.Errorf("go list -export: %w", err)
	}
	exports := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if path, file, ok := strings.Cut(line, "="); ok {
			exports[path] = file
		}
	}
	lookup := func(path string) (io.ReadCloser, error) {
		file, ok := exports[path]
		if !ok {
			return nil, fmt.Errorf("no export data of %s", path)
		}
		return os.Open(file)
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}, Types: map[ast.Expr]types.TypeAndValue{}}
	pkg, err := (&types.Config{Importer: importer.ForCompiler(fset, "gc", lookup)}).Check("generated", fset, files, info)
	if err != nil {
		return nil, err
	}
	c := &checked{pkg: pkg, info: info, decls: map[types.Object]*ast.FuncDecl{}, values: map[types.Object]*ast.ValueSpec{}}
	for _, file := range files {
		for _, declaration := range file.Decls {
			switch d := declaration.(type) {
			case *ast.FuncDecl:
				if d.Body == nil {
					continue
				}
				if d.Recv == nil && d.Name.Name == "init" {
					c.inits = append(c.inits, d)
					c.bodies = append(c.bodies, body{node: d.Body})
					continue
				}
				c.decls[info.Defs[d.Name]] = d
				c.bodies = append(c.bodies, body{object: info.Defs[d.Name], node: d.Body})
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if value, ok := spec.(*ast.ValueSpec); ok {
						for _, name := range value.Names {
							c.values[info.Defs[name]] = value
						}
					}
				}
			}
		}
	}
	return c, nil
}

// staticNoNetwork fails the test when the code that the named functions reach can make a network call (reach).
func staticNoNetwork(t *testing.T, roots ...string) {
	t.Helper()
	c, err := loadGenerated()
	if err != nil {
		t.Fatalf("the generated package does not type-check: %v", err)
	}
	faults, err := reach(c, roots...)
	if err != nil {
		t.Fatal(err)
	}
	if len(faults) > 0 {
		t.Fatalf("the code that %v reaches can make a network call: %v", roots, faults)
	}
}

// reach walks the static call graph of the package from the named functions and from each init function: each
// function or method of the package that a reachable body names, as a call or as a value, the declaration of each package variable or constant that it reads, and each function that
// writes a package variable that it reads. It gives a fault for a use of an object of a package that is not
// allowed (allowedObject), a call of an interface method other than error.Error, a go statement, a package
// variable whose value can hold a func, an interface, a map, a pointer, a slice or a chan and that is written outside
// its declaration, and a use of a package variable that can hold a map, a pointer, a slice or a chan other than a
// read through it. It gives an error for a root that the package does not declare.
func reach(c *checked, roots ...string) ([]string, error) {
	pkg, info := c.pkg, c.info
	var faults []string
	seen := map[types.Object]bool{}
	var walk func(types.Object)
	var inspect func(string, ast.Node)
	inspect = func(name string, node ast.Node) {
		ast.Inspect(node, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.GoStmt:
				faults = append(faults, name+": a go statement")
			case *ast.SelectorExpr:
				if selection, ok := info.Selections[n]; ok {
					// A field of a type of a package that is not allowed, such as Certificate.Leaf, is judged by its
					// struct type.
					if field, isField := selection.Obj().(*types.Var); isField && field.Pkg() != nil && field.Pkg() != pkg && !allowedPackages[field.Pkg().Path()] {
						if named, ok := derefNamed(selection.Recv()); !ok || !pureNetwork[named.Obj().Pkg().Path()+"."+named.Obj().Name()] {
							faults = append(faults, name+": reads the field "+field.Pkg().Path()+"."+field.Name())
						}
					}
					if _, isInterface := selection.Recv().Underlying().(*types.Interface); isInterface && selection.Kind() == types.MethodVal {
						if !allowedInterfaceMethod(selection.Obj()) {
							faults = append(faults, name+": calls the interface method "+selection.Obj().Name())
						}
					}
				}
			case *ast.Ident:
				used := info.Uses[n]
				if used == nil || used.Pkg() == nil {
					return true
				}
				if used.Pkg() != pkg {
					if !allowedObject(used) {
						faults = append(faults, name+": uses "+used.Pkg().Path()+"."+used.Name())
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
	walk = func(object types.Object) {
		if seen[object] {
			return
		}
		seen[object] = true
		name := object.Name()
		// A function or method gives its body; a package variable or constant gives its declaration, so that a
		// variable that holds a network function is judged by its value, and each writer of a package variable.
		if function := c.decls[object]; function != nil {
			inspect(name, function.Body)
			return
		}
		value := c.values[object]
		if value == nil {
			faults = append(faults, name+": no body in the package")
			return
		}
		inspect(name, value)
		if _, isVar := object.(*types.Var); !isVar {
			return
		}
		reference, alias := holdsReference(object.Type(), map[types.Type]bool{})
		for _, b := range c.bodies {
			writer := "init"
			if b.object != nil {
				writer = b.object.Name()
			}
			writes, escapes := uses(info, b.node, object)
			if writes {
				if reference {
					faults = append(faults, name+": a package variable that can hold a func, an interface, a map, a pointer, a slice or a chan is written outside its declaration, in "+writer)
				}
				if b.object != nil {
					walk(b.object)
				}
			}
			if escapes && alias {
				faults = append(faults, name+": a package variable that can hold a map, a pointer, a slice or a chan is used other than by a read through it, in "+writer)
			}
		}
	}
	for _, d := range c.inits {
		inspect("init", d.Body)
	}
	for _, root := range roots {
		object := pkg.Scope().Lookup(root)
		if _, ok := object.(*types.Func); !ok {
			return nil, fmt.Errorf("the generated package declares no function %s", root)
		}
		walk(object)
	}
	return faults, nil
}

// holdsReference tells whether a value of the type can hold a func, an interface, a map, a pointer, a slice or a chan
// (reference), and whether it can hold a map, a pointer, a slice or a chan, through which a copy can change it
// (alias).
func holdsReference(typ types.Type, seen map[types.Type]bool) (reference, alias bool) {
	if seen[typ] {
		return false, false
	}
	seen[typ] = true
	switch u := typ.Underlying().(type) {
	case *types.Signature, *types.Interface:
		return true, false
	case *types.Map, *types.Pointer, *types.Slice, *types.Chan:
		return true, true
	case *types.Array:
		return holdsReference(u.Elem(), seen)
	case *types.Struct:
		for field := range u.Fields() {
			r, a := holdsReference(field.Type(), seen)
			reference, alias = reference || r, alias || a
		}
	}
	return reference, alias
}

// uses tells whether the body writes the package variable object (an assignment, an increment or a range whose
// target is the variable or a part of it, or the address of the variable or of a part of it), and whether it uses it
// other than by a read through it: as the operand of a selector or of an index that is not written, or of a
// comparison.
func uses(info *types.Info, node ast.Node, object types.Object) (writes, escapes bool) {
	root := func(expr ast.Expr) bool {
		for {
			switch e := expr.(type) {
			case *ast.Ident:
				return info.Uses[e] == object
			case *ast.SelectorExpr:
				expr = e.X
			case *ast.IndexExpr:
				expr = e.X
			case *ast.IndexListExpr:
				expr = e.X
			case *ast.StarExpr:
				expr = e.X
			case *ast.ParenExpr:
				expr = e.X
			case *ast.SliceExpr:
				expr = e.X
			default:
				return false
			}
		}
	}
	read := map[*ast.Ident]bool{}
	ast.Inspect(node, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			for _, lhs := range s.Lhs {
				writes = writes || root(lhs)
			}
		case *ast.IncDecStmt:
			writes = writes || root(s.X)
		case *ast.RangeStmt:
			writes = writes || s.Key != nil && root(s.Key) || s.Value != nil && root(s.Value)
		case *ast.UnaryExpr:
			if s.Op == token.AND {
				writes = writes || root(s.X)
			}
		case *ast.SelectorExpr:
			if id, ok := s.X.(*ast.Ident); ok {
				read[id] = true
			}
		case *ast.IndexExpr:
			if id, ok := s.X.(*ast.Ident); ok {
				read[id] = true
			}
		case *ast.BinaryExpr:
			if s.Op == token.EQL || s.Op == token.NEQ {
				for _, operand := range []ast.Expr{s.X, s.Y} {
					if id, ok := operand.(*ast.Ident); ok {
						read[id] = true
					}
				}
			}
		}
		return true
	})
	ast.Inspect(node, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && info.Uses[id] == object && !read[id] {
			escapes = true
		}
		return true
	})
	return writes, escapes
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
	t.Run("ST-SUP-RC14-OC24-T04 The static walk refuses a dialer that an init function or another writer stores, and each network or process package that the helpers do not use", func(t *testing.T) {
		// Each plant is a file of this package with the root PlantedCheck. Under the walk of PR #509 (a refused list,
		// with no init function and no writer of a package variable walked), each one but the control passed.
		plants := []struct{ name, source, fault string }{
			{"an init function that stores a dialer in a func variable",
				"package kubernetes\nimport \"net\"\nvar plantedDial = func(string) error { return nil }\nfunc init() { plantedDial = func(address string) error { conn, err := net.Dial(\"tcp\", address); if err == nil { _ = conn.Close() }; return err } }\nfunc PlantedCheck() bool { return plantedDial(\"192.0.2.1:80\") == nil }\n",
				"uses net.Dial"},
			{"an init function that stores a connection in an io.Writer, then io.Copy",
				"package kubernetes\nimport (\"io\"; \"net\"; \"strings\")\nvar plantedWriter io.Writer = io.Discard\nfunc init() { if conn, err := net.Dial(\"tcp\", \"192.0.2.1:80\"); err == nil { plantedWriter = conn } }\nfunc PlantedCheck() bool { _, err := io.Copy(plantedWriter, strings.NewReader(\"x\")); return err == nil }\n",
				"is written outside its declaration, in init"},
			{"a function that the helper does not call and that stores a dialer in a func variable",
				"package kubernetes\nimport \"net\"\nvar plantedHook = func() error { return nil }\nfunc plantedSetup() { plantedHook = func() error { _, err := net.Dial(\"tcp\", \"192.0.2.1:80\"); return err } }\nfunc PlantedCheck() bool { return plantedHook() == nil }\n",
				"plantedSetup: uses net.Dial"},
			{"textproto.Dial",
				"package kubernetes\nimport \"net/textproto\"\nfunc PlantedCheck() bool { conn, err := textproto.Dial(\"tcp\", \"192.0.2.1:25\"); if err == nil { _ = conn.Close() }; return err == nil }\n",
				"uses net/textproto.Dial"},
			{"syslog.Dial",
				"package kubernetes\nimport \"log/syslog\"\nfunc PlantedCheck() bool { writer, err := syslog.Dial(\"tcp\", \"192.0.2.1:514\", syslog.LOG_INFO, \"observation\"); if err == nil { _ = writer.Close() }; return err == nil }\n",
				"uses log/syslog.Dial"},
			{"os.StartProcess",
				"package kubernetes\nimport \"os\"\nfunc PlantedCheck() bool { process, err := os.StartProcess(\"/bin/true\", nil, &os.ProcAttr{}); if err == nil { _, _ = process.Wait() }; return err == nil }\n",
				"uses os.StartProcess"},
			{"a map variable that a helper reads and another function fills",
				"package kubernetes\nvar plantedTable = map[string]string{}\nfunc plantedFill(table map[string]string) { table[\"a\"] = \"b\" }\nfunc plantedShare() { plantedFill(plantedTable) }\nfunc PlantedCheck() bool { return plantedTable[\"a\"] == \"b\" }\n",
				"is used other than by a read through it, in plantedShare"},
			{"the control: a check that uses only an allowed package", "package kubernetes\nimport \"strings\"\nvar plantedPrefix = \"tenant-\"\nfunc PlantedCheck() bool { return strings.HasPrefix(\"tenant-1\", plantedPrefix) }\n", ""},
		}
		for _, plant := range plants {
			c, err := loadPackage(map[string]string{"planted.go": plant.source})
			if err != nil {
				t.Fatalf("%s: the package with the plant does not type-check: %v", plant.name, err)
			}
			faults, err := reach(c, "PlantedCheck")
			if err != nil {
				t.Fatal(err)
			}
			if plant.fault == "" {
				if len(faults) != 0 {
					t.Errorf("%s: the walk gave the faults %v, want none", plant.name, faults)
				}
				continue
			}
			found := false
			for _, fault := range faults {
				found = found || strings.Contains(fault, plant.fault)
			}
			if !found {
				t.Errorf("%s: the walk gave the faults %v, want one that holds %q", plant.name, faults, plant.fault)
			}
		}
	})
}
