// Package domain описывает общие фоновые задания и проверки по расписанию.
package domain

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

var (
	ErrInvalid   = errors.New("invalid background work")
	ErrConflict  = errors.New("background work conflict")
	ErrLeaseLost = errors.New("background work lease lost")
)

const DefaultMaxAttempts = 5

type Status string

const (
	StatusPending    Status = "PENDING"
	StatusProcessing Status = "PROCESSING"
	StatusRetry      Status = "RETRY"
	StatusSucceeded  Status = "SUCCEEDED"
	StatusDead       Status = "DEAD"
)

func (status Status) Valid() bool {
	switch status {
	case StatusPending, StatusProcessing, StatusRetry, StatusSucceeded, StatusDead:
		return true
	default:
		return false
	}
}

// Job — долговечное задание. DedupKey задаёт логическую идентичность операции,
// а ID обязан использоваться обработчиком как ключ идемпотентности побочного эффекта.
type Job struct {
	ID            string
	TenantID      string
	Type          string
	DedupKey      string
	Payload       json.RawMessage
	Status        Status
	Priority      int16
	AvailableAt   time.Time
	AttemptCount  int
	MaxAttempts   int
	LeaseOwner    *string
	LeaseUntil    *time.Time
	LastErrorCode *string
	CompletedAt   *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// NewJob проверяет новое задание до обращения к PostgreSQL.
func NewJob(
	id, tenantID, jobType, dedupKey string,
	payload json.RawMessage,
	priority int16,
	availableAt, createdAt time.Time,
) (Job, error) {
	job := Job{
		ID: id, TenantID: tenantID, Type: strings.TrimSpace(jobType), DedupKey: strings.TrimSpace(dedupKey),
		Payload: append(json.RawMessage(nil), payload...), Status: StatusPending, Priority: priority,
		AvailableAt: availableAt.UTC(), MaxAttempts: DefaultMaxAttempts,
		CreatedAt: createdAt.UTC(), UpdatedAt: createdAt.UTC(),
	}
	if job.Validate() != nil {
		return Job{}, ErrInvalid
	}
	return job, nil
}

func (job Job) Validate() error {
	if job.ID == "" || job.TenantID == "" || !validName(job.Type, 100) || !validName(job.DedupKey, 512) ||
		!jsonObject(job.Payload) || !job.Status.Valid() || job.AvailableAt.IsZero() || job.MaxAttempts < 1 ||
		job.MaxAttempts > 20 || job.AttemptCount < 0 || job.AttemptCount > job.MaxAttempts ||
		job.CreatedAt.IsZero() || job.UpdatedAt.IsZero() {
		return ErrInvalid
	}
	if job.LastErrorCode != nil && !validErrorCode(*job.LastErrorCode) {
		return ErrInvalid
	}
	switch job.Status {
	case StatusProcessing:
		if job.LeaseOwner == nil || strings.TrimSpace(*job.LeaseOwner) == "" || job.LeaseUntil == nil || job.CompletedAt != nil {
			return ErrInvalid
		}
	case StatusPending, StatusRetry:
		if job.LeaseOwner != nil || job.LeaseUntil != nil || job.CompletedAt != nil {
			return ErrInvalid
		}
	case StatusSucceeded, StatusDead:
		if job.LeaseOwner != nil || job.LeaseUntil != nil || job.CompletedAt == nil {
			return ErrInvalid
		}
	}
	return nil
}

type CheckStatus string

const (
	CheckScheduled CheckStatus = "SCHEDULED"
	CheckEnqueued  CheckStatus = "ENQUEUED"
	CheckCancelled CheckStatus = "CANCELLED"
)

type ScheduledCheck struct {
	ID          string
	TenantID    string
	Type        string
	SubjectType string
	SubjectID   string
	JobType     string
	DedupKey    string
	Payload     json.RawMessage
	DueAt       time.Time
	Status      CheckStatus
	JobID       *string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// NewScheduledCheck создаёт проверку, которую scheduler позже превратит в Job.
func NewScheduledCheck(
	id, tenantID, checkType, subjectType, subjectID, jobType, dedupKey string,
	payload json.RawMessage,
	dueAt, createdAt time.Time,
) (ScheduledCheck, error) {
	check := ScheduledCheck{
		ID: id, TenantID: tenantID, Type: strings.TrimSpace(checkType), SubjectType: strings.TrimSpace(subjectType),
		SubjectID: subjectID, JobType: strings.TrimSpace(jobType), DedupKey: strings.TrimSpace(dedupKey),
		Payload: append(json.RawMessage(nil), payload...), DueAt: dueAt.UTC(), Status: CheckScheduled,
		CreatedAt: createdAt.UTC(), UpdatedAt: createdAt.UTC(),
	}
	if check.Validate() != nil {
		return ScheduledCheck{}, ErrInvalid
	}
	return check, nil
}

func (check ScheduledCheck) Validate() error {
	if check.ID == "" || check.TenantID == "" || check.SubjectID == "" || !validName(check.Type, 100) ||
		!validName(check.SubjectType, 100) || !validName(check.JobType, 100) || !validName(check.DedupKey, 512) ||
		!jsonObject(check.Payload) || check.DueAt.IsZero() || check.CreatedAt.IsZero() || check.UpdatedAt.IsZero() {
		return ErrInvalid
	}
	switch check.Status {
	case CheckScheduled, CheckCancelled:
		if check.JobID != nil {
			return ErrInvalid
		}
	case CheckEnqueued:
		if check.JobID == nil || *check.JobID == "" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

type FailureKind uint8

const (
	FailureRetryable FailureKind = iota + 1
	FailurePermanent
)

// Failure содержит только безопасный код, пригодный для долговечного журнала.
// Исходная ошибка остаётся в процессе и не записывается в базу автоматически.
type Failure struct {
	kind FailureKind
	code string
	err  error
}

func (failure *Failure) Error() string {
	if failure == nil || failure.err == nil {
		return "background work failed"
	}
	return failure.err.Error()
}

func (failure *Failure) Unwrap() error { return failure.err }

func Retryable(code string, err error) error { return newFailure(FailureRetryable, code, err) }
func Permanent(code string, err error) error { return newFailure(FailurePermanent, code, err) }

func newFailure(kind FailureKind, code string, err error) error {
	code = strings.TrimSpace(code)
	if err == nil {
		err = errors.New("background work failed")
	}
	if !validErrorCode(code) {
		code = "UNCLASSIFIED_FAILURE"
	}
	return &Failure{kind: kind, code: code, err: err}
}

// Classify считает неизвестную ошибку временной: потерять работу опаснее,
// чем довести её до DEAD после ограниченного числа попыток.
func Classify(err error) (retryable bool, code string) {
	var failure *Failure
	if errors.As(err, &failure) {
		return failure.kind == FailureRetryable, failure.code
	}
	return true, "UNCLASSIFIED_FAILURE"
}

// RetryDelay возвращает базовую сетку из ТЗ. Номер — уже выполненная попытка.
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

var errorCodePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,99}$`)

func validErrorCode(value string) bool { return errorCodePattern.MatchString(value) }

func validName(value string, maximum int) bool {
	return value == strings.TrimSpace(value) && len(value) >= 1 && len(value) <= maximum
}

func jsonObject(value json.RawMessage) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(value, &object) == nil && object != nil
}
