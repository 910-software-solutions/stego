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
	"math/big"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// noNetwork replaces the default resolver, the default transport and the default dialer of the process with ones
// that count each call and refuse it, for the time of the test, and gives the count.
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
	t.Cleanup(func() { net.DefaultResolver, http.DefaultTransport, http.DefaultClient.Transport = resolver, transport, client })
	return &calls
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
