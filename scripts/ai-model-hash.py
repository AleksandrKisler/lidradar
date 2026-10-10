#!/usr/bin/env python3
"""Отчёт о хэше файла весов: запускается НА УЗЛЕ, печатает JSON (ADR 0053).

  ssh узел python3 - /srv/lidradar/models/Qwen3-8B-Q4_K_M.gguf < scripts/ai-model-hash.py > models/reports/…-model-hash.json

Хэш нужен манифесту (`ai-manifest.py --model-hash-report`): он сверяется с `modelSHA256`
прежней квалификации. Скрипт читает файл только на чтение и ничего не меняет. Путь — файл,
который контейнер llama.cpp монтирует как /models (по умолчанию /srv/lidradar/models).
"""
import hashlib
import json
import os
import socket
import sys
from datetime import datetime, timezone

path = sys.argv[1] if len(sys.argv) > 1 else "/srv/lidradar/models/Qwen3-8B-Q4_K_M.gguf"
digest = hashlib.sha256()
with open(path, "rb") as handle:
    for chunk in iter(lambda: handle.read(8 << 20), b""):
        digest.update(chunk)
info = os.stat(path)
print(json.dumps({
    "checkedAt": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    "host": socket.gethostname(),
    "path": path,
    "sizeBytes": info.st_size,
    "modifiedAt": datetime.fromtimestamp(info.st_mtime, timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    "sha256": digest.hexdigest(),
}, ensure_ascii=False, indent=2))
