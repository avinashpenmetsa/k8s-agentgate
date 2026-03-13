package proxy

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	utilproxy "k8s.io/apimachinery/pkg/util/proxy"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
)

// Options configures the proxy server.
type Options struct {
	CertTTL        time.Duration // default 6h
	DataDir        string        // dir for CA + kubeconfig files, e.g. /var/agentgate
	ProxyServerURL string        // server URL to embed in kubeconfig, from PROXY_SERVER_URL env
	ServiceName    string        // added to cert SANs
	ServiceFQDN    string        // added to cert SANs
	ExtraDNSNames  []string
	ExtraIPs       []net.IP
}

// CertManager manages the CA, server cert, and client cert lifecycle.
type CertManager struct {
	opts       Options
	mu         sync.RWMutex
	caCert     *x509.Certificate
	caKey      crypto.Signer
	serverCert tls.Certificate
	expiresAt  time.Time
}

// Server is the mTLS proxy server.
type Server struct {
	cfg     *rest.Config
	cm      *CertManager
	mu      sync.Mutex
	httpSrv *http.Server
}

// New creates a new proxy Server.
func New(cfg *rest.Config, opts Options) (*Server, error) {
	if opts.CertTTL == 0 {
		opts.CertTTL = 6 * time.Hour
	}
	if opts.DataDir == "" {
		opts.DataDir = "/var/agentgate"
	}
	if opts.ProxyServerURL == "" {
		opts.ProxyServerURL = "https://127.0.0.1:8443"
	}

	cm := &CertManager{opts: opts}
	return &Server{cfg: cfg, cm: cm}, nil
}

// Start initialises certs, then serves mTLS on ln until ctx is done.
func (s *Server) Start(ctx context.Context, ln net.Listener) error {
	if err := s.cm.Start(ctx); err != nil {
		return fmt.Errorf("cert manager start: %w", err)
	}

	caPool := x509.NewCertPool()
	caPool.AddCert(s.cm.caCert)

	tlsCfg := &tls.Config{
		GetCertificate: s.cm.GetCertificate,
		ClientAuth:     tls.RequireAndVerifyClientCert,
		ClientCAs:      caPool,
		MinVersion:     tls.VersionTLS12,
	}

	handler, err := buildUpgradeAwareHandler(s.cfg)
	if err != nil {
		return fmt.Errorf("build handler: %w", err)
	}

	httpSrv := &http.Server{
		Handler:   handler,
		TLSConfig: tlsCfg,
	}
	s.mu.Lock()
	s.httpSrv = httpSrv
	s.mu.Unlock()

	tlsLn := tls.NewListener(ln, tlsCfg)
	log.Printf("k8-agentgate: serving mTLS proxy on %s", ln.Addr())

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(tlsLn); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	srv := s.httpSrv
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

// Start initialises the CA and certs, then starts the rotation loop.
func (cm *CertManager) Start(ctx context.Context) error {
	if err := os.MkdirAll(cm.opts.DataDir, 0700); err != nil {
		return fmt.Errorf("mkdir datadir: %w", err)
	}

	if err := cm.loadOrGenerateCA(); err != nil {
		return fmt.Errorf("CA init: %w", err)
	}

	if err := cm.loadOrGenerateCerts(); err != nil {
		return fmt.Errorf("cert init: %w", err)
	}

	go cm.rotateLoop(ctx)
	return nil
}

func (cm *CertManager) loadOrGenerateCA() error {
	caCrtPath := filepath.Join(cm.opts.DataDir, "ca.crt")
	caKeyPath := filepath.Join(cm.opts.DataDir, "ca.key")

	crtData, crtErr := os.ReadFile(caCrtPath)
	keyData, keyErr := os.ReadFile(caKeyPath)

	if crtErr == nil && keyErr == nil {
		cert, key, err := parseCertAndKey(crtData, keyData)
		if err == nil {
			cm.caCert = cert
			cm.caKey = key
			log.Printf("k8-agentgate: loaded existing CA from %s", cm.opts.DataDir)
			return nil
		}
		log.Printf("k8-agentgate: failed to parse existing CA, regenerating: %v", err)
	}

	log.Printf("k8-agentgate: generating new CA")
	caKey, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return fmt.Errorf("generate CA key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("generate CA serial: %w", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "k8-agentgate-ca",
			Organization: []string{"k8-agentgate"},
		},
		NotBefore:             time.Now().Add(-1 * time.Minute),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}

	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("create CA cert: %w", err)
	}

	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return fmt.Errorf("parse CA cert: %w", err)
	}

	if err := writePEMFile(caCrtPath, "CERTIFICATE", caDER, 0644); err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(caKey)
	if err != nil {
		return fmt.Errorf("marshal CA key: %w", err)
	}
	if err := writePEMFile(caKeyPath, "PRIVATE KEY", keyDER, 0600); err != nil {
		return err
	}

	cm.caCert = caCert
	cm.caKey = caKey
	log.Printf("k8-agentgate: CA generated and saved to %s", cm.opts.DataDir)
	return nil
}

func (cm *CertManager) loadOrGenerateCerts() error {
	srvCrtPath := filepath.Join(cm.opts.DataDir, "server.crt")
	srvKeyPath := filepath.Join(cm.opts.DataDir, "server.key")

	crtData, crtErr := os.ReadFile(srvCrtPath)
	keyData, keyErr := os.ReadFile(srvKeyPath)

	if crtErr == nil && keyErr == nil {
		tlsCert, err := tls.X509KeyPair(crtData, keyData)
		if err == nil {
			leaf, err := x509.ParseCertificate(tlsCert.Certificate[0])
			if err == nil && time.Now().Before(leaf.NotAfter.Add(-10*time.Minute)) {
				cm.mu.Lock()
				cm.serverCert = tlsCert
				cm.expiresAt = leaf.NotAfter
				cm.mu.Unlock()
				log.Printf("k8-agentgate: loaded existing server cert, expires %s", leaf.NotAfter.Format(time.RFC3339))
				return nil
			}
		}
	}

	return cm.rotate()
}

func (cm *CertManager) rotate() error {
	opts := cm.opts

	// Issue server cert
	srvKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("generate server key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("generate server serial: %w", err)
	}
	dnsNames := []string{"localhost"}
	if opts.ServiceName != "" {
		dnsNames = append(dnsNames, opts.ServiceName)
	}
	if opts.ServiceFQDN != "" {
		dnsNames = append(dnsNames, opts.ServiceFQDN)
	}
	dnsNames = append(dnsNames, opts.ExtraDNSNames...)

	ipAddresses := []net.IP{net.ParseIP("127.0.0.1")}
	ipAddresses = append(ipAddresses, opts.ExtraIPs...)

	notAfter := time.Now().Add(opts.CertTTL)
	srvTemplate := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "k8-agentgate-server"},
		NotBefore:    time.Now().Add(-1 * time.Minute),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ipAddresses,
	}

	srvDER, err := x509.CreateCertificate(rand.Reader, srvTemplate, cm.caCert, &srvKey.PublicKey, cm.caKey)
	if err != nil {
		return fmt.Errorf("create server cert: %w", err)
	}

	// Issue client cert
	clientKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("generate client key: %w", err)
	}

	clientSerial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("generate client serial: %w", err)
	}
	clientTemplate := &x509.Certificate{
		SerialNumber: clientSerial,
		Subject:      pkix.Name{CommonName: "agentgate-agent"},
		NotBefore:    time.Now().Add(-1 * time.Minute),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	clientDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, cm.caCert, &clientKey.PublicKey, cm.caKey)
	if err != nil {
		return fmt.Errorf("create client cert: %w", err)
	}

	// Write server cert + key
	srvCrtPath := filepath.Join(opts.DataDir, "server.crt")
	srvKeyPath := filepath.Join(opts.DataDir, "server.key")

	if err := writePEMFile(srvCrtPath, "CERTIFICATE", srvDER, 0644); err != nil {
		return err
	}
	srvKeyDER, err := x509.MarshalPKCS8PrivateKey(srvKey)
	if err != nil {
		return fmt.Errorf("marshal server key: %w", err)
	}
	if err := writePEMFile(srvKeyPath, "PRIVATE KEY", srvKeyDER, 0600); err != nil {
		return err
	}

	// Write kubeconfig
	caCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cm.caCert.Raw})
	clientCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER})
	clientKeyDER, err := x509.MarshalPKCS8PrivateKey(clientKey)
	if err != nil {
		return fmt.Errorf("marshal client key: %w", err)
	}
	clientKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: clientKeyDER})

	kubeconfig := buildKubeconfig(
		opts.ProxyServerURL,
		base64.StdEncoding.EncodeToString(caCertPEM),
		base64.StdEncoding.EncodeToString(clientCertPEM),
		base64.StdEncoding.EncodeToString(clientKeyPEM),
	)

	kubeconfigPath := filepath.Join(opts.DataDir, "kubeconfig.yaml")
	if err := os.WriteFile(kubeconfigPath, []byte(kubeconfig), 0600); err != nil {
		return fmt.Errorf("write kubeconfig: %w", err)
	}

	// Update in-memory state
	tlsCert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: srvKeyDER}),
	)
	if err != nil {
		return fmt.Errorf("load server TLS cert: %w", err)
	}

	cm.mu.Lock()
	cm.serverCert = tlsCert
	cm.expiresAt = notAfter
	cm.mu.Unlock()

	log.Printf("k8-agentgate: cert rotated, expires: %s", notAfter.Format(time.RFC3339))
	return nil
}

func (cm *CertManager) rotateLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cm.mu.RLock()
			expiresAt := cm.expiresAt
			cm.mu.RUnlock()
			if time.Now().After(expiresAt.Add(-10 * time.Minute)) {
				if err := cm.rotate(); err != nil {
					log.Printf("k8-agentgate: cert rotation failed: %v", err)
				}
			}
		}
	}
}

// GetCertificate returns the current server TLS certificate.
func (cm *CertManager) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	cert := cm.serverCert
	return &cert, nil
}

func buildUpgradeAwareHandler(cfg *rest.Config) (http.Handler, error) {
	target, err := url.Parse(cfg.Host)
	if err != nil {
		return nil, fmt.Errorf("parse host: %w", err)
	}

	regularTransport, err := rest.TransportFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("regular transport: %w", err)
	}

	transportCfg, err := cfg.TransportConfig()
	if err != nil {
		return nil, fmt.Errorf("transport config: %w", err)
	}

	tlsCfgUpgrade, err := transport.TLSConfigFor(transportCfg)
	if err != nil {
		return nil, fmt.Errorf("TLS config for upgrade transport: %w", err)
	}

	connTransport := &http.Transport{TLSClientConfig: tlsCfgUpgrade}
	upgradeRoundTripper := utilproxy.NewUpgradeRequestRoundTripper(connTransport, regularTransport)

	wrappedUpgradeRT, err := transport.HTTPWrappersForConfig(transportCfg, upgradeRoundTripper)
	if err != nil {
		return nil, fmt.Errorf("wrap upgrade transport: %w", err)
	}

	handler := utilproxy.NewUpgradeAwareHandler(target, regularTransport, false, false, &responder{})
	// wrappedUpgradeRT is http.RoundTripper; wrap it so it satisfies UpgradeRequestRoundTripper.
	handler.UpgradeTransport = &upgradeTransportWrapper{RoundTripper: wrappedUpgradeRT}
	handler.UseRequestLocation = true
	handler.UseLocationHost = true

	return handler, nil
}

// upgradeTransportWrapper satisfies utilproxy.UpgradeRequestRoundTripper by
// delegating RoundTrip to a wrapped http.RoundTripper and providing a no-op WrapRequest.
type upgradeTransportWrapper struct {
	http.RoundTripper
}

func (w *upgradeTransportWrapper) WrapRequest(req *http.Request) (*http.Request, error) {
	return req, nil
}

type responder struct{}

func (r *responder) Error(w http.ResponseWriter, req *http.Request, err error) {
	http.Error(w, err.Error(), http.StatusBadGateway)
}

// helpers

func parseCertAndKey(certPEM, keyPEM []byte) (*x509.Certificate, crypto.Signer, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, nil, fmt.Errorf("no PEM block in cert")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, err
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("no PEM block in key")
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	signer, ok := keyAny.(crypto.Signer)
	if !ok {
		return nil, nil, fmt.Errorf("key is not a crypto.Signer")
	}
	return cert, signer, nil
}

// writePEMFile writes a PEM file atomically (temp file + rename) so a crash
// mid-write never leaves a corrupt cert on disk.
func writePEMFile(path, pemType string, der []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	// Always clean up the temp file on any error path.
	ok := false
	defer func() {
		tmp.Close()
		if !ok {
			os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(perm); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpPath, err)
	}
	if err := pem.Encode(tmp, &pem.Block{Type: pemType, Bytes: der}); err != nil {
		return fmt.Errorf("encode PEM to %s: %w", tmpPath, err)
	}
	// Flush and check for deferred write errors before renaming.
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename %s → %s: %w", tmpPath, path, err)
	}
	ok = true
	return nil
}

func buildKubeconfig(serverURL, caB64, certB64, keyB64 string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
  - name: k8-agentgate
    cluster:
      server: %s
      certificate-authority-data: %s
contexts:
  - name: k8-agentgate
    context:
      cluster: k8-agentgate
      user: agentgate-agent
current-context: k8-agentgate
users:
  - name: agentgate-agent
    user:
      client-certificate-data: %s
      client-key-data: %s
`, serverURL, caB64, certB64, keyB64)
}
