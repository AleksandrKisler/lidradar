package infrastructure

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/big"
	"mime"
	"net"
	"net/mail"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"lidradar/backend/internal/mail/application"
	"lidradar/backend/internal/mail/domain"
)

// Транспорт SMTP против настоящего сервера протокола в процессе теста: шифрование обязательно, пароль не
// уходит без TLS, ответы сервера превращаются в классифицированные отказы, письмо собрано безопасно.

// smtpScript задаёт поведение тестового сервера.
type smtpScript struct {
	greeting      string
	offerSTARTTLS bool
	implicitTLS   bool
	tlsConfig     *tls.Config
	user, pass    string
	authReply     string
	rcptReply     string
	dataReply     string
	stall         bool
}

type smtpCapture struct {
	connections int
	sawAuth     bool
	authOverTLS bool
	sawMail     bool
	from        string
	rcpt        []string
	data        string
	sawQuit     bool
}

type smtpServer struct {
	listener net.Listener
	script   smtpScript
	mu       sync.Mutex
	capture  smtpCapture
}

func (s *smtpServer) port() int { return s.listener.Addr().(*net.TCPAddr).Port }

func (s *smtpServer) snapshot() smtpCapture {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.capture
}

func startSMTP(t *testing.T, script smtpScript) *smtpServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &smtpServer{listener: listener, script: script}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go server.handle(connection)
		}
	}()
	return server
}

func (s *smtpServer) handle(raw net.Conn) {
	defer raw.Close()
	s.mu.Lock()
	s.capture.connections++
	s.mu.Unlock()
	var connection net.Conn = raw
	tlsActive := false
	if s.script.implicitTLS {
		secured := tls.Server(connection, s.script.tlsConfig)
		if secured.Handshake() != nil {
			return
		}
		connection, tlsActive = secured, true
	}
	if s.script.stall {
		_, _ = io.Copy(io.Discard, connection)
		return
	}
	reader := textproto.NewReader(bufio.NewReader(connection))
	reply := func(line string) { _, _ = fmt.Fprintf(connection, "%s\r\n", line) }
	greeting := s.script.greeting
	if greeting == "" {
		greeting = "220 localhost ESMTP ready"
	}
	reply(greeting)
	if strings.HasPrefix(greeting, "4") || strings.HasPrefix(greeting, "5") {
		return
	}
	for {
		line, err := reader.ReadLine()
		if err != nil {
			return
		}
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			reply("250-localhost")
			if s.script.offerSTARTTLS && !tlsActive {
				reply("250-STARTTLS")
			}
			reply("250-AUTH PLAIN")
			reply("250 8BITMIME")
		case upper == "STARTTLS":
			reply("220 Ready to start TLS")
			secured := tls.Server(connection, s.script.tlsConfig)
			if secured.Handshake() != nil {
				return
			}
			connection, tlsActive = secured, true
			reader = textproto.NewReader(bufio.NewReader(connection))
		case strings.HasPrefix(upper, "AUTH PLAIN"):
			s.mu.Lock()
			s.capture.sawAuth, s.capture.authOverTLS = true, tlsActive
			s.mu.Unlock()
			if s.script.authReply != "" {
				reply(s.script.authReply)
				continue
			}
			decoded, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(line[len("AUTH PLAIN"):]))
			if string(decoded) == "\x00"+s.script.user+"\x00"+s.script.pass {
				reply("235 2.7.0 Accepted")
			} else {
				reply("535 5.7.8 Username and Password not accepted")
			}
		case strings.HasPrefix(upper, "MAIL FROM:"):
			from := strings.TrimLeft(line[len("MAIL FROM:"):], " <")
			if end := strings.Index(from, ">"); end >= 0 {
				from = from[:end] // дальше идут параметры вроде BODY=8BITMIME
			}
			s.mu.Lock()
			s.capture.sawMail, s.capture.from = true, from
			s.mu.Unlock()
			reply("250 2.1.0 OK")
		case strings.HasPrefix(upper, "RCPT TO:"):
			if s.script.rcptReply != "" {
				reply(s.script.rcptReply)
				continue
			}
			s.mu.Lock()
			s.capture.rcpt = append(s.capture.rcpt, strings.Trim(line[len("RCPT TO:"):], "<> "))
			s.mu.Unlock()
			reply("250 2.1.5 OK")
		case upper == "DATA":
			reply("354 Go ahead")
			var lines []string
			for {
				dataLine, err := reader.ReadLine()
				if err != nil {
					return
				}
				if dataLine == "." {
					break
				}
				lines = append(lines, strings.TrimPrefix(dataLine, "."))
			}
			s.mu.Lock()
			s.capture.data = strings.Join(lines, "\r\n")
			s.mu.Unlock()
			if s.script.dataReply != "" {
				reply(s.script.dataReply)
			} else {
				reply("250 2.0.0 Queued")
			}
		case upper == "QUIT":
			s.mu.Lock()
			s.capture.sawQuit = true
			s.mu.Unlock()
			reply("221 2.0.0 Bye")
			return
		default:
			reply("250 OK")
		}
	}
}

func testTLS(t *testing.T) (server *tls.Config, trusted *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12},
		&tls.Config{RootCAs: roots}
}

func transportFor(t *testing.T, server *smtpServer, security string, trusted *tls.Config) SMTPTransport {
	t.Helper()
	transport, err := NewSMTPTransport(SMTPConfig{
		Host: "127.0.0.1", Port: server.port(), Security: security, Username: "bot@example.test", Password: "app-password",
		FromAddress: "Bot@Example.test", FromName: "LidRadar", TLSConfig: trusted, Timeout: 5 * time.Second,
		Now: func() time.Time { return time.Date(2026, 10, 13, 9, 30, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return transport
}

func outgoing() application.Outgoing {
	return application.Outgoing{
		ID: "0198-mail-1", To: "owner@example.com", Subject: "Сброс пароля LidRadar",
		Text: "Здравствуйте!\n\nСсылка: https://app.example.test/reset-password#token=abc\n.\n.точка в начале строки\n" + strings.Repeat("длинная строка ", 30) + "\n",
	}
}

func failureOf(t *testing.T, err error) *domain.SendFailure {
	t.Helper()
	var failure *domain.SendFailure
	if !errors.As(err, &failure) {
		t.Fatalf("error = %v, want *domain.SendFailure", err)
	}
	return failure
}

func TestSMTPSendOverSTARTTLSDeliversAWellFormedMessage(t *testing.T) {
	serverTLS, trusted := testTLS(t)
	server := startSMTP(t, smtpScript{offerSTARTTLS: true, tlsConfig: serverTLS, user: "bot@example.test", pass: "app-password"})
	message := outgoing()
	if err := transportFor(t, server, SMTPSecurityStartTLS, trusted).Send(context.Background(), message); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	capture := server.snapshot()
	if !capture.sawAuth || !capture.authOverTLS {
		t.Fatalf("credentials must be sent, and only over TLS: %+v", capture)
	}
	if capture.from != "bot@example.test" || len(capture.rcpt) != 1 || capture.rcpt[0] != "owner@example.com" || !capture.sawQuit {
		t.Fatalf("envelope = %+v", capture)
	}
	parsed, err := mail.ReadMessage(strings.NewReader(capture.data))
	if err != nil {
		t.Fatalf("the message does not parse: %v\n%s", err, capture.data)
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(parsed.Header.Get("Subject"))
	if err != nil || subject != message.Subject {
		t.Fatalf("Subject = %q (%v)", subject, err)
	}
	from, err := mail.ParseAddress(parsed.Header.Get("From"))
	if err != nil || from.Address != "bot@example.test" || from.Name != "LidRadar" {
		t.Fatalf("From = %v (%v)", parsed.Header.Get("From"), err)
	}
	if to, err := mail.ParseAddress(parsed.Header.Get("To")); err != nil || to.Address != "owner@example.com" {
		t.Fatalf("To = %v (%v)", parsed.Header.Get("To"), err)
	}
	for header, want := range map[string]string{
		"Message-Id": "<0198-mail-1@example.test>", "Mime-Version": "1.0", "Content-Type": "text/plain; charset=UTF-8",
		"Content-Transfer-Encoding": "base64", "Auto-Submitted": "auto-generated", "Date": "Tue, 13 Oct 2026 09:30:00 +0000",
	} {
		if got := parsed.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	body, _ := io.ReadAll(parsed.Body)
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\r\n") {
		if len(line) > 76 {
			t.Fatalf("a base64 line is %d characters long: SMTP lines must stay short", len(line))
		}
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.TrimSpace(string(body)), "\r\n", ""))
	if err != nil || string(decoded) != message.Text {
		t.Fatalf("the body did not survive transport (%v):\n%q\nwant\n%q", err, decoded, message.Text)
	}
}

func TestSMTPSendOverImplicitTLS(t *testing.T) {
	serverTLS, trusted := testTLS(t)
	server := startSMTP(t, smtpScript{implicitTLS: true, tlsConfig: serverTLS, user: "bot@example.test", pass: "app-password"})
	if err := transportFor(t, server, SMTPSecurityTLS, trusted).Send(context.Background(), outgoing()); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if capture := server.snapshot(); !capture.sawAuth || !capture.authOverTLS || capture.data == "" {
		t.Fatalf("capture = %+v", capture)
	}
}

// Сервер без STARTTLS не должен получить ни пароля, ни письма: понижение до открытого канала исключено.
func TestSMTPRefusesToDowngradeToAnUnencryptedChannel(t *testing.T) {
	serverTLS, trusted := testTLS(t)
	server := startSMTP(t, smtpScript{offerSTARTTLS: false, tlsConfig: serverTLS, user: "bot@example.test", pass: "app-password"})
	err := transportFor(t, server, SMTPSecurityStartTLS, trusted).Send(context.Background(), outgoing())
	if failure := failureOf(t, err); failure.Code != domain.CodeSMTPTLS || !failure.Retryable {
		t.Fatalf("failure = %+v", failure)
	}
	if capture := server.snapshot(); capture.sawAuth || capture.sawMail || capture.data != "" {
		t.Fatalf("nothing may be sent without TLS: %+v", capture)
	}
}

func TestSMTPRejectsAnUntrustedCertificateBeforeSendingCredentials(t *testing.T) {
	serverTLS, _ := testTLS(t)
	server := startSMTP(t, smtpScript{offerSTARTTLS: true, tlsConfig: serverTLS, user: "bot@example.test", pass: "app-password"})
	// Корневых сертификатов тестового сервера у клиента нет: проверка обязана провалиться.
	for _, security := range []string{SMTPSecurityStartTLS} {
		err := transportFor(t, server, security, nil).Send(context.Background(), outgoing())
		if failure := failureOf(t, err); failure.Code != domain.CodeSMTPTLS {
			t.Fatalf("%s: failure = %+v", security, failure)
		}
	}
	if capture := server.snapshot(); capture.sawAuth || capture.sawMail {
		t.Fatalf("credentials must not reach an unverified server: %+v", capture)
	}
	implicit := startSMTP(t, smtpScript{implicitTLS: true, tlsConfig: serverTLS, user: "bot@example.test", pass: "app-password"})
	err := transportFor(t, implicit, SMTPSecurityTLS, nil).Send(context.Background(), outgoing())
	if failure := failureOf(t, err); failure.Code != domain.CodeSMTPTLS {
		t.Fatalf("implicit TLS: failure = %+v", failure)
	}
	if capture := implicit.snapshot(); capture.sawAuth {
		t.Fatalf("credentials must not reach an unverified server: %+v", capture)
	}
}

func TestSMTPClassifiesServerReplies(t *testing.T) {
	serverTLS, trusted := testTLS(t)
	base := smtpScript{offerSTARTTLS: true, tlsConfig: serverTLS, user: "bot@example.test", pass: "app-password"}
	for name, tc := range map[string]struct {
		mutate    func(*smtpScript)
		code      string
		retryable bool
	}{
		"отказ входа — временный, чтобы очередь пережила смену пароля": {func(s *smtpScript) { s.pass = "other" }, domain.CodeSMTPAuth, true},
		"ответ 534": {func(s *smtpScript) { s.authReply = "534 5.7.9 Application-specific password required" }, domain.CodeSMTPAuth, true},
		"ответ 530": {func(s *smtpScript) { s.authReply = "530 5.7.0 Authentication required" }, domain.CodeSMTPAuth, true},
		"адрес не существует — навсегда": {func(s *smtpScript) { s.rcptReply = "550 5.1.1 The email account does not exist" }, domain.CodeSMTPPermanent, false},
		"ящик переполнен — позже":        {func(s *smtpScript) { s.rcptReply = "452 4.2.2 Mailbox full" }, domain.CodeSMTPTemporary, true},
		"сервер занят":                   {func(s *smtpScript) { s.greeting = "421 4.3.2 Service not available" }, domain.CodeSMTPTemporary, true},
		"письмо отклонено — навсегда":    {func(s *smtpScript) { s.dataReply = "554 5.7.1 Message rejected as spam" }, domain.CodeSMTPPermanent, false},
		"письмо отложено":                {func(s *smtpScript) { s.dataReply = "451 4.7.1 Greylisted, try later" }, domain.CodeSMTPTemporary, true},
	} {
		script := base
		tc.mutate(&script)
		err := transportFor(t, startSMTP(t, script), SMTPSecurityStartTLS, trusted).Send(context.Background(), outgoing())
		failure := failureOf(t, err)
		if failure.Code != tc.code || failure.Retryable != tc.retryable {
			t.Errorf("%s: failure = %+v, want %s retryable=%v", name, failure, tc.code, tc.retryable)
		}
	}
}

func TestSMTPClassifiesNetworkFailuresAsTemporary(t *testing.T) {
	// Порт, на котором никто не слушает.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	transport, err := NewSMTPTransport(SMTPConfig{Host: "127.0.0.1", Port: closed, Security: SMTPSecurityStartTLS, FromAddress: "bot@example.test", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if failure := failureOf(t, transport.Send(context.Background(), outgoing())); failure.Code != domain.CodeSMTPNetwork || !failure.Retryable {
		t.Fatalf("refused connection: %+v", failure)
	}

	// Сервер принял соединение и молчит: отправка обязана уложиться в срок, а не зависнуть.
	stalled := startSMTP(t, smtpScript{stall: true})
	slow, err := NewSMTPTransport(SMTPConfig{Host: "127.0.0.1", Port: stalled.port(), Security: SMTPSecurityStartTLS, FromAddress: "bot@example.test", Timeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	failure := failureOf(t, slow.Send(context.Background(), outgoing()))
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("a silent server held the sender for %s", elapsed)
	}
	if failure.Code != domain.CodeSMTPNetwork || !failure.Retryable {
		t.Fatalf("silent server: %+v", failure)
	}
}

func TestSMTPRefusesInjectedHeadersWithoutConnecting(t *testing.T) {
	serverTLS, trusted := testTLS(t)
	server := startSMTP(t, smtpScript{offerSTARTTLS: true, tlsConfig: serverTLS, user: "bot@example.test", pass: "app-password"})
	transport := transportFor(t, server, SMTPSecurityStartTLS, trusted)
	for name, message := range map[string]application.Outgoing{
		"тема":         {ID: "1", To: "a@example.com", Subject: "Тема\r\nBcc: victim@example.com", Text: "x"},
		"получатель":   {ID: "1", To: "a@example.com\r\nBcc: victim@example.com", Subject: "Тема", Text: "x"},
		"два адреса":   {ID: "1", To: "a@example.com, victim@example.com", Subject: "Тема", Text: "x"},
		"без темы":     {ID: "1", To: "a@example.com", Subject: "  ", Text: "x"},
		"без номера":   {ID: "", To: "a@example.com", Subject: "Тема", Text: "x"},
		"имя в адресе": {ID: "1", To: "Victim <a@example.com>", Subject: "Тема", Text: "x"},
	} {
		failure := failureOf(t, transport.Send(context.Background(), message))
		if failure.Code != domain.CodeSMTPPermanent || failure.Retryable {
			t.Errorf("%s: %+v", name, failure)
		}
	}
	if capture := server.snapshot(); capture.connections != 0 {
		t.Fatalf("a malformed message must be refused before any connection, got %d", capture.connections)
	}
}

func TestSMTPCheckVerifiesAccessWithoutSendingAnything(t *testing.T) {
	serverTLS, trusted := testTLS(t)
	server := startSMTP(t, smtpScript{offerSTARTTLS: true, tlsConfig: serverTLS, user: "bot@example.test", pass: "app-password"})
	if err := transportFor(t, server, SMTPSecurityStartTLS, trusted).Check(context.Background()); err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if capture := server.snapshot(); !capture.sawAuth || capture.sawMail || capture.data != "" {
		t.Fatalf("Check must log in and nothing more: %+v", capture)
	}
	wrong := startSMTP(t, smtpScript{offerSTARTTLS: true, tlsConfig: serverTLS, user: "bot@example.test", pass: "different"})
	if failure := failureOf(t, transportFor(t, wrong, SMTPSecurityStartTLS, trusted).Check(context.Background())); failure.Code != domain.CodeSMTPAuth {
		t.Fatalf("bad credentials: %+v", failure)
	}
}

func TestSMTPPlainChannelIsAllowedOnlyForLoopback(t *testing.T) {
	server := startSMTP(t, smtpScript{user: "bot@example.test", pass: "app-password"})
	transport, err := NewSMTPTransport(SMTPConfig{
		Host: "127.0.0.1", Port: server.port(), Security: SMTPSecurityNone, Username: "bot@example.test", Password: "app-password",
		FromAddress: "bot@example.test", Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.Send(context.Background(), outgoing()); err != nil {
		t.Fatalf("a local mail sink must work without TLS: %v", err)
	}
	for _, host := range []string{"smtp.example.com", "192.0.2.10", "10.0.0.5", "0.0.0.0"} {
		if _, err := NewSMTPTransport(SMTPConfig{Host: host, Port: 25, Security: SMTPSecurityNone, FromAddress: "bot@example.test"}); err == nil {
			t.Errorf("an unencrypted channel to %s must be refused", host)
		}
	}
}

func TestNewSMTPTransportValidatesItsSettings(t *testing.T) {
	valid := SMTPConfig{Host: "smtp.example.com", Port: 587, Security: SMTPSecurityStartTLS, Username: "u", Password: "p", FromAddress: "bot@example.com"}
	if _, err := NewSMTPTransport(valid); err != nil {
		t.Fatalf("a valid configuration must be accepted: %v", err)
	}
	for name, mutate := range map[string]func(*SMTPConfig){
		"без хоста":               func(c *SMTPConfig) { c.Host = " " },
		"порт ноль":               func(c *SMTPConfig) { c.Port = 0 },
		"порт выше предела":       func(c *SMTPConfig) { c.Port = 70000 },
		"неизвестная защита":      func(c *SMTPConfig) { c.Security = "ssl" },
		"имя без пароля":          func(c *SMTPConfig) { c.Password = "" },
		"пароль без имени":        func(c *SMTPConfig) { c.Username = "" },
		"отправитель не адрес":    func(c *SMTPConfig) { c.FromAddress = "bot" },
		"отправитель с именем":    func(c *SMTPConfig) { c.FromAddress = "Bot <bot@example.com>" },
		"отправитель с переносом": func(c *SMTPConfig) { c.FromAddress = "bot@example.com\r\nBcc: x@y.z" },
	} {
		config := valid
		mutate(&config)
		if _, err := NewSMTPTransport(config); err == nil {
			t.Errorf("%s: the configuration must be refused", name)
		}
	}
	// Пустая защита означает STARTTLS, а не открытый канал.
	config := valid
	config.Security = ""
	if transport, err := NewSMTPTransport(config); err != nil || transport.config.Security != SMTPSecurityStartTLS {
		t.Fatalf("empty security must default to STARTTLS: %v", err)
	}
}

func TestTLSConfigCannotWeakenVerification(t *testing.T) {
	transport, err := NewSMTPTransport(SMTPConfig{
		Host: "smtp.example.com", Port: 587, FromAddress: "bot@example.com",
		TLSConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS10, ServerName: "evil.example"},
	})
	if err != nil {
		t.Fatal(err)
	}
	config := transport.tlsConfig()
	if config.InsecureSkipVerify || config.MinVersion < tls.VersionTLS12 || config.ServerName != "smtp.example.com" {
		t.Fatalf("tls config = %+v: certificate checks, TLS 1.2 and the host name are not negotiable", config)
	}
}

func TestClassifySMTPCoversEveryFamilyOfError(t *testing.T) {
	for name, tc := range map[string]struct {
		err       error
		code      string
		retryable bool
	}{
		"nil":               {nil, "", false},
		"EOF":               {io.EOF, domain.CodeSMTPNetwork, true},
		"обрыв":             {io.ErrUnexpectedEOF, domain.CodeSMTPNetwork, true},
		"сетевая ошибка":    {&net.OpError{Op: "dial", Err: errors.New("refused")}, domain.CodeSMTPNetwork, true},
		"хост не совпал":    {x509.HostnameError{Host: "a"}, domain.CodeSMTPTLS, true},
		"неизвестный центр": {x509.UnknownAuthorityError{}, domain.CodeSMTPTLS, true},
		"прочее":            {errors.New("unexpected"), domain.CodeSMTPUnspecified, true},
		"2xx как ошибка":    {&textproto.Error{Code: 250}, domain.CodeSMTPUnspecified, true},
		"уже классифицированная": {&domain.SendFailure{Code: domain.CodeExpired}, domain.CodeExpired, false},
	} {
		got := classifySMTP(tc.err)
		if tc.err == nil {
			if got != nil {
				t.Errorf("%s: %+v", name, got)
			}
			continue
		}
		if got.Code != tc.code || got.Retryable != tc.retryable {
			t.Errorf("%s: %+v, want %s retryable=%v", name, got, tc.code, tc.retryable)
		}
	}
	if got := classifySMTP(&textproto.Error{Code: 599, Msg: "x"}); got.Retryable || got.Code != domain.CodeSMTPPermanent {
		t.Errorf("599: %+v", got)
	}
	if got := classifySMTP(&textproto.Error{Code: 400, Msg: "x"}); !got.Retryable || got.Code != domain.CodeSMTPTemporary {
		t.Errorf("400: %+v", got)
	}
	if strconv.Itoa(587) != "587" { // защита от подмены strconv в тестовой сборке
		t.Fatal("unreachable")
	}
}
