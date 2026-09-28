package node

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/smtp"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/openbrandexchange/node-go/internal/config"
	"github.com/openbrandexchange/node-go/internal/db"
	"github.com/openbrandexchange/node-go/internal/ids"
)

// Mailer sends e-mail.
type Mailer interface {
	Send(ctx context.Context, to, subject, body string) error
	Configured() bool
}

func newMailer(cfg *config.Config, log *slog.Logger) Mailer {
	if cfg.SMTPURL == "" {
		return &logMailer{log: log, bodies: cfg.MailLogBodies}
	}
	return &smtpMailer{url: cfg.SMTPURL, from: cfg.MailFrom, log: log}
}

// logMailer writes e-mails to the log (development). Bodies contain
// one-time links, so they are only logged with OBX_MAIL_LOG_BODIES=true.
type logMailer struct {
	log    *slog.Logger
	bodies bool
	mu     sync.Mutex
	Sent   []SentMail
}

// SentMail is a mail captured by the log driver (tests).
type SentMail struct{ To, Subject, Body string }

func (m *logMailer) Send(_ context.Context, to, subject, body string) error {
	m.mu.Lock()
	m.Sent = append(m.Sent, SentMail{to, subject, body})
	if len(m.Sent) > 100 {
		m.Sent = m.Sent[1:]
	}
	m.mu.Unlock()
	if m.bodies {
		m.log.Info("e-mail (not sent: OBX_SMTP_URL is not set)", "to", to, "subject", subject, "body", body)
	} else {
		m.log.Info("e-mail (not sent: OBX_SMTP_URL is not set; set OBX_MAIL_LOG_BODIES=true in development to log bodies)", "to", to, "subject", subject)
	}
	return nil
}

func (m *logMailer) Configured() bool { return false }

// LastMails returns captured mails (log driver only).
func (a *App) LastMails() []SentMail {
	if m, ok := a.Mail.(*logMailer); ok {
		m.mu.Lock()
		defer m.mu.Unlock()
		return append([]SentMail(nil), m.Sent...)
	}
	return nil
}

type smtpMailer struct {
	url  string
	from string
	log  *slog.Logger
}

func (m *smtpMailer) Configured() bool { return true }

func (m *smtpMailer) Send(ctx context.Context, to, subject, body string) error {
	u, err := url.Parse(m.url)
	if err != nil {
		return fmt.Errorf("invalid OBX_SMTP_URL")
	}
	host := u.Hostname()
	port := u.Port()
	implicitTLS := u.Scheme == "smtps"
	if port == "" {
		port = "587"
		if implicitTLS {
			port = "465"
		}
	}
	addr := net.JoinHostPort(host, port)
	d := &net.Dialer{Timeout: 20 * time.Second}
	var conn net.Conn
	if implicitTLS {
		conn, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("smtp connect: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp: %w", err)
	}
	defer c.Close()
	if !implicitTLS {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				return fmt.Errorf("smtp starttls: %w", err)
			}
		} else if u.Query().Get("insecure") != "true" && host != "localhost" && host != "127.0.0.1" && u.User != nil {
			return fmt.Errorf("smtp server offers no STARTTLS; refusing to send credentials in clear text (add ?insecure=true for a local relay)")
		}
	}
	if u.User != nil {
		pw, _ := u.User.Password()
		if err := c.Auth(smtp.PlainAuth("", u.User.Username(), pw, host)); err != nil {
			return fmt.Errorf("smtp auth failed")
		}
	}
	from := m.from
	if err := c.Mail(from); err != nil {
		return fmt.Errorf("smtp MAIL FROM: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("smtp RCPT TO: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	msg := "From: " + from + "\r\nTo: " + to + "\r\nSubject: " + mime.QEncoding.Encode("utf-8", subject) +
		"\r\nDate: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\nMessage-ID: <" + ids.New("msg") + "@" + domainOf(from) + ">" +
		"\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n" +
		strings.ReplaceAll(body, "\n", "\r\n") + "\r\n"
	if _, err := w.Write([]byte(msg)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func domainOf(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 {
		return strings.Trim(addr[i+1:], "> ")
	}
	return "localhost"
}

// ---- localized e-mail texts -------------------------------------------------------

// localeCache reads locales/<lang>.json from the admin-ui build.
type localeCache struct {
	fs fs.FS
	mu sync.Mutex
	m  map[string]map[string]string
}

func newLocaleCache(f fs.FS) *localeCache {
	return &localeCache{fs: f, m: map[string]map[string]string{}}
}

func flatten(prefix string, v any, out map[string]string) {
	switch t := v.(type) {
	case map[string]any:
		for k, c := range t {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			flatten(key, c, out)
		}
	case string:
		out[prefix] = t
	}
}

func (l *localeCache) get(lang string) map[string]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if m, ok := l.m[lang]; ok {
		return m
	}
	m := map[string]string{}
	if l.fs != nil {
		if b, err := fs.ReadFile(l.fs, "locales/"+lang+".json"); err == nil {
			var v any
			if json.Unmarshal(b, &v) == nil {
				flatten("", v, m)
			}
		}
	}
	l.m[lang] = m
	return m
}

// SupportedLanguages are the UI/e-mail languages.
var SupportedLanguages = []string{"bg", "cs", "da", "de", "el", "en", "es", "et", "fi", "fr", "ga", "hr", "hu", "it", "lt", "lv", "mt", "nl", "nb", "pl", "pt", "ro", "sk", "sl", "sv"}

func supportedLang(l string) string {
	l = strings.ToLower(strings.TrimSpace(l))
	if i := strings.IndexAny(l, "-_"); i > 0 {
		l = l[:i]
	}
	if l == "no" || l == "nn" {
		l = "nb"
	}
	for _, s := range SupportedLanguages {
		if s == l {
			return s
		}
	}
	return "en"
}

// Mail kinds the node sends. Each maps to an email.<key> group in the
// admin-ui locale files (dist/locales/<lang>.json), which hold subject,
// heading, body, button and optional extra lines.
const (
	mailInvite                = "invite"
	mailAccessRequestReceived = "accessRequestReceived"
	mailAccessRequestNew      = "accessRequestNew"
	mailAccessApproved        = "accessApproved"
	mailAccessRejected        = "accessRejected"
	mailZipReady              = "zipReady"
	mailPasswordReset         = "passwordReset"
)

// builtinEmail are English fallbacks (a copy of admin-ui's en.json) for when
// the vendored locale files lack a key.
var builtinEmail = map[string]string{
	"email.greeting":                      "Hello {{name}},",
	"email.greetingNoName":                "Hello,",
	"email.signoff":                       "Kind regards,\n{{brand}}",
	"email.footer":                        "You receive this email because of your account or request at {{portal}}.",
	"email.linkFallback":                  "Button not working? Copy this address into your browser: {{url}}",
	"email.invite.subject":                "{{brand}} invites you to {{portal}}",
	"email.invite.body":                   "{{brand}} has created an account for you. There you can find their final product images and article data, and download them whenever you need.",
	"email.invite.button":                 "Choose your password",
	"email.invite.expiry":                 "This link works once and is valid until {{expires}}.",
	"email.invite.ignore":                 "Not expecting this? Then you can ignore this email.",
	"email.accessRequestReceived.subject": "We received your request for {{portal}}",
	"email.accessRequestReceived.body":    "Thank you for your request to {{brand}}. We will check it and email you as soon as it has been reviewed.",
	"email.accessRequestNew.subject":      "New access request from {{retailer}}",
	"email.accessRequestNew.body":         "{{contact}} from {{retailer}} has asked for access to {{portal}}.",
	"email.accessRequestNew.button":       "Review the request",
	"email.accessApproved.subject":        "Your access to {{portal}} is ready",
	"email.accessApproved.body":           "{{brand}} has approved your request.",
	"email.accessApproved.button":         "Open the portal",
	"email.accessApproved.apiNote":        "Your software can now collect its API key using the request number it received.",
	"email.accessRejected.subject":        "About your request for {{portal}}",
	"email.accessRejected.body":           "Unfortunately {{brand}} cannot give you access at the moment.",
	"email.accessRejected.reason":         "Explanation: {{reason}}",
	"email.accessRejected.contact":        "Questions? Reply to this email or contact {{brand}}.",
	"email.zipReady.subject":              "Your download from {{portal}} is ready",
	"email.zipReady.body":                 "The zip you asked for ({{files}} files) is ready.",
	"email.zipReady.button":               "Download",
	"email.zipReady.expiry":               "The file is available until {{expires}}.",
	"email.passwordReset.subject":         "Choose a new password for {{portal}}",
	"email.passwordReset.body":            "Someone asked to reset the password of your account. Choose a new one with the button below.",
	"email.passwordReset.button":          "Choose a new password",
	"email.passwordReset.expiry":          "This link works once and is valid for one hour.",
	"email.passwordReset.ignore":          "Didn't ask for this? Then you can ignore this email; your password stays the same.",
}

// mailDate formats an e-mail date the same way in every language.
func mailDate(t time.Time) string { return t.UTC().Format("2006-01-02") }

var placeholderRe = regexp.MustCompile(`\{\{\s*([a-z_]+)\s*\}\}|\{([a-z_]+)\}`)

func interpolate(s string, vars map[string]string) string {
	return placeholderRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := placeholderRe.FindStringSubmatch(m)
		k := sub[1]
		if k == "" {
			k = sub[2]
		}
		if v, ok := vars[k]; ok {
			return v
		}
		return m
	})
}

// text returns the localized string for key in lang (en fallback).
func (a *App) text(lang, key string) string {
	if v := a.locales.get(supportedLang(lang))[key]; v != "" {
		return v
	}
	if v := a.locales.get("en")[key]; v != "" {
		return v
	}
	return builtinEmail[key]
}

// sendMail renders the email.<kind> texts in lang (en fallback) as a plain
// text e-mail and sends it. It returns whether a real e-mail was sent.
//
// Layout: greeting, body, optional lines (reason, apiNote), the button text
// with the link (vars["url"]), expiry, ignore, contact, sign-off, footer.
// A line whose placeholders are not all filled in is left out.
func (a *App) sendMail(ctx context.Context, to, lang, kind string, vars map[string]string) bool {
	subject, body := a.renderMail(ctx, lang, kind, vars)
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := a.Mail.Send(sctx, to, subject, body); err != nil {
		a.Log.WarnContext(ctx, "e-mail could not be sent; it will be retried", "kind", kind, "error", err)
		a.queueMail(ctx, to, subject, body, kind, err)
		return false
	}
	return a.Mail.Configured()
}

// mailBackoff is the wait before each retry of a failed e-mail; after the
// last one the e-mail is given up (and logged as an error).
var mailBackoff = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour}

// queueMail stores a failed e-mail for retries. The body holds one-time
// links, so it is stored encrypted and removed once sent or given up.
func (a *App) queueMail(ctx context.Context, to, subject, body, kind string, cause error) {
	enc, err := a.Keys.Encrypt([]byte(body))
	if err != nil {
		return
	}
	_, err = a.DB.Exec(context.WithoutCancel(ctx), `INSERT INTO mail_outbox (id, recipient, subject, body_enc, kind, status, attempts, next_attempt_at, last_error, created_at) VALUES (?, ?, ?, ?, ?, 'pending', 1, ?, ?, ?)`,
		ids.New("mal"), to, subject, enc, kind, db.Fmt(a.now().Add(mailBackoff[0])), truncate(cause.Error(), 500), a.nowStr())
	if err != nil {
		a.Log.ErrorContext(ctx, "e-mail could not be queued for a retry", "kind", kind, "error", err)
	}
}

// retryMail sends due e-mails from the outbox (housekeeping).
func (a *App) retryMail(ctx context.Context) {
	rs, err := a.DB.Query(ctx, `SELECT id, recipient, subject, body_enc, kind, attempts FROM mail_outbox WHERE status = 'pending' AND next_attempt_at <= ? ORDER BY next_attempt_at LIMIT 20`, a.nowStr())
	if err != nil {
		return
	}
	type item struct {
		id, to, subject, body, kind string
		attempts                    int
	}
	var due []item
	for rs.Next() {
		var it item
		if rs.Scan(&it.id, &it.to, &it.subject, &it.body, &it.kind, &it.attempts) == nil {
			due = append(due, it)
		}
	}
	rs.Close()
	for _, it := range due {
		body, err := a.Keys.Decrypt(it.body)
		if err == nil {
			sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err = a.Mail.Send(sctx, it.to, it.subject, string(body))
			cancel()
		}
		if err == nil {
			_, _ = a.DB.Exec(ctx, `UPDATE mail_outbox SET status = 'sent', body_enc = '', sent_at = ?, last_error = NULL WHERE id = ?`, a.nowStr(), it.id)
			a.Log.Info("e-mail sent after a retry", "kind", it.kind, "attempts", it.attempts+1)
			continue
		}
		if it.attempts >= len(mailBackoff) {
			_, _ = a.DB.Exec(ctx, `UPDATE mail_outbox SET status = 'failed', body_enc = '', attempts = attempts + 1, last_error = ? WHERE id = ?`, truncate(err.Error(), 500), it.id)
			a.Log.Error("e-mail given up after retries", "kind", it.kind, "attempts", it.attempts+1, "error", err)
			continue
		}
		_, _ = a.DB.Exec(ctx, `UPDATE mail_outbox SET attempts = attempts + 1, next_attempt_at = ?, last_error = ? WHERE id = ?`,
			db.Fmt(a.now().Add(mailBackoff[it.attempts])), truncate(err.Error(), 500), it.id)
	}
}

func (a *App) renderMail(ctx context.Context, lang, kind string, vars map[string]string) (subject, body string) {
	lang = supportedLang(lang)
	s := a.Settings(ctx)
	filled := map[string]string{}
	for k, v := range vars {
		if v != "" {
			filled[k] = v
		}
	}
	vars = filled
	if vars["portal"] == "" {
		vars["portal"] = a.PortalName(ctx, lang)
	}
	if vars["brand"] == "" {
		vars["brand"] = s.Branding.DisplayName
		if vars["brand"] == "" {
			vars["brand"] = s.Company.Name
		}
	}
	line := func(key string) string {
		t := a.text(lang, key)
		if t == "" {
			return ""
		}
		out := interpolate(t, vars)
		if placeholderRe.MatchString(out) {
			return ""
		}
		return out
	}
	var parts []string
	add := func(t string) {
		if strings.TrimSpace(t) != "" {
			parts = append(parts, t)
		}
	}
	if vars["name"] != "" {
		add(line("email.greeting"))
	} else {
		add(line("email.greetingNoName"))
	}
	add(line("email." + kind + ".body"))
	add(line("email." + kind + ".reason"))
	if vars["api"] != "" {
		add(line("email." + kind + ".apiNote"))
	}
	if u := vars["url"]; u != "" {
		if b := line("email." + kind + ".button"); b != "" {
			add(b + ":\n" + u)
		} else {
			add(u)
		}
	}
	add(line("email." + kind + ".expiry"))
	add(line("email." + kind + ".ignore"))
	add(line("email." + kind + ".contact"))
	add(line("email.signoff"))
	body = strings.Join(parts, "\n\n") + "\n\n-- \n" + line("email.footer") + "\n"
	return line("email." + kind + ".subject"), body
}
