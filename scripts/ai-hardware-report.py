#!/usr/bin/env python3
"""Отчёт об оборудовании AI-узла по снимкам видеопамяти и отчётам benchmark (ADR 0053).

  scripts/ai-hardware-report.py --samples узел.csv --benchmark ОТЧЁТ.json [--benchmark …] --out ОТЧЁТ-оборудование.json
                                [--maximum-vram-mib 7500] [--minimum-tokens-per-second 20] [--tolerance-seconds 120]

Видеопамять, перезапуски и OOM снимает на узле scripts/ai-node-sample.sh, пока идёт прогон;
скорость генерации, самый длинный запрос и коды ответов приходят из отчётов `ai-benchmark`
(поле performance). Отчёт проходит, только если снимки покрывают время прогонов, видеопамять
не выше предела, нет OOM и перезапусков, скорость не ниже предела, ни один ответ не оборван
по длине и каждый запрос вместе с ответом уместился в контекст сервера.
"""
import argparse
import csv
import hashlib
import json
import re
import sys
from collections import Counter
from datetime import datetime, timezone
from pathlib import Path


def parse_samples(path):
    rows = []
    with open(path, newline="") as handle:
        for row in csv.DictReader(handle):
            try:
                rows.append({
                    "epoch": int(row["epoch"]),
                    "gpu": (row.get("gpu") or "").strip(),
                    "used": int(row["memoryUsedMiB"]),
                    "total": int(row["memoryTotalMiB"]),
                    "restarts": int(row["restartCount"]),
                    "oom": (row.get("oomKilled") or "").strip().lower() == "true",
                    "running": (row.get("running") or "").strip().lower() == "true",
                    # Необязательные столбцы: старые файлы снимков их не содержат.
                    "driver": (row.get("driverVersion") or "").strip(),
                    "image": (row.get("imageId") or "").strip(),
                })
            except (KeyError, TypeError, ValueError):
                continue  # строка без видеопамяти: nvidia-smi был недоступен в этот момент
    return rows


def epoch_of(stamp):
    # Python до 3.11 не разбирает дробную часть длиннее шести цифр.
    stamp = re.sub(r"\.(\d{6})\d+", r".\1", stamp.replace("Z", "+00:00"))
    return datetime.fromisoformat(stamp).astimezone(timezone.utc).timestamp()


def summarize_benchmarks(reports):
    builds, windows = set(), []
    totals = Counter()
    summary = {"minimumObservedTokensPerSecond": None, "maximumPromptTokens": 0, "maximumTotalTokens": 0, "contextSize": None}
    for name, report in reports:
        performance = report.get("performance")
        if not performance:
            raise SystemExit(f"в отчёте нет поля performance (прогон без измерений): {name}")
        totals["requests"] += performance["requests"]
        totals["responsesWithTimings"] += performance["responsesWithTimings"]
        totals["lengthFinishes"] += performance["lengthFinishes"]
        totals["transportErrors"] += performance["transportErrors"]
        totals["httpErrors"] += sum(count for status, count in performance["httpStatuses"].items() if not status.startswith("2"))
        rate = performance.get("minTokensPerSecond") or 0
        if rate and (summary["minimumObservedTokensPerSecond"] is None or rate < summary["minimumObservedTokensPerSecond"]):
            summary["minimumObservedTokensPerSecond"] = rate
        summary["maximumPromptTokens"] = max(summary["maximumPromptTokens"], performance["maxPromptTokens"])
        summary["maximumTotalTokens"] = max(summary["maximumTotalTokens"], performance["maxTotalTokens"])
        server = report.get("server") or {}
        if server.get("buildInfo"):
            builds.add(server["buildInfo"])
        if server.get("contextSize"):
            summary["contextSize"] = server["contextSize"]
        if performance.get("startedAt") and performance.get("finishedAt"):
            windows.append((name, epoch_of(performance["startedAt"]), epoch_of(performance["finishedAt"])))
        else:
            windows.append((name, None, None))
    return summary, dict(totals), sorted(builds), windows


def build_report(samples, reports, maximum_vram, minimum_rate, tolerance):
    summary, totals, builds, windows = summarize_benchmarks(reports)
    failures = []
    if not samples:
        raise SystemExit("в файле снимков нет ни одной строки с видеопамятью")
    peak = max(s["used"] for s in samples)
    names = Counter(s["gpu"] for s in samples if s["gpu"])
    restarts = max(s["restarts"] for s in samples) - min(s["restarts"] for s in samples)
    oom = 1 if any(s["oom"] for s in samples) else 0
    first, last = min(s["epoch"] for s in samples), max(s["epoch"] for s in samples)
    for name, started, finished in windows:
        if started is None:
            failures.append(f"в отчёте нет времени прогона, покрытие снимков не проверить: {name}")
        elif first - tolerance > started or last + tolerance < finished:
            failures.append(f"снимки видеопамяти не покрывают прогон {name}")
    if peak > maximum_vram:
        failures.append(f"пик видеопамяти {peak} МиБ выше {maximum_vram}")
    if oom:
        failures.append("контейнер остановлен по нехватке памяти (OOM)")
    if restarts:
        failures.append(f"контейнер перезапускался во время прогона: {restarts}")
    if any(not s["running"] for s in samples):
        failures.append("в части снимков контейнер не запущен")
    drivers = sorted({s["driver"] for s in samples if s["driver"]})
    images = sorted({s["image"] for s in samples if s["image"]})
    if len(drivers) > 1:
        failures.append(f"версия драйвера менялась во время прогона: {', '.join(drivers)}")
    if len(images) > 1:
        failures.append(f"образ контейнера менялся во время прогона: {', '.join(images)}")
    rate = summary["minimumObservedTokensPerSecond"]
    if rate is None:
        failures.append("нет ни одного ответа с измерением скорости")
    elif rate < minimum_rate:
        failures.append(f"скорость генерации {rate:.1f} токенов/с ниже {minimum_rate}")
    if totals["lengthFinishes"]:
        failures.append(f"ответов, оборванных по длине: {totals['lengthFinishes']}")
    if totals["httpErrors"] or totals["transportErrors"]:
        failures.append(f"ошибок обращения к серверу: HTTP {totals['httpErrors']}, транспорт {totals['transportErrors']}")
    if summary["contextSize"] and summary["maximumTotalTokens"] > summary["contextSize"]:
        failures.append(f"запрос с ответом ({summary['maximumTotalTokens']}) не помещается в контекст {summary['contextSize']}")
    if len(builds) > 1:
        failures.append(f"прогоны шли на разных сборках llama.cpp: {', '.join(builds)}")
    return {
        "gpu": names.most_common(1)[0][0] if names else None,
        "samples": len(samples),
        "sampledFrom": datetime.fromtimestamp(first, timezone.utc).isoformat(),
        "sampledTo": datetime.fromtimestamp(last, timezone.utc).isoformat(),
        "totalVRAMMiB": max(s["total"] for s in samples),
        "peakObservedVRAMMiB": peak,
        "oom": oom,
        "restartsDuringRun": restarts,
        "minimumObservedTokensPerSecond": rate,
        "responsesWithTimings": totals["responsesWithTimings"],
        "requests": totals["requests"],
        "maximumPromptTokens": summary["maximumPromptTokens"],
        "maximumTotalTokens": summary["maximumTotalTokens"],
        "contextSize": summary["contextSize"],
        "llamaCppBuilds": builds,
        "driverVersions": drivers,
        "imageIds": images,
        "benchmarkReports": [name for name, _ in reports],
        "limits": {"maximumVRAMMiB": maximum_vram, "minimumTokensPerSecond": minimum_rate},
        "failures": failures,
        "passed": not failures,
        "note": "VRAM sampled inside the model container during the benchmark runs listed above; speed, token counts and statuses come from the llama.cpp responses.",
    }


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--samples", required=True)
    parser.add_argument("--benchmark", action="append", required=True)
    parser.add_argument("--out", required=True)
    parser.add_argument("--maximum-vram-mib", type=int, default=7500)
    parser.add_argument("--minimum-tokens-per-second", type=float, default=20)
    parser.add_argument("--tolerance-seconds", type=int, default=120)
    args = parser.parse_args(argv)
    reports = [(name, json.loads(Path(name).read_text())) for name in args.benchmark]
    report = build_report(parse_samples(args.samples), reports, args.maximum_vram_mib, args.minimum_tokens_per_second, args.tolerance_seconds)
    # Отчёт ссылается на первоисточник: по сумме видно, что сохранённый файл снимков — тот, из которого он собран.
    report["samplesFile"] = Path(args.samples).name
    report["samplesSha256"] = hashlib.sha256(Path(args.samples).read_bytes()).hexdigest()
    Path(args.out).write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
    for failure in report["failures"]:
        print(failure, file=sys.stderr)
    print(f"{'пройден' if report['passed'] else 'НЕ пройден'}: пик {report['peakObservedVRAMMiB']} МиБ из {report['totalVRAMMiB']}, "
          f"{report['minimumObservedTokensPerSecond']} токенов/с минимум, снимков {report['samples']}")
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    sys.exit(main())
