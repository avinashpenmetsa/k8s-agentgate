package main

import (
	"context"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/avinashpenmetsa/k8s-hatch/proxy"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "get-kubeconfig" {
		data, err := os.ReadFile(getenv("HATCH_DATA_DIR", "/var/hatch") + "/kubeconfig.yaml")
		if err != nil {
			log.Fatalf("k8s-hatch: read kubeconfig: %v", err)
		}
		os.Stdout.Write(data)
		return
	}

	cfg, err := loadKubeConfig()
	if err != nil {
		log.Fatalf("k8s-hatch: load kubeconfig: %v", err)
	}

	srv, err := proxy.New(cfg, proxy.Options{
		CertTTL:        loadDuration("CERT_TTL", 6*time.Hour),
		DataDir:        getenv("HATCH_DATA_DIR", "/var/hatch"),
		ProxyServerURL: getenv("PROXY_SERVER_URL", "https://127.0.0.1:8443"),
		ServiceName:    os.Getenv("TLS_SERVICE_NAME"),
		ServiceFQDN:    os.Getenv("TLS_SERVICE_FQDN"),
		ExtraDNSNames:  splitCSV(os.Getenv("TLS_EXTRA_DNS_NAMES")),
		ExtraIPs:       parseIPs(os.Getenv("TLS_EXTRA_IPS")),
	})
	if err != nil {
		log.Fatalf("k8s-hatch: create proxy: %v", err)
	}

	ln, err := net.Listen("tcp", getenv("PROXY_ADDR", ":8443"))
	if err != nil {
		log.Fatalf("k8s-hatch: listen: %v", err)
	}

	log.Printf("k8s-hatch listening on %s", ln.Addr())
	log.Fatal(srv.Start(context.Background(), ln))
}

func loadKubeConfig() (*rest.Config, error) {
	if kc := os.Getenv("KUBECONFIG"); kc != "" {
		return clientcmd.BuildConfigFromFlags("", kc)
	}
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}
	// Fall back to default kubeconfig location
	home, _ := os.UserHomeDir()
	return clientcmd.BuildConfigFromFlags("", home+"/.kube/config")
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func loadDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("k8s-hatch: invalid %s=%q, using default %s", key, v, def)
		return def
	}
	return d
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseIPs(s string) []net.IP {
	parts := splitCSV(s)
	ips := make([]net.IP, 0, len(parts))
	for _, p := range parts {
		ip := net.ParseIP(p)
		if ip != nil {
			ips = append(ips, ip)
		} else {
			log.Printf("k8s-hatch: invalid IP %q in TLS_EXTRA_IPS, skipping", p)
		}
	}
	return ips
}
