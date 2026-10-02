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
