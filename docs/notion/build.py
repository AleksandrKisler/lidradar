#!/usr/bin/env python3
"""One-way documentation views and reproducible local handover (stdlib only)."""
from __future__ import annotations

import argparse
import hashlib
import io
import json
import os
import re
import sys
import zipfile
from pathlib import Path
from urllib.parse import quote, unquote, urlsplit

REPO = Path(__file__).resolve().parents[2]
ROOT = REPO / "docs/notion"
BATCH_FILES = 90  # Local batch size, not a claim about a Notion service limit.
VIEWS = {
    "LidRadar Backend.md": "backend/README.md",
    "01 Обзор системы.md": "backend/01-overview.md",
    "02 Глоссарий.md": "backend/00-glossary.md",
    "03 Архитектура.md": "backend/02-architecture.md",
    "04 Модули и зоны ответственности.md": "backend/03-modules.md",
    "05 HTTP API.md": "backend/04-api.md",
    "06 Модель данных.md": "backend/05-data-model.md",
    "07 Фоновая обработка.md": "backend/06-async-processing.md",
    "08 AI-контур.md": "backend/07-ai.md",
    "09 Защищённость.md": "backend/08-security.md",
    "10 Отказоустойчивость.md": "backend/09-reliability.md",
    "11 Эксплуатация и ёмкость.md": "backend/10-operations.md",
    "12 Тестирование и качество.md": "backend/11-testing.md",
    "13 Архитектурные решения ADR.md": "adr/README.md",
}
LINK = re.compile(r'!?\[[^\]\n]*\]\((<[^>]+>|[^)\n]+)\)')
FENCE = re.compile(r'^\s{0,3}(`{3,}|~{3,})')


def prose(text, strip_inline=True):
    """Ignore fenced and inline code; preserve lines for useful diagnostics."""
    result, fence = [], None
    for line in text.splitlines(keepends=True):
        match = FENCE.match(line)
        if match:
            marker = match.group(1)
            if fence is None:
                fence = marker
            elif marker[0] == fence[0] and len(marker) >= len(fence):
                fence = None
            result.append("\n")
        elif fence:
            result.append("\n")
        else:
            result.append(re.sub(r'(`+).*?\1', '', line) if strip_inline else line)
    return ''.join(result), fence is not None


def anchors(text):
    text, _ = prose(text, strip_inline=False)
    found = set(re.findall(r'<a\s+(?:id|name)=[\"\']([^\"\']+)', text))
    counts = {}
    for line in text.splitlines():
        match = re.match(r'^ {0,3}#{1,6}\s+(.+?)(?:\s+#+)?$', line)
        if not match:
            continue
        title = re.sub(r'<[^>]*>', '', match.group(1)).lower()
        title = re.sub(r'[^\w\- ]', '', title, flags=re.UNICODE).replace(' ', '-')
        count = counts.get(title, 0)
        counts[title] = count + 1
        found.add(title + (f'-{count}' if count else ''))
    return found


def destination(raw):
    return raw[1:-1] if raw.startswith('<') and raw.endswith('>') else re.split(r'\s+[\"\']', raw, maxsplit=1)[0]


def local_target(page, raw):
    url = urlsplit(destination(raw))
    if url.scheme or url.netloc:
        return None
    return ((page.parent / unquote(url.path)).resolve() if url.path else page.resolve(), unquote(url.fragment))


def validate_markdown(pages, root, allowed=None):
    root = root.resolve()
    errors, stats = [], {"pagesChecked": 0, "linksChecked": 0, "anchorsChecked": 0}
    for page in sorted(pages):
        text, unclosed = prose(page.read_text(encoding='utf-8'))
        stats['pagesChecked'] += 1
        if unclosed:
            errors.append(f'{page}: unclosed fence')
        for match in LINK.finditer(text):
            target = local_target(page, match.group(1))
            if target is None:
                continue
            stats['linksChecked'] += 1
            path, anchor = target
            if not path.is_relative_to(root):
                errors.append(f'{page}: outside root: {match.group(1)}')
            elif not path.exists():
                errors.append(f'{page}: missing target: {match.group(1)}')
            elif allowed is not None and path not in allowed and not (path.is_dir() and any(p.is_relative_to(path) for p in allowed)):
                errors.append(f'{page}: excluded target: {match.group(1)}')
            elif anchor and path.suffix == '.md':
                stats['anchorsChecked'] += 1
                if anchor not in anchors(path.read_text(encoding='utf-8')):
                    errors.append(f'{page}: missing anchor: {match.group(1)}')
    return errors, stats


def derived_text(source, target):
    data = source.read_bytes()
    fence = None
    def rebase(match):
        raw = match.group(1)
        resolved = local_target(source, raw)
        if resolved is None or not urlsplit(destination(raw)).path:
            return match.group(0)
        path, _ = resolved
        url = urlsplit(destination(raw))
        link = quote(os.path.relpath(path, target.parent.resolve()), safe='/.-_')
        if url.fragment:
            link += '#' + url.fragment
        return match.group(0).replace(raw, link)
    lines = []
    for line in data.decode('utf-8').splitlines(keepends=True):
        match = FENCE.match(line)
        if match:
            marker = match.group(1)
            if fence is None:
                fence = marker
            elif marker[0] == fence[0] and len(marker) >= len(fence):
                fence = None
            lines.append(line)
        elif fence:
            lines.append(line)
        else:
            pieces = re.split(r'(`+.*?`+)', line)
            lines.append(''.join(piece if piece.startswith('`') else LINK.sub(rebase, piece) for piece in pieces))
    origin = quote(os.path.relpath(source, target.parent), safe='/.-_')
    return f'<!-- GENERATED; source-sha256: {hashlib.sha256(data).hexdigest()} -->\n> Источник: [канонический документ]({origin}). Правки вносятся в источник.\n\n' + ''.join(lines)


def zip_bytes(entries):
    buffer = io.BytesIO()
    with zipfile.ZipFile(buffer, 'w', zipfile.ZIP_DEFLATED, compresslevel=9) as archive:
        for name, data in sorted(entries.items()):
            info = zipfile.ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
            info.compress_type = zipfile.ZIP_DEFLATED
            info.create_system = 3
            info.external_attr = 0o100644 << 16
            archive.writestr(info, data)
    return buffer.getvalue()


def emit(path, data, check):
    if path.exists() and path.read_bytes() == data:
        return
    if check:
        raise ValueError(f'stale or missing generated artifact: {path.relative_to(REPO)}')
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(data)


def partition(entries, metadata):
    if set(entries) & set(metadata):
        raise ValueError('metadata collides with source')
    ordered = sorted(entries.items())
    return [dict(ordered[start:start + BATCH_FILES]) for start in range(0, len(ordered), BATCH_FILES)]


def eligible(path):
    relative = path.relative_to(REPO)
    return not any(p.startswith('.') or p in ('runtime', 'backups', '__pycache__', 'node_modules') for p in relative.parts) and path.suffix not in ('.pyc', '.dump')


def collect():
    sources = {p.resolve() for p in (REPO / 'docs').rglob('*') if p.is_file() and eligible(p) and not (p.parent == ROOT and p.suffix == '.zip')}
    for name in ('AGENTS.md', 'README.md'):
        if (REPO / name).exists():
            sources.add(REPO / name)
    visited = set()
    while sources - visited:
        page = (sources - visited).pop()
        visited.add(page)
        if page.suffix != '.md':
            continue
        text, _ = prose(page.read_text(encoding='utf-8'))
        for match in LINK.finditer(text):
            target = local_target(page, match.group(1))
            if target is None:
                continue
            path, _ = target
            if path.is_relative_to(REPO) and path.is_file() and eligible(path):
                sources.add(path)
    return sources


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--check', action='store_true', help='verify without modifying anything')
    args = parser.parse_args()
    for name, canonical in VIEWS.items():
        target = ROOT / name
        emit(target, derived_text(REPO / 'docs' / canonical, target).encode(), args.check)
    sources = collect()
    errors, stats = validate_markdown([p for p in sources if p.suffix == '.md'], REPO, sources)
    entries = {p.relative_to(REPO).as_posix(): p.read_bytes() for p in sources}
    for name, data in entries.items():
        if data.startswith(b'version https://git-lfs.github.com/spec/v1'):
            errors.append(f'LFS pointer instead of content: {name}')
    if errors:
        raise ValueError('\n'.join(errors))
    inventory = []
    for name, data in sorted(entries.items()):
        kind = 'canonical'
        if name.startswith('docs/audit/legacy/') or name.startswith('docs/notion/images/'):
            kind = 'historical'
        elif name in {'docs/notion/' + view for view in VIEWS}:
            kind = 'generated'
        elif Path(name).suffix != '.md':
            kind = 'tool' if Path(name).suffix in ('.py', '.sh') else 'attachment'
        inventory.append({'path': name, 'kind': kind, 'sha256': hashlib.sha256(data).hexdigest(), 'bytes': len(data)})
    manifest = {'schemaVersion': 1, 'scope': 'local documentation; not runtime release approval',
                'notionImportStatus': 'NOT_EXECUTED', 'generatedViews': len(VIEWS), 'files': inventory}
    encode = lambda value: (json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + '\n').encode()
    metadata = {'MANIFEST.json': encode(manifest)}
    output = REPO / 'runtime/docs-package'
    emit(output / 'MANIFEST.json', metadata['MANIFEST.json'], args.check)
    emit(output / 'handover.zip', zip_bytes({**entries, **metadata}), args.check)
    batches = partition(entries, metadata)
    for index, batch in enumerate(batches, 1):
        emit(output / f'batch-{index:02}.zip', zip_bytes({**batch, **metadata}), args.check)
    # Do not leave obsolete batches after the source set shrinks.
    expected = {f'batch-{index:02}.zip' for index in range(1, len(batches) + 1)}
    for old in output.glob('batch-*.zip'):
        if old.name not in expected:
            if args.check:
                raise ValueError(f'obsolete batch: {old}')
            old.unlink()
    report = {**stats, 'files': len(entries), 'batches': len(batches), 'generatedViews': len(VIEWS),
              'localStatus': 'PASS', 'handoverSha256': hashlib.sha256(zip_bytes({**entries, **metadata})).hexdigest()}
    emit(output / 'BUILD_REPORT.json', encode(report), args.check)
    print(json.dumps(report, ensure_ascii=False, sort_keys=True))
    return 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (ValueError, OSError) as error:
        print(error, file=sys.stderr)
        sys.exit(1)
