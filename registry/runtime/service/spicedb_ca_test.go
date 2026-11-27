package service

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
)

// privateCA is a certificate authority that no system root holds, and a server certificate that it signs for
// 127.0.0.1, as cert-manager's CA issuer gives a SpiceDB in a cluster.
type privateCA struct {
	pem    []byte
	server tls.Certificate
}

func newPrivateCA(t *testing.T) privateCA {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "stego test SpiceDB CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "spicedb"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return privateCA{
		pem:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		server: tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: serverKey},
	}
}

// serveTLS starts a gRPC server over TLS with the certificate of the authority that serves the health service and
// records the authorization header of each call; it gives its address.
func serveTLS(t *testing.T, ca privateCA, headers chan<- string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewServerTLSFromCert(&ca.server)),
		grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			md, _ := metadata.FromIncomingContext(ctx)
			select {
			case headers <- strings.Join(md.Get("authorization"), ","):
			default:
			}
			return handler(ctx, req)
		}))
	healthpb.RegisterHealthServer(server, health.NewServer())
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener.Addr().String()
}

// clearSpiceDBSettings sets every SpiceDB setting of the test to empty, so the environment of the run decides none.
func clearSpiceDBSettings(t *testing.T) {
	t.Helper()
	for _, name := range []string{SpiceDBEndpointSetting, SpiceDBKeyFileSetting, SpiceDBInsecureLoopbackSetting, SpiceDBCAFileSetting} {
		t.Setenv(name, "")
	}
}

func TestSpiceDBCAFile(t *testing.T) {
	t.Run("ST-SUP-RP14-LC14-T01 The SpiceDB CA file adds its certificates to the system roots, and a bad one is refused by its name", func(t *testing.T) {
		ca := newPrivateCA(t)
		caFile := writeFile(t, "spicedb-ca.crt", ca.pem)
		set := func(t *testing.T, change map[string]string) {
			t.Helper()
			clearSpiceDBSettings(t)
			t.Setenv(SpiceDBEndpointSetting, "spicedb.erp.svc:50051")
			t.Setenv(SpiceDBKeyFileSetting, writeFile(t, "spicedb.key", []byte(secretValue+"-key\n")))
			for k, v := range change {
				t.Setenv(k, v)
			}
		}

		set(t, nil)
		s, err := readSpiceDB()
		if err != nil {
			t.Fatal(err)
		}
		if s.Roots != nil {
			t.Fatal("no CA file gives roots other than the system roots")
		}

		set(t, map[string]string{SpiceDBCAFileSetting: caFile})
		s, err = readSpiceDB()
		if err != nil {
			t.Fatalf("the good CA file was refused: %v", err)
		}
		leaf, err := x509.ParseCertificate(ca.server.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: s.Roots}); err != nil {
			t.Fatalf("the roots do not verify the certificate that the CA file signs: %v", err)
		}
		// The file adds to the system roots: a system root that it does not hold still verifies.
		system := x509.NewCertPool()
		other := newPrivateCA(t)
		system.AppendCertsFromPEM(other.pem)
		roots, err := spiceDBRoots(false, func() (*x509.CertPool, error) { return system, nil })
		if err != nil {
			t.Fatal(err)
		}
		otherLeaf, _ := x509.ParseCertificate(other.server.Certificate[0])
		for name, cert := range map[string]*x509.Certificate{"the system root": otherLeaf, "the CA file": leaf} {
			if _, err := cert.Verify(x509.VerifyOptions{Roots: roots}); err != nil {
				t.Errorf("the roots do not verify the certificate of %s: %v", name, err)
			}
		}
		if _, err := spiceDBRoots(false, func() (*x509.CertPool, error) { return nil, errors.New("no roots") }); err == nil {
			t.Error("a CA file was taken with no system roots")
		}

		notPEM := writeFile(t, "not-pem-"+secretValue, []byte(secretValue))
		writable := writeFile(t, "writable-ca.crt", ca.pem)
		if err := os.Chmod(writable, 0o622); err != nil {
			t.Fatal(err)
		}
		for name, change := range map[string]map[string]string{
			"a relative path":        {SpiceDBCAFileSetting: filepath.Base(caFile)},
			"a missing file":         {SpiceDBCAFileSetting: filepath.Join(t.TempDir(), "missing-"+secretValue)},
			"a folder":               {SpiceDBCAFileSetting: t.TempDir()},
			"a file group can write": {SpiceDBCAFileSetting: writable},
			"no PEM certificate":     {SpiceDBCAFileSetting: notPEM},
			"the loopback test mode": {SpiceDBCAFileSetting: caFile, SpiceDBEndpointSetting: "127.0.0.1:50051", SpiceDBInsecureLoopbackSetting: "1"},
		} {
			set(t, change)
			_, err := readSpiceDB()
			var setting *SettingError
			if !errors.As(err, &setting) || setting.Variable != SpiceDBCAFileSetting {
				t.Errorf("%s: want a refusal of %s, got %v", name, SpiceDBCAFileSetting, err)
				continue
			}
			if strings.Contains(err.Error(), secretValue) {
				t.Errorf("%s: the refusal shows the path or the content: %v", name, err)
			}
			if got, want := StartStage(err), "service.start.setting.spicedb-authz."+SpiceDBCAFileSetting; got != want {
				t.Errorf("%s: stage %q, want %q", name, got, want)
			}
		}

		// The migration job reads the same setting.
		set(t, map[string]string{SpiceDBCAFileSetting: notPEM})
		t.Setenv(MigrationDatabaseURLSetting, "postgres://owner:"+secretValue+"@db/app")
		if _, err := ReadMigrationSettings(); err == nil || !strings.Contains(err.Error(), SpiceDBCAFileSetting) {
			t.Errorf("the migration settings took a CA file with no certificate: %v", err)
		}
	})
}

func TestDialSpiceDBRoots(t *testing.T) {
	t.Run("ST-SUP-RP14-LC14-T02 The connection to SpiceDB verifies a certificate of a private authority with the CA file, and refuses it without", func(t *testing.T) {
		ca := newPrivateCA(t)
		headers := make(chan string, 4)
		endpoint := serveTLS(t, ca, headers)
		clearSpiceDBSettings(t)
		t.Setenv(SpiceDBEndpointSetting, endpoint)
		t.Setenv(SpiceDBKeyFileSetting, writeFile(t, "spicedb.key", []byte("preshared-"+secretValue+"\n")))
		check := func(s SpiceDB) error {
			conn, err := DialSpiceDB(s)
			if err != nil {
				return err
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err = healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
			return err
		}

		plain, err := readSpiceDB()
		if err != nil {
			t.Fatal(err)
		}
		if err := check(plain); err == nil || !strings.Contains(err.Error(), "certificate") {
			t.Fatalf("with the system roots alone, the certificate of a private authority was taken: %v", err)
		}
		select {
		case h := <-headers:
			t.Fatalf("a call reached the server over unverified TLS, with authorization %q", h)
		default:
		}

		t.Setenv(SpiceDBCAFileSetting, writeFile(t, "spicedb-ca.crt", ca.pem))
		trusted, err := readSpiceDB()
		if err != nil {
			t.Fatal(err)
		}
		if err := check(trusted); err != nil {
			t.Fatalf("with the CA file, the connection failed: %v", err)
		}
		if h := <-headers; h != "Bearer preshared-"+secretValue {
			t.Fatalf("the call sent the authorization %q, not the preshared key", h)
		}

		// A CA file that does not sign the server's certificate does not make it trusted.
		t.Setenv(SpiceDBCAFileSetting, writeFile(t, "other-ca.crt", newPrivateCA(t).pem))
		other, err := readSpiceDB()
		if err != nil {
			t.Fatal(err)
		}
		if err := check(other); err == nil {
			t.Fatal("a CA file of another authority verified the server")
		}
	})
}
