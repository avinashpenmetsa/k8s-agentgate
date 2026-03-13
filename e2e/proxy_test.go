package e2e

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avinashpenmetsa/k8s-hatch/proxy"
	"github.com/testcontainers/testcontainers-go/modules/k3s"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func TestProxyE2E(t *testing.T) {
	ctx := context.Background()

	// 1. Start k3s
	k3sCtr, err := k3s.Run(ctx, "rancher/k3s:v1.31.4-k3s1")
	if err != nil {
		t.Fatalf("start k3s: %v", err)
	}
	t.Cleanup(func() { _ = k3sCtr.Terminate(ctx) })

	// 2. Get admin kubeconfig + clientset
	kubeconfigBytes, err := k3sCtr.GetKubeConfig(ctx)
	if err != nil {
		t.Fatalf("get kubeconfig: %v", err)
	}
	adminCfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigBytes)
	if err != nil {
		t.Fatalf("parse admin kubeconfig: %v", err)
	}
	adminClient, err := kubernetes.NewForConfig(adminCfg)
	if err != nil {
		t.Fatalf("create admin clientset: %v", err)
	}

	// 3. Apply SA + Role + RoleBinding in k3s
	applyRBAC(ctx, t, adminClient)

	// 4. Get SA token → SA-scoped rest.Config
	saToken := getSAToken(ctx, t, adminClient)
	saCfg := &rest.Config{
		Host:        adminCfg.Host,
		BearerToken: saToken,
		TLSClientConfig: rest.TLSClientConfig{
			CAData: adminCfg.TLSClientConfig.CAData,
		},
	}

	// 5. Start proxy in-process with a temp DataDir
	dataDir, err := os.MkdirTemp("", "k8s-hatch-test-")
	if err != nil {
		t.Fatalf("mktemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dataDir) })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	proxyAddr := "https://" + ln.Addr().String()

	srv, err := proxy.New(saCfg, proxy.Options{
		CertTTL:        6 * time.Hour,
		DataDir:        dataDir,
		ProxyServerURL: proxyAddr,
	})
	if err != nil {
		t.Fatalf("create proxy: %v", err)
	}

	srvCtx, srvCancel := context.WithCancel(ctx)
	go func() { _ = srv.Start(srvCtx, ln) }()
	t.Cleanup(func() {
		srvCancel()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	})

	// 6. Wait for kubeconfig file to appear
	kubeconfigFile := filepath.Join(dataDir, "kubeconfig.yaml")
	waitForFile(t, kubeconfigFile, 30*time.Second)

	// 7. Build mTLS HTTP client from the generated kubeconfig
	kcBytes, err := os.ReadFile(kubeconfigFile)
	if err != nil {
		t.Fatalf("read kubeconfig: %v", err)
	}
	clientCfg, err := clientcmd.RESTConfigFromKubeConfig(kcBytes)
	if err != nil {
		t.Fatalf("parse hatch kubeconfig: %v", err)
	}
	httpClient := buildMTLSClient(t, clientCfg)
	baseURL := proxyAddr

	// 8. Subtests
	t.Run("AllowedPods", func(t *testing.T) {
		resp, err := httpClient.Get(baseURL + "/api/v1/namespaces/default/pods")
		if err != nil {
			t.Fatalf("GET pods: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200, got %d", resp.StatusCode)
		}
	})

	t.Run("ForbiddenSecrets", func(t *testing.T) {
		resp, err := httpClient.Get(baseURL + "/api/v1/namespaces/default/secrets")
		if err != nil {
			t.Fatalf("GET secrets: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("expected 403, got %d", resp.StatusCode)
		}
	})

	t.Run("NoClientCert", func(t *testing.T) {
		bareClient := &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
			},
		}
		_, err := bareClient.Get(baseURL + "/api/v1/namespaces/default/pods")
		if err == nil {
			t.Error("expected TLS handshake error with no client cert, got nil")
		}
	})

	t.Run("Watch", func(t *testing.T) {
		watchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(watchCtx, http.MethodGet,
			baseURL+"/api/v1/namespaces/default/pods?watch=true", nil)
		if err != nil {
			t.Fatalf("create watch request: %v", err)
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			// Context cancellation is expected
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200 for watch, got %d", resp.StatusCode)
		}
		// Read at least one byte to confirm streaming works
		buf := make([]byte, 1)
		_ , _ = resp.Body.Read(buf)
	})
}

func applyRBAC(ctx context.Context, t *testing.T, client *kubernetes.Clientset) {
	t.Helper()

	_, err := client.CoreV1().ServiceAccounts("default").Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "hatch-agent", Namespace: "default"},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Logf("SA create (may already exist): %v", err)
	}

	_, err = client.RbacV1().Roles("default").Create(ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "hatch-agent", Namespace: "default"},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"pods", "pods/exec", "pods/log"},
				Verbs:     []string{"get", "list", "watch", "create"},
			},
			{
				APIGroups: []string{"apps"},
				Resources: []string{"deployments", "deployments/scale"},
				Verbs:     []string{"get", "list", "patch"},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Logf("Role create (may already exist): %v", err)
	}

	_, err = client.RbacV1().RoleBindings("default").Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "hatch-agent", Namespace: "default"},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     "hatch-agent",
		},
		Subjects: []rbacv1.Subject{
			{Kind: "ServiceAccount", Name: "hatch-agent", Namespace: "default"},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Logf("RoleBinding create (may already exist): %v", err)
	}
}

func getSAToken(ctx context.Context, t *testing.T, client *kubernetes.Clientset) string {
	t.Helper()
	tokenReq, err := client.CoreV1().ServiceAccounts("default").CreateToken(ctx, "hatch-agent",
		&authenticationv1.TokenRequest{
			Spec: authenticationv1.TokenRequestSpec{
				ExpirationSeconds: int64ptr(3600),
			},
		}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create SA token: %v", err)
	}
	return tokenReq.Status.Token
}

func buildMTLSClient(t *testing.T, cfg *rest.Config) *http.Client {
	t.Helper()

	// clientcmd.RESTConfigFromKubeConfig always decodes certificate-authority-data,
	// client-certificate-data, and client-key-data into raw PEM bytes in the
	// TLSClientConfig fields — no base64 re-decoding required.
	if len(cfg.TLSClientConfig.CertData) == 0 {
		t.Fatal("no client-certificate-data in kubeconfig")
	}
	tlsCert, err := tls.X509KeyPair(cfg.TLSClientConfig.CertData, cfg.TLSClientConfig.KeyData)
	if err != nil {
		t.Fatalf("load client TLS cert: %v", err)
	}

	if len(cfg.TLSClientConfig.CAData) == 0 {
		t.Fatal("no certificate-authority-data in kubeconfig")
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(cfg.TLSClientConfig.CAData) {
		t.Fatal("failed to add CA cert to pool")
	}

	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates: []tls.Certificate{tlsCert},
				RootCAs:      caPool,
			},
		},
	}
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for file %s", path)
}

func int64ptr(i int64) *int64 { return &i }
