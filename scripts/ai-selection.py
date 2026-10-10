#!/usr/bin/env python3
"""Фиксация выбора AI tuple до открытия GOLDEN и проверка, что после прогона он не изменился.

  scripts/ai-selection.py write  --out models/reports/…-selection.json --prompt-version … --schema-version … [--dev-report ФАЙЛ …] [--note ТЕКСТ]
  scripts/ai-selection.py verify models/reports/…-selection.json

`write` записывает SHA-256 каждого файла, от которого зависит ответ модели и его проверка
(инструкция, примеры, схема генерации, валидаторы, runner, контракты), наборы данных и
отчёты DEV, по которым принято решение. `verify` пересчитывает то же и сообщает о каждом
расхождении кодом 1. Так «инструкция и параметры заморожены до открытия GOLDEN» становится
проверяемым утверждением, а не словами в примечании (в обзоре происхождения v6 этого не хватало).
"""
import argparse
import hashlib
import json
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]

# Каталоги, чьи файлы .go (кроме тестов) входят в tuple: всё, что формирует запрос к модели,
# разбирает и проверяет ответ или считает метрики.
SOURCE_DIRECTORIES = [
    "backend/internal/ai/application",
    "backend/internal/ai/infrastructure",
    "backend/internal/ai/domain",
    "backend/internal/ai/benchmark",
    "backend/cmd/ai-benchmark",
]
CONTRACTS = [
    "contracts/ai/analyze_conversation_v1.schema.json",
    "contracts/ai/analyze_conversation_v2.schema.json",
]
DATASETS = [
    "models/datasets/golden_v1.jsonl",
    "models/datasets/golden_v1.sha256",
    "models/datasets/dev_v1.jsonl",
    "models/datasets/intent_regression_v2.jsonl",
    "models/datasets/agreements_dev_v2.jsonl",
    "models/datasets/agreements_independent_v2.jsonl",
    "models/datasets/qa06_v1.jsonl",
    "models/datasets/context_probe_v1.jsonl",
]
MARKER = "Selected before opening GOLDEN; prompt and sampling frozen."


def sha256(path):
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for chunk in iter(lambda: handle.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def tuple_files(root=ROOT):
    files = []
    for directory in SOURCE_DIRECTORIES:
        files += sorted(str(p.relative_to(root)) for p in (root / directory).glob("*.go") if not p.name.endswith("_test.go"))
    return files + CONTRACTS


def tree_digest(hashes):
    """Одна сумма на весь набор: порядок и имена файлов входят в неё."""
    body = "".join(f"{name}\0{hashes[name]}\n" for name in sorted(hashes))
    return hashlib.sha256(body.encode()).hexdigest()


def line_count(path):
    with open(path, "rb") as handle:
        return sum(1 for line in handle if line.strip())


def go_version():
    try:
        return subprocess.run(["go", "version"], capture_output=True, text=True, check=True).stdout.strip()
    except (OSError, subprocess.CalledProcessError):
        return None


def build_selection(args, root=ROOT):
    hashes = {name: sha256(root / name) for name in tuple_files(root)}
    datasets = {}
    for name in DATASETS:
        entry = {"sha256": sha256(root / name)}
        if name.endswith(".jsonl"):
            entry["cases"] = line_count(root / name)
        datasets[name] = entry
    reports = {}
    for name in args.dev_report or []:
        report = json.loads((root / name).read_text())
        if not report.get("passed"):
            raise SystemExit(f"отчёт DEV не пройден, выбор не фиксируется: {name}")
        reports[name] = {"sha256": sha256(root / name), "cases": report.get("cases"), "passed": True}
    return {
        "selectedAt": datetime.now(timezone.utc).isoformat(),
        "promptVersion": args.prompt_version,
        "schemaVersion": args.schema_version,
        "sha256": hashes,
        "treeSha256": tree_digest(hashes),
        "datasets": datasets,
        "devReports": reports,
        "go": go_version(),
        "note": args.note or MARKER,
    }


def verify_selection(selection, root=ROOT):
    problems = []
    recorded = selection.get("sha256", {})
    for name in sorted(set(recorded) | set(tuple_files(root))):
        path = root / name
        if name not in recorded:
            problems.append(f"файл не входил в выбор: {name}")
        elif not path.exists():
            problems.append(f"файл удалён: {name}")
        elif sha256(path) != recorded[name]:
            problems.append(f"файл изменён после выбора: {name}")
    for name, entry in sorted(selection.get("datasets", {}).items()):
        if not (root / name).exists() or sha256(root / name) != entry["sha256"]:
            problems.append(f"набор данных изменён после выбора: {name}")
    for name, entry in sorted(selection.get("devReports", {}).items()):
        if not (root / name).exists() or sha256(root / name) != entry["sha256"]:
            problems.append(f"отчёт DEV изменён после выбора: {name}")
    if selection.get("treeSha256") and not problems and tree_digest(recorded) != selection["treeSha256"]:
        problems.append("сумма набора файлов не сходится с перечнем в выборе")
    return problems


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    commands = parser.add_subparsers(dest="command", required=True)
    write = commands.add_parser("write")
    write.add_argument("--out", required=True)
    write.add_argument("--prompt-version", required=True)
    write.add_argument("--schema-version", required=True)
    write.add_argument("--dev-report", action="append")
    write.add_argument("--note")
    verify = commands.add_parser("verify")
    verify.add_argument("selection")
    args = parser.parse_args(argv)
    if args.command == "write":
        selection = build_selection(args)
        Path(args.out).write_text(json.dumps(selection, ensure_ascii=False, indent=2) + "\n")
        print(f"выбор записан: {args.out} ({len(selection['sha256'])} файлов, сумма {selection['treeSha256'][:16]}…)")
        return 0
    selection = json.loads(Path(args.selection).read_text())
    problems = verify_selection(selection)
    for problem in problems:
        print(problem, file=sys.stderr)
    if problems:
        print(f"выбор {args.selection} не совпадает с рабочим деревом ({len(problems)})", file=sys.stderr)
        return 1
    print(f"выбор совпадает с деревом: {len(selection['sha256'])} файлов, сумма {selection['treeSha256'][:16]}…")
    return 0


if __name__ == "__main__":
    sys.exit(main())
