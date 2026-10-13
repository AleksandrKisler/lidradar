package infrastructure

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"lidradar/backend/internal/mail/application"
	"lidradar/backend/internal/mail/domain"
)

// Способы защиты SMTP-соединения (ADR 0058). Открытый канал допустим только для локального приёмника писем.
const (
	SMTPSecurityStartTLS = "starttls"
	SMTPSecurityTLS      = "tls"
	SMTPSecurityNone     = "none"
)

const defaultSMTPTimeout = 30 * time.Second

// SMTPConfig — настройки отправки. Пароль нигде не печатается: структуру не выводят в журнал целиком.
type SMTPConfig struct {
	Host     string
	Port     int
	Security string
	Username string
	Password string
	// FromAddress и FromName — отправитель письма. Google подменяет адрес адресом аккаунта, если другой
	// не настроен как псевдоним.
	FromAddress string
	FromName    string
	// Timeout ограничивает всю отправку одного письма; ноль означает 30 секунд.
	Timeout time.Duration
	// TLSConfig задаёт основу настроек TLS (корневые сертификаты в тестах). ServerName и минимальная
	// версия TLS подставляются всегда, проверка сертификата отключаться не может.
	TLSConfig *tls.Config
	// Now подставляется в тестах; по умолчанию time.Now.
	Now func() time.Time
}

// SMTPTransport отправляет письма по SMTP на стандартной библиотеке.
type SMTPTransport struct {
	config SMTPConfig
	from   mail.Address
}

// NewSMTPTransport проверяет настройки: защита соединения известна, отправитель разбирается, вход без
// пароля возможен только по открытому каналу к локальному хосту.
func NewSMTPTransport(config SMTPConfig) (SMTPTransport, error) {
	config.Host = strings.TrimSpace(config.Host)
	config.Security = strings.ToLower(strings.TrimSpace(config.Security))
	if config.Security == "" {
		config.Security = SMTPSecurityStartTLS
	}
	fromAddress, err := domain.NormalizeAddress(config.FromAddress)
	if err != nil || config.Host == "" || config.Port < 1 || config.Port > 65535 {
		return SMTPTransport{}, errors.New("invalid SMTP configuration")
	}
	switch config.Security {
	case SMTPSecurityStartTLS, SMTPSecurityTLS:
	case SMTPSecurityNone:
		if !loopbackHost(config.Host) {
			return SMTPTransport{}, errors.New("unencrypted SMTP is allowed only for a loopback host")
		}
	default:
		return SMTPTransport{}, errors.New("invalid SMTP security mode")
	}
	if (config.Username == "") != (config.Password == "") {
		return SMTPTransport{}, errors.New("SMTP username and password must be set together")
	}
	if config.Timeout <= 0 {
		config.Timeout = defaultSMTPTimeout
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	config.FromAddress = fromAddress
	return SMTPTransport{config: config, from: mail.Address{Name: strings.TrimSpace(config.FromName), Address: fromAddress}}, nil
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	address := net.ParseIP(strings.Trim(host, "[]"))
	return address != nil && address.IsLoopback()
}

// Send доставляет письмо. Любой отказ возвращается как *domain.SendFailure.
func (t SMTPTransport) Send(ctx context.Context, message application.Outgoing) error {
	to, err := domain.NormalizeAddress(message.To)
	if err != nil || message.ID == "" || strings.TrimSpace(message.Subject) == "" || strings.ContainsAny(message.Subject, "\r\n") {
		return &domain.SendFailure{Code: domain.CodeSMTPPermanent, Err: errors.New("invalid message")}
	}
	client, closeConnection, err := t.connect(ctx)
	if err != nil {
		return err
	}
	defer closeConnection()
	if err := client.Mail(t.from.Address); err != nil {
		return classifySMTP(err)
	}
	if err := client.Rcpt(to); err != nil {
		return classifySMTP(err)
	}
	writer, err := client.Data()
	if err != nil {
		return classifySMTP(err)
	}
	if _, err := writer.Write(t.render(message, to)); err != nil {
		_ = writer.Close()
		return classifySMTP(err)
	}
	if err := writer.Close(); err != nil {
		return classifySMTP(err)
	}
	// Письмо принято сервером; сбой закрытия сессии его не отменяет.
	_ = client.Quit()
	return nil
}

// Check проверяет доступность сервера без отправки: соединение, TLS и вход. Воркер вызывает её при старте.
func (t SMTPTransport) Check(ctx context.Context) error {
	client, closeConnection, err := t.connect(ctx)
	if err != nil {
		return err
	}
	defer closeConnection()
	_ = client.Quit()
	return nil
}

// connect открывает сессию, включает защиту и входит. Возвращает функцию закрытия соединения.
func (t SMTPTransport) connect(ctx context.Context) (*smtp.Client, func(), error) {
	deadline := time.Now().Add(t.config.Timeout)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	dialer := net.Dialer{Deadline: deadline}
	connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(t.config.Host, strconv.Itoa(t.config.Port)))
	if err != nil {
		return nil, nil, classifySMTP(err)
	}
	_ = connection.SetDeadline(deadline)
	if t.config.Security == SMTPSecurityTLS {
		secured := tls.Client(connection, t.tlsConfig())
		if err := secured.HandshakeContext(ctx); err != nil {
			_ = connection.Close()
			return nil, nil, classifySMTP(err)
		}
		connection = secured
	}
	client, err := smtp.NewClient(connection, t.config.Host)
	if err != nil {
		_ = connection.Close()
		return nil, nil, classifySMTP(err)
	}
	closeConnection := func() { _ = client.Close() }
	if t.config.Security == SMTPSecurityStartTLS {
		if offered, _ := client.Extension("STARTTLS"); !offered {
			closeConnection()
			return nil, nil, &domain.SendFailure{Code: domain.CodeSMTPTLS, Retryable: true, Err: errors.New("server does not offer STARTTLS")}
		}
		if err := client.StartTLS(t.tlsConfig()); err != nil {
			closeConnection()
			return nil, nil, classifySMTP(err)
		}
	}
	if t.config.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", t.config.Username, t.config.Password, t.config.Host)); err != nil {
			closeConnection()
			return nil, nil, classifySMTP(err)
		}
	}
	return client, closeConnection, nil
}

func (t SMTPTransport) tlsConfig() *tls.Config {
	var config *tls.Config
	if t.config.TLSConfig != nil {
		config = t.config.TLSConfig.Clone()
	} else {
		config = &tls.Config{}
	}
	config.ServerName = t.config.Host
	if config.MinVersion < tls.VersionTLS12 {
		config.MinVersion = tls.VersionTLS12
	}
	config.InsecureSkipVerify = false
	return config
}

// render собирает письмо: заголовки из проверенных значений, тема в RFC 2047, тело в base64 (UTF-8).
func (t SMTPTransport) render(message application.Outgoing, to string) []byte {
	_, senderDomain, _ := strings.Cut(t.from.Address, "@")
	var out bytes.Buffer
	header := func(name, value string) { out.WriteString(name + ": " + value + "\r\n") }
	header("From", t.from.String())
	header("To", (&mail.Address{Address: to}).String())
	header("Subject", mime.QEncoding.Encode("utf-8", message.Subject))
	header("Date", t.config.Now().UTC().Format(time.RFC1123Z))
	header("Message-ID", "<"+message.ID+"@"+senderDomain+">")
	header("MIME-Version", "1.0")
	header("Content-Type", "text/plain; charset=UTF-8")
	header("Content-Transfer-Encoding", "base64")
	header("Auto-Submitted", "auto-generated")
	header("X-Auto-Response-Suppress", "All")
	out.WriteString("\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(message.Text))
	for len(encoded) > 76 {
		out.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	out.WriteString(encoded + "\r\n")
	return out.Bytes()
}

// classifySMTP переводит ошибку net/smtp в отказ с кодом и признаком повтора. Отказ входа (530, 534, 535)
// считается временным: после исправления пароля очередь должна доработать, а не умереть.
func classifySMTP(err error) *domain.SendFailure {
	if err == nil {
		return nil
	}
	var failure *domain.SendFailure
	if errors.As(err, &failure) {
		return failure
	}
	var protocol *textproto.Error
	if errors.As(err, &protocol) {
		switch {
		case protocol.Code == 530 || protocol.Code == 534 || protocol.Code == 535:
			return &domain.SendFailure{Code: domain.CodeSMTPAuth, Retryable: true, Err: err}
		case protocol.Code >= 400 && protocol.Code < 500:
			return &domain.SendFailure{Code: domain.CodeSMTPTemporary, Retryable: true, Err: err}
		case protocol.Code >= 500:
			return &domain.SendFailure{Code: domain.CodeSMTPPermanent, Err: err}
		}
	}
	var certificate x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var verification *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	if errors.As(err, &certificate) || errors.As(err, &hostname) || errors.As(err, &invalid) ||
		errors.As(err, &verification) || errors.As(err, &record) {
		return &domain.SendFailure{Code: domain.CodeSMTPTLS, Retryable: true, Err: err}
	}
	var network net.Error
	if errors.As(err, &network) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return &domain.SendFailure{Code: domain.CodeSMTPNetwork, Retryable: true, Err: err}
	}
	return &domain.SendFailure{Code: domain.CodeSMTPUnspecified, Retryable: true, Err: fmt.Errorf("smtp: %w", err)}
}

var _ application.Transport = SMTPTransport{}
