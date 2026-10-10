// testgate runs the mandatory database suite and rejects missing evidence or skips.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type event struct {
	Action, Package, Test, Output string
}

type skipped struct {
	Test, Reason string
	Expected     bool
}

type report struct {
	StartedAt         time.Time `json:"startedAt"`
	Commit            string    `json:"commit"`
	WorkingDiffSHA256 string    `json:"workingDiffSha256"`
	MigrationsSHA256  string    `json:"migrationsSha256"`
	SourceSHA256      string    `json:"sourceSha256"`
	Passed            []string  `json:"passed"`
	Failed            []string  `json:"failed"`
	Skipped           []skipped `json:"skipped"`
	MissingRequired   []string  `json:"missingRequired"`
	Failure           string    `json:"failure,omitempty"`
	Status            string    `json:"status"`
}

var required = []string{
	"lidradar/backend/internal/integration/TestCrossTenantIdentifiersAreRejectedUnderRLS",
	"lidradar/backend/internal/integration/TestFrontendDataThroughRealAPI",
	"lidradar/backend/internal/revenue/infrastructure/TestPostgresRevenueConcurrentReplayAtomicRollbackUniqueAndAppendOnly",
	"lidradar/backend/internal/ai/infrastructure/TestPostgresFinalizationAtomicallyRejectsFreshnessRace",
	"lidradar/backend/internal/jobs/infrastructure/TestWorkerProcessCanBeKilledAfterClaim",
	"lidradar/backend/internal/corrective/infrastructure/TestPostgresActionSerializesWithRiskClosure",
	"lidradar/backend/internal/corrective/infrastructure/TestPostgresActionCommitsBeforeWaitingClosure",
	"lidradar/backend/platform/postgres/TestRoleBootstrapReproducesMigrationGrants",
	"lidradar/backend/platform/postgres/TestRoleBootstrapGrantsMembershipToSeparateApplicationUser",
	"lidradar/backend/internal/integration/TestPersistentAuthLimitsUseTheClientBehindTrustedProxy",
}

func main() {
	output := flag.String("output", "runtime/test-db", "evidence directory")
	flag.Parse()
	if err := os.MkdirAll(*output, 0700); err != nil {
		fmt.Fprintln(os.Stderr, "cannot create test evidence directory")
		os.Exit(1)
	}
	r := report{StartedAt: time.Now().UTC(), Status: "FAIL"}
	commit, _ := exec.Command("git", "rev-parse", "HEAD").Output()
	r.Commit = strings.TrimSpace(string(commit))
	diff, _ := exec.Command("git", "diff", "HEAD", "--", "backend", "go.mod", "go.sum").Output()
	r.WorkingDiffSHA256 = digest(diff)
	sourceHash := sha256.New()
	if err := filepath.WalkDir("backend", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(sourceHash, "%s\x00", path)
		sourceHash.Write(data)
		return nil
	}); err != nil {
		r.Failure = "cannot hash source tree"
	}
	for _, path := range []string{"go.mod", "go.sum"} {
		data, err := os.ReadFile(path)
		if err != nil {
			r.Failure = "cannot hash Go dependencies"
			break
		}
		fmt.Fprintf(sourceHash, "%s\x00", path)
		sourceHash.Write(data)
	}
	r.SourceSHA256 = hex.EncodeToString(sourceHash.Sum(nil))
	hash := sha256.New()
	files, _ := filepath.Glob("backend/platform/postgres/migrations/*.sql")
	for _, name := range files {
		data, err := os.ReadFile(name)
		if err != nil {
			r.Failure = "cannot hash migrations"
			break
		}
		fmt.Fprintf(hash, "%s\x00", filepath.Base(name))
		hash.Write(data)
	}
	r.MigrationsSHA256 = hex.EncodeToString(hash.Sum(nil))
	if r.Failure == "" {
		if err := run(*output, flag.Args(), &r); err != nil {
			r.Failure = err.Error()
		}
	}
	if r.Failure == "" {
		r.Status = "PASS"
	}
	data, _ := json.MarshalIndent(r, "", "  ")
	if err := os.WriteFile(filepath.Join(*output, "summary.json"), append(data, '\n'), 0600); err != nil {
		fmt.Fprintln(os.Stderr, "cannot write test summary")
		os.Exit(1)
	}
	fmt.Printf("DB gate: %s; passed=%d failed=%d skipped=%d; evidence=%s\n", r.Status, len(r.Passed), len(r.Failed), len(r.Skipped), *output)
	if r.Failure != "" {
		fmt.Fprintln(os.Stderr, r.Failure)
		os.Exit(1)
	}
}

func run(output string, args []string, r *report) error {
	dsn := os.Getenv("LIDRADAR_DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("mandatory PostgreSQL URL is missing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("invalid mandatory PostgreSQL configuration")
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("mandatory PostgreSQL is unavailable")
	}
	var database string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&database); err != nil || database != "lidradar_frontend" {
		return fmt.Errorf("full suite requires an isolated database named lidradar_frontend")
	}
	events, err := os.OpenFile(filepath.Join(output, "events.jsonl"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer events.Close()
	arguments := append([]string{"test", "-json", "-count=1", "-p=2"}, args...)
	command := exec.Command("go", append(arguments, "./...")...)
	command.Env = append(os.Environ(), "LIDRADAR_TEST_DATABASE_REQUIRED=1")
	command.Stderr = os.Stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	if err := command.Start(); err != nil {
		return err
	}
	parseErr := collect(io.TeeReader(stdout, events), r)
	if parseErr != nil {
		_ = command.Process.Kill()
	}
	waitErr := command.Wait()
	if parseErr != nil {
		return parseErr
	}
	if waitErr != nil {
		return fmt.Errorf("go test failed: %w", waitErr)
	}
	return validate(r)
}

func collect(input io.Reader, r *report) error {
	decoder := json.NewDecoder(input)
	outputs := map[string]string{}
	for {
		var e event
		if err := decoder.Decode(&e); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		id := e.Package + "/" + e.Test
		if e.Test == "" {
			if e.Action == "fail" {
				r.Failed = append(r.Failed, e.Package)
			}
			continue
		}
		switch e.Action {
		case "output":
			outputs[id] += e.Output
		case "pass":
			r.Passed = append(r.Passed, id)
			delete(outputs, id)
		case "fail":
			r.Failed = append(r.Failed, id)
			fmt.Fprint(os.Stderr, outputs[id])
			delete(outputs, id)
		case "skip":
			// This function is executed by the required crash-recovery parent in a subprocess.
			expected := id == "lidradar/backend/internal/jobs/infrastructure/TestJobClaimHelper" && strings.Contains(outputs[id], "вспомогательный процесс запускается только из crash-test")
			r.Skipped = append(r.Skipped, skipped{id, outputs[id], expected})
			delete(outputs, id)
		}
	}
}

func validate(r *report) error {
	passed := map[string]bool{}
	for _, name := range r.Passed {
		passed[name] = true
	}
	for _, name := range required {
		if !passed[name] {
			r.MissingRequired = append(r.MissingRequired, name)
		}
	}
	unexpected := 0
	for _, s := range r.Skipped {
		if !s.Expected {
			unexpected++
		}
	}
	if len(r.Failed) > 0 || len(r.MissingRequired) > 0 || unexpected > 0 {
		return fmt.Errorf("incomplete database evidence: failed=%d missing=%d unexpected skips=%d", len(r.Failed), len(r.MissingRequired), unexpected)
	}
	return nil
}

func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
