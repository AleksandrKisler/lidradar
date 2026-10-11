#!/usr/bin/env python3
"""Сборка манифеста модели из отчётов прогона (ADR 0053).

  scripts/ai-manifest.py --base models/manifests/lidradar-main-v1.json --previous-report ПУТЬ \\
      --selection …-selection.json --dev …-dev.json --golden …-golden.json \\
      [--v2-dev имя=отчёт.json …] [--hardware …-hardware.json] [--model-hash-report …-model-hash.json] [--limitation ТЕКСТ …] --out models/manifests/lidradar-main-v1.json

Числа в манифесте берутся из отчётов, а не вводятся руками, и перед записью сверяются:
версии инструкции и контракта, сумма GOLDEN, параметры генерации, которые сервер увидел
в последнем запросе, размер контекста и число слотов. Статус `FROZEN` ставится, только если
пройдены все пороги, включая оборудование; иначе манифест остаётся `CANDIDATE` со списком того,
чего не хватает (BACKEND_SPEC: «любой незаполненный обязательный результат оставляет модель
кандидатом»). При любом расхождении манифест не записывается, код выхода 1.
"""
import argparse
import json
import sys
from datetime import datetime, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
V2_DATASETS = {"intents": "intent_regression_v2", "agreements": "agreements_dev_v2", "independent": "agreements_independent_v2"}
GENERATION_FIELDS = {"seed": "seed", "temperature": "temperature", "topP": "topP", "topK": "topK", "minP": "minP", "presencePenalty": "presencePenalty"}
RESULT_FIELDS = ("cases", "precision", "recall", "f1", "exactRate", "validRate", "evidenceExactRate", "p95Ms", "passed")
RATE_GATES = {"minimumPrecision": "precision", "minimumRecall": "recall", "minimumF1": "f1", "minimumExactRate": "exactRate",
              "minimumValidRate": "validRate", "minimumEvidenceExactRate": "evidenceExactRate"}
FACT_TYPES = ("BOOKING_INTENT", "BUSINESS_COMMITMENT", "PRICE_MENTIONED", "FOLLOW_UP_CANDIDATE", "PURCHASE_INTENT")
# ADR 0054: границы для наборов обязательств компании заданы до слепого прогона и в манифесте не меняются.
COMMITMENT_BARS = {"minimumPrecision": 0.95, "minimumRecall": 0.85}
COMMITMENT_DEV = "models/datasets/commitment_dev_v1.jsonl"
COMMITMENT_HOLDOUT = "models/datasets/commitment_holdout_v1.jsonl"


def load(path):
    return json.loads(Path(path).read_text())


def result_of(report_path, report):
    result = {"report": report_path}
    for field in RESULT_FIELDS:
        result[field] = report[field]
    if report.get("agreementCases"):
        result["agreementExactRate"] = report["agreementExactRate"]
    result["recallByFactType"] = {name: round(metrics["recall"], 4) for name, metrics in sorted(report["byFactType"].items())}
    return result


def check_gates(name, report, gates, performance_gate, problems, unmet=None):
    """Числа отчёта сверяются с порогами манифеста независимо от флага `passed`.

    `passed` выставляет runner по порогам, которые передал запускавший; отчёт, снятый с ослабленными
    флагами, иначе попал бы в манифест с зелёным флагом. Непройденный порог — не противоречие данных, а результат:
    в режиме кандидата (`unmet` передан) он записывается в список непройденных границ, иначе блокирует запись.
    """
    problems = problems if unmet is None else unmet
    for gate, field in RATE_GATES.items():
        if gate in gates and report[field] < gates[gate]:
            problems.append(f"{name}: {field} {report[field]} ниже порога {gate} {gates[gate]}")
    if report.get("agreementCases") and "minimumExactRate" in gates and report["agreementExactRate"] < gates["minimumExactRate"]:
        problems.append(f"{name}: agreementExactRate {report['agreementExactRate']} ниже порога minimumExactRate {gates['minimumExactRate']}")
    for fact_type, metrics in sorted(report["byFactType"].items()):
        if "minimumFactPrecision" in gates and fact_type in FACT_TYPES and metrics.get("precision", 1) < gates["minimumFactPrecision"]:
            problems.append(f"{name}: точность {fact_type} {metrics['precision']} ниже порога minimumFactPrecision {gates['minimumFactPrecision']}")
        positives = metrics.get("truePositive", 0) + metrics.get("falseNegative", 0)
        if "minimumFactRecall" in gates and positives > 0 and metrics["recall"] < gates["minimumFactRecall"]:
            problems.append(f"{name}: полнота {fact_type} {metrics['recall']} ниже порога minimumFactRecall {gates['minimumFactRecall']}")
    maximum = performance_gate.get("maximumP95Ms")
    if maximum and report["p95Ms"] > maximum:
        problems.append(f"{name}: p95 {report['p95Ms']} мс выше порога {maximum} мс")


def check_run(name, report, prompt_version, schema_version, problems, require_server=True, gates=None, performance_gate=None, unmet=None):
    if report.get("promptVersions") != [prompt_version]:
        problems.append(f"{name}: версии инструкции {report.get('promptVersions')}, ожидалась {prompt_version}")
    if report.get("schemaVersions") != [schema_version]:
        problems.append(f"{name}: версии контракта {report.get('schemaVersions')}, ожидалась {schema_version}")
    if not report.get("passed"):
        (problems if unmet is None else unmet).append(f"{name}: пороги не пройдены")
    if gates is not None:
        check_gates(name, report, gates, performance_gate or {}, problems, unmet)
    if require_server and not report.get("server"):
        problems.append(f"{name}: в отчёте нет привязки к серверу (server)")


def commitment_counts(report):
    metrics = report.get("byFactType", {}).get("BUSINESS_COMMITMENT", {})
    return {"truePositive": metrics.get("truePositive", 0), "falsePositive": metrics.get("falsePositive", 0),
            "falseNegative": metrics.get("falseNegative", 0), "precision": metrics.get("precision", 1), "recall": metrics.get("recall", 1)}


def check_commitment_report(name, report, prompt_version, schema_version, selection, dataset, gates, performance_gate, problems, unmet=None):
    """Набор обязательств измеряет один тип факта, поэтому общие пороги здесь не применяются: отрицательные факты других типов
    считаются в нём ложными и скрыли бы результат. Решают полнота и точность BUSINESS_COMMITMENT, доля валидных ответов и p95."""
    if report.get("promptVersions") != [prompt_version]:
        problems.append(f"{name}: версии инструкции {report.get('promptVersions')}, ожидалась {prompt_version}")
    if report.get("schemaVersions") != [schema_version]:
        problems.append(f"{name}: версии контракта {report.get('schemaVersions')}, ожидалась {schema_version}")
    if not report.get("server"):
        problems.append(f"{name}: в отчёте нет привязки к серверу (server)")
    check_dataset(name, report, selection, dataset, problems)
    counts = commitment_counts(report)
    if counts["truePositive"] + counts["falseNegative"] == 0:
        problems.append(f"{name}: в наборе нет обещаний, полноту оценить нельзя")
    bars = problems if unmet is None else unmet
    if counts["precision"] < COMMITMENT_BARS["minimumPrecision"]:
        bars.append(f"{name}: точность BUSINESS_COMMITMENT {counts['precision']:.4f} ниже границы {COMMITMENT_BARS['minimumPrecision']}")
    if counts["recall"] < COMMITMENT_BARS["minimumRecall"]:
        bars.append(f"{name}: полнота BUSINESS_COMMITMENT {counts['recall']:.4f} ниже границы {COMMITMENT_BARS['minimumRecall']}")
    if "minimumValidRate" in gates and report.get("validRate", 0) < gates["minimumValidRate"]:
        bars.append(f"{name}: доля валидных ответов {report.get('validRate')} ниже порога {gates['minimumValidRate']}")
    maximum = performance_gate.get("maximumP95Ms")
    if maximum and report.get("p95Ms", 0) > maximum:
        bars.append(f"{name}: p95 {report['p95Ms']} мс выше порога {maximum} мс")
    return {"report": None, "cases": report.get("cases"), **counts, "validRate": report.get("validRate"), "p95Ms": report.get("p95Ms"),
            "build": (report.get("server") or {}).get("buildInfo")}


def check_dataset(name, report, selection, dataset, problems):
    """Отчёт снят на том наборе, который записан в выборе и лежит в репозитории, а не на преобразованной копии."""
    recorded = selection.get("datasets", {}).get(dataset, {}).get("sha256")
    if not recorded or report.get("datasetSha256") != recorded:
        problems.append(f"{name}: отчёт снят не на {dataset} из выбора (сумма {report.get('datasetSha256')}, в выборе {recorded})")


def check_generation(name, report, generation, problems):
    server = report.get("server") or {}
    sampling = server.get("observedSampling")
    if not sampling:
        problems.append(f"{name}: сервер не вернул параметры последнего запроса")
        return
    for field in GENERATION_FIELDS:
        if sampling.get(field) != generation.get(field):
            problems.append(f"{name}: параметр {field} на сервере {sampling.get(field)}, в манифесте {generation.get(field)}")
    if sampling.get("thinkingDisabled") != (generation.get("thinking") is False):
        problems.append(f"{name}: режим рассуждений на сервере не совпал с манифестом")
    if server.get("contextSize") != generation.get("contextSize"):
        problems.append(f"{name}: контекст сервера {server.get('contextSize')}, в манифесте {generation.get('contextSize')}")
    if server.get("totalSlots") != generation.get("parallelism"):
        problems.append(f"{name}: слотов на сервере {server.get('totalSlots')}, в манифесте {generation.get('parallelism')}")


def build(args, now=None):
    base = load(args.base)
    selection, dev, golden = load(args.selection), load(args.dev), load(args.golden)
    prompt_version, schema_version = selection["promptVersion"], selection["schemaVersion"]
    problems = []
    if not Path(args.previous_report).is_file():
        problems.append(f"прежний манифест не сохранён как отчёт: {args.previous_report} (cp {args.base} {args.previous_report})")
    gates = {**base["qualityGate"], "minimumFactRecall": args.minimum_fact_recall}
    unmet = [] if getattr(args, "candidate_on_unmet", False) else None
    check_run("DEV", dev, prompt_version, schema_version, problems, gates=gates, performance_gate=base["performanceGate"], unmet=unmet)
    check_run("GOLDEN", golden, prompt_version, schema_version, problems, gates=gates, performance_gate=base["performanceGate"], unmet=unmet)
    check_dataset("DEV", dev, selection, "models/datasets/dev_v1.jsonl", problems)
    if golden.get("cases") != 400 or dev.get("cases") != 100:
        problems.append(f"объём выборок: GOLDEN {golden.get('cases')}, DEV {dev.get('cases')}; нужно 400 и 100")
    checksum = Path(args.golden_checksum).read_text().split()[0]
    if golden.get("datasetSha256") != checksum:
        problems.append("сумма GOLDEN в отчёте не равна утверждённой в golden_v1.sha256")
    if selection.get("devReports", {}).get(args.dev, {}).get("passed") is not True:
        problems.append("DEV-отчёт не входит в выбор, принятый до открытия GOLDEN")
    generation = {**base["generation"]}
    if args.context_size:
        generation["contextSize"] = args.context_size  # решение владельца: контекст узла меняется явно, а не наследуется из прежнего манифеста
    check_generation("GOLDEN", golden, generation, problems)
    v2_results = {}
    for item in args.v2_dev or []:
        name, _, path = item.partition("=")
        report = load(path)
        check_run(f"DEV v2 {name}", report, prompt_version, schema_version, problems, gates=gates, performance_gate=base["performanceGate"], unmet=unmet)
        if name in V2_DATASETS:
            check_dataset(f"DEV v2 {name}", report, selection, f"models/datasets/{V2_DATASETS[name]}.jsonl", problems)
        v2_results[name] = result_of(path, report)

    commitment_dev = holdout = holdout_baseline = None
    if getattr(args, "commitment_dev", None):
        commitment_dev = check_commitment_report("Набор обязательств (DEV)", load(args.commitment_dev), prompt_version, schema_version, selection,
                                                 COMMITMENT_DEV, gates, base["performanceGate"], problems, unmet)
        commitment_dev["report"] = args.commitment_dev
    if getattr(args, "holdout", None):
        holdout_report = load(args.holdout)
        holdout = check_commitment_report("Слепая выборка обязательств", holdout_report, prompt_version, schema_version, selection,
                                          COMMITMENT_HOLDOUT, gates, base["performanceGate"], problems, unmet)
        holdout["report"] = args.holdout
        check_generation("Слепая выборка обязательств", holdout_report, generation, problems)
        if getattr(args, "holdout_baseline", None):
            baseline_report = load(args.holdout_baseline)
            if baseline_report.get("datasetSha256") != holdout_report.get("datasetSha256"):
                problems.append("базовая ревизия снята не на той слепой выборке, что новая")
            baseline = commitment_counts(baseline_report)
            holdout_baseline = {"report": args.holdout_baseline, "promptVersions": baseline_report.get("promptVersions"), "build": (baseline_report.get("server") or {}).get("buildInfo"),
                                "cases": baseline_report.get("cases"), **baseline}
            if getattr(args, "holdout_baseline_revision", None):
                holdout_baseline["revision"] = args.holdout_baseline_revision
            if holdout["recall"] < baseline["recall"]:
                (problems if unmet is None else unmet).append(f"полнота BUSINESS_COMMITMENT на слепой выборке упала: {baseline['recall']:.4f} → {holdout['recall']:.4f}")

    pending = []
    hardware_result = None
    if args.hardware:
        hardware = load(args.hardware)
        if not hardware.get("passed"):
            problems.append("отчёт об оборудовании не пройден: " + "; ".join(hardware.get("failures", [])))
        hardware_result = {"report": args.hardware, "peakObservedVRAMMiB": hardware["peakObservedVRAMMiB"], "oom": hardware["oom"],
                           "restartsDuringRun": hardware["restartsDuringRun"], "minimumObservedTokensPerSecond": hardware["minimumObservedTokensPerSecond"],
                           "maximumPromptTokens": hardware["maximumPromptTokens"], "maximumTotalTokens": hardware["maximumTotalTokens"], "passed": hardware["passed"],
                           "driverVersions": hardware.get("driverVersions", []), "imageIds": hardware.get("imageIds", [])}
    else:
        pending.append("пик видеопамяти, OOM и перезапуски на целевом узле (scripts/ai-node-sample.sh + scripts/ai-hardware-report.py)")
    model_verification = None
    if args.model_hash_report:
        hashed = load(args.model_hash_report)
        if hashed.get("sha256") != base["modelSHA256"]:
            problems.append(f"SHA-256 файла весов на узле {hashed.get('sha256')} не равен {base['modelSHA256']} из прежнего манифеста")
        else:
            model_verification = {"report": args.model_hash_report, "sha256": hashed["sha256"], "sizeBytes": hashed.get("sizeBytes"), "checkedAt": hashed.get("checkedAt")}
    else:
        pending.append("SHA-256 файла весов на узле не пересчитан (scripts/ai-model-hash.py на узле)")
    if problems:
        raise SystemExit("манифест не записан:\n  " + "\n  ".join(problems))

    hardware_description = {**base["hardware"]}
    if hardware_result:
        hardware_description["fit"] = (f"{golden['server'].get('modelFtype')}, контекст {generation['contextSize']}, параллелизм {generation['parallelism']}; "
                                       f"наблюдаемый пик {hardware_result['peakObservedVRAMMiB']} MiB при пределе {base['performanceGate'].get('maximumVRAMMiB', 'не задан')} MiB (отчёт об оборудовании)")
    elif generation["contextSize"] != base["generation"]["contextSize"]:
        hardware_description["fit"] = f"{base['hardware'].get('fit', '')}; для контекста {generation['contextSize']} видеопамять не измерена"

    performance = golden["performance"]
    manifest = {
        "name": base["name"],
        "status": "FROZEN" if not pending else "CANDIDATE",
        "contractVersion": schema_version,
        "promptVersion": prompt_version,
        "datasetVersion": base["datasetVersion"],
        "dataset": {
            "cases": base["dataset"]["cases"], "goldenCases": 400, "devCases": 100, "goldenSHA256": checksum,
            "measuredOn": {"devCases": 100, "devSHA256": selection["datasets"]["models/datasets/dev_v1.jsonl"]["sha256"], "goldenCases": 400, "goldenSHA256": checksum},
            "v2DevSets": {name: selection["datasets"][f"models/datasets/{file}.jsonl"] for name, file in V2_DATASETS.items()},
        },
        "runtime": base["runtime"],
        "runtimeBuild": golden["server"].get("buildInfo"),
        "runtimeImageId": (hardware_result or {}).get("imageIds", [None])[0] if (hardware_result or {}).get("imageIds") else None,
        "driverVersion": (hardware_result or {}).get("driverVersions", [None])[0] if (hardware_result or {}).get("driverVersions") else None,
        "targetHardware": base["targetHardware"],
        "hardware": hardware_description,
        "generation": generation,
        "qualityGate": gates,
        "performanceGate": base["performanceGate"],
        "modelArtifact": base["modelArtifact"],
        "modelSHA256": base["modelSHA256"],
        "modelServer": {key: golden["server"].get(key) for key in ("modelPath", "modelFtype", "modelSizeBytes", "modelParameters")},
        "modelVerification": model_verification,
        "selection": args.selection,
        "selectionTreeSHA256": selection["treeSha256"],
        "validationResult": {"split": "DEV", **result_of(args.dev, dev)},
        "v2ValidationResults": v2_results,
        "goldenResult": {**result_of(args.golden, golden), "maxPromptTokens": performance["maxPromptTokens"], "minTokensPerSecond": performance["minTokensPerSecond"]},
        "hardwareResult": hardware_result,
        "previousQualification": args.previous_report,
        "knownLimitations": list(args.limitation or []),
    }
    if getattr(args, "golden_regression", False):
        manifest["goldenResult"]["blind"] = False
        manifest["goldenResult"]["note"] = ("Регрессионный прогон: отчёт GOLDEN предыдущей ревизии был прочитан, выборка больше не слепая. "
                                            "Слепой результат ревизии даёт выборка обязательств.")
    if commitment_dev:
        manifest["commitmentValidation"] = commitment_dev
    if holdout:
        manifest["qualityGate"] = {**manifest["qualityGate"], "commitmentHoldout": COMMITMENT_BARS}
        manifest["holdoutResult"] = holdout
        manifest["dataset"]["holdout"] = selection["datasets"][COMMITMENT_HOLDOUT]
        if holdout_baseline:
            manifest["holdoutBaseline"] = holdout_baseline
    if unmet:
        manifest["status"] = "CANDIDATE"
        manifest["unmetBars"] = unmet
        pending = pending + [f"граница не пройдена: {item}" for item in unmet]
    if pending:
        manifest["pending"] = pending
    else:
        manifest["frozenAt"] = (now or datetime.now(timezone.utc)).isoformat()
    return manifest


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--base", required=True)
    parser.add_argument("--previous-report", required=True)
    parser.add_argument("--selection", required=True)
    parser.add_argument("--dev", required=True)
    parser.add_argument("--golden", required=True)
    parser.add_argument("--v2-dev", action="append")
    parser.add_argument("--hardware")
    parser.add_argument("--golden-checksum", default=str(ROOT / "models/datasets/golden_v1.sha256"))
    parser.add_argument("--minimum-fact-recall", type=float, default=0.85)
    parser.add_argument("--context-size", type=int, help="контекст узла, если он отличается от прежнего манифеста (решение владельца 2026-10-10: 8192)")
    parser.add_argument("--model-hash-report", help="отчёт scripts/ai-model-hash.py: SHA-256 файла весов, пересчитанный на узле")
    parser.add_argument("--commitment-dev", help="отчёт набора обязательств DEV (models/datasets/commitment_dev_v1.jsonl)")
    parser.add_argument("--holdout", help="отчёт слепой выборки обязательств (models/datasets/commitment_holdout_v1.jsonl)")
    parser.add_argument("--holdout-baseline", help="отчёт предыдущей ревизии tuple на той же слепой выборке")
    parser.add_argument("--holdout-baseline-revision", help="какой ревизии tuple принадлежит базовая линия (коммит и что в нём заменено), текстом")
    parser.add_argument("--candidate-on-unmet", action="store_true", help="непройденные границы качества не блокируют запись: манифест получает статус CANDIDATE и список unmetBars; противоречия данных по-прежнему блокируют")
    parser.add_argument("--golden-regression", action="store_true", help="GOLDEN прочитан при прежней ревизии: прогон регрессионный, не слепой")
    parser.add_argument("--limitation", action="append")
    parser.add_argument("--out", required=True)
    args = parser.parse_args(argv)
    manifest = build(args)
    Path(args.out).write_text(json.dumps(manifest, ensure_ascii=False, indent=2) + "\n")
    print(f"манифест записан: {args.out}, статус {manifest['status']}" + (f", не хватает: {len(manifest['pending'])}" if manifest.get("pending") else ""))
    return 0


if __name__ == "__main__":
    sys.exit(main())
