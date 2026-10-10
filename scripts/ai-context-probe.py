#!/usr/bin/env python3
"""Зонд длинного контекста: набор переписок до предела, который разрешает продукт (ADR 0053).

  scripts/ai-context-probe.py [--out models/datasets/context_probe_v1.jsonl] [--check]

Предел читается из backend/internal/ai/application/analysis.go (MaxContextMessages и
MaxContextRunes), поэтому набор следует за кодом: тест сверяет файл с генератором и пределом.
Метки пусты: зонд проверяет не качество разбора, а то, что самый длинный запрос, который
собирает продукт, умещается в контекст сервера вместе с ответом. Решает отчёт об оборудовании
(scripts/ai-hardware-report.py): ни одного отказа сервера, ни одного ответа, оборванного по длине,
запрос и ответ не больше контекста. В первом сообщении клиента и первом ответе компании стоят
признаки записи и обещания, чтобы запрос шёл по самому тяжёлому пути (с договорённостями).
Результат детерминирован.
"""
import argparse
import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
ANALYSIS = ROOT / "backend/internal/ai/application/analysis.go"
DEFAULT_OUT = ROOT / "models/datasets/context_probe_v1.jsonl"

INCOMING = [
    "Здравствуйте, подскажите, пожалуйста, как у вас устроена работа с постоянными клиентами и какие есть условия для тех, кто приходит регулярно.",
    "Мы уже пользовались вашими услугами в прошлом месяце, всё понравилось, но хотелось бы уточнить несколько деталей по поводу сроков и порядка работ.",
    "Дело в том, что в выходные я обычно занят, поэтому удобнее было бы обсуждать всё в будни, ближе к вечеру, когда я освобожусь с работы.",
    "Скажите, а можно ли заранее получить описание того, что именно входит в процедуру, и какие материалы вы используете в ходе работы.",
]
OUTGOING = [
    "Добрый день! Спасибо, что обратились к нам. Мы стараемся поддерживать долгие отношения с клиентами и всегда рады видеть вас снова.",
    "Рады, что вам понравилось. Подробности по срокам зависят от выбранной услуги и загрузки специалистов на выбранную дату.",
    "Понимаем, в будние дни действительно проще. Мы работаем с утра до позднего вечера, поэтому подберём удобное для вас окно.",
    "Описание процедуры и перечень материалов можно посмотреть в разделе с услугами, там же указана примерная длительность каждого этапа.",
]
BOOKING_ANCHOR = "Хочу записаться на услугу. "
PROMISE_ANCHOR = "Проверю расписание и отвечу. "


def limits(path=ANALYSIS):
    source = Path(path).read_text(encoding="utf-8")
    found = {name: int(re.search(rf"{name}\s*=\s*(\d+)", source).group(1)) for name in ("MaxContextMessages", "MaxContextRunes")}
    return found["MaxContextMessages"], found["MaxContextRunes"]


def body(index, direction, length):
    pool = INCOMING if direction == "INCOMING" else OUTGOING
    text = pool[index % len(pool)]
    while len(text) < length:
        text += " " + pool[(index + len(text)) % len(pool)]
    return text[:length].rstrip()


def conversation(name, count, length):
    messages = []
    for i in range(count):
        direction = "INCOMING" if i % 2 == 0 else "OUTGOING"
        text = body(i, direction, length)
        if i == 0:
            text = (BOOKING_ANCHOR + text)[:length]
        elif i == 1:
            text = (PROMISE_ANCHOR + text)[:length]
        messages.append({"id": f"{name}-message-{i + 1}", "direction": direction, "body": text})
    return {
        "version": "lidradar-ai-benchmark.v1", "id": name, "split": "DEV",
        "input": {
            "task": "ANALYZE_CONVERSATION", "schemaVersion": "analyze-conversation.v2", "promptVersion": "analyze-conversation.prompt.v9",
            "conversationId": f"synthetic-{name}", "baseConversationRevision": 1, "analysisThroughMessageId": messages[-1]["id"],
            "companyContext": "Салон услуг, основной язык русский, валюта RUB", "messages": messages,
        },
        "expectedFacts": [],
    }


def cases(max_messages, max_runes):
    """Предел, его половина и четверть при полном числе сообщений, длинные сообщения и короткая переписка."""
    context = len("Салон услуг, основной язык русский, валюта RUB")
    budget = max_runes - context
    shapes = [
        ("limit-many-short", max_messages, budget // max_messages),
        ("half-many-short", max_messages, budget // 2 // max_messages),
        ("quarter-many-short", max_messages, budget // 4 // max_messages),
        ("limit-few-long", 3, budget // 3),
        ("limit-ten", 10, budget // 10),
        ("typical-chat", 12, 120),
    ]
    return [conversation(name, count, length) for name, count, length in shapes]


def render(max_messages, max_runes):
    return "".join(json.dumps(case, ensure_ascii=False) + "\n" for case in cases(max_messages, max_runes))


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--out", default=str(DEFAULT_OUT))
    parser.add_argument("--check", action="store_true", help="сверить файл с генератором и пределом, ничего не записывая")
    args = parser.parse_args(argv)
    text = render(*limits())
    if args.check:
        if Path(args.out).read_text(encoding="utf-8") != text:
            print(f"{args.out} не совпадает с генератором и пределом из analysis.go: пересоберите без --check", file=sys.stderr)
            return 1
        print(f"{args.out} совпадает с генератором")
        return 0
    Path(args.out).write_text(text, encoding="utf-8")
    print(f"записано {len(text.splitlines())} случаев: {args.out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
