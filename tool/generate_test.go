package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDataComposeMailRelay(t *testing.T) {
	infra := t.TempDir()
	p := paths{infra: infra, root: filepath.Dir(infra)}
	t.Setenv("RESEND_API_KEY", "")
	t.Setenv("MAIL_FROM", "")

	if out := generateDataCompose(p, nil, nil); strings.Contains(out, "mail:") {
		t.Fatal("mail relay generated without RESEND_API_KEY")
	}

	os.WriteFile(filepath.Join(infra, ".env"), []byte("RESEND_API_KEY=re_x\nMAIL_FROM=\"Nucleus <alerts@Nucleus-Home.dev>\"\n"), 0o644)
	out := generateDataCompose(p, nil, nil)
	for _, want := range []string{"  mail:\n", `ALLOWED_SENDER_DOMAINS: "nucleus-home.dev"`, "RELAYHOST_PASSWORD: ${RESEND_API_KEY}", "name: nucleus_mail_queue"} {
		if !strings.Contains(out, want) {
			t.Errorf("data compose missing %q", want)
		}
	}
	if strings.Contains(out, "re_x") {
		t.Error("API key leaked into the generated compose file")
	}
}

// proxiedBlocks returns every location block that proxies to an upstream.
func proxiedBlocks(conf string) []string {
	var out []string
	for _, b := range strings.Split(conf, "location ")[1:] {
		if strings.Contains(b, "proxy_pass") {
			out = append(out, b)
		}
	}
	return out
}

func TestNginxForwardsOriginalRequest(t *testing.T) {
	p := testTree(t)
	write(t, filepath.Join(p.root, "plugin-runtime", "Dockerfile"), "FROM node")
	apps := []*Manifest{{ID: "auth", Nginx: &Nginx{Routes: []Route{{Path: "/api/v1", Upstream: "auth-server:3005", Websocket: true}}}}}

	dev := generateNginx(p, apps, nil)
	stack := generateStackNginx(p, apps, nil)
	if len(proxiedBlocks(dev)) < 4 || len(proxiedBlocks(stack)) < 3 {
		t.Fatalf("expected proxied locations, got dev=%d stack=%d", len(proxiedBlocks(dev)), len(proxiedBlocks(stack)))
	}

	// Dev nginx terminates TLS itself: it is the authoritative source.
	for _, b := range proxiedBlocks(dev) {
		for _, want := range []string{
			"proxy_set_header Host $host;",
			"proxy_set_header X-Real-IP $remote_addr;",
			"proxy_set_header X-Forwarded-Proto $scheme;",
			"proxy_set_header X-Forwarded-Host $http_host;",
			"proxy_set_header X-Forwarded-For $remote_addr;",
		} {
			if !strings.Contains(b, want) {
				t.Errorf("dev location missing %q:\n%s", want, b)
			}
		}
	}

	// Stack sits behind the edge: keep the edge's values, never touch X-Forwarded-For.
	head := stack[:strings.Index(stack, "server {")]
	for _, want := range []string{
		"map $http_x_forwarded_proto $nucleus_forwarded_proto {",
		"map $http_x_forwarded_host $nucleus_forwarded_host {",
	} {
		if !strings.Contains(head, want) {
			t.Errorf("stack conf missing http-level %q", want)
		}
	}
	for _, b := range proxiedBlocks(stack) {
		for _, want := range []string{
			"proxy_set_header X-Forwarded-Proto $nucleus_forwarded_proto;",
			"proxy_set_header X-Forwarded-Host $nucleus_forwarded_host;",
		} {
			if !strings.Contains(b, want) {
				t.Errorf("stack location missing %q:\n%s", want, b)
			}
		}
		if strings.Contains(b, "X-Forwarded-For") {
			t.Errorf("stack location rewrites X-Forwarded-For:\n%s", b)
		}
	}

	if !strings.Contains(generateEdgeNginx(p), "proxy_set_header X-Forwarded-Host $http_host;") {
		t.Error("edge does not forward the original host")
	}
}
