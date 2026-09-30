package jira

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testCert struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	der  []byte
}

func newTestCert(t *testing.T, cn string, parent *testCert, isCA bool, usage x509.ExtKeyUsage) *testCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
	}
	if isCA {
		tmpl.IsCA = true
		tmpl.BasicConstraintsValid = true
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{usage}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	signer, signerKey := tmpl, key
	if parent != nil {
		signer, signerKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCert{cert: cert, key: key, der: der}
}

// writePEM writes the cert and key as PEM files and returns their paths.
func (c *testCert) writePEM(t *testing.T, dir, name string) (certPath, keyPath string) {
	t.Helper()
	certPath = filepath.Join(dir, name+".pem")
	keyPath = filepath.Join(dir, name+".key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.der}), 0o600); err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(c.key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// newMTLSServer starts a TLS server that requires a client cert signed by ca.
func newMTLSServer(t *testing.T, ca, server *testCert) *httptest.Server {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer pat-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"key":"PROJ-1","fields":{"summary":"ok"}}`))
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{server.der}, PrivateKey: server.key}},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestConfigureTLSMutualAuth(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCert(t, "test-ca", nil, true, 0)
	server := newTestCert(t, "server", ca, false, x509.ExtKeyUsageServerAuth)
	client := newTestCert(t, "client", ca, false, x509.ExtKeyUsageClientAuth)
	caPath, _ := ca.writePEM(t, dir, "ca")
	certPath, keyPath := client.writePEM(t, dir, "client")

	srv := newMTLSServer(t, ca, server)

	t.Run("with client cert", func(t *testing.T) {
		c := NewClient(srv.URL, "", "pat-token")
		c.APIVersion = "2"
		if err := c.ConfigureTLS(certPath, keyPath, caPath); err != nil {
			t.Fatalf("ConfigureTLS: %v", err)
		}
		issue, err := c.GetIssue(context.Background(), "PROJ-1")
		if err != nil {
			t.Fatalf("GetIssue: %v", err)
		}
		if issue.Key != "PROJ-1" {
			t.Errorf("issue key = %q, want PROJ-1", issue.Key)
		}
	})

	t.Run("CA only is rejected by server", func(t *testing.T) {
		c := NewClient(srv.URL, "", "pat-token")
		if err := c.ConfigureTLS("", "", caPath); err != nil {
			t.Fatalf("ConfigureTLS: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := c.GetIssue(ctx, "PROJ-1"); err == nil {
			t.Fatal("GetIssue succeeded without a client certificate")
		}
	})
}

func TestConfigureTLSValidation(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCert(t, "test-ca", nil, true, 0)
	certPath, keyPath := ca.writePEM(t, dir, "ca")
	notPEM := filepath.Join(dir, "empty.pem")
	if err := os.WriteFile(notPEM, []byte("not a cert"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name               string
		cert, key, caFile  string
		wantErr            string
		wantTransportIsNil bool
	}{
		{name: "all empty is a no-op", wantTransportIsNil: true},
		{name: "cert without key", cert: certPath, wantErr: "configured together"},
		{name: "key without cert", key: keyPath, wantErr: "configured together"},
		{name: "missing cert file", cert: filepath.Join(dir, "nope.pem"), key: keyPath, wantErr: "load jira client certificate"},
		{name: "missing CA file", caFile: filepath.Join(dir, "nope.pem"), wantErr: "read jira CA certificate"},
		{name: "CA file without certs", caFile: notPEM, wantErr: "no certificates found"},
		{name: "valid cert and key", cert: certPath, key: keyPath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewClient("https://jira.example.com", "", "tok")
			err := c.ConfigureTLS(tt.cert, tt.key, tt.caFile)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if (c.HTTPClient.Transport == nil) != tt.wantTransportIsNil {
				t.Errorf("transport nil = %v, want %v", c.HTTPClient.Transport == nil, tt.wantTransportIsNil)
			}
			if tt.cert != "" {
				tlsConfig := c.HTTPClient.Transport.(*http.Transport).TLSClientConfig
				if tlsConfig.Renegotiation != tls.RenegotiateOnceAsClient {
					t.Errorf("Renegotiation = %v, want RenegotiateOnceAsClient", tlsConfig.Renegotiation)
				}
			}
		})
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	if got, want := expandHome("~/certs/a.pem"), filepath.Join(home, "certs/a.pem"); got != want {
		t.Errorf("expandHome(~/certs/a.pem) = %q, want %q", got, want)
	}
	if got := expandHome("/abs/a.pem"); got != "/abs/a.pem" {
		t.Errorf("expandHome(/abs/a.pem) = %q", got)
	}
}
