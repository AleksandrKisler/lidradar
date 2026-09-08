#!/usr/bin/env python3
"""Сборка издания документации для Notion.

Рисует графики в images/, проверяет относительные ссылки и картинки во всех
страницах и собирает zip для импорта. Запуск из корня репозитория или из
каталога docs/notion; нужен matplotlib (см. README.md).
"""

from __future__ import annotations

import re
import sys
import zipfile
from pathlib import Path
from urllib.parse import unquote

ROOT = Path(__file__).resolve().parent
IMAGES = ROOT / "images"
ARCHIVE = ROOT / "lidradar-backend-notion.zip"

# Палитра: одна серия — один цвет (слот 1 эталонной палитры), хром — чернила
# и hairline-сетка; текст никогда не окрашивается в цвет серии.
SERIES = "#2a78d6"
SURFACE = "#fcfcfb"
INK = "#0b0b0b"
INK_SECONDARY = "#52514e"
INK_MUTED = "#898781"
GRID = "#e1e0d9"
BASELINE = "#c3c2b7"


def configure_matplotlib():
    import matplotlib

    matplotlib.use("Agg")
    import matplotlib.pyplot as plt

    plt.rcParams.update(
        {
            "font.family": "sans-serif",
            "font.sans-serif": ["Helvetica Neue", "Arial", "DejaVu Sans"],
            "font.size": 10,
            "axes.edgecolor": BASELINE,
            "axes.labelcolor": INK_SECONDARY,
            "axes.titlecolor": INK,
            "axes.titleweight": "semibold",
            "axes.titlesize": 12,
            "axes.titlelocation": "left",
            "xtick.color": INK_MUTED,
            "ytick.color": INK_MUTED,
            "xtick.labelcolor": INK_SECONDARY,
            "ytick.labelcolor": INK_SECONDARY,
            "figure.facecolor": SURFACE,
            "axes.facecolor": SURFACE,
            "savefig.facecolor": SURFACE,
            "savefig.dpi": 200,
        }
    )
    return plt


def style_axes(ax, *, horizontal: bool):
    for side in ("top", "right"):
        ax.spines[side].set_visible(False)
    if horizontal:
        ax.spines["left"].set_visible(False)
        ax.xaxis.grid(True, color=GRID, linewidth=0.8)
        ax.set_axisbelow(True)
        ax.tick_params(axis="y", length=0)
        ax.tick_params(axis="x", length=0)
    else:
        ax.spines["bottom"].set_visible(False)
        ax.yaxis.grid(True, color=GRID, linewidth=0.8)
        ax.set_axisbelow(True)
        ax.tick_params(axis="x", length=0)
        ax.tick_params(axis="y", length=0)


def subtitle(fig, text: str):
    fig.text(0.01, 0.905, text, color=INK_SECONDARY, fontsize=9, ha="left", va="top")


def chart_api_p95(plt):
    endpoints = [
        ("GET /analytics/summary", 122.7),
        ("GET /radar", 49.2),
        ("GET /risks", 21.6),
        ("GET /conversations/{id}/messages", 19.0),
        ("GET /opportunities/{id}", 13.3),
        ("GET /conversations", 5.2),
    ]
    endpoints.sort(key=lambda item: item[1])
    labels = [name for name, _ in endpoints]
    values = [value for _, value in endpoints]
    fig, ax = plt.subplots(figsize=(8, 4.2))
    fig.subplots_adjust(left=0.36, right=0.97, top=0.78, bottom=0.14)
    bars = ax.barh(labels, values, color=SERIES, height=0.55)
    style_axes(ax, horizontal=True)
    ax.set_xlim(0, 320)
    ax.set_xlabel("p95, мс", color=INK_SECONDARY)
    ax.axvline(300, color=INK_SECONDARY, linewidth=1)
    ax.text(300, len(labels) - 0.45, "цель < 300 мс", color=INK_SECONDARY, fontsize=9, ha="right", va="bottom")
    for bar, value in zip(bars, values):
        ax.text(bar.get_width() + 4, bar.get_y() + bar.get_height() / 2, f"{value:.1f}".replace(".", ","), va="center", color=INK, fontsize=9)
    fig.suptitle("API без AI: p95 по конечным точкам", x=0.01, ha="left", color=INK, fontsize=12, fontweight="semibold")
    subtitle(fig, "300 запросов на точку, 16 параллельных, 100 организаций × 500 переписок × 10 сообщений")
    fig.savefig(IMAGES / "api-p95.png")
    plt.close(fig)


def chart_worker_throughput(plt):
    labels = ["1 процесс worker", "4 процесса worker"]
    values = [170.9, 479.8]
    fig, ax = plt.subplots(figsize=(6.4, 4.0))
    fig.subplots_adjust(left=0.12, right=0.97, top=0.78, bottom=0.14)
    bars = ax.bar(labels, values, color=SERIES, width=0.45)
    style_axes(ax, horizontal=False)
    ax.set_ylim(0, 560)
    ax.set_ylabel("заданий в секунду", color=INK_SECONDARY)
    for bar, value in zip(bars, values):
        ax.text(bar.get_x() + bar.get_width() / 2, bar.get_height() + 10, f"{value:.0f}", ha="center", color=INK, fontsize=10)
    fig.suptitle("Пропускная способность обработчика заданий", x=0.01, ha="left", color=INK, fontsize=12, fontweight="semibold")
    subtitle(fig, "1 600 заданий одного всплеска из 400 вебхуков; рост в 2,8 раза при четырёх процессах")
    fig.savefig(IMAGES / "worker-throughput.png")
    plt.close(fig)


def chart_ai_capacity(plt):
    labels = [
        "ёмкость узла при p50 вывода (1,8 с)",
        "ёмкость узла при p95 вывода (3,85 с)",
        "средняя нагрузка (500 000 сообщений в месяц)",
        "вечерний пик (в 3 раза выше среднего)",
    ]
    values = [0.56, 0.26, 0.19, 0.57]
    fig, ax = plt.subplots(figsize=(8, 4.0))
    fig.subplots_adjust(left=0.47, right=0.97, top=0.78, bottom=0.16)
    bars = ax.barh(labels[::-1], values[::-1], color=SERIES, height=0.5)
    style_axes(ax, horizontal=True)
    ax.set_xlim(0, 0.7)
    from matplotlib.ticker import FuncFormatter

    ax.xaxis.set_major_formatter(FuncFormatter(lambda value, _: f"{value:.1f}".replace(".", ",")))
    ax.set_xlabel("заданий анализа в секунду", color=INK_SECONDARY)
    for bar, value in zip(bars, values[::-1]):
        ax.text(bar.get_width() + 0.01, bar.get_y() + bar.get_height() / 2, f"{value:.2f}".replace(".", ","), va="center", color=INK, fontsize=9)
    fig.suptitle("AI-узел: ёмкость и нагрузка", x=0.01, ha="left", color=INK, fontsize=12, fontweight="semibold")
    subtitle(fig, "RTX 4060, один слот; пик превышает ёмкость — очередь растёт до конца пика")
    fig.savefig(IMAGES / "ai-capacity.png")
    plt.close(fig)


def chart_retry_schedule(plt):
    labels = ["попытка 2", "попытка 3", "попытка 4", "попытка 5"]
    seconds = [5, 30, 120, 600]
    texts = ["5 с", "30 с", "2 мин", "10 мин"]
    fig, ax = plt.subplots(figsize=(6.4, 4.0))
    fig.subplots_adjust(left=0.12, right=0.97, top=0.78, bottom=0.14)
    bars = ax.bar(labels, seconds, color=SERIES, width=0.45)
    style_axes(ax, horizontal=False)
    ax.set_ylim(0, 680)
    ax.set_ylabel("задержка перед попыткой, с", color=INK_SECONDARY)
    for bar, text in zip(bars, texts):
        ax.text(bar.get_x() + bar.get_width() / 2, bar.get_height() + 12, text, ha="center", color=INK, fontsize=10)
    fig.suptitle("Расписание повторов", x=0.01, ha="left", color=INK, fontsize=12, fontweight="semibold")
    subtitle(fig, "одно расписание для событий outbox, заданий и доставок; после пятой попытки — DEAD")
    fig.savefig(IMAGES / "retry-schedule.png")
    plt.close(fig)


def render_charts():
    plt = configure_matplotlib()
    IMAGES.mkdir(exist_ok=True)
    chart_api_p95(plt)
    chart_worker_throughput(plt)
    chart_ai_capacity(plt)
    chart_retry_schedule(plt)


def pages() -> list[Path]:
    return sorted(
        path
        for path in ROOT.glob("*.md")
        if path.name != "README.md" and not path.name.startswith("_")
    )


LINK = re.compile(r"!?\[[^\]]*\]\(([^)]+)\)")


def check_links() -> list[str]:
    problems: list[str] = []
    for page in pages():
        text = page.read_text(encoding="utf-8")
        for match in LINK.finditer(text):
            target = match.group(1)
            if target.startswith(("http://", "https://", "mailto:")):
                continue
            path = unquote(target.split("#", 1)[0])
            if not path:
                continue
            if not (ROOT / path).exists():
                problems.append(f"{page.name}: ссылка на отсутствующий файл {target}")
        if text.count("```") % 2:
            problems.append(f"{page.name}: незакрытый блок кода")
    return problems


def build_archive() -> int:
    if ARCHIVE.exists():
        ARCHIVE.unlink()
    count = 0
    with zipfile.ZipFile(ARCHIVE, "w", zipfile.ZIP_DEFLATED) as archive:
        for page in pages():
            archive.write(page, page.name)
            count += 1
        for image in sorted(IMAGES.glob("*.png")):
            archive.write(image, f"images/{image.name}")
            count += 1
    return count


def main() -> int:
    render_charts()
    problems = check_links()
    if problems:
        for problem in problems:
            print("ОШИБКА:", problem, file=sys.stderr)
        return 1
    files = build_archive()
    print(f"страниц: {len(pages())}, файлов в архиве: {files}, архив: {ARCHIVE}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
