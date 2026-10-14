package application

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"strings"
	"time"

	"lidradar/backend/internal/lifecycle/domain"
)

// Состав архива выгрузки организации (ADR 0059): README.txt, organization.json, data/*.jsonl и manifest.json
// последним. Манифест пишется только после всех файлов, поэтому архив без него считается оборванным.
const (
	readmeFile       = "README.txt"
	organizationFile = "organization.json"
	manifestFile     = "manifest.json"
)

func dataFile(dataset domain.Dataset) string { return "data/" + dataset.File + ".jsonl" }

// entryWriter считает размер и контрольную сумму записанного файла для манифеста.
type entryWriter struct {
	out   io.Writer
	hash  hash.Hash
	bytes int64
}

func (writer *entryWriter) Write(data []byte) (int, error) {
	written, err := writer.out.Write(data)
	writer.hash.Write(data[:written])
	writer.bytes += int64(written)
	return written, err
}

// writeArchive пишет архив потоком. При любой ошибке возвращает её, не закрывая архив: у оборванного файла нет ни
// манифеста, ни оглавления, и любой читатель ZIP его отвергнет.
func writeArchive(ctx context.Context, out io.Writer, snapshot Snapshot) error {
	at := snapshot.At().UTC()
	archive := zip.NewWriter(out)
	manifest := domain.Manifest{Format: domain.OrganizationExportFormat, GeneratedAt: at, Files: []domain.ManifestFile{}}

	add := func(name string, fill func(io.Writer) (int64, error)) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		writer, err := archive.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: at})
		if err != nil {
			return fmt.Errorf("start %s: %w", name, err)
		}
		entry := &entryWriter{out: writer, hash: sha256.New()}
		rows, err := fill(entry)
		if err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
		manifest.Files = append(manifest.Files, domain.ManifestFile{
			Name: name, Rows: rows, Bytes: entry.bytes, SHA256: hex.EncodeToString(entry.hash.Sum(nil)),
		})
		return nil
	}

	if err := add(readmeFile, func(w io.Writer) (int64, error) {
		_, err := io.WriteString(w, readmeText(at))
		return 0, err
	}); err != nil {
		return err
	}
	if err := add(organizationFile, func(w io.Writer) (int64, error) {
		document, err := snapshot.Organization(ctx)
		if err != nil {
			return 0, err
		}
		_, err = w.Write(append(document, '\n'))
		return 1, err
	}); err != nil {
		return err
	}
	for _, dataset := range domain.OrganizationDatasets {
		if err := add(dataFile(dataset), func(w io.Writer) (int64, error) {
			var rows int64
			err := snapshot.Rows(ctx, dataset, func(line []byte) error {
				if _, err := w.Write(line); err != nil {
					return err
				}
				if _, err := w.Write([]byte{'\n'}); err != nil {
					return err
				}
				rows++
				return nil
			})
			return rows, err
		}); err != nil {
			return err
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	writer, err := archive.CreateHeader(&zip.FileHeader{Name: manifestFile, Method: zip.Deflate, Modified: at})
	if err != nil {
		return fmt.Errorf("start %s: %w", manifestFile, err)
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	if _, err := writer.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("write %s: %w", manifestFile, err)
	}
	if err := archive.Close(); err != nil {
		return fmt.Errorf("finish archive: %w", err)
	}
	return nil
}

// readmeText — пояснение к архиву. Перечень наборов строится из реестра, поэтому не расходится с содержимым.
func readmeText(at time.Time) string {
	var text strings.Builder
	text.WriteString("Выгрузка данных организации LidRadar\n")
	text.WriteString("Формат: " + domain.OrganizationExportFormat + "\n")
	text.WriteString("Снимок данных: " + at.UTC().Format(time.RFC3339) + " (все времена в файлах указаны в UTC)\n\n")
	text.WriteString("Состав\n")
	text.WriteString("  " + organizationFile + " — название, часовой пояс, валюта и статус организации.\n")
	text.WriteString("  data/*.jsonl — по файлу на набор данных; в каждой строке один JSON-объект (JSON Lines, UTF-8).\n")
	text.WriteString("  " + manifestFile + " — число строк, размер и контрольная сумма SHA-256 каждого файла.\n")
	text.WriteString("  Архив без " + manifestFile + " оборван при передаче: выгрузку нужно повторить.\n\n")
	text.WriteString("Наборы данных\n")
	for _, dataset := range domain.OrganizationDatasets {
		text.WriteString("  " + dataFile(dataset) + " — " + dataset.Description + ".\n")
	}
	text.WriteString("\nЧто не входит в выгрузку\n")
	text.WriteString("  - секреты и реквизиты доступа: ключи подключённых каналов, хэши паролей, кодов и токенов;\n")
	text.WriteString("  - служебные очереди и копии обработки: задания, исходящий журнал событий, ответы модели, сырой буфер вебхуков;\n")
	text.WriteString("  - личные данные каждого участника (уведомления, настройки, привязки Telegram): участник получает их\n")
	text.WriteString("    в личной выгрузке в разделе «Аккаунт»;\n")
	text.WriteString("  - журнал команд оператора платформы.\n\n")
	text.WriteString("Как читать\n")
	text.WriteString("  Идентификаторы (UUID) связывают файлы: conversation_id указывает на data/conversations.jsonl,\n")
	text.WriteString("  contact_id — на data/contacts.jsonl, risk_id — на data/risks.jsonl и так далее.\n")
	text.WriteString("  Идентификатор самой организации в файлах не повторяется.\n")
	text.WriteString("  Содержимое, которое уже стёрто по срокам хранения, в выгрузке отсутствует.\n")
	return text.String()
}
