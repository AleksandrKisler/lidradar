// Package application coordinates the authenticated, pull-based AI queue.
package application

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"lidradar/backend/internal/ai/domain"
)

var (
	ErrUnauthorized     = errors.New("invalid AI node credentials")
	ErrInvalid          = errors.New("invalid AI request")
	ErrNotFound         = errors.New("AI resource not found")
	ErrLeaseLost        = errors.New("AI job lease is not owned by node")
	ErrReplay           = errors.New("AI node request was already accepted")
	ErrConflict         = errors.New("AI resource conflicts with existing state")
	ErrFreshnessChanged = errors.New("AI conversation freshness changed during finalization")
)

const (
	DefaultLease             = 120 * time.Second
	DefaultSignatureWindow   = 60 * time.Second
	NodeUnavailableAfter     = 60 * time.Second
	DefaultMaxAttempts       = 5
	JobTypeAnalyze           = "ANALYZE_CONVERSATION"
	EntityTypeConversation   = "CONVERSATION"
	NodeIDHeader             = "X-LidRadar-Node-ID"
	TimestampHeader          = "X-LidRadar-Timestamp"
	RequestIDHeader          = "X-LidRadar-Request-ID"
	SignatureHeader          = "X-LidRadar-Signature"
	AnalysisAppliedEventType = "ai.analysis.applied.v1"
)

var errorCodePattern = regexp.MustCompile(`^[A-Z0-9_]{1,100}$`)

type IDs interface{ NewID() (string, error) }

// Store — долговечный контракт Cloud Core. Рабочая композиция реализует его
// PostgreSQL-адаптером; память допустима только в испытаниях.
type Store interface {
	RegisterNode(context.Context, domain.Node) error
	RotateNodeSecret(context.Context, string, [32]byte, time.Time) error
	RevokeNode(context.Context, string, time.Time) error
	AuthenticateNode(context.Context, string, [32]byte) (domain.Node, bool, error)
	UseRequestNonce(context.Context, string, string, time.Time, time.Time) error
	Heartbeat(context.Context, string, domain.NodeStatus, string, int, time.Time, time.Time) error
	Enqueue(context.Context, domain.Job) (domain.Job, error)
	Claim(context.Context, string, time.Time, time.Time) (domain.Job, bool, error)
	Start(context.Context, domain.Run, time.Time) (domain.Run, error)
	Run(context.Context, string) (domain.Run, error)
	ConversationSnapshot(context.Context, string, string) (domain.ConversationSnapshot, error)
	Finalize(context.Context, Finalization) (domain.Run, error)
}

// StaleJobBuilder перечитывает PostgreSQL и строит новый контекст, когда
// завершившийся анализ уже устарел. Старый prompt повторно не используется.
type StaleJobBuilder interface {
	BuildAnalysisJob(context.Context, string, string) (EnqueueCommand, error)
}

type Finalization struct {
	NodeID, JobID, RunID string
	RunStatus            domain.RunStatus
	ApplicationStatus    domain.ApplicationStatus
	Output, ErrorCode    string
	ValidationError      string
	CompletedAt          time.Time
	Summary              *domain.ConversationSummary
	Replacement          *domain.Job
}

type Service struct {
	store           Store
	ids             IDs
	now             func() time.Time
	lease           time.Duration
	signatureWindow time.Duration
	staleBuilder    StaleJobBuilder
}

func NewService(store Store, ids IDs, now func() time.Time, lease time.Duration) Service {
	if lease <= 0 {
		lease = DefaultLease
	}
	return Service{
		store: store, ids: ids, now: now, lease: lease,
		signatureWindow: DefaultSignatureWindow,
	}
}

func (s Service) WithSignatureWindow(window time.Duration) Service {
	if window > 0 {
		s.signatureWindow = window
	}
	return s
}

func (s Service) WithStaleJobBuilder(builder StaleJobBuilder) Service {
	s.staleBuilder = builder
	return s
}

// RegisterNode returns the only plaintext association between the generated
// node ID and the caller-provided secret. Persistence receives only SHA-256.
func (s Service) RegisterNode(ctx context.Context, name, secret string) (domain.Node, error) {
	if s.ids == nil {
		return domain.Node{}, ErrInvalid
	}
	id, err := s.ids.NewID()
	if err != nil {
		return domain.Node{}, fmt.Errorf("generate AI node ID: %w", err)
	}
	return s.RegisterNodeWithID(ctx, id, name, secret)
}

// RegisterNodeWithID позволяет команде регистрации сначала надёжно записать
// единственный открытый экземпляр реквизитов, а затем создать запись в БД.
func (s Service) RegisterNodeWithID(ctx context.Context, id, name, secret string) (domain.Node, error) {
	name = strings.TrimSpace(name)
	if s.store == nil || s.now == nil || id == "" || name == "" || len(name) > 100 || len(secret) < 32 || len(secret) > 200 {
		return domain.Node{}, ErrInvalid
	}
	now := s.now().UTC()
	node := domain.Node{
		ID: id, Name: name, SecretHash: sha256.Sum256([]byte(secret)),
		Status: domain.NodeOffline, MaxInflight: 1, CreatedAt: now, UpdatedAt: now,
	}
	return node, s.store.RegisterNode(ctx, node)
}

// RotateNodeSecret немедленно делает старый секрет недействительным. Текущая
// аренда не отбирается: без heartbeat она естественно истечёт и будет выдана
// другому узлу.
func (s Service) RotateNodeSecret(ctx context.Context, id, secret string) error {
	if s.store == nil || s.now == nil || id == "" || len(secret) < 32 || len(secret) > 200 {
		return ErrInvalid
	}
	return s.store.RotateNodeSecret(ctx, id, sha256.Sum256([]byte(secret)), s.now().UTC())
}

// RevokeNode запрещает все последующие запросы узла, не удаляя историю.
func (s Service) RevokeNode(ctx context.Context, id string) error {
	if s.store == nil || s.now == nil || id == "" {
		return ErrInvalid
	}
	return s.store.RevokeNode(ctx, id, s.now().UTC())
}

func (s Service) authenticate(ctx context.Context, id, secret string) error {
	if id == "" || secret == "" || s.store == nil {
		return ErrUnauthorized
	}
	node, ok, err := s.store.AuthenticateNode(ctx, id, sha256.Sum256([]byte(secret)))
	if err != nil {
		return err
	}
	if !ok || node.Status == domain.NodeRevoked {
		return ErrUnauthorized
	}
	return nil
}

type MachineRequest struct {
	NodeID, Secret, RequestID string
	Timestamp                 time.Time
	Method, Path, Signature   string
	Body                      []byte
}

// SignMachineRequest computes the exact signature shared by Cloud Core and
// the outbound agent. Authorization contains the secret, while this signature
// binds method, path, timestamp, request ID and body hash.
func SignMachineRequest(secret, method, path, timestamp, requestID string, body []byte) string {
	bodyHash := sha256.Sum256(body)
	canonical := strings.Join([]string{
		strings.ToUpper(method), path, timestamp, requestID, hex.EncodeToString(bodyHash[:]),
	}, "\n")
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(canonical))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// AuthenticateMachineRequest rejects bad credentials, clock-skewed messages,
// a mismatched body signature and reuse of a request ID inside the replay
// window. Secrets and payloads are never included in returned errors.
func (s Service) AuthenticateMachineRequest(ctx context.Context, request MachineRequest) error {
	if s.now == nil || request.RequestID == "" || request.Timestamp.IsZero() ||
		request.Method == "" || request.Path == "" || request.Signature == "" {
		return ErrUnauthorized
	}
	if err := s.authenticate(ctx, request.NodeID, request.Secret); err != nil {
		return err
	}
	now := s.now().UTC()
	window := s.signatureWindow
	if window <= 0 {
		window = DefaultSignatureWindow
	}
	delta := now.Sub(request.Timestamp.UTC())
	if delta < -window || delta > window {
		return ErrUnauthorized
	}
	expected := SignMachineRequest(
		request.Secret, request.Method, request.Path,
		request.Timestamp.UTC().Format(time.RFC3339Nano), request.RequestID, request.Body,
	)
	if !hmac.Equal([]byte(expected), []byte(strings.ToLower(request.Signature))) {
		return ErrUnauthorized
	}
	if err := s.store.UseRequestNonce(ctx, request.NodeID, request.RequestID, now, now.Add(window)); err != nil {
		if errors.Is(err, ErrReplay) {
			return ErrUnauthorized
		}
		return err
	}
	return nil
}

type HeartbeatCommand struct {
	Status         domain.NodeStatus
	ModelVersion   string
	AvailableSlots int
}

func (s Service) Heartbeat(ctx context.Context, id, secret string, command HeartbeatCommand) error {
	if err := s.authenticate(ctx, id, secret); err != nil {
		return err
	}
	command.ModelVersion = strings.TrimSpace(command.ModelVersion)
	if (command.Status != domain.NodeReady && command.Status != domain.NodeOffline) ||
		command.AvailableSlots < 0 || command.AvailableSlots > 1 ||
		(command.Status == domain.NodeReady && command.ModelVersion == "") ||
		(command.Status != domain.NodeReady && command.AvailableSlots != 0) {
		return ErrInvalid
	}
	now := s.now().UTC()
	return s.store.Heartbeat(ctx, id, command.Status, command.ModelVersion, command.AvailableSlots, now, now.Add(s.lease))
}

type EnqueueCommand struct {
	TenantID, ConversationID, Prompt, AnalysisThroughMessageID string
	BaseConversationRevision                                   int64
	ModelVersion, PromptVersion, SchemaVersion                 string
	Priority                                                   int
}

func (s Service) Enqueue(ctx context.Context, command EnqueueCommand) (domain.Job, error) {
	if s.store == nil || s.ids == nil || s.now == nil || command.TenantID == "" ||
		command.ConversationID == "" || strings.TrimSpace(command.Prompt) == "" ||
		command.AnalysisThroughMessageID == "" || command.BaseConversationRevision < 1 ||
		command.Priority < -100 || command.Priority > 100 {
		return domain.Job{}, ErrInvalid
	}
	job, err := s.newJob(command)
	if err != nil {
		return domain.Job{}, err
	}
	return s.store.Enqueue(ctx, job)
}

func (s Service) Claim(ctx context.Context, id, secret string) (domain.Job, bool, error) {
	if err := s.authenticate(ctx, id, secret); err != nil {
		return domain.Job{}, false, err
	}
	now := s.now().UTC()
	return s.store.Claim(ctx, id, now, now.Add(s.lease))
}

func (s Service) Started(ctx context.Context, id, secret, jobID string) (domain.Run, error) {
	if err := s.authenticate(ctx, id, secret); err != nil {
		return domain.Run{}, err
	}
	if jobID == "" {
		return domain.Run{}, ErrInvalid
	}
	runID, err := s.ids.NewID()
	if err != nil {
		return domain.Run{}, fmt.Errorf("generate AI run ID: %w", err)
	}
	now := s.now().UTC()
	return s.store.Start(ctx, domain.Run{
		ID: runID, JobID: jobID, NodeID: id, Status: domain.RunRunning,
		ApplicationStatus: domain.ApplicationPending, StartedAt: now,
	}, now)
}

func (s Service) Complete(ctx context.Context, id, secret, jobID, runID, output string) (domain.Run, error) {
	if err := s.authenticate(ctx, id, secret); err != nil {
		return domain.Run{}, err
	}
	if jobID == "" || runID == "" || output == "" {
		return domain.Run{}, ErrInvalid
	}
	run, err := s.store.Run(ctx, runID)
	if err != nil {
		return domain.Run{}, err
	}
	if run.JobID != jobID || run.NodeID != id {
		return domain.Run{}, ErrLeaseLost
	}
	if run.Status == domain.RunSucceeded && run.Output == output {
		return run, nil
	}
	if run.Status != domain.RunRunning {
		return domain.Run{}, ErrLeaseLost
	}
	result, validationErr := ValidateAnalysisResultV1(output, run.AnalysisThroughMessageID)
	applicationStatus := domain.ApplicationApplied
	validationMessage := ""
	if validationErr != nil {
		applicationStatus = domain.ApplicationRejected
		validationMessage = validationErr.Error()
	}

	var summary *domain.ConversationSummary
	var replacement *domain.Job
	if validationErr == nil {
		snapshot, snapshotErr := s.store.ConversationSnapshot(ctx, run.TenantID, run.ConversationID)
		if snapshotErr != nil {
			return domain.Run{}, snapshotErr
		}
		if snapshot.Revision != run.BaseConversationRevision || snapshot.LastMessageID != run.AnalysisThroughMessageID {
			applicationStatus = domain.ApplicationStale
			if s.staleBuilder != nil {
				command, buildErr := s.staleBuilder.BuildAnalysisJob(ctx, run.TenantID, run.ConversationID)
				if buildErr != nil {
					return domain.Run{}, buildErr
				}
				job, enqueueErr := s.newJob(command)
				if enqueueErr != nil {
					return domain.Run{}, enqueueErr
				}
				replacement = &job
			}
		} else {
			now := s.now().UTC()
			summary = &domain.ConversationSummary{
				TenantID: run.TenantID, ConversationID: run.ConversationID,
				Text: strings.TrimSpace(result.Summary), BaseConversationRevision: snapshot.Revision,
				AnalysisThroughMessageID: snapshot.LastMessageID, ModelVersion: run.ModelVersion,
				PromptVersion: run.PromptVersion, SchemaVersion: run.SchemaVersion,
				RunID: run.ID, Facts: TrustedFacts(result), UpdatedAt: now,
			}
		}
	}
	finalization := Finalization{
		NodeID: id, JobID: jobID, RunID: runID, RunStatus: domain.RunSucceeded,
		ApplicationStatus: applicationStatus, Output: output,
		ValidationError: validationMessage, CompletedAt: s.now().UTC(),
		Summary: summary, Replacement: replacement,
	}
	finalized, err := s.store.Finalize(ctx, finalization)
	if !errors.Is(err, ErrFreshnessChanged) {
		return finalized, err
	}

	// Переписка могла измениться между предварительным чтением снимка и
	// транзакцией завершения. В этом случае старый результат фиксируется только
	// как STALE, а новое задание строится из повторно прочитанных данных.
	finalization.ApplicationStatus = domain.ApplicationStale
	finalization.Summary = nil
	finalization.Replacement = nil
	if s.staleBuilder != nil {
		command, buildErr := s.staleBuilder.BuildAnalysisJob(ctx, run.TenantID, run.ConversationID)
		if buildErr != nil {
			return domain.Run{}, buildErr
		}
		job, enqueueErr := s.newJob(command)
		if enqueueErr != nil {
			return domain.Run{}, enqueueErr
		}
		finalization.Replacement = &job
	}
	finalization.CompletedAt = s.now().UTC()
	return s.store.Finalize(ctx, finalization)
}

func (s Service) newJob(command EnqueueCommand) (domain.Job, error) {
	if command.TenantID == "" || command.ConversationID == "" || strings.TrimSpace(command.Prompt) == "" ||
		command.AnalysisThroughMessageID == "" || command.BaseConversationRevision < 1 ||
		command.Priority < -100 || command.Priority > 100 {
		return domain.Job{}, ErrInvalid
	}
	id, err := s.ids.NewID()
	if err != nil {
		return domain.Job{}, fmt.Errorf("generate AI job ID: %w", err)
	}
	if command.ModelVersion == "" {
		command.ModelVersion = DefaultModelVersion
	}
	if command.PromptVersion == "" {
		command.PromptVersion = CurrentAnalysisPrompt
	}
	if command.SchemaVersion == "" {
		command.SchemaVersion = AnalysisSchemaV1
	}
	now := s.now().UTC()
	return domain.Job{
		ID: id, TenantID: command.TenantID, JobType: JobTypeAnalyze,
		EntityType: EntityTypeConversation, ConversationID: command.ConversationID,
		Prompt: command.Prompt, BaseConversationRevision: command.BaseConversationRevision,
		AnalysisThroughMessageID: command.AnalysisThroughMessageID,
		ModelVersion:             command.ModelVersion, PromptVersion: command.PromptVersion,
		SchemaVersion: command.SchemaVersion, Priority: command.Priority,
		Status: domain.JobPending, MaxAttempts: DefaultMaxAttempts,
		AvailableAt: now, CreatedAt: now, UpdatedAt: now,
	}, nil
}

func (s Service) Failed(ctx context.Context, id, secret, jobID, runID, errorCode string) (domain.Run, error) {
	if err := s.authenticate(ctx, id, secret); err != nil {
		return domain.Run{}, err
	}
	if jobID == "" || runID == "" || !errorCodePattern.MatchString(errorCode) {
		return domain.Run{}, ErrInvalid
	}
	return s.store.Finalize(ctx, Finalization{
		NodeID: id, JobID: jobID, RunID: runID, RunStatus: domain.RunFailed,
		ApplicationStatus: domain.ApplicationRejected, ErrorCode: errorCode,
		CompletedAt: s.now().UTC(),
	})
}

// BearerSecret returns a credential without reflecting malformed input into an
// error. Transport uses it only after limiting request headers and body.
func BearerSecret(header string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return ""
	}
	secret := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	if len(secret) < 32 || len(secret) > 200 {
		return ""
	}
	return secret
}

func IsMachineMethod(method string) bool { return method == http.MethodPost }
