package nodes

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/siderolabs/talos/pkg/machinery/constants"
)

// genCAAndClient creates a self-signed CA and a client cert signed by it.
func genCAAndClient(t *testing.T) (caPEM, crtPEM, keyPEM []byte) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}

	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "test-client", Organization: []string{"machine:admin"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(6 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTmpl, caTmpl, &clientKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(clientKey)
	if err != nil {
		t.Fatal(err)
	}

	return pemEncode("CERTIFICATE", caDER), pemEncode("CERTIFICATE", clientDER), pemEncode("EC PRIVATE KEY", keyDER)
}

func pemEncode(blockType string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
}

// writeTalosconfig writes a single-context talosconfig into dir and
// returns its path.
func writeTalosconfig(t *testing.T, dir string, ca, crt, key []byte) string {
	t.Helper()
	path := filepath.Join(dir, "config")
	content := fmt.Sprintf(`context: test
contexts:
  test:
    endpoints: []
    ca: %s
    crt: %s
    key: %s
`,
		base64.StdEncoding.EncodeToString(ca),
		base64.StdEncoding.EncodeToString(crt),
		base64.StdEncoding.EncodeToString(key))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// isolateTalosEnv points TALOS_HOME at a fresh temp dir so the real
// ~/.talos/config never interferes.
func isolateTalosEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv(constants.TalosHomeEnvVar, home)
	return home
}

func TestLoadTalosConfigFromEnvVar(t *testing.T) {
	isolateTalosEnv(t)
	ca, crt, key := genCAAndClient(t)
	path := writeTalosconfig(t, t.TempDir(), ca, crt, key)
	t.Setenv(constants.TalosConfigEnvVar, path)

	tc, err := LoadTalosConfig()
	if err != nil {
		t.Fatal(err)
	}
	if tc.Path != path {
		t.Errorf("Path = %q, want %q (TALOSCONFIG must win)", tc.Path, path)
	}
	if tc.ContextName != "test" {
		t.Errorf("ContextName = %q, want %q", tc.ContextName, "test")
	}
	if !tc.CertNotAfter.After(time.Now()) {
		t.Errorf("CertNotAfter %v is not in the future", tc.CertNotAfter)
	}
	if tc.Context() == nil {
		t.Error("Context() = nil")
	}
}

func TestLoadTalosConfigFromTalosHome(t *testing.T) {
	home := isolateTalosEnv(t)
	ca, crt, key := genCAAndClient(t)
	writeTalosconfig(t, home, ca, crt, key) // writes $TALOS_HOME/config

	tc, err := LoadTalosConfig()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "config"); tc.Path != want {
		t.Errorf("Path = %q, want %q", tc.Path, want)
	}
}

func TestLoadTalosConfigNotFound(t *testing.T) {
	isolateTalosEnv(t)
	_, err := LoadTalosConfig()
	if err == nil {
		t.Fatal("expected error when no talosconfig exists")
	}
	if !strings.Contains(err.Error(), "no readable talosconfig") {
		t.Errorf("error = %v, want mention of missing talosconfig", err)
	}
}

func TestLoadTalosConfigInvalidYAML(t *testing.T) {
	home := isolateTalosEnv(t)
	if err := os.WriteFile(filepath.Join(home, "config"), []byte("not: [valid yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTalosConfig(); err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestLoadTalosConfigMissingContext(t *testing.T) {
	home := isolateTalosEnv(t)
	ca, crt, key := genCAAndClient(t)
	content := fmt.Sprintf(`context: missing
contexts:
  test:
    endpoints: []
    ca: %s
    crt: %s
    key: %s
`,
		base64.StdEncoding.EncodeToString(ca),
		base64.StdEncoding.EncodeToString(crt),
		base64.StdEncoding.EncodeToString(key))
	if err := os.WriteFile(filepath.Join(home, "config"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadTalosConfig()
	if err == nil || !strings.Contains(err.Error(), `context "missing" not found`) {
		t.Errorf("err = %v, want missing-context error", err)
	}
}

func TestLoadTalosConfigFingerprint(t *testing.T) {
	home := isolateTalosEnv(t)
	ca, crt, key := genCAAndClient(t)
	writeTalosconfig(t, home, ca, crt, key)

	first, err := LoadTalosConfig()
	if err != nil {
		t.Fatal(err)
	}
	same, err := LoadTalosConfig()
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint != same.Fingerprint {
		t.Error("fingerprint changed for identical file content")
	}

	ca2, crt2, key2 := genCAAndClient(t)
	writeTalosconfig(t, home, ca2, crt2, key2)
	changed, err := LoadTalosConfig()
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint == changed.Fingerprint {
		t.Error("fingerprint unchanged after file change")
	}
}
