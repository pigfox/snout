package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
)

func TestPick(t *testing.T) {
	all := []Agency{
		{Agency: "A", Email: "hello@a.com"},
		{Agency: "B", Email: ""},
		{Agency: "C", Email: "info@c.com"},
		{Agency: "D", Email: "x@blocked.com"},
		{Agency: "E", Email: "not an address"},
		{Agency: "A2", Email: "hello@a.com"},
		{Agency: "F", Email: "sales@f.com"},
		{Agency: "G", Email: "hi@g.com"},
	}
	sent := map[string]bool{"info@c.com": true}
	suppress := map[string]bool{"@blocked.com": true}
	got := pick(all, sent, suppress, 2)
	if len(got) != 2 || got[0].Agency != "A" || got[1].Agency != "F" {
		t.Fatalf("pick = %+v", got)
	}
}

func TestCompose(t *testing.T) {
	tm := template.Must(template.New("t").Parse("Subject: Hi {{.Agency}}\n\nHello {{.Agency}}.\n"))
	msg, err := compose(tm, Agency{Agency: "Acme", Email: "hello@acme.com"}, "hello@trireens.com", "Trireens", "1 Main St, Town")
	if err != nil {
		t.Fatal(err)
	}
	s := string(msg)
	for _, want := range []string{
		"From: \"Trireens\" <hello@trireens.com>\r\n",
		"To: hello@acme.com\r\n",
		"Subject: Hi Acme\r\n",
		"List-Unsubscribe: <mailto:hello@trireens.com?subject=unsubscribe>\r\n",
		"\r\n\r\nHello Acme.\r\n",
		"1 Main St, Town\r\n",
		"unsubscribe",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("message lacks %q:\n%s", want, s)
		}
	}
	bad := template.Must(template.New("t").Parse("No subject here\n\nbody"))
	if _, err := compose(bad, Agency{}, "hello@trireens.com", "", ""); err == nil {
		t.Error("template without Subject: accepted")
	}
}

func TestRunDryRun(t *testing.T) {
	dir := t.TempDir()
	write := func(name, s string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	list := write("a.csv", "agency,website,email\nAcme,https://acme.com,hello@acme.com\nBeta,https://beta.com,\n")
	tm := write("t.txt", "Subject: Hi\n\nHello {{.Agency}}\n")
	sent := write("sent.csv", "old@x.com,Old,2026-01-01T00:00:00Z\n")
	var out bytes.Buffer
	if err := run(list, tm, sent, filepath.Join(dir, "none.txt"), "hello@trireens.com", "Trireens", 20, 0, false, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "2 agencies, 1 already sent, 1 this run") || !strings.Contains(out.String(), "Hello Acme") {
		t.Fatalf("dry run output:\n%s", out.String())
	}
	t.Setenv("OUTREACH_POSTAL_ADDRESS", "")
	if err := run(list, tm, sent, "", "hello@trireens.com", "", 20, 0, true, &out); err == nil {
		t.Error("send without postal address accepted")
	}
}

func TestFindEmails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Host + r.URL.Path {
		case "acme.test/":
			w.Write([]byte(`<a href="mailto:privacy@acme.test">privacy</a> partner@other.com`))
		case "acme.test/contact":
			w.Write([]byte(`Write to sales&#64;acme.test or <a href="mailto:hello@acme.test">hello@acme.test</a>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
		},
	}}

	path := filepath.Join(t.TempDir(), "a.csv")
	os.WriteFile(path, []byte("agency,website,email\nAcme,http://acme.test,\nNone,http://none.test,\nKept,http://acme.test,mine@kept.com\n"), 0o600)
	var out bytes.Buffer
	if err := findEmails(path, client, &out); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	want := "agency,website,email,email_source\n" +
		"Acme,http://acme.test,hello@acme.test,http://acme.test/contact\n" +
		"None,http://none.test,,\n" +
		"Kept,http://acme.test,mine@kept.com,\n"
	if string(b) != want {
		t.Fatalf("list after -find:\n%s\nwant:\n%s\nlog:\n%s", b, want, out.String())
	}
}
