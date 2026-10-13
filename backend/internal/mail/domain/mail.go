// Package domain describes outbound transactional mail: what a message may contain and how delivery
// attempts end (ADR 0058).
package domain

import (
	"errors"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	ErrInvalid = errors.New("invalid mail")
	// ErrLeaseLost: письмо уже завершено или подобрано другим исполнителем; результат этой попытки не записывается.
	ErrLeaseLost = errors.New("mail lease lost")
)

const (
	// MaxAttempts — сколько раз письмо пробуют отправить, считая попытки с истёкшей арендой.
	MaxAttempts = 8
	// DefaultLease — на сколько письмо арендуется на одну попытку; больше срока отправки SMTP.
	DefaultLease = 2 * time.Minute

	maxAddressLength = 254
	maxSubjectRunes  = 200
	maxTextBytes     = 20000
)

// Kind — вид письма для диагностики и метрик; значения задают модули-отправители.
type Kind string

const (
	KindEmailVerification Kind = "EMAIL_VERIFICATION"
	KindPasswordReset     Kind = "PASSWORD_RESET"
	KindPasswordChanged   Kind = "PASSWORD_CHANGED"
)

var kindPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,49}$`)

// Status — состояние письма в очереди: PENDING и PROCESSING хранят текст и адрес, SENT и DEAD их стирают.
type Status string

const (
	StatusPending    Status = "PENDING"
	StatusProcessing Status = "PROCESSING"
	StatusSent       Status = "SENT"
	StatusDead       Status = "DEAD"
)

// Коды сбоев доставки. Хранятся в базе и попадают в журнал, поэтому не содержат ни адреса, ни текста.
const (
	CodeSMTPTemporary   = "SMTP_TEMPORARY"
	CodeSMTPPermanent   = "SMTP_PERMANENT"
	CodeSMTPAuth        = "SMTP_AUTH"
	CodeSMTPNetwork     = "SMTP_NETWORK"
	CodeSMTPTLS         = "SMTP_TLS"
	CodeExpired         = "EXPIRED"
	CodeAttemptsSpent   = "ATTEMPTS_EXHAUSTED"
	CodeSMTPUnspecified = "SMTP_ERROR"
)

// Message — письмо, поставленное в очередь. Тело — обычный текст UTF-8.
type Message struct {
	ID        string
	Kind      Kind
	To        string
	Subject   string
	Text      string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// NewMessage проверяет письмо целиком: адрес без имени, тема без управляющих символов (защита от внедрения
// заголовков), ограниченные тема и текст, срок позже создания.
func NewMessage(id string, kind Kind, to, subject, text string, createdAt, expiresAt time.Time) (Message, error) {
	address, err := NormalizeAddress(to)
	subject = strings.TrimSpace(subject)
	if err != nil || id == "" || !kindPattern.MatchString(string(kind)) ||
		subject == "" || utf8.RuneCountInString(subject) > maxSubjectRunes || hasControl(subject) ||
		!validText(text) || createdAt.IsZero() || !expiresAt.After(createdAt) {
		return Message{}, ErrInvalid
	}
	return Message{
		ID: id, Kind: kind, To: address, Subject: subject, Text: text,
		CreatedAt: createdAt.UTC(), ExpiresAt: expiresAt.UTC(),
	}, nil
}

// NormalizeAddress приводит адрес получателя к нижнему регистру и отвергает всё, кроме простого адреса.
func NormalizeAddress(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Address != value || len(value) > maxAddressLength || hasControl(value) {
		return "", ErrInvalid
	}
	return value, nil
}

// RecipientDomain возвращает часть адреса после @: единственное, что остаётся в строке после отправки.
func (message Message) RecipientDomain() string {
	_, domain, found := strings.Cut(message.To, "@")
	if !found {
		return ""
	}
	return domain
}

// Delivery — письмо, арендованное для попытки отправки. Attempt считает и текущую попытку.
type Delivery struct {
	Message
	Attempt int
}

// RetryDelay возвращает паузу после неудачной попытки с данным номером (первая — 1).
func RetryDelay(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 5 * time.Second
	case 2:
		return 30 * time.Second
	case 3:
		return 2 * time.Minute
	default:
		return 10 * time.Minute
	}
}

// SendFailure — классифицированный отказ транспорта. Retryable отличает временный сбой от постоянного.
type SendFailure struct {
	Code      string
	Retryable bool
	Err       error
}

func (failure *SendFailure) Error() string {
	if failure.Err == nil {
		return failure.Code
	}
	return failure.Code + ": " + failure.Err.Error()
}

func (failure *SendFailure) Unwrap() error { return failure.Err }

func hasControl(value string) bool {
	return strings.IndexFunc(value, unicode.IsControl) >= 0
}

// validText допускает переводы строк, но не другие управляющие символы.
func validText(text string) bool {
	if text == "" || len(text) > maxTextBytes || !utf8.ValidString(text) {
		return false
	}
	for _, character := range text {
		if unicode.IsControl(character) && character != '\n' && character != '\r' && character != '\t' {
			return false
		}
	}
	return true
}
