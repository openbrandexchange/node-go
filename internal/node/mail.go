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

// builtinEmail are English fallbacks when the admin-ui locale files do not
// (yet) contain a key.
var builtinEmail = map[string]string{
	"email.greeting":                "Hello {name},",
	"email.footer":                  "This e-mail was sent by {portal}.",
	"email.invite.subject":          "Your account for {portal}",
	"email.invite.body":             "You have been invited to {portal}, where {brand} shares product images and article data with its retailers.\n\nChoose your password with this link (valid for {expires_days} days, works once):\n{link}",
	"email.invite_admin.subject":    "You are invited to manage {portal}",
	"email.invite_admin.body":       "You have been invited as an administrator of {portal}.\n\nChoose your password with this link (valid for {expires_days} days, works once):\n{link}",
	"email.access_request.subject":  "New access request from {retailer}",
	"email.access_request.body":     "{retailer} ({contact}) asks for access to {portal}.\n\nReview the request:\n{link}",
	"email.access_approved.subject": "Your access to {portal} was approved",
	"email.access_approved.body":    "Good news: {brand} approved your request. You can now use {portal}.\n\n{link}",
	"email.access_rejected.subject": "Your request for {portal}",
	"email.access_rejected.body":    "{brand} did not approve your request for access to {portal}.\n\n{reason}",
	"email.zip_ready.subject":       "Your download from {portal} is ready",
	"email.zip_ready.body":          "Your download with {files} files is ready. It stays available for {expires_days} days:\n{link}",
	"email.password_reset.subject":  "Reset your password for {portal}",
	"email.password_reset.body":     "Someone (hopefully you) asked to reset your password. Choose a new one with this link (valid for 1 hour, works once):\n{link}\n\nDid not ask for this? Ignore this e-mail.",
}

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

// sendMail renders email.<kind>.subject/body in lang and sends it. It
// returns whether a real e-mail was sent.
func (a *App) sendMail(ctx context.Context, to, lang, kind string, vars map[string]string) bool {
	lang = supportedLang(lang)
	s := a.Settings(ctx)
	if vars["portal"] == "" {
		vars["portal"] = a.PortalName(ctx, lang)
	}
	if vars["brand"] == "" {
		vars["brand"] = s.Branding.DisplayName
		if vars["brand"] == "" {
			vars["brand"] = s.Company.Name
		}
	}
	subject := interpolate(a.text(lang, "email."+kind+".subject"), vars)
	body := interpolate(a.text(lang, "email."+kind+".body"), vars)
	if vars["name"] != "" {
		body = interpolate(a.text(lang, "email.greeting"), vars) + "\n\n" + body
	}
	body += "\n\n-- \n" + interpolate(a.text(lang, "email.footer"), vars) + "\n"
	if err := a.Mail.Send(ctx, to, subject, body); err != nil {
		a.Log.Warn("e-mail could not be sent", "kind", kind, "error", err)
		return false
	}
	return a.Mail.Configured()
}
