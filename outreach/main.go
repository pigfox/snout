// Command outreach sends a small daily batch of one-to-one cold emails to the
// agencies in a CSV list, from one sender address, over SMTP.
//
// -find first fills in the list's missing addresses from the contact pages of
// each agency's own website. Sending is a dry run unless -send is given. Every run skips addresses already in
// the sent log and addresses or domains in the suppression list, and stops at
// -limit messages, so running it once a day works through the list without
// writing to anyone twice.
//
//	SMTP_HOST, SMTP_PORT (587 STARTTLS or 465 TLS), SMTP_USER, SMTP_PASS
//	OUTREACH_POSTAL_ADDRESS   sender's physical address, required by CAN-SPAM
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"os"
	"strings"
	"text/template"
	"time"
)

// Agency is one row of the list.
type Agency struct {
	Agency, Website, City, State, Email string
}

func main() {
	list := flag.String("list", "agencies.csv", "agency CSV")
	tmpl := flag.String("template", "template.txt", "message template: a Subject: line, a blank line, then the body")
	sentPath := flag.String("sent", "sent.csv", "log of addresses already written to (appended)")
	suppressPath := flag.String("suppress", "suppress.txt", "addresses or @domains never to write to, one per line")
	from := flag.String("from", "hello@trireens.com", "sender address")
	name := flag.String("name", "Trireens", "sender display name")
	limit := flag.Int("limit", 20, "most messages to send this run")
	gap := flag.Duration("gap", 45*time.Second, "pause between messages")
	send := flag.Bool("send", false, "actually send; without it, print what would be sent")
	find := flag.Bool("find", false, "fill empty email cells from each agency's own contact pages, then exit")
	flag.Parse()

	if *find {
		if err := findEmails(*list, newClient(), os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "outreach:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(*list, *tmpl, *sentPath, *suppressPath, *from, *name, *limit, *gap, *send, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "outreach:", err)
		os.Exit(1)
	}
}

func run(list, tmplPath, sentPath, suppressPath, from, name string, limit int, gap time.Duration, send bool, out io.Writer) error {
	agencies, err := readAgencies(list)
	if err != nil {
		return err
	}
	t, err := template.ParseFiles(tmplPath)
	if err != nil {
		return err
	}
	sent, err := readSet(sentPath, true)
	if err != nil {
		return err
	}
	suppress, err := readSet(suppressPath, false)
	if err != nil {
		return err
	}
	postal := strings.TrimSpace(os.Getenv("OUTREACH_POSTAL_ADDRESS"))
	if send && postal == "" {
		return errors.New("OUTREACH_POSTAL_ADDRESS is required to send (CAN-SPAM)")
	}
	if postal == "" {
		postal = "<OUTREACH_POSTAL_ADDRESS>"
	}

	var smtpSend func(to string, msg []byte) error
	if send {
		if smtpSend, err = dialer(from); err != nil {
			return err
		}
	}

	batch := pick(agencies, sent, suppress, limit)
	fmt.Fprintf(out, "%d agencies, %d already sent, %d this run\n", len(agencies), len(sent), len(batch))
	for i, a := range batch {
		msg, err := compose(t, a, from, name, postal)
		if err != nil {
			return fmt.Errorf("%s: %w", a.Agency, err)
		}
		if !send {
			fmt.Fprintf(out, "\n----- %s <%s> -----\n%s", a.Agency, a.Email, msg)
			continue
		}
		if i > 0 {
			time.Sleep(gap)
		}
		if err := smtpSend(a.Email, msg); err != nil {
			return fmt.Errorf("%s <%s>: %w", a.Agency, a.Email, err)
		}
		if err := logSent(sentPath, a); err != nil {
			return err
		}
		fmt.Fprintf(out, "sent %s <%s>\n", a.Agency, a.Email)
	}
	return nil
}

func readAgencies(path string) ([]Agency, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	for _, k := range []string{"agency", "email"} {
		if _, ok := col[k]; !ok {
			return nil, fmt.Errorf("%s: no %q column", path, k)
		}
	}
	get := func(r []string, k string) string {
		if i, ok := col[k]; ok && i < len(r) {
			return strings.TrimSpace(r[i])
		}
		return ""
	}
	var out []Agency
	for _, r := range rows[1:] {
		out = append(out, Agency{
			Agency:  get(r, "agency"),
			Website: get(r, "website"),
			City:    get(r, "city"),
			State:   get(r, "state"),
			Email:   strings.ToLower(get(r, "email")),
		})
	}
	return out, nil
}

// readSet reads lower-cased entries from a file that may not exist yet: the
// first CSV column when csvFile, else one entry per non-comment line.
func readSet(path string, csvFile bool) (map[string]bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		if csvFile {
			line, _, _ = strings.Cut(line, ",")
		}
		line = strings.ToLower(strings.TrimSpace(line))
		if line != "" && !strings.HasPrefix(line, "#") {
			set[line] = true
		}
	}
	return set, nil
}

// pick returns up to limit agencies with a valid address that has not been
// written to and is not suppressed by address or by @domain.
func pick(all []Agency, sent, suppress map[string]bool, limit int) []Agency {
	var out []Agency
	seen := map[string]bool{}
	for _, a := range all {
		if len(out) == limit {
			break
		}
		if p, err := mail.ParseAddress(a.Email); err != nil || p.Address != a.Email {
			continue
		}
		_, domain, _ := strings.Cut(a.Email, "@")
		if seen[a.Email] || sent[a.Email] || suppress[a.Email] || suppress["@"+domain] {
			continue
		}
		seen[a.Email] = true
		out = append(out, a)
	}
	return out
}

func compose(t *template.Template, a Agency, from, name, postal string) ([]byte, error) {
	var body bytes.Buffer
	if err := t.Execute(&body, a); err != nil {
		return nil, err
	}
	text := strings.ReplaceAll(body.String(), "\r\n", "\n")
	head, rest, ok := strings.Cut(text, "\n\n")
	subject, found := strings.CutPrefix(head, "Subject:")
	if !ok || !found || strings.Contains(head, "\n") {
		return nil, errors.New("template must start with one Subject: line and a blank line")
	}
	rest = strings.TrimRight(rest, "\n") + "\n\n--\n" + postal +
		"\nNot interested? Reply \"unsubscribe\" and we won't email you again.\n"

	id := make([]byte, 12)
	rand.Read(id)
	_, domain, _ := strings.Cut(from, "@")
	var m bytes.Buffer
	hdr := func(k, v string) { fmt.Fprintf(&m, "%s: %s\r\n", k, v) }
	hdr("From", (&mail.Address{Name: name, Address: from}).String())
	hdr("To", a.Email)
	hdr("Subject", mimeHeader(strings.TrimSpace(subject)))
	hdr("Date", time.Now().Format(time.RFC1123Z))
	hdr("Message-ID", "<"+hex.EncodeToString(id)+"@"+domain+">")
	hdr("List-Unsubscribe", "<mailto:"+from+"?subject=unsubscribe>")
	hdr("MIME-Version", "1.0")
	hdr("Content-Type", "text/plain; charset=UTF-8")
	hdr("Content-Transfer-Encoding", "8bit")
	m.WriteString("\r\n")
	m.WriteString(strings.ReplaceAll(rest, "\n", "\r\n"))
	return m.Bytes(), nil
}

func mimeHeader(s string) string {
	for _, r := range s {
		if r > 127 {
			return mime.QEncoding.Encode("UTF-8", s)
		}
	}
	return s
}

func logSent(path string, a Agency) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	w.Write([]string{a.Email, a.Agency, time.Now().UTC().Format(time.RFC3339)})
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// dialer returns a sender for the SMTP server in the environment: implicit TLS
// on port 465, STARTTLS (required by PlainAuth) otherwise.
func dialer(from string) (func(string, []byte) error, error) {
	host, port := os.Getenv("SMTP_HOST"), os.Getenv("SMTP_PORT")
	user, pass := os.Getenv("SMTP_USER"), os.Getenv("SMTP_PASS")
	if host == "" || user == "" || pass == "" {
		return nil, errors.New("SMTP_HOST, SMTP_USER and SMTP_PASS are required to send")
	}
	if port == "" {
		port = "587"
	}
	auth := smtp.PlainAuth("", user, pass, host)
	addr := net.JoinHostPort(host, port)
	if port != "465" {
		return func(to string, msg []byte) error {
			return smtp.SendMail(addr, auth, from, []string{to}, msg)
		}, nil
	}
	return func(to string, msg []byte) error {
		conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: host})
		if err != nil {
			return err
		}
		c, err := smtp.NewClient(conn, host)
		if err != nil {
			conn.Close()
			return err
		}
		defer c.Close()
		if err := c.Auth(auth); err != nil {
			return err
		}
		if err := c.Mail(from); err != nil {
			return err
		}
		if err := c.Rcpt(to); err != nil {
			return err
		}
		w, err := c.Data()
		if err != nil {
			return err
		}
		if _, err := w.Write(msg); err != nil {
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
		return c.Quit()
	}, nil
}
